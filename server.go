package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"ageTone":      func(t time.Time) string { return ageTone(t, time.Now()) },
	"dateLabel":    func(t time.Time) string { return dateLabel(t, time.Now()) },
	"isToday":      func(t time.Time) bool { return isToday(t, time.Now()) },
	"iso":          func(t time.Time) string { return t.Format(time.RFC3339) },
	"slug":         slug,
	"join":         strings.Join,
	"shortHome":    shortenHome,
	"taskControls": taskControls,
	"prControls":   prControls,
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
	tlsListen  string
	tlsDir     string

	mu   sync.Mutex
	busy map[string]bool
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
	Native         bool
	CanAct         bool
	Stations       []string
}

// controls says which session buttons a row shows; an empty ID shows none. Native buttons open a terminal
// on the taskboard machine; web ones open the browser terminal.
type controls struct {
	ID      string
	Path    string
	Session bool
	Native  bool
	Web     bool
	Stop    bool
	Start   string
	Clean   string
}

func taskControls(t Task, d indexData) controls {
	c := controls{ID: t.Key, Session: t.Session != nil}
	if c.Session {
		c.Native, c.Web, c.Stop = d.Native, d.CanAct, d.CanAct
	} else if d.CanAct && t.CanStart() {
		c.Native, c.Web = d.Native, true
		c.Start = "Start session"
		if t.Worktree != nil {
			c.Start = "Resume session"
		}
	}
	if d.CanAct && t.NeedsCleanup() {
		c.Clean = "Clean up: close the session, remove the worktree and branch"
	}
	return c
}

func prControls(p PRLink, d indexData) controls {
	if p.ReviewName == "" {
		return controls{}
	}
	c := controls{ID: p.ReviewName, Path: "/review"}
	var wt *Worktree
	if p.Review != nil {
		c.Session, wt = p.Review.Session != nil, p.Review.Worktree
	}
	if c.Session {
		c.Native, c.Web, c.Stop = d.Native, d.CanAct, d.CanAct
	} else if d.CanAct && p.Role != RoleLeftover {
		c.Native, c.Web = d.Native, true
		c.Start = "Start review"
		if wt != nil {
			c.Start = "Resume review"
		}
	}
	if d.CanAct && p.Review != nil {
		c.Clean = "Clean up: close the session and remove the review worktree"
	}
	return c
}

type settingsData struct {
	Config     *Config
	ConfigPath string
	Listen     string
	TLSListen  string
	NewRepo    RepoConfig
}

type planData struct {
	Task    Task
	Content string
}

func newServer(board *Board, configPath string) http.Handler {
	cfg := board.Config()
	s := &server{board: board, configPath: configPath, listen: cfg.Listen, tlsListen: cfg.TLSListen, tlsDir: cfg.TLSDir, busy: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /version", s.version)
	mux.HandleFunc("GET /plan/{key}", s.plan)
	mux.HandleFunc("POST /attach/{key}", s.attach)
	mux.HandleFunc("POST /start/{key}", s.start)
	mux.HandleFunc("POST /stop/{key}", s.stop)
	mux.HandleFunc("POST /cleanup/{key}", s.cleanup)
	mux.HandleFunc("POST /review/attach/{name}", s.reviewAttach)
	mux.HandleFunc("POST /review/start/{name}", s.reviewStart)
	mux.HandleFunc("POST /review/stop/{name}", s.reviewStop)
	mux.HandleFunc("POST /review/cleanup/{name}", s.reviewCleanup)
	mux.HandleFunc("POST /refresh", s.refresh)
	mux.HandleFunc("GET /term/{id}", s.termPage)
	mux.HandleFunc("GET /term/{id}/ws", s.termSocket)
	mux.HandleFunc("GET /static/{file}", s.static)
	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("POST /settings", s.saveSettings)
	return s.checkHost(mux)
}

// Rejecting unknown hostnames blocks DNS rebinding on the plain listener. TLS requests skip it: a browser
// only completes the handshake for names in the server certificate, which a rebinding attacker can't use.
func (s *server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			next.ServeHTTP(w, r)
			return
		}
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
		Native:         len(cfg.AttachCommand) > 0 && s.accessOf(r) == local,
		CanAct:         s.accessOf(r) >= remote,
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
	if !s.allowNative(w, r) {
		return
	}
	t, ok := s.board.Task(r.PathValue("key"))
	if !ok || t.Session == nil {
		http.NotFound(w, r)
		return
	}
	s.openNative(w, t.Session.Target)
}

