package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type StartMode string

const (
	StartResume StartMode = "resume"
	StartBranch StartMode = "branch"
	StartPR     StartMode = "pr"
	StartNew    StartMode = "new"
)

type StartPlan struct {
	Mode   StartMode `json:"mode"`
	Repo   string    `json:"repo"`
	Path   string    `json:"path"`
	Branch string    `json:"branch"`
	PR     int       `json:"pr,omitempty"`
	Repos  []string  `json:"repos,omitempty"`
}

type Removal struct {
	Uncommitted []string `json:"uncommitted,omitempty"`
	Unpushed    []string `json:"unpushed,omitempty"`
	Kept        string   `json:"kept,omitempty"`
}

func (r Removal) Clean() bool { return len(r.Uncommitted) == 0 && len(r.Unpushed) == 0 }

var errUnsafe = errors.New("the worktree has uncommitted or unpushed work")

const (
	actionTimeout = 3 * time.Minute
	maxBranchLen  = 50
	maxListed     = 20
)

func localRepos(cfg *Config) []RepoConfig {
	var repos []RepoConfig
	for _, r := range cfg.Repos {
		if r.Path != "" {
			repos = append(repos, r)
		}
	}
	return repos
}

// branches maps repo names to their local branches, newest first.
func startPlan(cfg *Config, t Task, branches map[string][]string) (StartPlan, error) {
	if wt := t.Worktree; wt != nil {
		return StartPlan{Mode: StartResume, Repo: wt.Repo, Path: wt.Path, Branch: wt.Branch}, nil
	}
	repos := localRepos(cfg)
	if len(repos) == 0 {
		return StartPlan{}, errors.New("no repo has a local checkout; add one in Settings")
	}
	var prRepo *RepoConfig
	ordered := make([]RepoConfig, 0, len(repos))
	for i, r := range repos {
		if t.PR != nil && strings.EqualFold(r.Name, t.PR.Repo) {
			prRepo = &repos[i]
			ordered = append([]RepoConfig{r}, ordered...)
		} else {
			ordered = append(ordered, r)
		}
	}
	for _, r := range ordered {
		for _, b := range branches[r.Name] {
			if cfg.findKey(b) == t.Key {
				return StartPlan{Mode: StartBranch, Repo: r.Name, Path: worktreePath(cfg, r, t.Key), Branch: b}, nil
			}
		}
	}
	if prRepo != nil && t.PR.State == "OPEN" {
		return StartPlan{Mode: StartPR, Repo: prRepo.Name, Path: worktreePath(cfg, *prRepo, t.Key), Branch: t.PR.Branch, PR: t.PR.Number}, nil
	}
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.Name
	}
	return StartPlan{
		Mode:   StartNew,
		Repo:   repos[0].Name,
		Path:   worktreePath(cfg, repos[0], t.Key),
		Branch: defaultBranch(t.Key, t.Summary),
		Repos:  names,
	}, nil
}

func chooseNew(cfg *Config, p *StartPlan, key, repo, branch string) error {
	if repo != "" {
		r, ok := cfg.repo(repo)
		if !ok || r.Path == "" {
			return fmt.Errorf("%s is not a configured repo with a local checkout", repo)
		}
		p.Repo = r.Name
		p.Path = worktreePath(cfg, r, key)
	}
	if cfg.findKey(branch) != key {
		return fmt.Errorf("the branch name must contain %s, so the board can find it", key)
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	p.Branch = branch
	return nil
}

func worktreePath(cfg *Config, r RepoConfig, suffix string) string {
	dir := expandHome(cfg.WorktreeDir)
	if dir == "" {
		dir = filepath.Dir(filepath.Clean(r.Dir()))
	}
	return filepath.Join(dir, checkoutName(r)+"-"+suffix)
}

func checkoutName(r RepoConfig) string { return filepath.Base(filepath.Clean(r.Dir())) }

func defaultBranch(key, summary string) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(summary) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	name := key
	if s := strings.Trim(b.String(), "-"); s != "" {
		name += "-" + s
	}
	if len(name) > maxBranchLen {
		name = name[:maxBranchLen]
		if i := strings.LastIndexByte(name, '-'); i > len(key) {
			name = name[:i]
		}
	}
	return strings.TrimRight(name, "-")
}

