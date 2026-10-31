package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{
		PollInterval: Duration{time.Minute},
		DoneDays:     30,
		ProjectKeys:  []string{"MM"},
		Jira:         JiraConfig{BaseURL: "https://example.atlassian.net/"},
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
		{"merged, jira not found", Task{PR: &PR{State: "MERGED"}, JiraMissing: true}, StageDone},
		{"merged, no ticket", Task{PR: &PR{State: "MERGED"}, NoTicket: true}, StageDone},
		{"merged, leftover worktree", Task{PR: &PR{State: "MERGED"}, JiraDone: true, Worktree: &Worktree{}}, StageCleanup},
		{"done ticket, open session", Task{JiraDone: true, Session: &Session{}}, StageCleanup},
		{"merged, jira open, leftover worktree", Task{PR: &PR{State: "MERGED"}, Worktree: &Worktree{}}, StageMergedJiraPending},
		{"open PR, jira not found", Task{PR: &PR{State: "OPEN"}, JiraMissing: true}, StageInReview},
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
			{Repo: "mattermost/logr", Number: 13, State: "OPEN", Title: "No ticket"},
		},
		plans: []Plan{
			{Key: "MM-2", ModTime: now.Add(-time.Hour), Approved: true},
			{Key: "MM-2", ModTime: now},
			{Key: "MM-99", ModTime: now},
		},
		sessions: []Session{{Key: "MM-2", Target: "work:1"}},
	}, now)

	if len(tasks) != 4 {
		t.Fatalf("got %d tasks, want 4: %+v", len(tasks), tasks)
	}
	want := []struct {
		key   string
		stage Stage
	}{{"MM-1", StageCIFailing}, {"MM-2", StagePlanning}, {"MM-3", StageInReview}, {"mattermost/logr#13", StageInReview}}
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
	if k := tasks[3]; !k.NoTicket || k.DisplayKey() != "logr#13" || k.Summary != "No ticket" {
		t.Errorf("keyless task = %+v", k)
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

func TestDecodeIssues(t *testing.T) {
	out := []byte(`[{"changelog":null,"fields":{"status":{"id":"6","name":"Closed","statusCategory":{"id":3,"key":"done","name":"Done"}},"summary":"Cross Guard fix"},"id":"154491","key":"MM-71003","names":null},
{"fields":{"status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}},"summary":"Other"},"key":"MM-2"}]`)
	issues, err := decodeIssues(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []JiraIssue{
		{Key: "MM-71003", Summary: "Cross Guard fix", Status: "Closed", Done: true},
		{Key: "MM-2", Summary: "Other", Status: "In Progress"},
	}
	if len(issues) != len(want) {
		t.Fatalf("got %d issues", len(issues))
	}
	for i := range want {
		if issues[i] != want[i] {
			t.Errorf("issues[%d] = %+v, want %+v", i, issues[i], want[i])
		}
	}
}

func TestMergePRLinks(t *testing.T) {
	pr := func(repo string, n, updated int) ghSearchPR {
		var r ghSearchPR
		r.Repository.NameWithOwner = repo
		r.Number = n
		r.URL = fmt.Sprintf("https://github.com/%s/pull/%d", repo, n)
		r.UpdatedAt = time.Unix(int64(updated), 0)
		return r
	}
	cfg := testConfig(t)
	cfg.Repos = []RepoConfig{{Name: "mattermost/mattermost"}}
	found := [3][]ghSearchPR{
		RoleRequested: {pr("mattermost/mattermost", 1, 1), pr("other/repo", 2, 3)},
		RoleAuthor:    {pr("mattermost/mattermost", 3, 9), pr("Other/Repo", 4, 2)},
		RoleReviewed:  {pr("mattermost/mattermost", 1, 1), pr("Mattermost/Mattermost", 5, 5), pr("other/repo", 6, 4), pr("mattermost/mattermost", 7, 2)},
	}
	reviews, others := splitPRLinks(cfg, mergePRLinks(found))

	type want struct {
		n    int
		role Role
	}
	check := func(name string, got []PRLink, want []want) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %d links, want %d", name, len(got), len(want))
		}
		for i, w := range want {
			if got[i].Number != w.n || got[i].Role != w.role {
				t.Errorf("%s[%d] = #%d role %d, want #%d role %d", name, i, got[i].Number, got[i].Role, w.n, w.role)
			}
		}
	}
	check("reviews", reviews, []want{{1, RoleRequested}, {5, RoleReviewed}, {7, RoleReviewed}})
	check("others", others, []want{{2, RoleRequested}, {4, RoleAuthor}, {6, RoleReviewed}})
}

