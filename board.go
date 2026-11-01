package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Snapshot struct {
	Tasks    []Task
	Reviews  []PRLink
	OtherPRs []PRLink
	Errors   []string
	Updated  time.Time
	Version  string
}

type Board struct {
	cfg atomic.Pointer[Config]

	doneIssues    map[string]JiraIssue
	missingIssues map[string]bool
	cachesReset   time.Time

	jiraUpdated  map[string]time.Time
	updatedCheck time.Time

	wake chan struct{}

	mu        sync.RWMutex
	snap      Snapshot
	lastView  time.Time
	running   bool
	requested bool
}

const (
	idleAfter     = 15 * time.Minute
	issueCacheTTL = 24 * time.Hour
)

func NewBoard(cfg *Config) *Board {
	b := &Board{
		doneIssues:    map[string]JiraIssue{},
		missingIssues: map[string]bool{},
		jiraUpdated:   map[string]time.Time{},
		cachesReset:   time.Now(),
		wake:          make(chan struct{}, 1),
	}
	b.cfg.Store(cfg)
	return b
}

// Config returns the current config, which callers must treat as read-only.
func (b *Board) Config() *Config { return b.cfg.Load() }

// SetConfig always requests a refresh, even during one, since that refresh used the old config.
func (b *Board) SetConfig(cfg *Config) {
	b.cfg.Store(cfg)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requestLocked()
}

func (b *Board) Viewed() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.idleLocked() && !b.running {
		b.requestLocked()
	}
	b.lastView = time.Now()
}

func (b *Board) Refresh() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running {
		b.requestLocked()
	}
}

// Invalidate requests a refresh even during one, since that refresh may have missed a change just made.
func (b *Board) Invalidate() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requestLocked()
}

func (b *Board) requestLocked() {
	b.requested = true
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Board) Refreshing() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.running || b.requested || b.snap.Updated.IsZero()
}

func (b *Board) idleLocked() bool { return time.Since(b.lastView) > idleAfter }

// nextWait returns 0 when the loop should wait for a view or a refresh request instead of a timer.
func (b *Board) nextWait() time.Duration {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.idleLocked() {
		return b.Config().IdlePollInterval.Duration
	}
	return b.Config().PollInterval.Duration
}

func (b *Board) Snapshot() Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.snap
}

func (b *Board) Task(key string) (Task, bool) {
	for _, t := range b.Snapshot().Tasks {
		if t.Key == key {
			return t, true
		}
	}
	return Task{}, false
}

func (b *Board) Review(name string) (PRLink, bool) {
	for _, l := range b.Snapshot().Reviews {
		if l.ReviewName != "" && l.ReviewName == name {
			return l, true
		}
	}
	return PRLink{}, false
}

func (b *Board) Run(ctx context.Context) {
	for {
		b.mu.Lock()
		b.running, b.requested = true, false
		select {
		case <-b.wake:
		default:
		}
		b.mu.Unlock()

		b.refresh(ctx)

		b.mu.Lock()
		b.running = false
		b.mu.Unlock()

		var timer *time.Timer
		var fire <-chan time.Time
		if wait := b.nextWait(); wait > 0 {
			timer = time.NewTimer(wait)
			fire = timer.C
		}
		select {
		case <-ctx.Done():
		case <-fire:
		case <-b.wake:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

type repoResult struct {
	prs       []PR
	worktrees []Worktree
	plans     []Plan
	errs      []string
}

func (b *Board) refresh(ctx context.Context) {
	cfg := b.Config()
	ctx, cancel := context.WithTimeout(ctx, cfg.RefreshTimeout.Duration)
	defer cancel()

	if time.Since(b.cachesReset) > issueCacheTTL {
		clear(b.doneIssues)
		clear(b.missingIssues)
		b.cachesReset = time.Now()
	}

	var (
		wg        sync.WaitGroup
		issues    []JiraIssue
		resolved  []JiraIssue
		jiraErr   error
		windows   []Session
		reviews   []PRLink
		otherPRs  []PRLink
		prLinkErr error
		repos     = make([]repoResult, len(cfg.Repos))
	)
	wg.Add(3 + len(cfg.Repos))
	go func() {
		defer wg.Done()
		issues, jiraErr = jiraSearch(ctx, cfg.jql())
		if jiraErr != nil {
			return
		}
		resolved, jiraErr = jiraSearch(ctx, fmt.Sprintf("assignee = currentUser() AND statusCategory = Done AND resolved >= -%dd", cfg.DoneDays))
	}()
	go func() {
		defer wg.Done()
		sweepLinked(ctx)
		windows = listWindows(ctx, cfg)
	}()
	go func() {
		defer wg.Done()
		reviews, otherPRs, prLinkErr = listPRLinks(ctx, cfg)
	}()
	for i, repo := range cfg.Repos {
		go func() {
			defer wg.Done()
			repos[i] = scanRepo(ctx, cfg, repo)
		}()
	}
	wg.Wait()

	var errs []string
	s := sources{issues: map[string]JiraIssue{}, resolved: map[string]bool{}, missing: b.missingIssues, sessions: ticketSessions(windows)}
	if jiraErr != nil {
		errs = append(errs, "Jira search: "+jiraErr.Error())
	}
	if prLinkErr != nil {
		errs = append(errs, "GitHub search: "+prLinkErr.Error())
	}
	for _, is := range issues {
		s.issues[is.Key] = is
	}
	for _, is := range resolved {
		s.issues[is.Key] = is
		s.resolved[is.Key] = true
	}
	for _, r := range repos {
		s.prs = append(s.prs, r.prs...)
		s.worktrees = append(s.worktrees, r.worktrees...)
		s.plans = append(s.plans, r.plans...)
		errs = append(errs, r.errs...)
	}

	for _, key := range b.unknownKeys(s) {
		if is, ok := b.doneIssues[key]; ok {
			s.issues[key] = is
			continue
		}
		if b.missingIssues[key] {
			continue
		}
		is, ok, err := jiraGet(ctx, key)
		if err != nil {
			errs = append(errs, fmt.Sprintf("Jira %s: %v", key, err))
			continue
		}
		if !ok {
			b.missingIssues[key] = true
			continue
		}
		s.issues[key] = is
		if is.Done {
			b.doneIssues[key] = is
		}
	}

	reviews, leftovers := attachReviews(cfg, reviews, s.worktrees, windows)
	for i := range leftovers {
		if err := viewPR(ctx, &leftovers[i]); err != nil {
			errs = append(errs, fmt.Sprintf("%s #%d: %v", leftovers[i].Repo, leftovers[i].Number, err))
		}
	}
	reviews = append(reviews, leftovers...)

	if err := b.fillJiraUpdated(ctx, s.issues); err != nil {
		errs = append(errs, "Jira updated dates: "+err.Error())
	}

	snap := Snapshot{Tasks: buildTasks(cfg, s, time.Now()), Reviews: reviews, OtherPRs: otherPRs, Errors: errs, Updated: time.Now()}
	snap.Version = snapshotVersion(snap)
	for _, e := range errs {
		log.Print(e)
	}
	b.mu.Lock()
	b.snap = snap
	b.mu.Unlock()
}

func scanRepo(ctx context.Context, cfg *Config, repo RepoConfig) repoResult {
	var r repoResult
	open, err := listPRs(ctx, cfg, repo.Name, "open", 100, "")
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s open PRs: %v", repo.Name, err))
	}
	merged, err := listPRs(ctx, cfg, repo.Name, "merged", cfg.MergedPRLimit, "")
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s merged PRs: %v", repo.Name, err))
	}
	cutoff := time.Now().AddDate(0, 0, -cfg.DoneDays).Format("2006-01-02")
	recent, err := listPRs(ctx, cfg, repo.Name, "merged", 100, "merged:>="+cutoff)
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s recently merged PRs: %v", repo.Name, err))
	}
	seen := map[int]bool{}
	for _, pr := range merged {
		seen[pr.Number] = true
	}
	for _, pr := range recent {
		if !seen[pr.Number] {
			merged = append(merged, pr)
		}
	}
	r.prs = append(open, merged...)

	if repo.Path == "" {
		return r
	}
	r.worktrees, err = listWorktrees(ctx, cfg, repo)
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s worktrees: %v", repo.Name, err))
		return r
	}
	r.plans, err = findPlans(cfg, repo, r.worktrees)
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s plans: %v", repo.Name, err))
	}
	return r
}

