package main

import (
	"cmp"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"rsc.io/qr"
)

const (
	pairingTTL    = 10 * time.Minute
	minPassphrase = 12
	// Crockford's base32: no I, L, O or U, so a typed code is hard to get wrong.
	pairAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	pairCodeLen  = 8
)

// A pairing hands one new device its certificate. The code in the QR is good for one open; that open gets a
// secret path for the downloads, so a second device with the same code gets nothing.
type pairing struct {
	code     string
	name     string
	serial   string
	p12      []byte
	ca       []byte
	expires  time.Time
	secret   string
	openedBy string
	openedAt time.Time
}

type pairings struct {
	mu sync.Mutex
	m  map[string]*pairing
}

func (ps *pairings) add(p *pairing, now time.Time) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.pruneLocked(now)
	if ps.m == nil {
		ps.m = map[string]*pairing{}
	}
	ps.m[p.code] = p
}

func (ps *pairings) pruneLocked(now time.Time) {
	for code, p := range ps.m {
		if !now.Before(p.expires) {
			delete(ps.m, code)
		}
	}
}

func (ps *pairings) lookupLocked(code string, now time.Time) *pairing {
	ps.pruneLocked(now)
	return ps.m[normalizePairCode(code)]
}

func (ps *pairings) open(code, remote string, now time.Time) (string, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.lookupLocked(code, now)
	if p == nil || p.secret != "" {
		return "", false
	}
	secret, err := randomToken(16)
	if err != nil {
		return "", false
	}
	p.secret, p.openedBy, p.openedAt, p.expires = secret, remote, now, now.Add(pairingTTL)
	return secret, true
}

func (ps *pairings) get(code, secret string, now time.Time) (pairing, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.lookupLocked(code, now)
	if p == nil || p.secret == "" || subtle.ConstantTimeCompare([]byte(p.secret), []byte(secret)) != 1 {
		return pairing{}, false
	}
	return *p, true
}

func (ps *pairings) status(code string, now time.Time) (pairing, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if p := ps.lookupLocked(code, now); p != nil {
		return *p, true
	}
	return pairing{}, false
}

func (ps *pairings) cancel(code string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.m, normalizePairCode(code))
}

func newPairCode() (string, error) {
	b := make([]byte, pairCodeLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = pairAlphabet[b[i]%byte(len(pairAlphabet))]
	}
	return string(b), nil
}

// normalizePairCode accepts a typed code in any case, with or without the dash, and with the letters
// Crockford's base32 reads as digits.
func normalizePairCode(s string) string {
	return strings.NewReplacer("-", "", " ", "", "I", "1", "L", "1", "O", "0").Replace(strings.ToUpper(s))
}

