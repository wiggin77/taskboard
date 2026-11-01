package main

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

//go:embed static
var staticFS embed.FS

const closeNoSession websocket.StatusCode = 4404

type termData struct {
	ID       string
	Title    string
	StartURL string
	Start    bool
}

func (s *server) termTarget(id string) (termData, bool) {
	if t, ok := s.board.Task(id); ok && !t.NoTicket {
		return termData{ID: id, Title: t.Summary, StartURL: "/start/" + id}, true
	}
	if l, ok := s.board.Review(id); ok {
		return termData{ID: id, Title: l.Title, StartURL: "/review/start/" + id}, true
	}
	return termData{}, false
}

func (s *server) termPage(w http.ResponseWriter, r *http.Request) {
	if s.accessOf(r) < remote {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	d, ok := s.termTarget(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	d.Start = r.URL.Query().Get("start") == "1"
	render(w, r, "term.html", d)
}

func findWindow(ctx context.Context, cfg *Config, id string) (string, bool) {
	byKey := cfg.findKey(id) == id
	for _, w := range listWindows(ctx, cfg) {
		if byKey && w.Key == id || !byKey && w.Name == id {
			return w.Target, true
		}
	}
	return "", false
}

type resize struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// termSocket bridges a WebSocket to tmux attached through a new linked session. websocket.Accept refuses
// cross-origin upgrades, which matters because neither CORS nor the X-Taskboard header protect WebSockets.
func (s *server) termSocket(w http.ResponseWriter, r *http.Request) {
	if s.accessOf(r) < remote {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	if _, ok := s.termTarget(id); !ok {
		http.NotFound(w, r)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cfg := s.board.Config()
	target, ok := findWindow(ctx, cfg, id)
	if !ok {
		conn.Close(closeNoSession, id+" has no session")
		return
	}
	linked, err := openLinked(ctx, target)
	if err != nil {
		conn.Close(websocket.StatusInternalError, truncate(err.Error(), 120))
		return
	}
	defer closeLinked(linked)

	cmd := exec.Command("tmux", "attach-session", "-t", "="+linked)
	cmd.Env = append(withoutEnv(os.Environ(), "TMUX"), "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		conn.Close(websocket.StatusInternalError, truncate(err.Error(), 120))
		return
	}
	defer func() {
		f.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	go func() {
		defer cancel()
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 && conn.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
				return
			}
			if err != nil {
				conn.Close(websocket.StatusNormalClosure, "session ended")
				return
			}
		}
	}()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var sz resize
			if json.Unmarshal(data, &sz) == nil && sz.Cols > 0 && sz.Rows > 0 {
				pty.Setsize(f, &pty.Winsize{Cols: sz.Cols, Rows: sz.Rows})
			}
			continue
		}
		if _, err := f.Write(data); err != nil {
			return
		}
	}
}

func withoutEnv(env []string, name string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// WebSocket close reasons are limited to 123 bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (s *server) static(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFileFS(w, r, staticFS, "static/"+r.PathValue("file"))
}
