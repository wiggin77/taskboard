package main

import (
	"cmp"
	"path"
	"slices"
	"strconv"
	"time"
)

type Stage int

// Declared in display priority order; sorting relies on it.
const (
	StageCIFailing Stage = iota
	StageChangesRequested
	StageReviewRequested
	StageReadyToMerge
	StagePlanning
	StageMergedJiraPending
	StageAwaitingChanges
	StageInReview
	StageCoding
	StageTicket
	StageCleanup
	StageDone
)

type stageMeta struct {
	label   string
	tone    string
	group   string
	station int
}

var stageInfo = map[Stage]stageMeta{
	StageCIFailing:         {"CI failing", "fail", groupNeedsYou, 3},
	StageChangesRequested:  {"Changes requested", "fail", groupNeedsYou, 3},
	StageReviewRequested:   {"Review requested", "needs", groupNeedsYou, 3},
	StageReadyToMerge:      {"Ready to merge", "ok", groupNeedsYou, 4},
	StagePlanning:          {"Plan needs review", "needs", groupNeedsYou, 1},
	StageMergedJiraPending: {"Merged, update Jira", "needs", groupNeedsYou, 5},
	StageAwaitingChanges:   {"Changes requested, waiting on author", "active", groupWaiting, 3},
	StageInReview:          {"In review", "active", groupWaiting, 3},
	StageCoding:            {"Coding", "active", groupInProgress, 2},
	StageTicket:            {"Not started", "idle", groupNotStarted, 0},
	StageCleanup:           {"Done, clean up", "needs", groupCleanup, 6},
	StageDone:              {"Done", "ok", groupDone, 6},
}

const (
	groupNeedsYou   = "Needs you"
	groupWaiting    = "Waiting on others"
	groupInProgress = "In progress"
	groupNotStarted = "Not started"
	groupEpics      = "Epics"
	groupCleanup    = "Needs clean-up"
	groupDone       = "Done"
)

var stations = []string{"Ticket", "Plan", "Code", "Review", "Ready", "Merged", "Done"}

type Task struct {
	Key           string
	Summary       string
	JiraURL       string
	JiraStatus    string
	JiraDone      bool
	JiraMissing   bool
	NoTicket      bool
	Epic          bool
	Parent        string
	ParentSummary string
	ParentURL     string
	Updated       time.Time
	Stage         Stage
	PR            *PR
	Plan          *Plan
	Session       *Session
	Worktree      *Worktree
	// NoCheckout is true when the PR's repo has a configured folder that does not exist on disk.
	NoCheckout bool
}

func (t Task) StageLabel() string { return stageInfo[t.Stage].label }
func (t Task) Tone() string       { return stageInfo[t.Stage].tone }

// taskGroup leaves an epic with no work of its own out of the stage sections; it shows only under Epics.
func taskGroup(t Task) string {
	if t.Epic && t.Stage == StageTicket {
		return groupEpics
	}
	return stageInfo[t.Stage].group
}

func (t Task) NeedsCleanup() bool { return t.Stage == StageCleanup }

// removableWorktree is nil for a main checkout, which clean-up leaves in place.
func (t Task) removableWorktree() *Worktree {
	if t.Worktree == nil || t.Worktree.Main {
		return nil
	}
	return t.Worktree
}

func (t Task) CanStart() bool {
	if t.NoTicket || t.Session != nil {
		return false
	}
	switch t.Stage {
	case StageMergedJiraPending, StageCleanup, StageDone:
		return false
	}
	return true
}

func (t Task) DisplayKey() string {
	if t.NoTicket {
		return path.Base(t.PR.Repo) + "#" + strconv.Itoa(t.PR.Number)
	}
	return t.Key
}

func (t Task) doneSince(cutoff time.Time, recentlyResolved bool) bool {
	if recentlyResolved {
		return true
	}
	if t.PR != nil && t.PR.State == "MERGED" {
		return !t.PR.MergedAt.Before(cutoff)
	}
	return !t.Updated.Before(cutoff)
}

type Stop struct {
	Label string
	State string
}

func (t Task) Track() []Stop {
	cur := stageInfo[t.Stage].station
	stops := make([]Stop, len(stations))
	for i, label := range stations {
		state := "next"
		if i < cur {
			state = "past"
		} else if i == cur {
			state = "now"
		}
		stops[i] = Stop{Label: label, State: state}
	}
	return stops
}

func stageFor(t *Task) Stage {
	st := progressStage(t)
	if st == StageDone && (t.removableWorktree() != nil || t.Session != nil) {
		return StageCleanup
	}
	return st
}

func progressStage(t *Task) Stage {
	if pr := t.PR; pr != nil {
		switch pr.State {
		case "OPEN":
			switch {
			case pr.Draft:
				return StageCoding
			case pr.CI == CIFailing:
				return StageCIFailing
			case pr.ReviewRequested:
				return StageReviewRequested
			case pr.Review == "CHANGES_REQUESTED" && pr.Assigned:
				return StageAwaitingChanges
			case pr.Review == "CHANGES_REQUESTED":
				return StageChangesRequested
			case pr.Review == "APPROVED" && pr.CI != CIPending && !pr.ReviewerPending:
				return StageReadyToMerge
			default:
				return StageInReview
			}
		case "MERGED":
			if t.JiraDone || t.JiraMissing || t.NoTicket {
				return StageDone
			}
			return StageMergedJiraPending
		}
	}
	if t.JiraDone {
		return StageDone
	}
	if t.Plan != nil && !t.Plan.Approved {
		return StagePlanning
	}
	if t.Session != nil || t.Worktree != nil {
		return StageCoding
	}
	return StageTicket
}