func displayPairCode(code string) string {
	return code[:pairCodeLen/2] + "-" + code[pairCodeLen/2:]
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func randomUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// mobileconfig is an Apple configuration profile that installs the CA as a trusted root and the device's
// identity. It leaves out the .p12 password, so the device asks for the passphrase while installing.
func mobileconfig(name, serial string, caPEM, p12 []byte) []byte {
	block, _ := pem.Decode(caPEM)
	id := "com.github.wiggin77.taskboard.device." + serial
	var b strings.Builder
	str := func(s string) string {
		var e strings.Builder
		xml.EscapeText(&e, []byte(s))
		return "<string>" + e.String() + "</string>"
	}
	payload := func(typ, display, file, suffix string, data []byte) {
		fmt.Fprintf(&b, `<dict>
<key>PayloadCertificateFileName</key>%s
<key>PayloadContent</key><data>%s</data>
<key>PayloadDisplayName</key>%s
<key>PayloadIdentifier</key>%s
<key>PayloadType</key>%s
<key>PayloadUUID</key>%s
<key>PayloadVersion</key><integer>1</integer>
</dict>
`, str(file), base64.StdEncoding.EncodeToString(data), str(display), str(id+suffix), str(typ), str(randomUUID()))
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
<key>PayloadContent</key>
<array>
`)
	payload("com.apple.security.root", "taskboard CA", "taskboard-ca.crt", ".ca", block.Bytes)
	payload("com.apple.security.pkcs12", "taskboard "+name, name+".p12", ".identity", p12)
	fmt.Fprintf(&b, `</array>
<key>PayloadDescription</key>%s
<key>PayloadDisplayName</key>%s
<key>PayloadIdentifier</key>%s
<key>PayloadType</key><string>Configuration</string>
<key>PayloadUUID</key>%s
<key>PayloadVersion</key><integer>1</integer>
</dict>
</plist>
`, str("Lets this device use taskboard remotely."), str("taskboard ("+name+")"), str(id), str(randomUUID()))
	return []byte(b.String())
}

func qrDataURL(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 4
	n := code.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n, n, n)
	for y := range code.Size {
		for x := range code.Size {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(b.String())), nil
}

type remoteAccessData struct {
	Dir     string
	CA      *certInfo
	Server  *certInfo
	Hosts   string
	Running bool
	Error   string

	MinPassphrase int
}

type certInfo struct {
	Names   []string
	Expires time.Time
}

func (s *server) remoteAccess() remoteAccessData {
	d := remoteAccessData{Dir: s.certDir, Running: s.tlsListen != "", MinPassphrase: minPassphrase}
	ca, err := readCert(filepath.Join(s.certDir, "ca.crt"))
	if err != nil {
		if !os.IsNotExist(err) {
			d.Error = err.Error()
		}
		d.Hosts = strings.Join(suggestHosts(), ", ")
		return d
	}
	d.CA = &certInfo{Expires: ca.NotAfter}
	server, err := readCert(filepath.Join(s.certDir, "server.crt"))
	if err != nil {
		d.Error = err.Error()
		d.Hosts = strings.Join(suggestHosts(), ", ")
		return d
	}
	d.Server = &certInfo{Names: pairHosts(server), Expires: server.NotAfter}
	d.Hosts = strings.Join(d.Server.Names, ", ")
	return d
}

// pairHosts are the server certificate's names other devices can use, so not the loopback ones every
// server certificate gets.
func pairHosts(server *x509.Certificate) []string {
	return slices.DeleteFunc(certNames(server), func(h string) bool {
		return h == "localhost" || net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback()
	})
}

func (s *server) canChange(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Taskboard") != "1" || s.accessOf(r) < remote {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	if err == nil {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// createCerts makes the CA and server certificate, or with a CA already there re-issues the server certificate.
func (s *server) createCerts(w http.ResponseWriter, r *http.Request) {
	if !s.canChange(w, r) {
		return
	}
	var req struct {
		Hosts string `json:"hosts"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	hosts := splitHosts(req.Hosts)
	if len(hosts) == 0 {
		http.Error(w, "enter at least one name or address", http.StatusBadRequest)
		return
	}
	s.certsMu.Lock()
	defer s.certsMu.Unlock()
	issueFn := initCerts
	if _, err := os.Stat(filepath.Join(s.certDir, "ca.crt")); err == nil {
		issueFn = issueServer
	}
	cert, err := issueFn(s.certDir, hosts, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"names": pairHosts(cert), "expires": cert.NotAfter})
}

type pairLink struct {
	Host string `json:"host"`
	URL  string `json:"url"`
	QR   string `json:"qr"`
}

func (s *server) addDevice(w http.ResponseWriter, r *http.Request) {
	if !s.canChange(w, r) {
		return
	}
	if s.tlsListen == "" {
		http.Error(w, "the remote access address isn't running; set it, save and restart taskboard", http.StatusConflict)
		return
	}
	var req struct {
		Name       string `json:"name"`
		Passphrase string `json:"passphrase"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name, passphrase := strings.TrimSpace(req.Name), strings.TrimSpace(req.Passphrase)
	if !validClientName(name) {
		http.Error(w, "enter a device name of up to 64 letters, digits, spaces, dots, dashes or underscores", http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(passphrase) < minPassphrase {
		http.Error(w, fmt.Sprintf("the passphrase needs at least %d characters", minPassphrase), http.StatusBadRequest)
		return
	}
	server, err := readCert(filepath.Join(s.certDir, "server.crt"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, port, err := net.SplitHostPort(s.tlsListen)
	if err != nil {
		http.Error(w, "remote access address: "+err.Error(), http.StatusInternalServerError)
		return
	}
	code, err := newPairCode()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hosts := pairHosts(server)
	if len(hosts) == 0 {
		http.Error(w, "the server certificate has no name other devices can use; re-issue it", http.StatusConflict)
		return
	}
	// A device adding another one lists the name it is using first, since that one works from here.
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		if i := slices.Index(hosts, strings.Trim(h, "[]")); i > 0 {
			hosts = slices.Concat(hosts[i:i+1], hosts[:i], hosts[i+1:])
		}
	}
	var links []pairLink
	for _, h := range hosts {
		u := "https://" + net.JoinHostPort(h, port) + "/pair/" + displayPairCode(code)
		img, err := qrDataURL(u)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		links = append(links, pairLink{Host: h, URL: u, QR: img})
	}
	caPEM, err := os.ReadFile(filepath.Join(s.certDir, "ca.crt"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	cert, p12, err := issueClient(s.certDir, name, passphrase, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	serial := serialHex(cert.SerialNumber)
	s.pairings.add(&pairing{code: code, name: name, serial: serial, p12: p12, ca: caPEM, expires: now.Add(pairingTTL)}, now)
	log.Printf("issued a client certificate for %q, serial %s", name, serial)
	writeJSON(w, http.StatusOK, map[string]any{
		"code":    code,
		"display": displayPairCode(code),
		"serial":  serial,
		"expires": now.Add(pairingTTL),
		"links":   links,
	})
}

func (s *server) deviceStatus(w http.ResponseWriter, r *http.Request) {
	if s.accessOf(r) < remote {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	p, ok := s.pairings.status(r.PathValue("code"), time.Now())
	switch {
	case !ok:
		writeJSON(w, http.StatusOK, map[string]any{"state": "expired"})
	case p.secret == "":
		writeJSON(w, http.StatusOK, map[string]any{"state": "waiting", "expires": p.expires})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"state": "opened", "by": p.openedBy, "at": p.openedAt})
	}
}

func (s *server) cancelDevice(w http.ResponseWriter, r *http.Request) {
	if !s.canChange(w, r) {
		return
	}
	s.pairings.cancel(r.PathValue("code"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) revokeCert(w http.ResponseWriter, r *http.Request) {
	if !s.canChange(w, r) {
		return
	}
	var req struct {
		Serial string `json:"serial"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	serial := normalizeSerial(req.Serial)
	if serial == "" {
		http.Error(w, "no serial", http.StatusBadRequest)
		return
	}
	s.certsMu.Lock()
	defer s.certsMu.Unlock()
	cfg := *s.board.Config()
	if !slices.Contains(cfg.RevokedCerts, serial) {
		cfg.RevokedCerts = append(slices.Clone(cfg.RevokedCerts), serial)
		if err := SaveConfig(s.configPath, &cfg); err != nil {
			http.Error(w, "save "+s.configPath+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.board.SetConfig(&cfg)
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked_certs": cfg.RevokedCerts})
}

// Pairing pages are served only over TLS, so the .p12 never crosses the network in the clear.
func (s *server) pairOpen(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil {
		http.NotFound(w, r)
		return
	}
	code := r.PathValue("code")
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	secret, ok := s.pairings.open(code, host, time.Now())
	if !ok {
		renderStatus(w, r, http.StatusNotFound, "pair.html", pairData{})
		return
	}
	http.Redirect(w, r, "/pair/"+normalizePairCode(code)+"/"+secret+"/", http.StatusSeeOther)
}

type pairData struct {
	Name string
	// Primary is the steps for the browser that opened the page, when it is recognised; Others are the rest.
	Primary *pairSteps
	Others  []pairSteps
}

// pairSteps picks a block of the pair page's "steps" template. Browser and Device name what the user is on,
// or are empty in the list of other devices.
type pairSteps struct {
	Key     string
	Title   string
	Browser string
	Device  string
	P12     string
}

func (p pairSteps) BrowserName() string { return cmp.Or(p.Browser, "your browser") }

var pairStepTitles = []struct{ key, title string }{
	{"ios", "iPhone or iPad"},
	{"mac", "Mac with Safari, Chrome or Edge"},
	{"android", "Android"},
	{"windows", "Windows with Chrome or Edge"},
	{"chromeos", "Chromebook"},
	{"linux", "Linux with Chrome, Edge or Chromium"},
	{"firefox", "Firefox on Windows, Mac or Linux"},
}

// pairPlatform reads the browser and device from a User-Agent. The key follows the certificate store the browser
// uses, so desktop Firefox, which keeps its own, is "firefox" whatever the OS.
func pairPlatform(ua string) (key, browser, device string) {
	has := func(subs ...string) bool {
		return slices.ContainsFunc(subs, func(sub string) bool { return strings.Contains(ua, sub) })
	}
	switch {
	case has("FxiOS/", "Firefox/"):
		browser = "Firefox"
	case has("EdgiOS/", "EdgA/", "Edg/"):
		browser = "Edge"
	case has("SamsungBrowser/"):
		browser = "Samsung Internet"
	case has("OPR/", "OPiOS/"):
		browser = "Opera"
	case has("CriOS/", "Chrome/"):
		browser = "Chrome"
	case has("Safari/"):
		browser = "Safari"
	}
	switch {
	case has("iPhone", "iPod"):
		device = "iPhone"
	case has("iPad"):
		device = "iPad"
	case has("Android"):
		device = "Android"
	case has("Macintosh"):
		device = "Mac"
	case has("Windows"):
		device = "Windows"
	case has("CrOS"):
		device = "Chromebook"
	case has("Linux", "X11"):
		device = "Linux"
	default:
		return "", "", ""
	}
	switch {
	case device == "iPhone" || device == "iPad":
		key = "ios"
	case device == "Android":
		key = "android"
	case browser == "Firefox":
		key = "firefox"
	default:
		key = map[string]string{"Mac": "mac", "Windows": "windows", "Chromebook": "chromeos", "Linux": "linux"}[device]
	}
	return key, browser, device
}

func newPairData(name, ua string) pairData {
	d := pairData{Name: name}
	key, browser, device := pairPlatform(ua)
	for _, t := range pairStepTitles {
		steps := pairSteps{Key: t.key, Title: t.title, P12: name + ".p12"}
		if t.key == key {
			steps.Browser, steps.Device = browser, device
			steps.Title = cmp.Or(browser, "Your browser") + " on " + device
			d.Primary = &steps
			continue
		}
		d.Others = append(d.Others, steps)
	}
	return d
}

func (s *server) pairPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pairingFor(r)
	if !ok {
		renderStatus(w, r, http.StatusNotFound, "pair.html", pairData{})
		return
	}
	render(w, r, "pair.html", newPairData(p.name, r.UserAgent()))
}

func (s *server) pairFile(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pairingFor(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var body []byte
	switch file := r.PathValue("file"); file {
	case "taskboard.mobileconfig":
		w.Header().Set("Content-Type", "application/x-apple-aspen-config")
		body = mobileconfig(p.name, p.serial, p.ca, p.p12)
	case "taskboard-ca.crt":
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		body = p.ca
	case p.name + ".p12":
		w.Header().Set("Content-Type", "application/x-pkcs12")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", file))
		body = p.p12
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

func (s *server) pairingFor(r *http.Request) (pairing, bool) {
	if r.TLS == nil {
		return pairing{}, false
	}
	return s.pairings.get(r.PathValue("code"), r.PathValue("secret"), time.Now())
}