func (s *server) reviewAttach(w http.ResponseWriter, r *http.Request) {
	if !s.allowNative(w, r) {
		return
	}
	l, ok := s.board.Review(r.PathValue("name"))
	if !ok || l.Review == nil || l.Review.Session == nil {
		http.NotFound(w, r)
		return
	}
	s.openNative(w, l.Review.Session.Target)
}

// Native terminals open on the taskboard machine, so only a browser on that machine may ask for one.
func (s *server) allowNative(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Taskboard") != "1" || s.accessOf(r) != local || len(s.board.Config().AttachCommand) == 0 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (s *server) openNative(w http.ResponseWriter, target string) {
	if err := runAttach(s.board.Config(), target); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func runAttach(cfg *Config, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	linked, err := openLinked(ctx, target)
	if err != nil {
		return err
	}
	args := make([]string, len(cfg.AttachCommand))
	for i, a := range cfg.AttachCommand {
		args[i] = strings.ReplaceAll(a, "{target}", linked)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = withoutEnv(os.Environ(), "TMUX")
	if err := cmd.Start(); err != nil {
		closeLinked(linked)
		return err
	}
	go cmd.Wait()
	return nil
}

func (s *server) allowAction(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Taskboard") != "1" || s.accessOf(r) < remote {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// claim stops a double click from running the same action twice at once.
func (s *server) claim(w http.ResponseWriter, id string) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[id] {
		http.Error(w, "already working on "+id, http.StatusLocked)
		return nil, false
	}
	s.busy[id] = true
	return func() {
		s.mu.Lock()
		delete(s.busy, id)
		s.mu.Unlock()
	}, true
}

func (s *server) action(w http.ResponseWriter, r *http.Request) (*Config, Task, func(), bool) {
	if !s.allowAction(w, r) {
		return nil, Task{}, nil, false
	}
	t, ok := s.board.Task(r.PathValue("key"))
	if !ok || t.NoTicket {
		http.NotFound(w, r)
		return nil, Task{}, nil, false
	}
	release, ok := s.claim(w, t.Key)
	return s.board.Config(), t, release, ok
}

func (s *server) reviewAction(w http.ResponseWriter, r *http.Request) (*Config, PRLink, func(), bool) {
	if !s.allowAction(w, r) {
		return nil, PRLink{}, nil, false
	}
	l, ok := s.board.Review(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return nil, PRLink{}, nil, false
	}
	release, ok := s.claim(w, l.ReviewName)
	return s.board.Config(), l, release, ok
}

// Actions use their own context because a page reload must not abort a half-made worktree.
func actionContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), actionTimeout)
}

