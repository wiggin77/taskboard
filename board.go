package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Snapshot struct {
	Tasks    []Task
	Epics    []EpicGroup
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

	jiraDetails  map[string]jiraDetail
	updatedCheck time.Time

	wake chan struct{}

	highlights *highlights

	composeMu sync.Mutex
	fetched   *fetched
	localGen  atomic.Uint64

	mu        sync.RWMutex
	snap      Snapshot
	lastView  time.Time
	running   bool
	requested bool
}

const (
	idleAfter     = 90 * time.Second
	issueCacheTTL = 24 * time.Hour
)

func NewBoard(cfg *Config) *Board {
	b := &Board{
		doneIssues:    map[string]JiraIssue{},
		missingIssues: map[string]bool{},
		jiraDetails:   map[string]jiraDetail{},
		cachesReset:   time.Now(),
		wake:          make(chan struct{}, 1),
		highlights:    &highlights{ids: map[string]bool{}},
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

// Viewed wakes the poll loop when the board comes back on screen. It refreshes at once only when the last
// refresh is older than poll_interval, so switching tabs can't refresh more often than polling would.
func (b *Board) Viewed() {
	b.mu.Lock()
	defer b.mu.Unlock()
	wasIdle := b.idleLocked()
	b.lastView = time.Now()
	cfg := b.Config()
	if !wasIdle || cfg.ManualRefresh || b.running {
		return
	}
	if time.Since(b.snap.Updated) >= cfg.PollInterval.Duration {
		b.requestLocked()
	} else {
		b.wakeLocked()
	}
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
	b.wakeLocked()
}

func (b *Board) wakeLocked() {
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

// nextWait returns the time left until the next timed refresh, or false when there is none because the
// board is off screen or refreshes only on demand.
func (b *Board) nextWait() (time.Duration, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	cfg := b.Config()
	if cfg.ManualRefresh || b.idleLocked() {
		return 0, false
	}
	return max(cfg.PollInterval.Duration-time.Since(b.snap.Updated), 0), true
}

func (b *Board) Snapshot() Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.snap
}

func (b *Board) Task(key string) (Task, bool) {
	tasks := b.Snapshot().Tasks
	if i := slices.IndexFunc(tasks, func(t Task) bool { return t.Key == key }); i >= 0 {
		return tasks[i], true
	}
	return Task{}, false
}

func (b *Board) Review(name string) (PRLink, bool) {
	reviews := b.Snapshot().Reviews
	if i := slices.IndexFunc(reviews, func(l PRLink) bool { return l.ReviewName != "" && l.ReviewName == name }); i >= 0 {
		return reviews[i], true
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

		if !b.waitForRefresh(ctx) {
			return
		}
	}
}

// waitForRefresh returns when the poll timer fires or a refresh is requested. A wake without a request
// (the board back on screen with recent data) only re-arms the timer.
func (b *Board) waitForRefresh(ctx context.Context) bool {
	for {
		var timer *time.Timer
		var fire <-chan time.Time
		if wait, ok := b.nextWait(); ok {
			timer = time.NewTimer(wait)
			fire = timer.C
		}
		select {
		case <-ctx.Done():
			return false
		case <-fire:
			return true
		case <-b.wake:
		}
		if timer != nil {
			timer.Stop()
		}
		b.mu.RLock()
		requested := b.requested
		b.mu.RUnlock()
		if requested {
			return true
		}
	}
}

// fetched is what a full refresh got from Jira and GitHub. It is never modified once stored, so a
// session action can rebuild the board on top of it without waiting for another refresh.
type fetched struct {
	cfg      *Config
	issues   map[string]JiraIssue
	resolved map[string]bool
	missing  map[string]bool
	children map[string][]JiraIssue
	prs      []PR
	reviews  []PRLink
	otherPRs []PRLink
	views    map[string]PRLink
	errs     []string
	updated  time.Time
}

type localState struct {
	worktrees []Worktree
	plans     []Plan
	windows   []Session
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

	gen := b.localGen.Load()
	var (
		wg        sync.WaitGroup
		issues    []JiraIssue
		resolved  []JiraIssue
		jiraErr   error
		loc       localState
		reviews   []PRLink
		otherPRs  []PRLink
		prLinkErr error
		prs       = make([][]PR, len(cfg.Repos))
		prErrs    = make([][]string, len(cfg.Repos))
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
		loc = scanLocal(ctx, cfg)
	}()
	go func() {
		defer wg.Done()
		reviews, otherPRs, prLinkErr = listPRLinks(ctx, cfg)
	}()
	for i, repo := range cfg.Repos {
		go func() {
			defer wg.Done()
			prs[i], prErrs[i] = repoPRs(ctx, cfg, repo)
		}()
	}
	wg.Wait()

	r := &fetched{
		cfg:      cfg,
		issues:   map[string]JiraIssue{},
		resolved: map[string]bool{},
		reviews:  reviews,
		otherPRs: otherPRs,
		views:    map[string]PRLink{},
	}
	if jiraErr != nil {
		r.errs = append(r.errs, "Jira search: "+jiraErr.Error())
	}
	if prLinkErr != nil {
		r.errs = append(r.errs, "GitHub search: "+prLinkErr.Error())
	}
	for _, is := range issues {
		r.issues[is.Key] = is
	}
	for _, is := range resolved {
		r.issues[is.Key] = is
		r.resolved[is.Key] = true
	}
	for i := range prs {
		r.prs = append(r.prs, prs[i]...)
		r.errs = append(r.errs, prErrs[i]...)
	}

	for _, key := range b.unknownKeys(sources{issues: r.issues, prs: r.prs, worktrees: loc.worktrees, sessions: ticketSessions(loc.windows)}) {
		if is, ok := b.doneIssues[key]; ok {
			r.issues[key] = is
			continue
		}
		if b.missingIssues[key] || jiraErr != nil {
			continue
		}
		is, ok, err := jiraGet(ctx, key)
		if err != nil {
			r.errs = append(r.errs, fmt.Sprintf("Jira %s: %v", key, err))
			continue
		}
		if !ok {
			b.missingIssues[key] = true
			continue
		}
		r.issues[key] = is
		if is.Done {
			b.doneIssues[key] = is
		}
	}
	r.missing = maps.Clone(b.missingIssues)

	var epics []string
	for key, is := range r.issues {
		if is.Epic && !is.Done && jiraErr == nil {
			epics = append(epics, key)
		}
	}
	slices.Sort(epics)
	var childErrs []string
	childrenDone := make(chan struct{})
	go func() {
		defer close(childrenDone)
		r.children, childErrs = epicChildren(ctx, epics)
	}()

	_, leftovers := attachReviews(cfg, reviews, loc.worktrees, loc.windows)
	for _, l := range leftovers {
		url := l.URL
		if err := viewPR(ctx, cfg, &l); err != nil {
			r.errs = append(r.errs, fmt.Sprintf("%s #%d: %v", l.Repo, l.Number, err))
			continue
		}
		r.views[url] = l
	}

	if jiraErr != nil {
		b.applyJiraDetails(r.issues)
	} else if err := b.fillJiraDetails(ctx, r.issues); err != nil {
		r.errs = append(r.errs, "Jira details: "+err.Error())
	}
	<-childrenDone
	r.errs = append(r.errs, childErrs...)
	r.updated = time.Now()

	for _, e := range b.compose(ctx, r, &loc, gen) {
		log.Print(e)
	}
}

// LocalChanged shows a session action's result at once, from fresh local state and the last remote
// data, and requests a full refresh for the rest.
func (b *Board) LocalChanged(ctx context.Context) {
	b.localGen.Add(1)
	b.compose(ctx, nil, nil, 0)
	b.Invalidate()
}

// compose publishes a snapshot from r (the stored fetched data when nil) and loc. It re-reads local state
// when loc is nil or a session action has run since gen, so a slow refresh can't undo an action's result.
func (b *Board) compose(ctx context.Context, r *fetched, loc *localState, gen uint64) []string {
	b.composeMu.Lock()
	defer b.composeMu.Unlock()
	if r == nil {
		if r = b.fetched; r == nil {
			return nil
		}
	}
	b.fetched = r
	if loc == nil || b.localGen.Load() != gen {
		l := scanLocal(ctx, r.cfg)
		loc = &l
	}

	s := sources{issues: r.issues, resolved: r.resolved, missing: r.missing, prs: r.prs,
		worktrees: loc.worktrees, plans: loc.plans, sessions: ticketSessions(loc.windows)}
	reviews, leftovers := attachReviews(r.cfg, r.reviews, loc.worktrees, loc.windows)
	for i, l := range leftovers {
		if v, ok := r.views[l.URL]; ok {
			v.Role, v.ReviewName, v.Review = l.Role, l.ReviewName, l.Review
			leftovers[i] = v
		}
	}
	errs := append(slices.Clone(r.errs), loc.errs...)
	tasks := buildTasks(r.cfg, s, time.Now())
	snap := Snapshot{Tasks: tasks, Epics: buildEpics(r.cfg, tasks, r.children), Reviews: append(reviews, leftovers...), OtherPRs: r.otherPRs, Errors: errs, Updated: r.updated}
	snap.Version = snapshotVersion(snap)
	b.mu.Lock()
	b.snap = snap
	b.mu.Unlock()
	return errs
}

func repoPRs(ctx context.Context, cfg *Config, repo RepoConfig) ([]PR, []string) {
	var errs []string
	open, err := listPRs(ctx, cfg, repo.Name, "open", 100, "")
	if err != nil {
		errs = append(errs, fmt.Sprintf("%s open PRs: %v", repo.Name, err))
	}
	merged, err := listPRs(ctx, cfg, repo.Name, "merged", cfg.MergedPRLimit, "")
	if err != nil {
		errs = append(errs, fmt.Sprintf("%s merged PRs: %v", repo.Name, err))
	}
	cutoff := time.Now().AddDate(0, 0, -cfg.DoneDays).Format("2006-01-02")
	recent, err := listPRs(ctx, cfg, repo.Name, "merged", 100, "merged:>="+cutoff)
	if err != nil {
		errs = append(errs, fmt.Sprintf("%s recently merged PRs: %v", repo.Name, err))
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
	return append(open, merged...), errs
}

func scanLocal(ctx context.Context, cfg *Config) localState {
	l := localState{windows: listWindows(ctx, cfg)}
	for _, repo := range cfg.Repos {
		if repo.Path == "" {
			continue
		}
		wts, err := listWorktrees(ctx, cfg, repo)
		if err != nil {
			l.errs = append(l.errs, fmt.Sprintf("%s worktrees: %v", repo.Name, err))
			continue
		}
		l.worktrees = append(l.worktrees, wts...)
		plans, err := findPlans(cfg, repo, wts)
		if err != nil {
			l.errs = append(l.errs, fmt.Sprintf("%s plans: %v", repo.Name, err))
		}
		l.plans = append(l.plans, plans...)
	}
	return l
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

type jiraDetail struct {
	Updated       time.Time
	Parent        string
	ParentSummary string
}

func detailOf(is JiraIssue) jiraDetail {
	return jiraDetail{Updated: is.Updated, Parent: is.Parent, ParentSummary: is.ParentSummary}
}

// acli search cannot return the updated or parent fields, so they come from one view call per issue. They
// are cached, and each refresh re-fetches only the issues a search reports as changed since the last check.
// Moving an issue to another epic changes its updated date, so that check catches new parents too.
func (b *Board) fillJiraDetails(ctx context.Context, issues map[string]JiraIssue) error {
	if b.updatedCheck.IsZero() {
		b.updatedCheck = time.Now()
	}
	var cached, missing []string
	for key, is := range issues {
		switch {
		case !is.Updated.IsZero():
			b.jiraDetails[key] = detailOf(is)
		case b.jiraDetails[key].Updated.IsZero():
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

	fetched := make([]JiraIssue, len(missing))
	errs := make([]error, len(missing))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, key := range missing {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			fetched[i], _, errs[i] = jiraGet(ctx, key)
		})
	}
	wg.Wait()
	for i, key := range missing {
		if !fetched[i].Updated.IsZero() {
			b.jiraDetails[key] = detailOf(fetched[i])
		}
	}
	err := errors.Join(append(errs, searchErr)...)
	if err == nil {
		b.updatedCheck = checked
	}

	for key := range b.jiraDetails {
		if _, ok := issues[key]; !ok {
			delete(b.jiraDetails, key)
		}
	}
	b.applyJiraDetails(issues)
	return err
}

func (b *Board) applyJiraDetails(issues map[string]JiraIssue) {
	for key, is := range issues {
		d, ok := b.jiraDetails[key]
		if !ok || !is.Updated.IsZero() {
			continue
		}
		is.Updated, is.Parent, is.ParentSummary = d.Updated, d.Parent, d.ParentSummary
		issues[key] = is
	}
}

func epicChildren(ctx context.Context, epics []string) (map[string][]JiraIssue, []string) {
	found := make([][]JiraIssue, len(epics))
	errs := make([]error, len(epics))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, key := range epics {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			found[i], errs[i] = jiraChildren(ctx, key)
		})
	}
	wg.Wait()
	children := map[string][]JiraIssue{}
	var msgs []string
	for i, key := range epics {
		if errs[i] != nil {
			msgs = append(msgs, fmt.Sprintf("Jira children of %s: %v", key, errs[i]))
			continue
		}
		children[key] = found[i]
	}
	return children, msgs
}

// The version leaves out Updated so a refresh that finds nothing new does not reload open pages.
func snapshotVersion(snap Snapshot) string {
	b, err := json.Marshal([]any{snap.Tasks, snap.Epics, snap.Reviews, snap.OtherPRs, snap.Errors})
	if err != nil {
		return snap.Updated.String()
	}
	h := fnv.New64a()
	h.Write(b)
	return strconv.FormatUint(h.Sum64(), 36)
}