func listBranches(ctx context.Context, cfg *Config) (map[string][]string, error) {
	branches := map[string][]string{}
	for _, r := range localRepos(cfg) {
		out, err := runCmd(ctx, r.Dir(), "git", "for-each-ref", "--sort=-committerdate", "--format=%(refname:short)", "refs/heads")
		if err != nil {
			return nil, fmt.Errorf("%s branches: %w", r.Name, err)
		}
		branches[r.Name] = strings.Fields(string(out))
	}
	return branches, nil
}

// liveTask replaces a task's session and worktree with what exists now, since the snapshot can be minutes old.
func liveTask(ctx context.Context, cfg *Config, t Task) (Task, error) {
	sessions := ticketSessions(listWindows(ctx, cfg))
	t.Session = nil
	for i := range sessions {
		if sessions[i].Key == t.Key {
			t.Session = &sessions[i]
			break
		}
	}
	t.Worktree = nil
	for _, r := range localRepos(cfg) {
		wts, err := listWorktrees(ctx, cfg, r)
		if err != nil {
			return t, fmt.Errorf("%s worktrees: %w", r.Name, err)
		}
		for i := range wts {
			if wts[i].Key == t.Key && t.Worktree == nil {
				t.Worktree = &wts[i]
			}
		}
	}
	return t, nil
}

func startSession(ctx context.Context, cfg *Config, key string, p StartPlan) (string, error) {
	if p.Mode != StartResume {
		if err := createWorktree(ctx, cfg, p); err != nil {
			return "", err
		}
	}
	command := cfg.agent(p.Mode == StartResume)
	return openWindow(ctx, cfg, key, p.Path, strings.ReplaceAll(command, "{key}", key))
}

func createWorktree(ctx context.Context, cfg *Config, p StartPlan) error {
	r, ok := cfg.repo(p.Repo)
	if !ok {
		return fmt.Errorf("unknown repo %s", p.Repo)
	}
	dir := r.Dir()
	if _, err := os.Lstat(p.Path); err == nil {
		return fmt.Errorf("%s already exists", p.Path)
	}
	parent := filepath.Dir(p.Path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	if err := ensureLinks(parent, cfg.WorktreeLinks); err != nil {
		return err
	}
	switch p.Mode {
	case StartBranch:
		_, err := runCmd(ctx, dir, "git", "worktree", "add", p.Path, p.Branch)
		return err
	case StartPR:
		if _, err := runCmd(ctx, dir, "git", "worktree", "add", "--detach", p.Path); err != nil {
			return err
		}
		if _, err := runCmd(ctx, p.Path, "gh", "pr", "checkout", strconv.Itoa(p.PR), "--repo", r.Name); err != nil {
			runCmd(context.Background(), dir, "git", "worktree", "remove", "--force", p.Path)
			return err
		}
		return nil
	case StartNew:
		if _, err := runCmd(ctx, dir, "git", "check-ref-format", "--branch", p.Branch); err != nil {
			return fmt.Errorf("invalid branch name %q", p.Branch)
		}
		base, err := baseRef(ctx, r)
		if err != nil {
			return err
		}
		remote, branch, _ := strings.Cut(base, "/")
		if _, err := runCmd(ctx, dir, "git", "fetch", remote, branch); err != nil {
			return err
		}
		// Without --no-track the new branch would track the base branch, and a plain git push could target it.
		_, err = runCmd(ctx, dir, "git", "worktree", "add", "--no-track", "-b", p.Branch, p.Path, base)
		return err
	}
	return fmt.Errorf("unknown start mode %q", p.Mode)
}

func baseRef(ctx context.Context, r RepoConfig) (string, error) {
	if r.BaseBranch != "" {
		return strings.Trim(r.BaseBranch, "/"), nil
	}
	out, err := runCmd(ctx, r.Dir(), "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("%s: origin/HEAD is not set; run git remote set-head origin --auto, or set the repo's base branch in Settings", r.Name)
	}
	return strings.TrimSpace(string(out)), nil
}

// openWindow types the command into the window's shell, so quitting the agent leaves a shell instead of closing the window.
func openWindow(ctx context.Context, cfg *Config, key, dir, command string) (string, error) {
	const format = "#{session_name}:#{window_index}"
	var out []byte
	var err error
	session := cfg.tmuxSession()
	if _, herr := runCmd(ctx, "", "tmux", "has-session", "-t", "="+session); herr != nil {
		out, err = runCmd(ctx, "", "tmux", "new-session", "-d", "-s", session, "-n", key, "-c", dir, "-P", "-F", format)
	} else {
		out, err = runCmd(ctx, "", "tmux", "new-window", "-d", "-t", "="+session+":", "-n", key, "-c", dir, "-P", "-F", format)
	}
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(string(out))
	if command == "" {
		return target, nil
	}
	if _, err := runCmd(ctx, "", "tmux", "send-keys", "-t", target, "-l", command); err != nil {
		return target, err
	}
	_, err = runCmd(ctx, "", "tmux", "send-keys", "-t", target, "Enter")
	return target, err
}

const (
	linkedMark  = "@taskboard"
	linkedGrace = 2 * time.Minute
)

// openLinked creates a linked session showing target's window for one terminal to attach to. A detached
// session with destroy-unattached set is destroyed at once, so a hook sets it when the terminal attaches.
func openLinked(ctx context.Context, target string) (string, error) {
	session, index := splitTarget(target)
	b := make([]byte, 4)
	rand.Read(b)
	name := "tb-" + hex.EncodeToString(b)
	if _, err := runCmd(ctx, "", "tmux", "new-session", "-d", "-t", "="+session, "-s", name); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"set-option", "-t", name, linkedMark, "1"},
		{"set-hook", "-t", name, "client-attached", "set-option destroy-unattached on"},
		{"select-window", "-t", name + ":" + index},
	} {
		if _, err := runCmd(ctx, "", "tmux", args...); err != nil {
			closeLinked(name)
			return "", err
		}
	}
	return name, nil
}