func (s *server) start(w http.ResponseWriter, r *http.Request) {
	cfg, t, release, ok := s.action(w, r)
	if !ok {
		return
	}
	defer release()
	if !t.CanStart() && t.Session == nil {
		http.Error(w, t.Key+" is "+strings.ToLower(t.StageLabel())+", so there is nothing to start", http.StatusConflict)
		return
	}
	ctx, cancel := actionContext()
	defer cancel()
	t, err := liveTask(ctx, cfg, t)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	target := ""
	if t.Session != nil {
		target = t.Session.Target
	} else {
		branches, err := listBranches(ctx, cfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		plan, err := startPlan(cfg, t, branches)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if plan.Mode == StartNew {
			branch := strings.TrimSpace(r.FormValue("branch"))
			if branch == "" {
				writeJSON(w, http.StatusConflict, plan)
				return
			}
			if err := chooseNew(cfg, &plan, t.Key, r.FormValue("repo"), branch); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		target, err = startSession(ctx, cfg, t.Key, plan)
		s.board.LocalChanged(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.startDone(w, r, cfg, target)
}

func (s *server) startDone(w http.ResponseWriter, r *http.Request, cfg *Config, target string) {
	opened := r.FormValue("open") == "terminal" && s.accessOf(r) == local && len(cfg.AttachCommand) > 0 &&
		runAttach(cfg, target) == nil
	writeJSON(w, http.StatusOK, map[string]any{"target": target, "opened": opened})
}

func (s *server) stop(w http.ResponseWriter, r *http.Request) {
	cfg, t, release, ok := s.action(w, r)
	if !ok {
		return
	}
	defer release()
	if t.Session == nil {
		http.Error(w, t.Key+" has no session", http.StatusNotFound)
		return
	}
	ctx, cancel := actionContext()
	defer cancel()
	t, err := liveTask(ctx, cfg, t)
	if err == nil && t.Session != nil {
		err = stopSession(ctx, t.Session)
	}
	s.board.LocalChanged(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) cleanup(w http.ResponseWriter, r *http.Request) {
	cfg, t, release, ok := s.action(w, r)
	if !ok {
		return
	}
	defer release()
	if !t.NeedsCleanup() {
		http.Error(w, t.Key+" is not done, so taskboard will not clean it up", http.StatusConflict)
		return
	}
	ctx, cancel := actionContext()
	defer cancel()
	t, err := liveTask(ctx, cfg, t)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rm, err := cleanup(ctx, cfg, t, r.FormValue("force") == "1")
	if errors.Is(err, errUnsafe) {
		writeJSON(w, http.StatusConflict, rm)
		return
	}
	s.board.LocalChanged(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rm)
}

func (s *server) reviewStart(w http.ResponseWriter, r *http.Request) {
	cfg, l, release, ok := s.reviewAction(w, r)
	if !ok {
		return
	}
	defer release()
	if l.Role == RoleLeftover {
		http.Error(w, l.Repo+" #"+strconv.Itoa(l.Number)+" is no longer awaiting your review", http.StatusConflict)
		return
	}
	ctx, cancel := actionContext()
	defer cancel()
	l, err := liveReview(ctx, cfg, l)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	target, err := startReview(ctx, cfg, l)
	s.board.LocalChanged(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.startDone(w, r, cfg, target)
}

func (s *server) reviewStop(w http.ResponseWriter, r *http.Request) {
	cfg, l, release, ok := s.reviewAction(w, r)
	if !ok {
		return
	}
	defer release()
	if l.Review == nil || l.Review.Session == nil {
		http.Error(w, l.ReviewName+" has no session", http.StatusNotFound)
		return
	}
	ctx, cancel := actionContext()
	defer cancel()
	l, err := liveReview(ctx, cfg, l)
	if err == nil && l.Review != nil && l.Review.Session != nil {
		err = stopSession(ctx, l.Review.Session)
	}
	s.board.LocalChanged(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) reviewCleanup(w http.ResponseWriter, r *http.Request) {
	cfg, l, release, ok := s.reviewAction(w, r)
	if !ok {
		return
	}
	defer release()
	if l.Review == nil {
		http.Error(w, l.ReviewName+" has no worktree or session to clean up", http.StatusNotFound)
		return
	}
	ctx, cancel := actionContext()
	defer cancel()
	l, err := liveReview(ctx, cfg, l)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rm, err := cleanupReview(ctx, cfg, l, r.FormValue("force") == "1")
	if errors.Is(err, errUnsafe) {
		writeJSON(w, http.StatusConflict, rm)
		return
	}
	s.board.LocalChanged(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rm)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
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

// Settings can change commands taskboard runs, so viewers cannot open them.
func (s *server) settings(w http.ResponseWriter, r *http.Request) {
	if s.accessOf(r) < remote {
		http.Error(w, "settings need a browser on the taskboard machine or a client certificate", http.StatusForbidden)
		return
	}
	render(w, r, "settings.html", settingsData{Config: s.board.Config(), ConfigPath: s.configPath, Listen: s.listen, TLSListen: s.tlsListen})
}

func (s *server) saveSettings(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Taskboard") != "1" || s.accessOf(r) < remote {
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
	restart := cfg.Listen != s.listen || cfg.TLSListen != s.tlsListen || cfg.TLSDir != s.tlsDir
	json.NewEncoder(w).Encode(map[string]bool{"restart": restart})
}

type access int

const (
	viewer access = iota
	remote
	local
)

// accessOf: local is a browser on this machine using the plain listener; remote is one presenting a
// client certificate on the TLS listener, re-checked here so revoking cuts off kept-alive connections.
func (s *server) accessOf(r *http.Request) access {
	if r.TLS != nil {
		if len(r.TLS.VerifiedChains) > 0 && !isRevoked(s.board.Config().RevokedCerts, r.TLS.PeerCertificates[0]) {
			return remote
		}
		return viewer
	}
	if isLoopback(r) {
		return local
	}
	return viewer
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
