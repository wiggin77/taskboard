package main

import (
	"context"
	"encoding/json"
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
	Notices  []string
	AsOf     []string
	Updated  time.Time
	Version  string
}

type Board struct {
	cfg atomic.Pointer[Config]

	doneIssues    map[string]JiraIssue
	missingIssues map[string]bool
	unlistedOpen  map[string]JiraIssue
	cachesReset   time.Time

	jiraDetails  map[string]jiraDetail
	children     map[string][]JiraIssue
	updatedCheck time.Time
	fullCheck    time.Time

	ghPause ghPause
	ghRate  ghRate

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
		unlistedOpen:  map[string]JiraIssue{},
		jiraDetails:   map[string]jiraDetail{},
		children:      map[string][]JiraIssue{},
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
	jiraAt   time.Time
	github   ghData
	started  time.Time
	notices  []string
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
		clear(b.unlistedOpen)
		b.cachesReset = time.Now()
	}

	gen := b.localGen.Load()
	prev := b.lastFetched()
	started := time.Now()
	var (
		wg       sync.WaitGroup
		issues   []JiraIssue
		resolved []JiraIssue
		jiraErr  error
		loc      localState
		ghd      ghData
		ghErrs   []string
		ghNotice string
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		issues, jiraErr = jiraSearch(ctx, cfg.jql())
		if jiraErr != nil {
			return
		}
		resolved, jiraErr = jiraSearch(ctx, cfg.doneJQL())
	}()
	go func() {
		defer wg.Done()
		sweepLinked(ctx)
		loc = scanLocal(ctx, cfg)
	}()
	go func() {
		defer wg.Done()
		ghd, ghErrs, ghNotice = b.fetchGitHub(ctx, cfg, prev.github)
	}()
	wg.Wait()
	gh := githubResult(cfg, ghd.login, ghd.found)
	reviews := gh.reviews

	now := time.Now()
	r := &fetched{
		cfg:      cfg,
		issues:   map[string]JiraIssue{},
		resolved: map[string]bool{},
		jiraAt:   now,
		github:   ghd,
		started:  started,
		prs:      gh.prs,
		reviews:  reviews,
		otherPRs: gh.others,
		views:    map[string]PRLink{},
		errs:     ghErrs,
	}
	if ghNotice != "" {
		r.notices = append(r.notices, ghNotice)
	}
	if jiraErr != nil {
		r.issues, r.resolved, r.jiraAt = maps.Clone(prev.issues), maps.Clone(prev.resolved), prev.jiraAt
		if r.issues == nil {
			r.issues, r.resolved = map[string]JiraIssue{}, map[string]bool{}
		}
		r.errs = append(r.errs, "Jira search: "+jiraErr.Error())
	}
	for _, is := range issues {
		r.issues[is.Key] = is
		delete(r.resolved, is.Key)
	}
	for _, is := range resolved {
		r.issues[is.Key] = is
		r.resolved[is.Key] = true
	}

	var recheck, view []string
	for _, key := range b.unknownKeys(sources{issues: r.issues, prs: r.prs, worktrees: loc.worktrees, sessions: ticketSessions(loc.windows)}) {
		done, isDone := b.doneIssues[key]
		open, isOpen := b.unlistedOpen[key]
		switch {
		case isDone:
			r.issues[key] = done
		case b.missingIssues[key] || jiraErr != nil:
		case isOpen:
			r.issues[key] = open
			recheck = append(recheck, key)
		default:
			view = append(view, key)
		}
	}
	full := time.Since(b.fullCheck) > fullCheckEvery
	if full {
		view = append(view, b.searchUnlisted(ctx, r.issues, recheck)...)
		recheck = nil
	}

	var jiraErrs []string
	jiraDone := make(chan struct{})
	go func() {
		defer close(jiraDone)
		if jiraErr != nil {
			b.applyJiraDetails(r.issues)
			r.children = prev.children
			return
		}
		r.children, jiraErrs = b.updateJira(ctx, r.issues, prev.issues, recheck, view, full)
	}()

	_, leftovers := attachReviews(cfg, reviews, loc.worktrees, loc.windows)
	if len(leftovers) > 0 {
		var gone map[string]bool
		if !time.Now().Before(b.ghPause.until) {
			var err error
			if r.views, gone, err = viewPRs(ctx, ghGraphQL, cfg, leftovers); err != nil {
				r.errs = append(r.errs, "GitHub review checkouts: "+err.Error())
				if isRateLimited(err) {
					b.pauseAfter(ctx, err, ghRate{}, time.Now())
					r.notices = []string{b.ghPause.notice()}
				}
			}
		}
		for _, l := range leftovers {
			if _, ok := r.views[l.URL]; !ok && !gone[l.URL] {
				if v, ok := prev.views[l.URL]; ok {
					r.views[l.URL] = v
				}
			}
		}
	}

	<-jiraDone
	r.missing = maps.Clone(b.missingIssues)
	r.errs = append(r.errs, jiraErrs...)
	r.updated = time.Now()

	for _, e := range b.compose(ctx, r, &loc, gen) {
		log.Print(e)
	}
}

