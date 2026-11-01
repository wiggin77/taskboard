package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type Worktree struct {
	Repo   string
	Path   string
	Branch string
	Key    string
	Main   bool
}

type Plan struct {
	Key      string
	Repo     string
	Path     string
	RelPath  string
	Branch   string
	Approved bool
	ModTime  time.Time
}

func (w *Worktree) Dir() string { return filepath.Base(w.Path) }

func (p *Plan) GitHubURL() string {
	if p.Branch == "" {
		return ""
	}
	return "https://github.com/" + p.Repo + "/blob/" + p.Branch + "/" + p.RelPath
}

type Session struct {
	Key    string
	Target string
	Name   string
}

func runCmd(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

func listWorktrees(ctx context.Context, cfg *Config, repo RepoConfig) ([]Worktree, error) {
	out, err := runCmd(ctx, repo.Dir(), "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	wts := parseWorktrees(out)
	for i := range wts {
		wts[i].Repo = repo.Name
		wts[i].Key = cfg.findKey(wts[i].Branch)
	}
	return wts, nil
}

func parseWorktrees(out []byte) []Worktree {
	var wts []Worktree
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "worktree "):
			wts = append(wts, Worktree{Path: strings.TrimPrefix(line, "worktree "), Main: len(wts) == 0})
		case strings.HasPrefix(line, "branch ") && len(wts) > 0:
			wts[len(wts)-1].Branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}
	return wts
}

func findPlans(cfg *Config, repo RepoConfig, wts []Worktree) ([]Plan, error) {
	var plans []Plan
	for _, wt := range wts {
		dir := filepath.Join(wt.Path, repo.plansDir())
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			key := cfg.findKey(e.Name())
			if key == "" {
				continue
			}
			p := filepath.Join(dir, e.Name())
			info, err := e.Info()
			if err != nil {
				return nil, err
			}
			content, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			plans = append(plans, Plan{
				Key:      key,
				Repo:     repo.Name,
				Path:     p,
				RelPath:  path.Join(filepath.ToSlash(repo.plansDir()), e.Name()),
				Branch:   wt.Branch,
				Approved: planApproved(content),
				ModTime:  info.ModTime(),
			})
		}
	}
	return plans, nil
}

func planApproved(content []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(content))
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return false
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "---" {
			return false
		}
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "status" {
			return strings.EqualFold(strings.Trim(strings.TrimSpace(v), `"'`), "approved")
		}
	}
	return false
}

// listWindows returns every tmux window once, skipping the linked sessions taskboard attaches through;
// Key is set only for windows named after a ticket.
func listWindows(ctx context.Context, cfg *Config) []Session {
	out, err := runCmd(ctx, "", "tmux", "list-windows", "-a", "-F", "#{session_name}:#{window_index}\t#{window_name}\t#{window_id}\t#{"+linkedMark+"}")
	if err != nil {
		// tmux exits non-zero when no server is running, which just means no sessions.
		return nil
	}
	return parseWindows(cfg, out)
}

func parseWindows(cfg *Config, out []byte) []Session {
	var windows []Session
	seen := map[string]bool{}
	// Only newlines are trimmed: the last field is empty for unmarked sessions, and TrimSpace would eat its tab.
	for _, line := range strings.Split(strings.TrimRight(string(out), "\r\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || f[3] != "" || seen[f[2]] {
			continue
		}
		seen[f[2]] = true
		windows = append(windows, Session{
			Key:    cfg.findKey(f[1]),
			Target: f[0],
			Name:   f[1],
		})
	}
	return windows
}

func ticketSessions(windows []Session) []Session {
	var sessions []Session
	for _, w := range windows {
		if w.Key != "" {
			sessions = append(sessions, w)
		}
	}
	return sessions
}

func splitTarget(target string) (session, index string) {
	i := strings.LastIndexByte(target, ':')
	if i < 0 {
		return target, ""
	}
	return target[:i], target[i+1:]
}
