package main

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"ageTone":   func(t time.Time) string { return ageTone(t, time.Now()) },
	"dateLabel": func(t time.Time) string { return dateLabel(t, time.Now()) },
	"isToday":   func(t time.Time) bool { return isToday(t, time.Now()) },
	"iso":       func(t time.Time) string { return t.Format(time.RFC3339) },
	"slug":      slug,
	"join":      strings.Join,
}).ParseFS(templateFS, "templates/*.html"))

func slug(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), " ", "-")
}

func ageTone(t, now time.Time) string {
	switch age := now.Sub(t); {
	case age > 7*24*time.Hour:
		return "old"
	case age > 2*24*time.Hour:
		return "stale"
	}
	return "fresh"
}

func isToday(t, now time.Time) bool {
	ty, tm, td := t.In(now.Location()).Date()
	ny, nm, nd := now.Date()
	return ty == ny && tm == nm && td == nd
}

// For today it returns just "today"; templates add the time so the browser can format it.
func dateLabel(t, now time.Time) string {
	switch {
	case isToday(t, now):
		return "today"
	case t.In(now.Location()).Year() == now.Year():
		return t.In(now.Location()).Format("Jan 2")
	}
	return t.In(now.Location()).Format("Jan 2, 2006")
}

type server struct {
	board      *Board
	configPath string
	listen     string
}

type group struct {
	Name  string
	Tasks []Task
}

type prSection struct {
	Name string
	PRs  []PRLink
}

type navItem struct {
	Name  string
	Count int
	Tone  string
}

type indexData struct {
	Groups         []group
	PRSections     []prSection
	PRsFirst       bool
	Nav            []navItem
	Errors         []string
	Updated        time.Time
	Refreshing     bool
	Version        string
	RefreshSeconds int
	CanAttach      bool
	Local          bool
	Stations       []string
}

type settingsData struct {
	Config     *Config
	ConfigPath string
	Listen     string
	NewRepo    RepoConfig
}

type planData struct {
	Task    Task
	Content string
}

func newServer(board *Board, configPath string) http.Handler {
	s := &server{board: board, configPath: configPath, listen: board.Config().Listen}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /version", s.version)
	mux.HandleFunc("GET /plan/{key}", s.plan)
	mux.HandleFunc("POST /attach/{key}", s.attach)
	mux.HandleFunc("POST /refresh", s.refresh)
	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("POST /settings", s.saveSettings)
	return s.checkHost(mux)
}

// Rejecting unknown hostnames blocks DNS rebinding, since this server has no auth and can open terminals.
func (s *server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		host = strings.Trim(host, "[]")
		if host != "localhost" && net.ParseIP(host) == nil && !slices.Contains(s.board.Config().AllowedHosts, host) {
			http.Error(w, "unknown host "+host+"; add it to allowed_hosts", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	cfg := s.board.Config()
	s.board.Viewed()
	snap := s.board.Snapshot()
	byGroup := map[string][]Task{}
	for _, t := range snap.Tasks {
		byGroup[t.Group()] = append(byGroup[t.Group()], t)
	}
	data := indexData{
		Errors:         snap.Errors,
		Updated:        snap.Updated,
		RefreshSeconds: int(cfg.PollInterval.Seconds()),
		Refreshing:     s.board.Refreshing(),
		Version:        snap.Version,
		CanAttach:      len(cfg.AttachCommand) > 0 && isLoopback(r),
		Local:          isLoopback(r),
		Stations:       stations,
	}
	for _, name := range groupOrder {
		if tasks := byGroup[name]; len(tasks) > 0 {
			data.Groups = append(data.Groups, group{Name: name, Tasks: tasks})
		}
	}
	for _, sec := range []prSection{{"Reviews", snap.Reviews}, {"Other repos", snap.OtherPRs}} {
		if len(sec.PRs) > 0 {
			data.PRSections = append(data.PRSections, sec)
		}
	}
	if data.Refreshing {
		data.RefreshSeconds = 5
	}
	data.PRsFirst = len(data.Groups) == 0 || data.Groups[0].Name != groupNeedsYou

	var prNav, groupNav []navItem
	for _, sec := range data.PRSections {
		prNav = append(prNav, navItem{Name: sec.Name, Count: len(sec.PRs)})
	}
	for _, g := range data.Groups {
		item := navItem{Name: g.Name, Count: len(g.Tasks)}
		if g.Name == groupNeedsYou {
			item.Tone = "needs"
		}
		groupNav = append(groupNav, item)
	}
	if data.PRsFirst {
		data.Nav = append(prNav, groupNav...)
	} else {
		data.Nav = append(append(groupNav[:1:1], prNav...), groupNav[1:]...)
	}
	render(w, r, "index.html", data)
}

func updatedLabel(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func (s *server) version(w http.ResponseWriter, r *http.Request) {
	s.board.Viewed()
	snap := s.board.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"version":    snap.Version,
		"updated":    updatedLabel(snap.Updated),
		"refreshing": s.board.Refreshing(),
	})
}

func (s *server) plan(w http.ResponseWriter, r *http.Request) {
	t, ok := s.board.Task(r.PathValue("key"))
	if !ok || t.Plan == nil {
		http.NotFound(w, r)
		return
	}
	content, err := os.ReadFile(t.Plan.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, r, "plan.html", planData{Task: t, Content: string(content)})
}

// The custom header forces a CORS preflight, which this server never answers, so other sites cannot trigger it.
func (s *server) attach(w http.ResponseWriter, r *http.Request) {
	cfg := s.board.Config()
	if r.Header.Get("X-Taskboard") != "1" || !isLoopback(r) || len(cfg.AttachCommand) == 0 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	t, ok := s.board.Task(r.PathValue("key"))
	if !ok || t.Session == nil {
		http.NotFound(w, r)
		return
	}
	args := make([]string, len(cfg.AttachCommand))
	for i, a := range cfg.AttachCommand {
		args[i] = strings.ReplaceAll(a, "{target}", t.Session.Target)
	}
	cmd := exec.Command(args[0], args[1:]...)
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	go cmd.Wait()
	w.WriteHeader(http.StatusNoContent)
}

// Same CORS-preflight guard as attach, so other sites cannot make this server spend API calls.
func (s *server) refresh(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Taskboard") != "1" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.board.Refresh()
	w.WriteHeader(http.StatusNoContent)
}

// Settings are loopback-only because attach_command runs on this machine.
func (s *server) settings(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		http.Error(w, "settings can only be changed from the machine running taskboard", http.StatusForbidden)
		return
	}
	render(w, r, "settings.html", settingsData{Config: s.board.Config(), ConfigPath: s.configPath, Listen: s.listen})
}

func (s *server) saveSettings(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Taskboard") != "1" || !isLoopback(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err := parseConfig(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := SaveConfig(s.configPath, cfg); err != nil {
		http.Error(w, "save "+s.configPath+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.board.SetConfig(cfg)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"restart": cfg.Listen != s.listen})
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func render(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "Accept-Encoding")
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Write(buf.Bytes())
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	gz := gzip.NewWriter(w)
	gz.Write(buf.Bytes())
	gz.Close()
}