// lastFetched returns the last refresh's data, or an empty one before the first refresh.
func (b *Board) lastFetched() *fetched {
	b.composeMu.Lock()
	defer b.composeMu.Unlock()
	if b.fetched == nil {
		return &fetched{}
	}
	return b.fetched
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
	tasks := buildTasks(r.cfg, s, time.Now())
	reviews, leftovers := attachReviews(r.cfg, r.reviews, loc.worktrees, loc.windows)
	reviews = withoutTaskPRs(reviews, tasks, r.github.login)
	for i, l := range leftovers {
		if v, ok := r.views[l.URL]; ok {
			v.Role, v.ReviewName, v.Review = l.Role, l.ReviewName, l.Review
			leftovers[i] = v
		}
	}
	errs := append(slices.Clone(r.errs), loc.errs...)
	var stale []string
	if at := r.github.staleSince(r.started); !at.IsZero() {
		stale = append(stale, "GitHub as of "+asOf(at, r.updated))
	}
	if !r.jiraAt.IsZero() && r.jiraAt.Before(r.started) {
		stale = append(stale, "Jira as of "+asOf(r.jiraAt, r.updated))
	}
	snap := Snapshot{Tasks: tasks, Epics: buildEpics(r.cfg, tasks, r.children), Reviews: append(reviews, leftovers...), OtherPRs: r.otherPRs,
		Errors: errs, Notices: r.notices, AsOf: stale, Updated: r.updated}
	snap.Version = snapshotVersion(snap)
	b.mu.Lock()
	b.snap = snap
	b.mu.Unlock()
	return errs
}

// searchUnlisted returns the keys it didn't get back, such as moved or deleted issues, so they can be viewed one by one.
func (b *Board) searchUnlisted(ctx context.Context, issues map[string]JiraIssue, keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	found, err := jiraSearch(ctx, "key in ("+strings.Join(keys, ",")+")")
	if err != nil {
		return keys
	}
	got := map[string]JiraIssue{}
	for _, is := range found {
		got[is.Key] = is
	}
	return slices.DeleteFunc(keys, func(key string) bool {
		is, ok := got[key]
		if ok {
			b.keepUnlisted(issues, key, is)
		}
		return ok
	})
}

