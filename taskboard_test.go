package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{
		PollInterval: Duration{time.Minute},
		ProjectKeys:  []string{"MM"},
		Jira:         JiraConfig{BaseURL: "https://example.atlassian.net/", Email: "a@b.c"},
		Repos:        []RepoConfig{{Name: "mattermost/mattermost"}},
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestFindKey(t *testing.T) {
	cfg := testConfig(t)
	cases := map[string]string{
		"MM-1234-fix-sync":         "MM-1234",
		"feature/mm-42_shared":     "MM-42",
		"[MM-7] Fix thing":         "MM-7",
		"UTF-8 handling":           "",
		"master":                   "",
		"XMM-12 is not our prefix": "",
	}
	for in, want := range cases {
		if got := cfg.findKey(in); got != want {
			t.Errorf("findKey(%q) = %q, want %q", in, got, want)
		}
	}
	if got := cfg.issueURL("MM-1"); got != "https://example.atlassian.net/browse/MM-1" {
		t.Errorf("issueURL = %q", got)
	}
}

func TestRollupCI(t *testing.T) {
	cases := []struct {
		name   string
		checks []ghCheck
		want   CIState
	}{
		{"none", nil, CINone},
		{"passing", []ghCheck{{TypeName: "CheckRun", Status: "COMPLETED", Conclusion: "SUCCESS"}, {TypeName: "StatusContext", State: "SUCCESS"}}, CIPassing},
		{"skipped counts as passing", []ghCheck{{TypeName: "CheckRun", Status: "COMPLETED", Conclusion: "SKIPPED"}}, CIPassing},
		{"pending run", []ghCheck{{TypeName: "CheckRun", Status: "IN_PROGRESS"}}, CIPending},
		{"pending status", []ghCheck{{TypeName: "StatusContext", State: "PENDING"}}, CIPending},
		{"failure beats pending", []ghCheck{{TypeName: "CheckRun", Status: "IN_PROGRESS"}, {TypeName: "CheckRun", Status: "COMPLETED", Conclusion: "FAILURE"}}, CIFailing},
		{"status error", []ghCheck{{TypeName: "StatusContext", State: "ERROR"}}, CIFailing},
	}
	for _, c := range cases {
		if got := rollupCI(c.checks); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestStageFor(t *testing.T) {
	cases := []struct {
		name string
		task Task
		want Stage
	}{
		{"ticket only", Task{}, StageTicket},
		{"draft plan", Task{Plan: &Plan{}}, StagePlanning},
		{"draft plan with session", Task{Plan: &Plan{}, Session: &Session{}}, StagePlanning},
		{"approved plan with session", Task{Plan: &Plan{Approved: true}, Session: &Session{}}, StageCoding},
		{"worktree only", Task{Worktree: &Worktree{}}, StageCoding},
		{"draft PR", Task{PR: &PR{State: "OPEN", Draft: true, CI: CIFailing}}, StageCoding},
		{"open PR", Task{PR: &PR{State: "OPEN", Review: "REVIEW_REQUIRED", CI: CIPassing}}, StageInReview},
		{"ci failing", Task{PR: &PR{State: "OPEN", Review: "APPROVED", CI: CIFailing}}, StageCIFailing},
		{"changes requested", Task{PR: &PR{State: "OPEN", Review: "CHANGES_REQUESTED", CI: CIPassing}}, StageChangesRequested},
		{"approved, checks running", Task{PR: &PR{State: "OPEN", Review: "APPROVED", CI: CIPending}}, StageInReview},
		{"ready", Task{PR: &PR{State: "OPEN", Review: "APPROVED", CI: CIPassing}}, StageReadyToMerge},
		{"merged, jira open", Task{PR: &PR{State: "MERGED"}}, StageMergedJiraPending},
		{"merged, jira done", Task{PR: &PR{State: "MERGED"}, JiraDone: true}, StageDone},
		{"open PR beats done ticket", Task{PR: &PR{State: "OPEN"}, JiraDone: true}, StageInReview},
	}
	for _, c := range cases {
		if got := stageFor(&c.task); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, stageInfo[got].label, stageInfo[c.want].label)
		}
	}
}

func TestBuildTasks(t *testing.T) {
	cfg := testConfig(t)
	now := time.Now()
	tasks := buildTasks(cfg, sources{
		issues: map[string]JiraIssue{
			"MM-1": {Key: "MM-1", Summary: "Ticket one", Status: "In Progress"},
			"MM-2": {Key: "MM-2", Summary: "Ticket two", Status: "To Do"},
		},
		prs: []PR{
			{Key: "MM-1", Number: 10, State: "MERGED", UpdatedAt: now},
			{Key: "MM-1", Number: 11, State: "OPEN", CI: CIFailing, UpdatedAt: now.Add(-time.Hour)},
			{Key: "MM-3", Number: 12, State: "OPEN", Title: "PR title fallback"},
			{Number: 13, State: "OPEN"},
		},
		plans: []Plan{
			{Key: "MM-2", ModTime: now.Add(-time.Hour), Approved: true},
			{Key: "MM-2", ModTime: now},
			{Key: "MM-99", ModTime: now},
		},
		sessions: []Session{{Key: "MM-2", Target: "work:1"}},
	})

	if len(tasks) != 3 {
		t.Fatalf("got %d tasks, want 3: %+v", len(tasks), tasks)
	}
	want := []struct {
		key   string
		stage Stage
	}{{"MM-1", StageCIFailing}, {"MM-2", StagePlanning}, {"MM-3", StageInReview}}
	for i, w := range want {
		if tasks[i].Key != w.key || tasks[i].Stage != w.stage {
			t.Errorf("tasks[%d] = %s %s, want %s %s", i, tasks[i].Key, tasks[i].StageLabel(), w.key, stageInfo[w.stage].label)
		}
	}
	if tasks[0].PR.Number != 11 {
		t.Errorf("MM-1 PR = %d, want open PR 11", tasks[0].PR.Number)
	}
	if tasks[1].Plan.Approved {
		t.Error("MM-2 should use the newest plan file")
	}
	if tasks[2].Summary != "PR title fallback" {
		t.Errorf("MM-3 summary = %q", tasks[2].Summary)
	}
}

func TestParseWorktrees(t *testing.T) {
	out := []byte("worktree /src/mm\nHEAD abc\nbranch refs/heads/master\n\nworktree /src/mm-wt/MM-5\nHEAD def\nbranch refs/heads/MM-5-fix\n\nworktree /src/detached\nHEAD 123\ndetached\n")
	wts := parseWorktrees(out)
	if len(wts) != 3 {
		t.Fatalf("got %d worktrees", len(wts))
	}
	if wts[1].Path != "/src/mm-wt/MM-5" || wts[1].Branch != "MM-5-fix" {
		t.Errorf("worktree 1 = %+v", wts[1])
	}
	if wts[2].Branch != "" {
		t.Errorf("detached worktree branch = %q", wts[2].Branch)
	}
}

func TestPlanApproved(t *testing.T) {
	cases := map[string]bool{
		"---\nstatus: approved\n---\n# Plan":       true,
		"---\ntitle: x\nstatus: \"Approved\"\n---": true,
		"---\nstatus: draft\n---\n":                false,
		"# Plan\nstatus: approved\n":               false,
		"---\ntitle: x\n---\nstatus: approved\n":   false,
		"":                                         false,
	}
	for in, want := range cases {
		if got := planApproved([]byte(in)); got != want {
			t.Errorf("planApproved(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestAttachCommand(t *testing.T) {
	cases := []struct{ host, target, want string }{
		{"", "work:1", "tmux attach -t work:1"},
		{"ubuntu", "work:1", "ssh -t ubuntu 'tmux attach -t work:1'"},
		{"", "my work:1", "tmux attach -t 'my work:1'"},
		{"ubuntu", "it's:2", `ssh -t ubuntu 'tmux attach -t '\''it'\''\'\'''\''s:2'\'''`},
	}
	for _, c := range cases {
		if got := attachCommand(c.host, c.target); got != c.want {
			t.Errorf("attachCommand(%q, %q) = %s, want %s", c.host, c.target, got, c.want)
		}
	}
}

func TestCheckHost(t *testing.T) {
	cfg := testConfig(t)
	cfg.AllowedHosts = []string{"ubuntu.tail1234.ts.net"}
	h := (&server{cfg: cfg}).checkHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cases := map[string]int{
		"localhost:8484":             http.StatusOK,
		"127.0.0.1:8484":             http.StatusOK,
		"[::1]:8484":                 http.StatusOK,
		"192.168.1.20:8484":          http.StatusOK,
		"ubuntu.tail1234.ts.net":     http.StatusOK,
		"evil.example.com:8484":      http.StatusForbidden,
		"localhost.evil.example.com": http.StatusForbidden,
	}
	for host, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("host %s: got %d, want %d", host, rec.Code, want)
		}
	}
}