func (b *Board) unknownKeys(s sources) []string {
	seen := map[string]bool{}
	var keys []string
	add := func(k string) {
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		if _, ok := s.issues[k]; !ok {
			keys = append(keys, k)
		}
	}
	for _, pr := range s.prs {
		add(pr.Key)
	}
	for _, wt := range s.worktrees {
		add(wt.Key)
	}
	for _, sess := range s.sessions {
		add(sess.Key)
	}
	return keys
}

// acli search cannot return the updated field, so dates come from one view call per issue. They are
// cached, and each refresh re-fetches only the issues a search reports as changed since the last check.
func (b *Board) fillJiraUpdated(ctx context.Context, issues map[string]JiraIssue) error {
	if b.updatedCheck.IsZero() {
		b.updatedCheck = time.Now()
	}
	var cached, missing []string
	for key, is := range issues {
		switch {
		case !is.Updated.IsZero():
			b.jiraUpdated[key] = is.Updated
		case b.jiraUpdated[key].IsZero():
			missing = append(missing, key)
		default:
			cached = append(cached, key)
		}
	}

	var searchErr error
	checked := time.Now()
	if len(cached) > 0 {
		mins := int(checked.Sub(b.updatedCheck).Minutes()) + 2
		var changed []JiraIssue
		changed, searchErr = jiraSearch(ctx, fmt.Sprintf("key in (%s) AND updated >= -%dm", strings.Join(cached, ","), mins))
		for _, is := range changed {
			missing = append(missing, is.Key)
		}
	}

	fetched := make([]time.Time, len(missing))
	errs := make([]error, len(missing))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, key := range missing {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var is JiraIssue
			is, _, errs[i] = jiraGet(ctx, key)
			fetched[i] = is.Updated
		}()
	}
	wg.Wait()
	for i, key := range missing {
		if !fetched[i].IsZero() {
			b.jiraUpdated[key] = fetched[i]
		}
	}
	err := errors.Join(append(errs, searchErr)...)
	if err == nil {
		b.updatedCheck = checked
	}

	for key := range b.jiraUpdated {
		if _, ok := issues[key]; !ok {
			delete(b.jiraUpdated, key)
		}
	}
	for key, is := range issues {
		is.Updated = b.jiraUpdated[key]
		issues[key] = is
	}
	return err
}

// The version leaves out Updated so a refresh that finds nothing new does not reload open pages.
func snapshotVersion(snap Snapshot) string {
	b, err := json.Marshal([]any{snap.Tasks, snap.Reviews, snap.OtherPRs, snap.Errors})
	if err != nil {
		return snap.Updated.String()
	}
	h := fnv.New64a()
	h.Write(b)
	return strconv.FormatUint(h.Sum64(), 36)
}