func (b *Board) keepUnlisted(issues map[string]JiraIssue, key string, is JiraIssue) {
	issues[key] = is
	if is.Done {
		b.doneIssues[key] = is
		delete(b.unlistedOpen, key)
	} else {
		b.unlistedOpen[key] = is
	}
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

const fullCheckEvery = time.Hour

// updateJira views the issues in view, and fills in what acli search cannot return (the updated and parent
// fields) with one view per issue. Those details, open issues outside the JQL (recheck) and epic children
// are cached, and one search for what changed since the last check decides which issues to view again.
// Moving an issue to another epic changes its updated date, so that search catches new parents too. Deleted
// issues never show up in it, so with full every open epic's children are searched again.
func (b *Board) updateJira(ctx context.Context, issues, prevIssues map[string]JiraIssue, recheck, view []string, full bool) (map[string][]JiraIssue, []string) {
	checked := time.Now()
	if b.updatedCheck.IsZero() {
		b.updatedCheck = checked
	}
	viewed, rechecked := setOf(view), setOf(recheck)
	lookup := maps.Clone(viewed)
	watch := maps.Clone(rechecked)
	for key, is := range issues {
		if !is.Updated.IsZero() {
			b.jiraDetails[key] = detailOf(is)
		}
		switch {
		case b.jiraDetails[key].Updated.IsZero():
			lookup[key] = true
		case is.Updated.IsZero():
			watch[key] = true
		}
	}
	for _, kids := range b.children {
		for _, c := range kids {
			watch[c.Key] = true
		}
	}

	var errs []string
	changed := map[string]bool{}
	if epics := slices.Sorted(maps.Keys(b.children)); len(watch) > 0 || len(epics) > 0 {
		mins := int(checked.Sub(b.updatedCheck).Minutes()) + 2
		found, err := jiraSearch(ctx, changedJQL(slices.Sorted(maps.Keys(watch)), epics, mins))
		if err != nil {
			errs = append(errs, "Jira changes: "+err.Error())
		}
		for _, is := range found {
			changed[is.Key], lookup[is.Key] = true, true
		}
	}

	keys := slices.Sorted(maps.Keys(lookup))
	got := make([]JiraIssue, len(keys))
	ok := make([]bool, len(keys))
	getErrs := make([]error, len(keys))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, key := range keys {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			got[i], ok[i], getErrs[i] = jiraGet(ctx, key)
		})
	}
	wg.Wait()
	for i, key := range keys {
		if getErrs[i] != nil {
			errs = append(errs, fmt.Sprintf("Jira %s: %v", key, getErrs[i]))
			if is, found := prevIssues[key]; found && viewed[key] {
				issues[key] = is
			}
			continue
		}
		if changed[key] {
			moveChild(b.children, key, got[i], ok[i])
		}
		switch {
		case !viewed[key] && !rechecked[key]:
		case !ok[i]:
			b.missingIssues[key] = true
			delete(b.unlistedOpen, key)
			delete(issues, key)
		default:
			b.keepUnlisted(issues, key, got[i])
		}
		if _, listed := issues[key]; listed && ok[i] && !got[i].Updated.IsZero() {
			b.jiraDetails[key] = detailOf(got[i])
		}
	}
	if len(errs) == 0 {
		b.updatedCheck = checked
	}
	for key := range b.jiraDetails {
		if _, listed := issues[key]; !listed {
			delete(b.jiraDetails, key)
		}
	}
	b.applyJiraDetails(issues)

	var open, search []string
	for key, is := range issues {
		if is.Epic && !is.Done {
			open = append(open, key)
		}
	}
	for epic := range b.children {
		if !slices.Contains(open, epic) {
			delete(b.children, epic)
		}
	}
	for _, epic := range open {
		if _, cached := b.children[epic]; full || !cached {
			search = append(search, epic)
		}
	}
	slices.Sort(search)
	found, childErrs := epicChildren(ctx, search)
	maps.Copy(b.children, found)
	if full {
		b.fullCheck = checked
	}
	return maps.Clone(b.children), append(errs, childErrs...)
}

func changedJQL(keys, epics []string, mins int) string {
	var terms []string
	if len(keys) > 0 {
		terms = append(terms, "key in ("+strings.Join(keys, ",")+")")
	}
	if len(epics) > 0 {
		terms = append(terms, "parent in ("+strings.Join(epics, ",")+")")
	}
	return fmt.Sprintf("(%s) AND updated >= -%dm", strings.Join(terms, " OR "), mins)
}

// moveChild puts a changed issue first under the epic it now belongs to, as the most recently updated. It
// never changes a list in place, since earlier refreshes' data shares them.
func moveChild(children map[string][]JiraIssue, key string, is JiraIssue, found bool) {
	for epic, kids := range children {
		if i := slices.IndexFunc(kids, func(c JiraIssue) bool { return c.Key == key }); i >= 0 {
			children[epic] = slices.Delete(slices.Clone(kids), i, i+1)
		}
	}
	if kids, cached := children[is.Parent]; found && cached {
		children[is.Parent] = slices.Concat([]JiraIssue{is}, kids)
	}
}

func setOf(keys []string) map[string]bool {
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	return set
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
	b, err := json.Marshal([]any{snap.Tasks, snap.Epics, snap.Reviews, snap.OtherPRs, snap.Errors, snap.Notices, snap.AsOf})
	if err != nil {
		return snap.Updated.String()
	}
	h := fnv.New64a()
	h.Write(b)
	return strconv.FormatUint(h.Sum64(), 36)
}