func preferPR(candidate, current *PR) bool {
	if current == nil {
		return true
	}
	if (candidate.State == "OPEN") != (current.State == "OPEN") {
		return candidate.State == "OPEN"
	}
	return candidate.UpdatedAt.After(current.UpdatedAt)
}

type sources struct {
	issues    map[string]JiraIssue
	resolved  map[string]bool
	missing   map[string]bool
	prs       []PR
	plans     []Plan
	worktrees []Worktree
	sessions  []Session
}

// Plans only attach to tasks found elsewhere, so old plan files for long-closed tickets stay off the board.
func buildTasks(cfg *Config, s sources, now time.Time) []Task {
	byKey := map[string]*Task{}
	get := func(key string) *Task {
		t, ok := byKey[key]
		if !ok {
			t = &Task{Key: key, JiraURL: cfg.issueURL(key)}
			byKey[key] = t
		}
		return t
	}

	for key, is := range s.issues {
		t := get(key)
		t.Summary = is.Summary
		t.JiraStatus = is.Status
		t.JiraDone = is.Done
		t.Updated = is.Updated
		t.Epic = is.Epic
		if is.Parent != "" {
			t.Parent, t.ParentSummary, t.ParentURL = is.Parent, is.ParentSummary, cfg.issueURL(is.Parent)
		}
	}
	for i := range s.prs {
		pr := &s.prs[i]
		if pr.Key == "" {
			key := pr.Repo + "#" + strconv.Itoa(pr.Number)
			byKey[key] = &Task{Key: key, NoTicket: true, PR: pr}
			continue
		}
		if t := get(pr.Key); preferPR(pr, t.PR) {
			t.PR = pr
		}
	}
	for i := range s.worktrees {
		wt := &s.worktrees[i]
		if wt.Key == "" {
			continue
		}
		if t := get(wt.Key); t.Worktree == nil {
			t.Worktree = wt
		}
	}
	for i := range s.sessions {
		sess := &s.sessions[i]
		if t := get(sess.Key); t.Session == nil {
			t.Session = sess
		}
	}
	for i := range s.plans {
		p := &s.plans[i]
		t, ok := byKey[p.Key]
		if ok && (t.Plan == nil || p.ModTime.After(t.Plan.ModTime)) {
			t.Plan = p
		}
	}

	doneCutoff := now.AddDate(0, 0, -cfg.DoneDays)
	tasks := make([]Task, 0, len(byKey))
	for _, t := range byKey {
		if t.Summary == "" && t.PR != nil {
			t.Summary = t.PR.Title
		}
		if t.PR != nil && t.PR.UpdatedAt.After(t.Updated) {
			t.Updated = t.PR.UpdatedAt
		}
		t.JiraMissing = s.missing[t.Key]
		t.Stage = stageFor(t)
		if t.Stage == StageDone && !t.doneSince(doneCutoff, s.resolved[t.Key]) {
			continue
		}
		tasks = append(tasks, *t)
	}
	slices.SortFunc(tasks, func(a, b Task) int {
		return cmp.Or(
			cmp.Compare(a.Stage, b.Stage),
			compareNewerFirst(a.Updated, b.Updated),
			cmp.Compare(a.Key, b.Key),
		)
	})
	return tasks
}

// Unknown (zero) times sort last.
func compareNewerFirst(a, b time.Time) int {
	switch {
	case a.IsZero() && b.IsZero():
		return 0
	case a.IsZero():
		return 1
	case b.IsZero():
		return -1
	}
	return b.Compare(a)
}

type EpicGroup struct {
	Task     Task
	Children []EpicChild
	Open     int
	Done     int
}

// EpicChild is read-only. OnBoard children also have a full row in a stage section, which holds their actions.
type EpicChild struct {
	Key        string
	Summary    string
	JiraURL    string
	Status     string
	Done       bool
	Assignee   string
	OnBoard    bool
	StageLabel string
	Tone       string
}

// buildEpics expects tasks in board order. Children come from both the tasks' parents and the
// children search, so either one failing still leaves the other's.
func buildEpics(cfg *Config, tasks []Task, children map[string][]JiraIssue) []EpicGroup {
	var groups []EpicGroup
	for _, epic := range tasks {
		if !epic.Epic || epic.JiraDone {
			continue
		}
		listed := map[string]bool{}
		for _, is := range children[epic.Key] {
			listed[is.Key] = true
		}
		var onBoard, open, done []EpicChild
		seen := map[string]bool{}
		for _, t := range tasks {
			if t.Key == epic.Key || (t.Parent != epic.Key && !listed[t.Key]) {
				continue
			}
			seen[t.Key] = true
			c := EpicChild{Key: t.Key, Summary: t.Summary, JiraURL: t.JiraURL, Status: t.JiraStatus, Done: t.JiraDone,
				OnBoard: true, StageLabel: t.StageLabel(), Tone: t.Tone()}
			if c.Done {
				done = append(done, c)
			} else {
				onBoard = append(onBoard, c)
			}
		}
		for _, is := range children[epic.Key] {
			if seen[is.Key] {
				continue
			}
			seen[is.Key] = true
			c := EpicChild{Key: is.Key, Summary: is.Summary, JiraURL: cfg.issueURL(is.Key), Status: is.Status, Done: is.Done, Assignee: is.Assignee}
			if c.Done {
				done = append(done, c)
			} else {
				open = append(open, c)
			}
		}
		g := EpicGroup{Task: epic, Children: slices.Concat(onBoard, open, done), Open: len(onBoard) + len(open), Done: len(done)}
		groups = append(groups, g)
	}
	return groups
}