func closeLinked(name string) {
	runCmd(context.Background(), "", "tmux", "kill-session", "-t", "="+name)
}

// staleLinked lists linked sessions nobody attached to, such as ones whose terminal failed to open.
func staleLinked(out []byte, now time.Time) []string {
	var stale []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || f[1] != "1" || f[2] != "0" {
			continue
		}
		created, err := strconv.ParseInt(f[3], 10, 64)
		if err == nil && now.Sub(time.Unix(created, 0)) > linkedGrace {
			stale = append(stale, f[0])
		}
	}
	return stale
}

func sweepLinked(ctx context.Context) {
	out, err := runCmd(ctx, "", "tmux", "list-sessions", "-F", "#{session_name}\t#{"+linkedMark+"}\t#{session_attached}\t#{session_created}")
	if err != nil {
		return
	}
	for _, name := range staleLinked(out, time.Now()) {
		closeLinked(name)
	}
}

func stopSession(ctx context.Context, s *Session) error {
	_, err := runCmd(ctx, "", "tmux", "kill-window", "-t", s.Target)
	return err
}

func outputLines(out []byte) []string {
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimRight(l, "\r"); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > maxListed {
		lines = append(lines[:maxListed], fmt.Sprintf("and %d more", len(lines)-maxListed))
	}
	return lines
}

// A squash-merged PR's branch is often deleted from the remote, so a HEAD matching the merged PR's head counts as pushed.
func removalCheck(status, unpushed []byte, head string, pr *PR) Removal {
	r := Removal{Uncommitted: outputLines(status), Unpushed: outputLines(unpushed)}
	if pr != nil && pr.State == "MERGED" && pr.HeadSHA != "" && pr.HeadSHA == head {
		r.Unpushed = nil
	}
	return r
}

func checkWorktree(ctx context.Context, wt Worktree, pr *PR) (Removal, error) {
	status, err := runCmd(ctx, wt.Path, "git", "status", "--porcelain")
	if err != nil {
		return Removal{}, err
	}
	head, err := runCmd(ctx, wt.Path, "git", "rev-parse", "HEAD")
	if err != nil {
		return Removal{}, err
	}
	unpushed, err := runCmd(ctx, wt.Path, "git", "log", "--format=%h %s", "HEAD", "--not", "--remotes")
	if err != nil {
		return Removal{}, err
	}
	return removalCheck(status, unpushed, strings.TrimSpace(string(head)), pr), nil
}

// cleanup keeps a branch with unpushed commits even when forced, so an override only loses uncommitted changes.
func cleanup(ctx context.Context, cfg *Config, t Task, force bool) (Removal, error) {
	rm, r, err := removeWorktree(ctx, cfg, t.Worktree, t.Session, t.PR, force)
	if err != nil || t.Worktree == nil || t.Worktree.Branch == "" {
		return rm, err
	}
	if len(rm.Unpushed) > 0 {
		rm.Kept = t.Worktree.Branch
		return rm, nil
	}
	_, err = runCmd(ctx, r.Dir(), "git", "branch", "-D", t.Worktree.Branch)
	return rm, err
}

