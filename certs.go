package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"software.sslmate.com/src/go-pkcs12"
)

const (
	caValidity = 10 * 365 * 24 * time.Hour
	// Apple rejects TLS server certificates valid for longer than 825 days, even from a user-trusted CA.
	serverValidity = 825 * 24 * time.Hour
	clientValidity = 2 * 365 * 24 * time.Hour
)

const certsUsage = `usage:
  taskboard certs init   -host NAMES [-config FILE] [-dir DIR]
  taskboard certs server -host NAMES [-config FILE] [-dir DIR]
  taskboard certs client NAME        [-config FILE] [-dir DIR]

NAMES is a comma-separated list of the host names and IP addresses browsers use to reach taskboard.`

func runCerts(args []string) error {
	if len(args) == 0 {
		return errors.New(certsUsage)
	}
	cmd, args := args[0], args[1:]
	var name string
	if cmd == "client" {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return errors.New(certsUsage)
		}
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("certs "+cmd, flag.ContinueOnError)
	configPath := fs.String("config", "taskboard.json", "config file whose tls_dir holds the certificates")
	dir := fs.String("dir", "", "certificate folder, instead of the config's tls_dir")
	hosts := fs.String("host", "", "comma-separated host names and IP addresses")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		cfg, err := LoadConfig(*configPath)
		if err != nil {
			return fmt.Errorf("%w (or pass -dir)", err)
		}
		*dir = cfg.tlsDir(*configPath)
	}
	now := time.Now()
	switch cmd {
	case "init":
		return certsInit(*dir, splitHosts(*hosts), now)
	case "server":
		return certsServer(*dir, splitHosts(*hosts), now)
	case "client":
		return certsClient(*dir, name, now)
	}
	return errors.New(certsUsage)
}

func splitHosts(s string) []string {
	var hosts []string
	for h := range strings.SplitSeq(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

func certsInit(dir string, hosts []string, now time.Time) error {
	if len(hosts) == 0 {
		return errors.New("-host is required, e.g. -host ubuntu,ubuntu.tailnet.ts.net,100.64.0.5")
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err == nil {
		return fmt.Errorf("%s already has a CA; replacing it would invalidate every client certificate. Use certs server to re-issue the server certificate", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ca, caKey, err := newCA(now)
	if err != nil {
		return err
	}
	if err := writeCertAndKey(dir, "ca", ca, caKey); err != nil {
		return err
	}
	if err := certsServer(dir, hosts, now); err != nil {
		return err
	}
	fmt.Printf("Install %s on each device as a trusted root, then issue each one a client certificate with certs client.\n", filepath.Join(dir, "ca.crt"))
	return nil
}

func certsServer(dir string, hosts []string, now time.Time) error {
	if len(hosts) == 0 {
		return errors.New("-host is required")
	}
	ca, caKey, err := loadCA(dir)
	if err != nil {
		return err
	}
	cert, key, err := issue(ca, caKey, serverTemplate(hosts, now))
	if err != nil {
		return err
	}
	if err := writeCertAndKey(dir, "server", cert, key); err != nil {
		return err
	}
	fmt.Printf("Wrote the server certificate for %s, valid until %s. Restart taskboard to use it.\n",
		strings.Join(slices.Concat(cert.DNSNames, ipStrings(cert.IPAddresses)), ", "), cert.NotAfter.Format("2006-01-02"))
	return nil
}

func certsClient(dir, name string, now time.Time) error {
	if strings.ContainsAny(name, `/\`) || name == "" || strings.HasPrefix(name, ".") {
		return fmt.Errorf("invalid client name %q", name)
	}
	out := filepath.Join(dir, name+".p12")
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("%s already exists", out)
	}
	ca, caKey, err := loadCA(dir)
	if err != nil {
		return err
	}
	cert, key, err := issue(ca, caKey, clientTemplate(name, now))
	if err != nil {
		return err
	}
	password, err := randomPassword()
	if err != nil {
		return err
	}
	p12, err := encodeP12(key, cert, ca, password)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, p12, 0o600); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n  password: %s\n  serial:   %s (add to revoked_certs to cut this device off)\n",
		out, password, serialHex(cert.SerialNumber))
	return nil
}

func newCA(now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "taskboard CA", Organization: []string{"taskboard"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func serverTemplate(hosts []string, now time.Time) *x509.Certificate {
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: hosts[0], Organization: []string{"taskboard"}},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(serverValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range slices.Concat(hosts, []string{"localhost", "127.0.0.1", "::1"}) {
		if ip := net.ParseIP(h); ip != nil {
			if !slices.ContainsFunc(tmpl.IPAddresses, ip.Equal) {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			}
		} else if !slices.Contains(tmpl.DNSNames, h) {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	return tmpl
}

func clientTemplate(name string, now time.Time) *x509.Certificate {
	return &x509.Certificate{
		Subject:     pkix.Name{CommonName: name, Organization: []string{"taskboard"}},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(clientValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
}

func issue(ca *x509.Certificate, caKey crypto.Signer, tmpl *x509.Certificate) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	if tmpl.SerialNumber, err = randomSerial(); err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

// Legacy encryption, because macOS and iOS Keychain cannot import the modern PKCS#12 algorithms.
func encodeP12(key *ecdsa.PrivateKey, cert, ca *x509.Certificate, password string) ([]byte, error) {
	return pkcs12.Legacy.Encode(key, cert, []*x509.Certificate{ca}, password)
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func randomPassword() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func serialHex(n *big.Int) string { return n.Text(16) }

func ipStrings(ips []net.IP) []string {
	s := make([]string, len(ips))
	for i, ip := range ips {
		s[i] = ip.String()
	}
	return s
}

func writeCertAndKey(dir, name string, cert *x509.Certificate, key *ecdsa.PrivateKey) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644)
}

func loadCA(dir string) (*x509.Certificate, crypto.Signer, error) {
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, nil, fmt.Errorf("load CA from %s (run taskboard certs init first): %w", dir, err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("CA key cannot sign")
	}
	return cert, signer, nil
}

// serverTLS requires a client certificate from taskboard's CA. Revocation is checked against the live
// config on every handshake, so revoking a device needs no restart.
func serverTLS(dir string, revoked func() []string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if err != nil {
		return nil, fmt.Errorf("load server certificate from %s (run taskboard certs init): %w", dir, err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s has no certificates", filepath.Join(dir, "ca.crt"))
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) > 0 && isRevoked(revoked(), cs.PeerCertificates[0]) {
				return errors.New("client certificate revoked")
			}
			return nil
		},
	}, nil
}

func isRevoked(revoked []string, cert *x509.Certificate) bool {
	return slices.Contains(revoked, serialHex(cert.SerialNumber))
}
