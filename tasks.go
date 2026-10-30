package main

import (
	"sort"
)

type Stage int

// Declared in display priority order; sorting relies on it.
const (
	StageCIFailing Stage = iota
	StageChangesRequested
	StageReadyToMerge
	StagePlanning
	StageMergedJiraPending
	StageInReview
	StageCoding
	StageTicket
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
	StageReadyToMerge:      {"Ready to merge", "ok", groupNeedsYou, 4},
	StagePlanning:          {"Plan needs review", "needs", groupNeedsYou, 1},
	StageMergedJiraPending: {"Merged, update Jira", "needs", groupNeedsYou, 5},
	StageInReview:          {"In review", "active", groupWaiting, 3},
	StageCoding:            {"Coding", "active", groupInProgress, 2},
	StageTicket:            {"Not started", "idle", groupNotStarted, 0},
	StageDone:              {"Done", "ok", groupDone, 6},
}

const (
	groupNeedsYou   = "Needs you"
	groupWaiting    = "Waiting on others"
	groupInProgress = "In progress"
	groupNotStarted = "Not started"
	groupDone       = "Done"
)

var groupOrder = []string{groupNeedsYou, groupWaiting, groupInProgress, groupNotStarted, groupDone}

var stations = []string{"Ticket", "Plan", "Code", "Review", "Ready", "Merged", "Done"}

type Task struct {
	Key        string
	Summary    string
	JiraURL    string
	JiraStatus string
	JiraDone   bool
	Stage      Stage
	PR         *PR
	Plan       *Plan
	Session    *Session
	Worktree   *Worktree
}

func (t Task) StageLabel() string { return stageInfo[t.Stage].label }
func (t Task) Tone() string       { return stageInfo[t.Stage].tone }
func (t Task) Group() string      { return stageInfo[t.Stage].group }

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
	if pr := t.PR; pr != nil {
		switch pr.State {
		case "OPEN":
			switch {
			case pr.Draft:
				return StageCoding
			case pr.CI == CIFailing:
				return StageCIFailing
			case pr.Review == "CHANGES_REQUESTED":
				return StageChangesRequested
			case pr.Review == "APPROVED" && pr.CI != CIPending:
				return StageReadyToMerge
			default:
				return StageInReview
			}
		case "MERGED":
			if t.JiraDone {
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
	prs       []PR
	plans     []Plan
	worktrees []Worktree
	sessions  []Session
}

// Plans only attach to tasks found elsewhere, so old plan files for long-closed tickets stay off the board.
func buildTasks(cfg *Config, s sources) []Task {
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
	}
	for i := range s.prs {
		pr := &s.prs[i]
		if pr.Key == "" {
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

	tasks := make([]Task, 0, len(byKey))
	for _, t := range byKey {
		if t.Summary == "" && t.PR != nil {
			t.Summary = t.PR.Title
		}
		t.Stage = stageFor(t)
		tasks = append(tasks, *t)
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Stage != tasks[j].Stage {
			return tasks[i].Stage < tasks[j].Stage
		}
		return tasks[i].Key < tasks[j].Key
	})
	return tasks
}
