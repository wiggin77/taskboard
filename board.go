package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

type Snapshot struct {
	Tasks   []Task
	Errors  []string
	Updated time.Time
}

type Board struct {
	cfg *Config

	doneIssues map[string]JiraIssue

	mu   sync.RWMutex
	snap Snapshot
}

func NewBoard(cfg *Config) *Board {
	return &Board{cfg: cfg, doneIssues: map[string]JiraIssue{}}
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

func (b *Board) Run(ctx context.Context) {
	ticker := time.NewTicker(b.cfg.PollInterval.Duration)
	defer ticker.Stop()
	for {
		b.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
	ctx, cancel := context.WithTimeout(ctx, b.cfg.PollInterval.Duration)
	defer cancel()

	var (
		wg       sync.WaitGroup
		issues   []JiraIssue
		jiraErr  error
		sessions []Session
		repos    = make([]repoResult, len(b.cfg.Repos))
	)
	wg.Add(2 + len(b.cfg.Repos))
	go func() {
		defer wg.Done()
		issues, jiraErr = jiraSearch(ctx, b.cfg.Jira.JQL)
	}()
	go func() {
		defer wg.Done()
		sessions, _ = listSessions(ctx, b.cfg)
	}()
	for i, repo := range b.cfg.Repos {
		go func() {
			defer wg.Done()
			repos[i] = b.scanRepo(ctx, repo)
		}()
	}
	wg.Wait()

	var errs []string
	s := sources{issues: map[string]JiraIssue{}, sessions: sessions}
	if jiraErr != nil {
		errs = append(errs, "Jira search: "+jiraErr.Error())
	}
	for _, is := range issues {
		s.issues[is.Key] = is
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
		is, ok, err := jiraGet(ctx, key)
		if err != nil {
			errs = append(errs, fmt.Sprintf("Jira %s: %v", key, err))
			continue
		}
		if ok {
			s.issues[key] = is
			if is.Done {
				b.doneIssues[key] = is
			}
		}
	}

	snap := Snapshot{Tasks: buildTasks(b.cfg, s), Errors: errs, Updated: time.Now()}
	for _, e := range errs {
		log.Print(e)
	}
	b.mu.Lock()
	b.snap = snap
	b.mu.Unlock()
}

func (b *Board) scanRepo(ctx context.Context, repo RepoConfig) repoResult {
	var r repoResult
	open, err := listPRs(ctx, b.cfg, repo.Name, "open", 100)
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s open PRs: %v", repo.Name, err))
	}
	merged, err := listPRs(ctx, b.cfg, repo.Name, "merged", b.cfg.MergedPRLimit)
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s merged PRs: %v", repo.Name, err))
	}
	r.prs = append(open, merged...)

	if repo.Path == "" {
		return r
	}
	r.worktrees, err = listWorktrees(ctx, b.cfg, repo)
	if err != nil {
		r.errs = append(r.errs, fmt.Sprintf("%s worktrees: %v", repo.Name, err))
		return r
	}
	r.plans, err = findPlans(b.cfg, repo, r.worktrees)
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