func TestTaskOrderNewestFirst(t *testing.T) {
	now := time.Now()
	ago := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	tasks := buildTasks(testConfig(t), sources{
		issues: map[string]JiraIssue{
			"MM-1": {Key: "MM-1", Updated: ago(1)},
			"MM-2": {Key: "MM-2", Updated: ago(48)},
			"MM-3": {Key: "MM-3"},
			"MM-4": {Key: "MM-4", Updated: ago(100)},
			"MM-5": {Key: "MM-5", Updated: ago(2)},
		},
		prs: []PR{
			{Key: "MM-4", State: "OPEN", UpdatedAt: ago(3)},
			{Key: "MM-5", State: "OPEN", UpdatedAt: ago(200)},
		},
	}, now)
	var got []string
	for _, task := range tasks {
		got = append(got, task.Key)
	}
	want := []string{"MM-5", "MM-4", "MM-1", "MM-2", "MM-3"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if !tasks[1].Updated.Equal(ago(3)) {
		t.Errorf("MM-4 updated = %v, want the newer PR time", tasks[1].Updated)
	}
}

func TestAgeTone(t *testing.T) {
	now := time.Now()
	tests := map[time.Duration]string{
		time.Hour:          "fresh",
		47 * time.Hour:     "fresh",
		49 * time.Hour:     "stale",
		7 * 24 * time.Hour: "stale",
		8 * 24 * time.Hour: "old",
	}
	for age, want := range tests {
		if got := ageTone(now.Add(-age), now); got != want {
			t.Errorf("ageTone(%v) = %s, want %s", age, got, want)
		}
	}
}

func TestDateLabel(t *testing.T) {
	now := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 10, 31, 9, 5, 0, 0, time.UTC), "today 09:05"},
		{time.Date(2026, 10, 30, 23, 0, 0, 0, time.UTC), "Oct 30"},
		{time.Date(2024, 8, 6, 12, 0, 0, 0, time.UTC), "Aug 6, 2024"},
	}
	for _, tt := range tests {
		if got := dateLabel(tt.t, now); got != tt.want {
			t.Errorf("dateLabel(%v) = %q, want %q", tt.t, got, tt.want)
		}
	}
}

func TestJiraUpdatedParse(t *testing.T) {
	var raw jiraIssueJSON
	if err := json.Unmarshal([]byte(`{"key":"MM-1","fields":{"updated":"2026-10-27T06:51:35.697-0700"}}`), &raw); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 27, 13, 51, 35, 697e6, time.UTC)
	if got := raw.issue().Updated; !got.Equal(want) {
		t.Errorf("updated = %v, want %v", got, want)
	}
}

func TestDoneWindow(t *testing.T) {
	now := time.Now()
	daysAgo := func(d int) time.Time { return now.AddDate(0, 0, -d) }
	tasks := buildTasks(testConfig(t), sources{
		issues: map[string]JiraIssue{
			"MM-1": {Key: "MM-1", Done: true, Updated: daysAgo(1)},
			"MM-2": {Key: "MM-2", Done: true, Updated: daysAgo(1)},
			"MM-3": {Key: "MM-3", Done: true, Updated: daysAgo(90)},
			"MM-4": {Key: "MM-4", Done: true, Updated: daysAgo(5)},
			"MM-5": {Key: "MM-5", Done: true, Updated: daysAgo(90)},
			"MM-6": {Key: "MM-6", Done: true, Updated: daysAgo(3)},
		},
		resolved: map[string]bool{"MM-6": true},
		prs: []PR{
			{Key: "MM-1", State: "MERGED", MergedAt: daysAgo(10), UpdatedAt: daysAgo(1)},
			{Key: "MM-2", State: "MERGED", MergedAt: daysAgo(40), UpdatedAt: daysAgo(1)},
			{Key: "MM-3", State: "MERGED", MergedAt: daysAgo(400)},
			{Key: "MM-6", State: "MERGED", MergedAt: daysAgo(60)},
		},
		worktrees: []Worktree{{Key: "MM-3", Path: "/src/mm-MM-3"}},
	}, now)
	got := map[string]Stage{}
	for _, task := range tasks {
		got[task.Key] = task.Stage
	}
	want := map[string]Stage{"MM-1": StageDone, "MM-3": StageCleanup, "MM-4": StageDone, "MM-6": StageDone}
	if !maps.Equal(got, want) {
		t.Errorf("stages = %v, want %v", got, want)
	}
}