func removeWorktree(ctx context.Context, cfg *Config, wt *Worktree, sess *Session, pr *PR, force bool) (Removal, RepoConfig, error) {
	var rm Removal
	var r RepoConfig
	if wt != nil {
		if wt.Main {
			return rm, r, fmt.Errorf("%s is the main checkout of %s, which taskboard never removes", wt.Path, wt.Repo)
		}
		var ok bool
		if r, ok = cfg.repo(wt.Repo); !ok {
			return rm, r, fmt.Errorf("unknown repo %s", wt.Repo)
		}
		var err error
		if rm, err = checkWorktree(ctx, *wt, pr); err != nil {
			return rm, r, err
		}
		if !rm.Clean() && !force {
			return rm, r, errUnsafe
		}
		// Detached commits vanish with the worktree, so an override first gives them a branch.
		if len(rm.Unpushed) > 0 && wt.Branch == "" {
			name := filepath.Base(wt.Path)
			if _, err := runCmd(ctx, wt.Path, "git", "branch", name, "HEAD"); err != nil {
				return rm, r, err
			}
			rm.Kept = name
		}
	}
	if sess != nil {
		if err := stopSession(ctx, sess); err != nil {
			return rm, r, err
		}
	}
	if wt == nil {
		return rm, r, nil
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	_, err := runCmd(ctx, r.Dir(), "git", append(args, wt.Path)...)
	return rm, r, err
}

func ensureLinks(dir string, links []string) error {
	for _, l := range links {
		target := expandHome(l)
		ti, err := os.Stat(target)
		if err != nil {
			return fmt.Errorf("worktree link: %w", err)
		}
		name := filepath.Join(dir, filepath.Base(target))
		ni, err := os.Stat(name)
		if err == nil {
			if os.SameFile(ni, ti) {
				continue
			}
			return fmt.Errorf("%s already exists and is not %s; move it aside or drop the worktree link", name, target)
		}
		if _, err := os.Lstat(name); err == nil {
			return fmt.Errorf("%s is a broken link; remove it so taskboard can link %s", name, target)
		}
		if err := os.Symlink(target, name); err != nil {
			return err
		}
	}
	return nil
}

type Review struct {
	Session  *Session
	Worktree *Worktree
}

func reviewName(r RepoConfig, number int) string {
	return checkoutName(r) + "-pr" + strconv.Itoa(number)
}

func parseReviewName(r RepoConfig, name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, checkoutName(r)+"-pr")
	if !ok || rest == "" || strings.TrimLeft(rest, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n > 0
}

// Review worktrees or windows whose PR is no longer listed come back as leftovers, so they can still be cleaned up.
func attachReviews(cfg *Config, links []PRLink, wts []Worktree, windows []Session) ([]PRLink, []PRLink) {
	type ref struct {
		repo   string
		number int
	}
	found := map[ref]*Review{}
	var order []ref
	get := func(k ref) *Review {
		if found[k] == nil {
			found[k] = &Review{}
			order = append(order, k)
		}
		return found[k]
	}
	repos := localRepos(cfg)
	for i := range wts {
		wt := &wts[i]
		r, ok := cfg.repo(wt.Repo)
		if !ok || r.Path == "" || wt.Main {
			continue
		}
		if n, ok := parseReviewName(r, filepath.Base(wt.Path)); ok {
			get(ref{r.Name, n}).Worktree = wt
		}
	}
	for i := range windows {
		for _, r := range repos {
			if n, ok := parseReviewName(r, windows[i].Name); ok {
				get(ref{r.Name, n}).Session = &windows[i]
				break
			}
		}
	}

	out := make([]PRLink, len(links))
	used := map[ref]bool{}
	for i, l := range links {
		if r, ok := cfg.repo(l.Repo); ok && r.Path != "" {
			k := ref{r.Name, l.Number}
			l.ReviewName = reviewName(r, l.Number)
			l.Review = found[k]
			used[k] = true
		}
		out[i] = l
	}
	var leftovers []PRLink
	for _, k := range order {
		if used[k] {
			continue
		}
		r, _ := cfg.repo(k.repo)
		leftovers = append(leftovers, PRLink{
			Repo:       k.repo,
			Number:     k.number,
			Title:      "Review worktree for " + path.Base(k.repo) + " #" + strconv.Itoa(k.number),
			URL:        "https://github.com/" + k.repo + "/pull/" + strconv.Itoa(k.number),
			Role:       RoleLeftover,
			ReviewName: reviewName(r, k.number),
			Review:     found[k],
		})
	}
	return out, leftovers
}

func liveReview(ctx context.Context, cfg *Config, l PRLink) (PRLink, error) {
	r, ok := cfg.repo(l.Repo)
	if !ok || r.Path == "" {
		return l, fmt.Errorf("%s has no local checkout", l.Repo)
	}
	wts, err := listWorktrees(ctx, cfg, r)
	if err != nil {
		return l, fmt.Errorf("%s worktrees: %w", r.Name, err)
	}
	links, _ := attachReviews(cfg, []PRLink{{Repo: l.Repo, Number: l.Number}}, wts, listWindows(ctx, cfg))
	l.Review = links[0].Review
	return l, nil
}

func reviewRemote(r RepoConfig) string {
	if remote, _, ok := strings.Cut(strings.Trim(r.BaseBranch, "/"), "/"); ok {
		return remote
	}
	return "origin"
}

// The PR head is fetched under refs/remotes so its commits count as pushed when the worktree is cleaned up.
func reviewRef(r RepoConfig, number int) string {
	return "refs/remotes/" + reviewRemote(r) + "/pr/" + strconv.Itoa(number)
}

func fetchReviewRef(ctx context.Context, r RepoConfig, number int) error {
	_, err := runCmd(ctx, r.Dir(), "git", "fetch", reviewRemote(r), "+refs/pull/"+strconv.Itoa(number)+"/head:"+reviewRef(r, number))
	return err
}

func reviewBranch(number int) string { return "review/" + strconv.Itoa(number) }

func startReview(ctx context.Context, cfg *Config, l PRLink) (string, error) {
	if l.Review != nil && l.Review.Session != nil {
		return l.Review.Session.Target, nil
	}
	r, ok := cfg.repo(l.Repo)
	if !ok || r.Path == "" {
		return "", fmt.Errorf("%s has no local checkout", l.Repo)
	}
	name := reviewName(r, l.Number)
	resume := l.Review != nil && l.Review.Worktree != nil
	dir := worktreePath(cfg, r, "pr"+strconv.Itoa(l.Number))
	if resume {
		dir = l.Review.Worktree.Path
	} else {
		if _, err := os.Lstat(dir); err == nil {
			return "", fmt.Errorf("%s already exists", dir)
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if err := ensureLinks(filepath.Dir(dir), cfg.WorktreeLinks); err != nil {
			return "", err
		}
		if err := fetchReviewRef(ctx, r, l.Number); err != nil {
			return "", err
		}
		if _, err := runCmd(ctx, r.Dir(), "git", "worktree", "add", "--detach", dir, reviewRef(r, l.Number)); err != nil {
			return "", err
		}
		// gh sets the branch's upstream, and its push target for fork PRs that allow maintainer edits.
		if _, err := runCmd(ctx, dir, "gh", "pr", "checkout", strconv.Itoa(l.Number), "--repo", r.Name, "--branch", reviewBranch(l.Number)); err != nil {
			runCmd(context.Background(), r.Dir(), "git", "worktree", "remove", "--force", dir)
			return "", err
		}
	}
	return openWindow(ctx, cfg, name, dir, reviewCommand(cfg, resume))
}

// Reviews have no ticket, so {key} is dropped; the user types the review prompt themselves.
func reviewCommand(cfg *Config, resume bool) string {
	return strings.ReplaceAll(cfg.agent(resume), "{key}", "")
}

// cleanupReview deletes only the review/<N> branch it created; any other branch in the worktree is the user's.
func cleanupReview(ctx context.Context, cfg *Config, l PRLink, force bool) (Removal, error) {
	var wt *Worktree
	var sess *Session
	if l.Review != nil {
		wt, sess = l.Review.Worktree, l.Review.Session
	}
	r, ok := cfg.repo(l.Repo)
	if !ok {
		return Removal{}, fmt.Errorf("unknown repo %s", l.Repo)
	}
	// Commits pulled since the review started are only reachable from a fresh PR head; a failed fetch errs toward asking.
	if wt != nil {
		fetchReviewRef(ctx, r, l.Number)
	}
	rm, _, err := removeWorktree(ctx, cfg, wt, sess, nil, force)
	if err != nil || wt == nil {
		return rm, err
	}
	switch {
	case wt.Branch == "":
	case wt.Branch != reviewBranch(l.Number) || len(rm.Unpushed) > 0:
		rm.Kept = wt.Branch
	default:
		if _, err := runCmd(ctx, r.Dir(), "git", "branch", "-D", wt.Branch); err != nil {
			return rm, err
		}
	}
	_, err = runCmd(ctx, r.Dir(), "git", "update-ref", "-d", reviewRef(r, l.Number))
	return rm, err
}
