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

func (p *Plan) GitHubURL() string {
	if p.Branch == "" {
		return ""
	}
	return "https://github.com/" + p.Repo + "/blob/" + p.Branch + "/" + p.RelPath
}

type Session struct {
	Key       string
	Target    string
	Name      string
	AttachCmd string
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
	out, err := runCmd(ctx, repo.Path, "git", "worktree", "list", "--porcelain")
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
			wts = append(wts, Worktree{Path: strings.TrimPrefix(line, "worktree ")})
		case strings.HasPrefix(line, "branch ") && len(wts) > 0:
			wts[len(wts)-1].Branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}
	return wts
}

func findPlans(cfg *Config, repo RepoConfig, wts []Worktree) ([]Plan, error) {
	var plans []Plan
	for _, wt := range wts {
		dir := filepath.Join(wt.Path, repo.PlansDir)
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
				RelPath:  path.Join(filepath.ToSlash(repo.PlansDir), e.Name()),
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

func listSessions(ctx context.Context, cfg *Config) ([]Session, error) {
	out, err := runCmd(ctx, "", "tmux", "list-windows", "-a", "-F", "#{session_name}:#{window_index}\t#{window_name}")
	if err != nil {
		// tmux exits non-zero when no server is running, which just means no sessions.
		return nil, nil
	}
	var sessions []Session
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		target, name, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		key := cfg.findKey(name)
		if key == "" {
			continue
		}
		sessions = append(sessions, Session{
			Key:       key,
			Target:    target,
			Name:      name,
			AttachCmd: attachCommand(cfg.SSHHost, target),
		})
	}
	return sessions, nil
}

func attachCommand(sshHost, target string) string {
	tmux := "tmux attach -t " + shellQuote(target)
	if sshHost == "" {
		return tmux
	}
	return "ssh -t " + shellQuote(sshHost) + " " + shellQuote(tmux)
}

func shellQuote(s string) string {
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-.:@/=+", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
