package main

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"html/template"
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
	"slug":      slug,
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

func dateLabel(t, now time.Time) string {
	t = t.In(now.Location())
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	switch {
	case ty == ny && tm == nm && td == nd:
		return "today " + t.Format("15:04")
	case ty == ny:
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

type server struct {
	cfg   *Config
	board *Board
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
	Stations       []string
}

type planData struct {
	Task    Task
	Content string
}

func newServer(cfg *Config, board *Board) http.Handler {
	s := &server{cfg: cfg, board: board}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /version", s.version)
	mux.HandleFunc("GET /plan/{key}", s.plan)
	mux.HandleFunc("POST /attach/{key}", s.attach)
	mux.HandleFunc("POST /refresh", s.refresh)
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
		if host != "localhost" && net.ParseIP(host) == nil && !slices.Contains(s.cfg.AllowedHosts, host) {
			http.Error(w, "unknown host "+host+"; add it to allowed_hosts", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	s.board.Viewed()
	snap := s.board.Snapshot()
	byGroup := map[string][]Task{}
	for _, t := range snap.Tasks {
		byGroup[t.Group()] = append(byGroup[t.Group()], t)
	}
	data := indexData{
		Errors:         snap.Errors,
		Updated:        snap.Updated,
		RefreshSeconds: int(s.cfg.PollInterval.Seconds()),
		Refreshing:     s.board.Refreshing(),
		Version:        snap.Version,
		CanAttach:      len(s.cfg.AttachCommand) > 0 && isLoopback(r),
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
	return t.Format("15:04")
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
	if r.Header.Get("X-Taskboard") != "1" || !isLoopback(r) || len(s.cfg.AttachCommand) == 0 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	t, ok := s.board.Task(r.PathValue("key"))
	if !ok || t.Session == nil {
		http.NotFound(w, r)
		return
	}
	args := make([]string, len(s.cfg.AttachCommand))
	for i, a := range s.cfg.AttachCommand {
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
