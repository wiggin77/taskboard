package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"software.sslmate.com/src/go-pkcs12"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{
		PollInterval:   Duration{time.Minute},
		RefreshTimeout: Duration{5 * time.Minute},
		MergedPRLimit:  20,
		DoneDays:       30,
		ProjectKeys:    []string{"MM"},
		Jira:           JiraConfig{BaseURL: "https://example.atlassian.net/"},
		Repos:          []RepoConfig{{Name: "mattermost/mattermost"}},
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
		{"your review requested", Task{PR: &PR{State: "OPEN", Review: "APPROVED", CI: CIPassing, ReviewRequested: true}}, StageReviewRequested},
		{"your review requested, ci failing", Task{PR: &PR{State: "OPEN", CI: CIFailing, ReviewRequested: true}}, StageCIFailing},
		{"assigned, changes requested", Task{PR: &PR{State: "OPEN", Review: "CHANGES_REQUESTED", CI: CIPassing, Assigned: true}}, StageAwaitingChanges},
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

func TestAssignedPRs(t *testing.T) {
	now := time.Now()
	tasks := buildTasks(testConfig(t), sources{
		issues: map[string]JiraIssue{
			"MM-1": {Key: "MM-1", Summary: "Part 2 delegated", Status: "In Progress"},
			"MM-2": {Key: "MM-2", Summary: "Delegated", Status: "In Progress"},
			"MM-3": {Key: "MM-3", Summary: "Both open", Status: "In Progress"},
		},
		prs: []PR{
			{Key: "MM-1", Number: 10, State: "MERGED", UpdatedAt: now},
			{Key: "MM-1", Number: 11, State: "OPEN", CI: CIFailing, UpdatedAt: now.Add(-time.Hour), Assigned: true},
			{Key: "MM-2", Number: 12, State: "OPEN", Review: "APPROVED", CI: CIPassing, Assigned: true},
			{Key: "MM-3", Number: 13, State: "OPEN", UpdatedAt: now.Add(-time.Hour)},
			{Key: "MM-3", Number: 14, State: "OPEN", UpdatedAt: now, Assigned: true},
		},
	}, now)
	byKey := map[string]Task{}
	for _, task := range tasks {
		byKey[task.Key] = task
	}
	if task := byKey["MM-1"]; task.PR == nil || task.PR.Number != 11 || task.Stage != StageCIFailing {
		t.Errorf("MM-1 = PR %+v stage %s, want open assigned PR 11 with CI failing over merged authored PR 10", task.PR, task.StageLabel())
	}
	if task := byKey["MM-2"]; task.PR == nil || task.PR.Number != 12 || task.Stage != StageReadyToMerge {
		t.Errorf("MM-2 = PR %+v stage %s, want assigned PR 12 ready to merge", task.PR, task.StageLabel())
	}
	if pr := byKey["MM-3"].PR; pr == nil || pr.Number != 13 {
		t.Errorf("MM-3 PR = %+v, want authored PR 13 over a newer assigned one in the same state", pr)
	}
}

func TestAssignedSearch(t *testing.T) {
	cfg := testConfig(t)
	if got, want := cfg.assignedSearch(), "assignee:@me -author:@me"; got != want {
		t.Errorf("assignedSearch = %q, want %q", got, want)
	}
	cfg.GitHub.IgnoreAssignedAuthors = []string{"mattermost-build", "app/cursor"}
	if got, want := cfg.assignedSearch(), "assignee:@me -author:@me -author:mattermost-build -author:app/cursor"; got != want {
		t.Errorf("assignedSearch = %q, want %q", got, want)
	}
}

func TestReviewRowsForTaskPRs(t *testing.T) {
	links := []PRLink{
		{Repo: "Mattermost/Mattermost", Number: 12, Role: RoleRequested},
		{Repo: "mattermost/mattermost", Number: 13, Role: RoleRequested},
		{Repo: "mattermost/logr", Number: 12, Role: RoleReviewed},
		{Repo: "mattermost/mattermost", Number: 14, Role: RoleReviewed},
		{Repo: "mattermost/mattermost", Number: 15, Role: RoleRequested, Review: &Review{Worktree: &Worktree{}}},
	}
	prs := []PR{
		{Repo: "mattermost/mattermost", Number: 12},
		{Repo: "mattermost/mattermost", Number: 13},
		{Repo: "mattermost/mattermost", Number: 14, ReviewRequested: true},
		{Repo: "mattermost/mattermost", Number: 15},
	}
	markReviewRequests(links, prs)
	for i, want := range []bool{true, true, false, true} {
		if prs[i].ReviewRequested != want {
			t.Errorf("PR %d ReviewRequested = %v, want %v", prs[i].Number, prs[i].ReviewRequested, want)
		}
	}

	tasks := []Task{{PR: &prs[0]}, {PR: &prs[2]}, {PR: &prs[3]}}
	var got []int
	for _, l := range withoutTaskPRs(links, tasks) {
		got = append(got, l.Number)
	}
	if want := []int{13, 12, 15}; !slices.Equal(got, want) {
		t.Errorf("rows kept %v, want %v: drop rows for PRs a task shows, keep other PRs' rows and rows with a review session", got, want)
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
	if !wts[0].Main || wts[1].Main || wts[2].Main {
		t.Error("only the first worktree is the main checkout")
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

func TestApprovePlan(t *testing.T) {
	cases := map[string]string{
		"# Plan\n":                        "---\nstatus: approved\n---\n# Plan\n",
		"":                                "---\nstatus: approved\n---\n",
		"---\nstatus: draft\n---\n# Plan": "---\nstatus: approved\n---\n# Plan",
		"---\ntitle: x\nstatus: \"Draft\"\n---\n": "---\ntitle: x\nstatus: approved\n---\n",
		"---\ntitle: x\n---\nstatus: draft\n":     "---\nstatus: approved\ntitle: x\n---\nstatus: draft\n",
		"---\r\nstatus: draft\r\n---\r\nbody":     "---\r\nstatus: approved\r\n---\r\nbody",
		"---\nstatus: draft\n":                    "---\nstatus: approved\n---\n---\nstatus: draft\n",
	}
	for in, want := range cases {
		got := approvePlan([]byte(in))
		if string(got) != want {
			t.Errorf("approvePlan(%q) = %q, want %q", in, got, want)
		}
		if !planApproved(got) {
			t.Errorf("approvePlan(%q) is not approved", in)
		}
	}
}

func TestApproveEndpoint(t *testing.T) {
	plan := filepath.Join(t.TempDir(), "MM-1.md")
	draft := []byte("---\nstatus: draft\n---\n# Plan\n")
	if err := os.WriteFile(plan, draft, 0o644); err != nil {
		t.Fatal(err)
	}
	board := NewBoard(testConfig(t))
	board.snap = Snapshot{
		Tasks:   []Task{{Key: "MM-1", Stage: StagePlanning, Plan: &Plan{Key: "MM-1", Path: plan}}, {Key: "MM-2", Stage: StageTicket}},
		Updated: time.Now(),
	}
	h := newServer(board, "")
	do := func(path, remote, hash string) int {
		req := httptest.NewRequest("POST", path, strings.NewReader(url.Values{"hash": {hash}}.Encode()))
		req.Host = "localhost"
		req.RemoteAddr = remote
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Taskboard", "1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	const local = "127.0.0.1:1234"
	hash := planHash(draft)
	page := func(remote string) string {
		req := httptest.NewRequest("GET", "/plan/MM-1", nil)
		req.Host = "localhost"
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("plan page: %d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if p := page(local); !strings.Contains(p, `id="approve"`) || !strings.Contains(p, hash) {
		t.Error("local plan page should offer Approve with the plan's hash")
	}
	if strings.Contains(page("192.0.2.1:1234"), `id="approve"`) {
		t.Error("a viewer should not get the Approve button")
	}
	if code := do("/approve/MM-1", "192.0.2.1:1234", hash); code != http.StatusForbidden {
		t.Errorf("viewer: %d, want 403", code)
	}
	if code := do("/approve/MM-2", local, hash); code != http.StatusNotFound {
		t.Errorf("no plan: %d, want 404", code)
	}
	if code := do("/approve/MM-1", local, planHash([]byte("older"))); code != http.StatusConflict {
		t.Errorf("changed plan: %d, want 409", code)
	}
	if code := do("/approve/MM-1", local, hash); code != http.StatusNoContent {
		t.Fatalf("approve: %d, want 204", code)
	}
	if got, _ := os.ReadFile(plan); !planApproved(got) {
		t.Errorf("plan not approved: %q", got)
	}
}

func TestParseWindows(t *testing.T) {
	cfg := testConfig(t)
	out := []byte("tb-1a2b:0\tMM-1\t@0\t1\nwork:0\tMM-1\t@0\t\nwork:1\tnotes\t@1\t\nmine:1\tnotes\t@1\t\n")
	got := parseWindows(cfg, out)
	if len(got) != 2 || got[0].Target != "work:0" || got[0].Key != "MM-1" || got[1].Target != "work:1" || got[1].Key != "" {
		t.Errorf("windows = %+v; want each window once, from unmarked sessions", got)
	}
	if got := parseWindows(cfg, []byte("work:0\tnotes\t@0\t\nwork:1\tMM-2\t@1\t\n")); len(got) != 2 || got[1].Key != "MM-2" {
		t.Errorf("the last window must not be dropped: %+v", got)
	}
}

func TestStaleLinked(t *testing.T) {
	now := time.Unix(10_000, 0)
	out := []byte("work\t\t0\t100\ntb-old\t1\t0\t100\ntb-attached\t1\t1\t100\ntb-new\t1\t0\t9990\n")
	if got := staleLinked(out, now); !slices.Equal(got, []string{"tb-old"}) {
		t.Errorf("stale = %v, want only the old unattached linked session", got)
	}
}

func TestCheckHost(t *testing.T) {
	cfg := testConfig(t)
	cfg.AllowedHosts = []string{"ubuntu.tail1234.ts.net"}
	h := (&server{board: NewBoard(cfg)}).checkHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
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
	found[RoleRequested][0].Title = "MM-10 Fix the thing"
	found[RoleRequested][0].Body = "Ticket: https://example.atlassian.net/browse/MM-11"
	found[RoleReviewed][1].Body = "#### Ticket Link\nhttps://example.atlassian.net/browse/MM-12"
	reviews, others := splitPRLinks(cfg, mergePRLinks(cfg, found))

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
	for i, key := range []string{"MM-10", "MM-12", ""} {
		if reviews[i].Key != key {
			t.Errorf("reviews[%d].Key = %q, want %q", i, reviews[i].Key, key)
		}
	}
	if want := cfg.issueURL("MM-10"); reviews[0].JiraURL != want {
		t.Errorf("reviews[0].JiraURL = %q, want %q", reviews[0].JiraURL, want)
	}
	if reviews[2].JiraURL != "" {
		t.Errorf("reviews[2].JiraURL = %q, want none", reviews[2].JiraURL)
	}
	check("others", others, []want{{2, RoleRequested}, {4, RoleAuthor}, {6, RoleReviewed}})
}

func TestSplitPRLinksOtherOrgs(t *testing.T) {
	link := func(repo string, n int, role Role) PRLink {
		return PRLink{Repo: repo, Number: n, Role: role}
	}
	links := []PRLink{
		link("mattermost/mattermost", 1, RoleRequested),
		link("mattermost-community/x", 2, RoleRequested),
		link("acme/y", 3, RoleReviewed),
		link("other/repo", 4, RoleRequested),
		link("other/repo", 5, RoleAuthor),
		link("other/repo", 6, RoleReviewed),
		link("mattermost/mattermost", 7, RoleAuthor),
	}
	numbers := func(ls []PRLink) []int {
		var out []int
		for _, l := range ls {
			out = append(out, l.Number)
		}
		return out
	}
	cfg := testConfig(t)
	cfg.Repos = []RepoConfig{{Name: "mattermost/mattermost"}}

	reviews, others := splitPRLinks(cfg, links)
	if want := []int{1}; !slices.Equal(numbers(reviews), want) {
		t.Errorf("reviews = %v, want %v", numbers(reviews), want)
	}
	if want := []int{2, 3, 4, 5, 6}; !slices.Equal(numbers(others), want) {
		t.Errorf("empty github.organizations: others = %v, want %v", numbers(others), want)
	}

	cfg.GitHub.Organizations = []string{"mattermost-community", "Acme"}
	reviews, others = splitPRLinks(cfg, links)
	if want := []int{1}; !slices.Equal(numbers(reviews), want) {
		t.Errorf("reviews with github.organizations = %v, want %v (configured repos are never filtered)", numbers(reviews), want)
	}
	if want := []int{2, 3}; !slices.Equal(numbers(others), want) {
		t.Errorf("others with github.organizations = %v, want %v", numbers(others), want)
	}
}

func TestShowsOther(t *testing.T) {
	for _, c := range []struct {
		orgs []string
		repo string
		want bool
	}{
		{nil, "any/repo", true},
		{[]string{"mattermost"}, "mattermost/mattermost", true},
		{[]string{"mattermost"}, "Mattermost/mattermost", true},
		{[]string{"Mattermost"}, "mattermost/mattermost", true},
		{[]string{"mattermost"}, "mattermost-x/repo", false},
		{[]string{"mattermost"}, "x-mattermost/repo", false},
		{[]string{"mattermost"}, "other/mattermost", false},
		{[]string{"mattermost"}, "mattermost", true},
		{[]string{"a", "b"}, "b/repo", true},
	} {
		cfg := &Config{GitHub: GitHubConfig{Organizations: c.orgs}}
		if got := cfg.showsOther(c.repo); got != c.want {
			t.Errorf("showsOther(%v, %q) = %v, want %v", c.orgs, c.repo, got, c.want)
		}
	}
}

func TestIgnoreAssignedAuthorsConfig(t *testing.T) {
	base := `"project_keys":["MM"],"jira":{"base_url":"https://x.atlassian.net"},"repos":[{"name":"a/b"}]`
	cfg, err := parseConfig([]byte(`{` + base + `,"github":{"ignore_assigned_authors":[" mattermost-build ","app/cursor"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"mattermost-build", "app/cursor"}; !slices.Equal(cfg.GitHub.IgnoreAssignedAuthors, want) {
		t.Errorf("github.ignore_assigned_authors = %q, want %q", cfg.GitHub.IgnoreAssignedAuthors, want)
	}
	for _, bad := range []string{`[""]`, `["a b"]`, `["@me"]`, `["a/b"]`, `["app/"]`, `["x -author:y"]`} {
		_, err := parseConfig([]byte(`{` + base + `,"github":{"ignore_assigned_authors":` + bad + `}}`))
		if err == nil || !strings.Contains(err.Error(), "github.ignore_assigned_authors") {
			t.Errorf("github.ignore_assigned_authors %s: error = %v, want one naming github.ignore_assigned_authors", bad, err)
		}
	}
	path := filepath.Join(t.TempDir(), "taskboard.json")
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "ignore_assigned_authors") {
		t.Errorf("a github block with only ignore_assigned_authors was not saved:\n%s", raw)
	}
}

func TestOtherOrgsConfig(t *testing.T) {
	base := `"project_keys":["MM"],"jira":{"base_url":"https://x.atlassian.net"},"repos":[{"name":"a/b"}]`
	cfg, err := parseConfig([]byte(`{` + base + `,"github":{"organizations":["mattermost","  mattermost-community "]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"mattermost", "mattermost-community"}; !slices.Equal(cfg.GitHub.Organizations, want) {
		t.Errorf("github.organizations = %q, want %q", cfg.GitHub.Organizations, want)
	}
	for _, bad := range []string{`["a/b"]`, `["a b"]`, `[""]`, `["ok","  "]`, `["@mattermost"]`, `["github.com"]`, `["mattermost."]`, `["-x"]`, `["a\u00a0b"]`} {
		_, err := parseConfig([]byte(`{` + base + `,"github":{"organizations":` + bad + `}}`))
		if err == nil || !strings.Contains(err.Error(), "github.organizations") {
			t.Errorf("github.organizations %s: error = %v, want one naming github.organizations", bad, err)
		}
	}

	path := filepath.Join(t.TempDir(), "taskboard.json")
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(saved.GitHub.Organizations, cfg.GitHub.Organizations) {
		t.Errorf("round trip github.organizations = %q, want %q", saved.GitHub.Organizations, cfg.GitHub.Organizations)
	}
	for _, empty := range [][]string{nil, {}} {
		cfg.GitHub.Organizations = empty
		if err := SaveConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
		if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "github") {
			t.Errorf("an empty github block (%#v) should not be saved:\n%s", empty, raw)
		}
	}
}

func TestConfigMarshalGitHub(t *testing.T) {
	marshal := func(cfg *Config) map[string]json.RawMessage {
		t.Helper()
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	t.Run("nil organizations leave out github", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.GitHub = GitHubConfig{}
		if raw, ok := marshal(cfg)["github"]; ok {
			t.Errorf("github = %s, want the key left out", raw)
		}
	})
	t.Run("empty organizations leave out github", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.GitHub = GitHubConfig{Organizations: []string{}}
		if raw, ok := marshal(cfg)["github"]; ok {
			t.Errorf("github = %s, want the key left out", raw)
		}
	})
	t.Run("organizations are written", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.GitHub = GitHubConfig{Organizations: []string{"mattermost", "mattermost-community"}}
		raw, ok := marshal(cfg)["github"]
		if !ok {
			t.Fatal("github key missing")
		}
		if got, want := string(raw), `{"organizations":["mattermost","mattermost-community"]}`; got != want {
			t.Errorf("github = %s, want %s", got, want)
		}
	})
	t.Run("other fields are unchanged", func(t *testing.T) {
		cfg := testConfig(t)
		with := marshal(cfg)
		cfg.GitHub = GitHubConfig{Organizations: []string{"mattermost"}}
		without := marshal(cfg)
		delete(without, "github")
		if !reflect.DeepEqual(with, without) {
			t.Errorf("adding organizations changed other keys:\n%v\n%v", with, without)
		}
		for _, key := range []string{"listen", "jira", "repos"} {
			if _, ok := with[key]; !ok {
				t.Errorf("key %q missing", key)
			}
		}
	})
	t.Run("an encoder that keeps HTML characters is honored", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.Jira.JQL = "a & b < c"
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(cfg); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), "a & b < c") {
			t.Errorf("output escapes HTML characters: %s", buf.String())
		}
	})
	t.Run("a value config marshals like a pointer", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.GitHub = GitHubConfig{Organizations: []string{"mattermost"}}
		byPtr, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		byValue, err := json.Marshal(*cfg)
		if err != nil {
			t.Fatal(err)
		}
		if string(byPtr) != string(byValue) {
			t.Errorf("pointer and value output differ:\n%s\n%s", byPtr, byValue)
		}
	})
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
		{time.Date(2026, 10, 31, 9, 5, 0, 0, time.UTC), "today"},
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

func TestIdlePolling(t *testing.T) {
	b := NewBoard(testConfig(t))
	woke := func() bool {
		select {
		case <-b.wake:
			return true
		default:
			return false
		}
	}
	if _, ok := b.nextWait(); ok {
		t.Error("before any view: want no timer")
	}

	b.Viewed()
	if !woke() || !b.requested {
		t.Error("first view with no snapshot should request a refresh")
	}
	b.requested = false
	b.snap.Updated = time.Now()
	if got, ok := b.nextWait(); !ok || got > time.Minute || got < 50*time.Second {
		t.Errorf("after a view: wait %v %v, want about the poll interval", got, ok)
	}
	b.Viewed()
	if woke() {
		t.Error("a view while active should not wake the poll loop")
	}

	b.lastView = time.Now().Add(-idleAfter - time.Second)
	if _, ok := b.nextWait(); ok {
		t.Error("after idling: want no timer")
	}
	b.Viewed()
	if !woke() || b.requested {
		t.Error("a view after idling with recent data should wake the loop without requesting a refresh")
	}
	if got, ok := b.nextWait(); !ok || got > time.Minute {
		t.Errorf("back on screen: wait %v %v, want the rest of the poll interval", got, ok)
	}

	b.lastView = time.Now().Add(-idleAfter - time.Second)
	b.snap.Updated = time.Now().Add(-2 * time.Minute)
	b.Viewed()
	if !woke() || !b.requested {
		t.Error("a view after idling with stale data should request a refresh")
	}
	b.requested = false
	if got, ok := b.nextWait(); !ok || got != 0 {
		t.Errorf("stale data on screen: wait %v %v, want 0", got, ok)
	}
}

func TestWaitForRefresh(t *testing.T) {
	b := NewBoard(testConfig(t))
	b.snap.Updated = time.Now()
	b.lastView = time.Now()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan bool, 1)
	go func() { done <- b.waitForRefresh(ctx) }()

	b.mu.Lock()
	b.wakeLocked()
	b.mu.Unlock()
	select {
	case <-done:
		t.Fatal("a wake without a request should only re-arm the timer")
	case <-time.After(50 * time.Millisecond):
	}

	b.mu.Lock()
	b.requestLocked()
	b.mu.Unlock()
	select {
	case ok := <-done:
		if !ok {
			t.Error("a request should start a refresh")
		}
	case <-time.After(time.Second):
		t.Fatal("a request should end the wait")
	}

	go func() { done <- b.waitForRefresh(ctx) }()
	cancel()
	if <-done {
		t.Error("a cancelled wait should stop the loop")
	}
}

func TestManualRefresh(t *testing.T) {
	b := NewBoard(testConfig(t))
	b.Config().ManualRefresh = true
	b.Viewed()
	if len(b.wake) != 0 {
		t.Error("with manual_refresh a view should not wake the poll loop")
	}
	if _, ok := b.nextWait(); ok {
		t.Error("active with manual_refresh: want no timer")
	}
	if got := b.Config().refreshSeconds(); got != 0 {
		t.Errorf("manual_refresh: page reload every %ds, want none", got)
	}
	b.Refresh()
	if len(b.wake) != 1 {
		t.Error("Refresh should still wake the loop with manual_refresh")
	}
}

func TestPollConfig(t *testing.T) {
	base := `"project_keys":["MM"],"jira":{"base_url":"https://x.atlassian.net"},"repos":[{"name":"a/b"}]`
	cfg, err := parseConfig([]byte(`{` + base + `}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval.Duration != 5*time.Minute || cfg.ManualRefresh {
		t.Errorf("defaults: poll %v manual %v, want 5m and automatic", cfg.PollInterval, cfg.ManualRefresh)
	}
	for _, ok := range []string{`"poll_interval":"1m"`, `"poll_interval":"1h","manual_refresh":true`, `"idle_poll_interval":"30m"`} {
		if _, err := parseConfig([]byte(`{` + base + `,` + ok + `}`)); err != nil {
			t.Errorf("config %s: %v", ok, err)
		}
	}
	for _, bad := range []string{`"poll_interval":"0s"`, `"poll_interval":"30s"`, `"poll_interval":"-1m"`} {
		if _, err := parseConfig([]byte(`{` + base + `,` + bad + `}`)); err == nil {
			t.Errorf("config %s should be rejected", bad)
		}
	}
}

func TestRefreshRequests(t *testing.T) {
	b := NewBoard(testConfig(t))
	b.snap.Updated = time.Now()
	b.lastView = time.Now()
	if b.Refreshing() {
		t.Error("an idle board with a snapshot should not report refreshing")
	}

	b.Refresh()
	if !b.Refreshing() || len(b.wake) != 1 {
		t.Error("Refresh should wake the loop and report refreshing")
	}

	b.running, b.requested = true, false
	<-b.wake
	b.lastView = time.Time{}
	b.Viewed()
	if b.requested {
		t.Error("a view during a refresh should not queue another")
	}
	b.Refresh()
	if b.requested || len(b.wake) != 0 {
		t.Error("Refresh during a refresh should not queue another")
	}
}

func TestSnapshotVersion(t *testing.T) {
	snap := Snapshot{Tasks: []Task{{Key: "MM-1", PR: &PR{Number: 1}}}, Updated: time.Now()}
	v := snapshotVersion(snap)
	snap.Updated = snap.Updated.Add(time.Minute)
	if got := snapshotVersion(snap); got != v {
		t.Error("version should ignore Updated")
	}
	snap.Tasks[0].PR = &PR{Number: 1}
	if got := snapshotVersion(snap); got != v {
		t.Error("version should depend on PR contents, not the pointer")
	}
	snap.Tasks[0].PR.CI = CIFailing
	if got := snapshotVersion(snap); got == v {
		t.Error("version should change when a task changes")
	}
}

func TestIndexGzipAndVersion(t *testing.T) {
	cfg := testConfig(t)
	board := NewBoard(cfg)
	h := newServer(board, "")

	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "localhost"
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<title>taskboard</title>") {
		t.Error("gzipped body is not the board page")
	}
	<-board.wake

	board.lastView = time.Time{}
	req = httptest.NewRequest("GET", "/version", nil)
	req.Host = "localhost"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var v struct {
		Version    string `json:"version"`
		Updated    string `json:"updated"`
		Refreshing bool   `json:"refreshing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("version body %q: %v", rec.Body.String(), err)
	}
	if v.Updated != "" || !v.Refreshing {
		t.Errorf("before the first refresh: %+v, want no time and refreshing", v)
	}
	select {
	case <-board.wake:
	default:
		t.Error("a version check should count as a view")
	}
}

func TestRefreshEndpoint(t *testing.T) {
	cfg := testConfig(t)
	board := NewBoard(cfg)
	h := newServer(board, "")
	post := func(header bool) int {
		req := httptest.NewRequest("POST", "/refresh", nil)
		req.Host = "localhost"
		if header {
			req.Header.Set("X-Taskboard", "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(false); code != http.StatusForbidden || len(board.wake) != 0 {
		t.Errorf("without the header: %d, want 403 and no refresh", code)
	}
	if code := post(true); code != http.StatusNoContent || len(board.wake) != 1 {
		t.Errorf("with the header: %d, want 204 and a refresh", code)
	}
}

func TestDurationString(t *testing.T) {
	for in, want := range map[time.Duration]string{
		0: "0s", 90 * time.Second: "1m30s", 2 * time.Minute: "2m", time.Hour: "1h", 90 * time.Minute: "1h30m",
	} {
		if got := (Duration{in}).String(); got != want {
			t.Errorf("Duration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "taskboard.json")
	cfg := testConfig(t)
	cfg.Listen = "127.0.0.1:8484"
	cfg.AttachCommand = []string{"sh", "-c", "a && b"}
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), `"a && b"`) {
		t.Errorf("saved config should keep & readable:\n%s", raw)
	}
	board := NewBoard(cfg)
	h := newServer(board, path)
	post := func(remote string, header bool, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/settings", strings.NewReader(body))
		req.Host = "localhost"
		req.RemoteAddr = remote
		if header {
			req.Header.Set("X-Taskboard", "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	valid := `{"listen":"127.0.0.1:9000","poll_interval":"1h","project_keys":["MM","ABC"],
		"jira":{"base_url":"https://example.atlassian.net"},"repos":[{"name":"mattermost/logr"}]}`

	if rec := post("192.0.2.1:1234", true, valid); rec.Code != http.StatusForbidden {
		t.Errorf("non-loopback: %d, want 403", rec.Code)
	}
	if rec := post("127.0.0.1:1234", false, valid); rec.Code != http.StatusForbidden {
		t.Errorf("without the header: %d, want 403", rec.Code)
	}
	if rec := post("127.0.0.1:1234", true, `{"poll_interval":"1h"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid config: %d, want 400", rec.Code)
	}
	if board.Config() != cfg || len(board.wake) != 0 {
		t.Fatal("a rejected save should leave the config alone")
	}

	rec := post("127.0.0.1:1234", true, valid)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"restart":true`) {
		t.Fatalf("valid save: %d %s, want 200 asking for a restart", rec.Code, rec.Body)
	}
	got := board.Config()
	if got.PollInterval.Duration != time.Hour || got.findKey("abc-12") != "ABC-12" || got.Repos[0].plansDir() != "plans" {
		t.Errorf("live config not updated: %+v", got)
	}
	if len(board.wake) != 1 {
		t.Error("a save should request a refresh")
	}
	saved, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.PollInterval != got.PollInterval || saved.Listen != "127.0.0.1:9000" || saved.Repos[0].Name != "mattermost/logr" {
		t.Errorf("saved config = %+v", saved)
	}
}

func TestSettingsPage(t *testing.T) {
	cfg := testConfig(t)
	cfg.AttachCommand = []string{"tmux", "attach", "-t", "{target}"}
	cfg.GitHub.Organizations = []string{"mattermost", "acme"}
	h := newServer(NewBoard(cfg), "/tmp/taskboard.json")
	get := func(remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/settings", nil)
		req.Host = "localhost"
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("192.0.2.1:1234"); rec.Code != http.StatusForbidden {
		t.Errorf("non-loopback: %d, want 403", rec.Code)
	}
	rec := get("127.0.0.1:1234")
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback: %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`value="1m"`, ">tmux\nattach\n-t\n{target}</textarea>", `value="mattermost/mattermost"`, `value="MM"`, `id="orgs"`, `id="add-org"`, `<input data-field="name" placeholder="e.g. mattermost" value="acme"`, `name="auto_refresh" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing %q", want)
		}
	}
}

func TestHomePaths(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	for in, want := range map[string]string{
		"~":          "/home/me",
		"~/src/repo": "/home/me/src/repo",
		"~other/src": "~other/src",
		"/abs/path":  "/abs/path",
		"":           "",
	} {
		if got := expandHome(in); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"/home/me":          "~",
		"/home/me/src/repo": "~/src/repo",
		"/home/meow/src":    "/home/meow/src",
		"~/src":             "~/src",
		"":                  "",
	} {
		if got := shortenHome(in); got != want {
			t.Errorf("shortenHome(%q) = %q, want %q", in, got, want)
		}
	}
}

func sessionConfig(t *testing.T) *Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Repos = []RepoConfig{
		{Name: "mattermost/mattermost", Path: "/src/mattermost"},
		{Name: "mattermost/logr"},
		{Name: "mattermost/mmetl", Path: "/src/mmetl/"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestStartPlan(t *testing.T) {
	cfg := sessionConfig(t)
	openPR := &PR{Repo: "mattermost/mmetl", Number: 7, State: "OPEN", Branch: "fix-import"}
	cases := []struct {
		name     string
		task     Task
		branches map[string][]string
		want     StartPlan
	}{
		{"existing worktree resumes", Task{Key: "MM-1", Worktree: &Worktree{Repo: "mattermost/mattermost", Path: "/wt/x", Branch: "MM-1-x"}, PR: openPR},
			nil, StartPlan{Mode: StartResume, Repo: "mattermost/mattermost", Path: "/wt/x", Branch: "MM-1-x"}},
		{"local branch", Task{Key: "MM-1"}, map[string][]string{"mattermost/mattermost": {"master", "mm-1-old"}},
			StartPlan{Mode: StartBranch, Repo: "mattermost/mattermost", Path: "/src/mattermost-MM-1", Branch: "mm-1-old"}},
		{"PR repo's branch wins", Task{Key: "MM-1", PR: openPR}, map[string][]string{"mattermost/mattermost": {"MM-1-a"}, "mattermost/mmetl": {"MM-1-b"}},
			StartPlan{Mode: StartBranch, Repo: "mattermost/mmetl", Path: "/src/mmetl-MM-1", Branch: "MM-1-b"}},
		{"branch for another key", Task{Key: "MM-1"}, map[string][]string{"mattermost/mattermost": {"MM-12-other"}},
			StartPlan{Mode: StartNew, Repo: "mattermost/mattermost", Path: "/src/mattermost-MM-1", Branch: "MM-1", Repos: []string{"mattermost/mattermost", "mattermost/mmetl"}}},
		{"open PR", Task{Key: "MM-1", PR: openPR}, nil,
			StartPlan{Mode: StartPR, Repo: "mattermost/mmetl", Path: "/src/mmetl-MM-1", Branch: "fix-import", PR: 7}},
		{"PR in a repo without a checkout", Task{Key: "MM-1", Summary: "Fix it", PR: &PR{Repo: "mattermost/logr", State: "OPEN"}}, nil,
			StartPlan{Mode: StartNew, Repo: "mattermost/mattermost", Path: "/src/mattermost-MM-1", Branch: "MM-1-fix-it", Repos: []string{"mattermost/mattermost", "mattermost/mmetl"}}},
	}
	for _, c := range cases {
		got, err := startPlan(cfg, c.task, c.branches)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}

	cfg.WorktreeDir = "/wt"
	if got, _ := startPlan(cfg, Task{Key: "MM-2"}, nil); got.Path != "/wt/mattermost-MM-2" {
		t.Errorf("with worktree_dir: path = %s", got.Path)
	}
	cfg.Repos = []RepoConfig{{Name: "mattermost/logr"}}
	if _, err := startPlan(cfg, Task{Key: "MM-2"}, nil); err == nil {
		t.Error("no local checkout should be an error")
	}
}

func TestChooseNew(t *testing.T) {
	cfg := sessionConfig(t)
	p := StartPlan{Mode: StartNew, Repo: "mattermost/mattermost", Path: "/src/mattermost-MM-1"}
	if err := chooseNew(cfg, &p, "MM-1", "mattermost/mmetl", "mm-1-custom"); err != nil || p.Repo != "mattermost/mmetl" || p.Path != "/src/mmetl-MM-1" || p.Branch != "mm-1-custom" {
		t.Errorf("chooseNew = %v %+v", err, p)
	}
	for _, c := range []struct{ repo, branch string }{
		{"mattermost/logr", "MM-1-x"},
		{"other/repo", "MM-1-x"},
		{"", "MM-12-x"},
		{"", "fix-things"},
		{"", "-MM-1"},
	} {
		if err := chooseNew(cfg, &p, "MM-1", c.repo, c.branch); err == nil {
			t.Errorf("chooseNew(%q, %q) should fail", c.repo, c.branch)
		}
	}
}

func TestDefaultBranch(t *testing.T) {
	for _, c := range []struct{ summary, want string }{
		{"", "MM-1"},
		{"Fix the bug!", "MM-1-fix-the-bug"},
		{"  Ünïcode — and/or SPACES  ", "MM-1-n-code-and-or-spaces"},
		{"Make the plugin API return a useful error when the channel is archived", "MM-1-make-the-plugin-api-return-a-useful-error"},
		{strings.Repeat("x", 80), "MM-1-" + strings.Repeat("x", 45)},
	} {
		if got := defaultBranch("MM-1", c.summary); got != c.want || len(got) > maxBranchLen {
			t.Errorf("defaultBranch(%q) = %q, want %q", c.summary, got, c.want)
		}
	}
}

func TestRemovalCheck(t *testing.T) {
	status := []byte(" M server/app.go\n?? notes.txt\n")
	unpushed := []byte("abc123 WIP\n")
	merged := &PR{State: "MERGED", HeadSHA: "head1"}
	if r := removalCheck(nil, nil, "head1", nil); !r.Clean() {
		t.Errorf("clean worktree: %+v", r)
	}
	if r := removalCheck(status, nil, "head1", nil); r.Clean() || !slices.Equal(r.Uncommitted, []string{" M server/app.go", "?? notes.txt"}) {
		t.Errorf("dirty worktree: %+v", r)
	}
	if r := removalCheck(nil, unpushed, "head1", nil); r.Clean() || r.Unpushed[0] != "abc123 WIP" {
		t.Errorf("unpushed: %+v", r)
	}
	if r := removalCheck(nil, unpushed, "head1", merged); !r.Clean() {
		t.Errorf("HEAD is the merged PR's head, so nothing is unpushed: %+v", r)
	}
	if r := removalCheck(nil, unpushed, "head2", merged); r.Clean() {
		t.Error("commits after the merged head are unpushed")
	}
	if r := removalCheck(nil, unpushed, "head1", &PR{State: "OPEN", HeadSHA: "head1"}); r.Clean() {
		t.Error("only a merged PR vouches for deleted remote branches")
	}
	var many strings.Builder
	for i := range 25 {
		fmt.Fprintf(&many, "?? f%d\n", i)
	}
	if r := removalCheck([]byte(many.String()), nil, "", nil); len(r.Uncommitted) != maxListed+1 || r.Uncommitted[maxListed] != "and 5 more" {
		t.Errorf("long list = %d lines, last %q", len(r.Uncommitted), r.Uncommitted[len(r.Uncommitted)-1])
	}
}

func TestCleanupNeverRemovesMainCheckout(t *testing.T) {
	cfg := sessionConfig(t)
	task := Task{Key: "MM-1", Worktree: &Worktree{Repo: "mattermost/mattermost", Path: "/src/mattermost", Branch: "MM-1-x", Main: true}}
	if _, err := cleanup(t.Context(), cfg, task, true); err == nil || !strings.Contains(err.Error(), "main checkout") {
		t.Errorf("cleanup of the main checkout: %v", err)
	}
}

func TestEnsureLinks(t *testing.T) {
	root := t.TempDir()
	dir := root + "/worktrees"
	enterprise := root + "/src/enterprise"
	for _, d := range []string{dir, enterprise, root + "/src/other/enterprise"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensureLinks(dir, []string{enterprise}); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(dir + "/enterprise"); err != nil || target != enterprise {
		t.Fatalf("link = %q, %v", target, err)
	}
	if err := ensureLinks(dir, []string{enterprise + "/"}); err != nil {
		t.Errorf("an existing link to the same place should be accepted: %v", err)
	}
	if err := ensureLinks(root+"/src", []string{enterprise}); err != nil {
		t.Errorf("linking a checkout into its own folder should be a no-op: %v", err)
	}
	if err := ensureLinks(dir, []string{root + "/src/other/enterprise"}); err == nil {
		t.Error("a name taken by a different target should be an error")
	}
	if err := ensureLinks(dir, []string{root + "/missing"}); err == nil {
		t.Error("a missing target should be an error")
	}
	if err := os.Symlink(root+"/gone", dir+"/gone"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root+"/src/gone", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureLinks(dir, []string{root + "/src/gone"}); err == nil {
		t.Error("a broken link in the way should be an error")
	}
}

func TestSessionEndpointGuards(t *testing.T) {
	cfg := testConfig(t)
	board := NewBoard(cfg)
	board.snap.Tasks = []Task{
		{Key: "MM-1", Stage: StageTicket},
		{Key: "MM-2", Stage: StageCoding, Session: &Session{Key: "MM-2", Target: "work:1"}},
		{Key: "mattermost/logr#3", NoTicket: true, Stage: StageInReview},
	}
	h := newServer(board, "")
	post := func(path, remote string, header bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, nil)
		req.Host = "localhost"
		req.RemoteAddr = remote
		if header {
			req.Header.Set("X-Taskboard", "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	const local = "127.0.0.1:1234"
	cases := []struct {
		path, remote string
		header       bool
		want         int
	}{
		{"/start/MM-1", local, false, http.StatusForbidden},
		{"/start/MM-1", "192.0.2.1:1234", true, http.StatusForbidden},
		{"/stop/MM-2", local, false, http.StatusForbidden},
		{"/cleanup/MM-1", "192.0.2.1:1234", true, http.StatusForbidden},
		{"/start/MM-9", local, true, http.StatusNotFound},
		{"/start/mattermost%2Flogr%233", local, true, http.StatusNotFound},
		{"/stop/MM-1", local, true, http.StatusNotFound},
		{"/cleanup/MM-2", local, true, http.StatusConflict},
	}
	for _, c := range cases {
		if rec := post(c.path, c.remote, c.header); rec.Code != c.want {
			t.Errorf("POST %s from %s (header %v): %d %s, want %d", c.path, c.remote, c.header, rec.Code, strings.TrimSpace(rec.Body.String()), c.want)
		}
	}

	cfg.AgentCommand = "claude"
	board.snap.Tasks = append(board.snap.Tasks, Task{Key: "MM-4", Stage: StageDone})
	if rec := post("/start/MM-4", local, true); rec.Code != http.StatusConflict {
		t.Errorf("starting a done task: %d, want 409", rec.Code)
	}

	s := &server{board: board, busy: map[string]bool{"MM-1": true}}
	req := httptest.NewRequest("POST", "/start/MM-1", nil)
	req.RemoteAddr = local
	req.Header.Set("X-Taskboard", "1")
	req.SetPathValue("key", "MM-1")
	rec := httptest.NewRecorder()
	s.start(rec, req)
	if rec.Code != http.StatusLocked || !s.busy["MM-1"] {
		t.Errorf("action already running: %d, want 423 and the claim kept", rec.Code)
	}
}

func TestSessionConfig(t *testing.T) {
	base := `"project_keys":["MM"],"jira":{"base_url":"https://x.atlassian.net"}`
	cfg, err := parseConfig([]byte(`{` + base + `,"repos":[{"name":"a/b","base_branch":"upstream/main"}],"worktree_dir":"~/wt","worktree_links":["~/src/enterprise"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.tmuxSession() != "taskboard" {
		t.Errorf("tmux_session default = %q", cfg.tmuxSession())
	}
	for _, bad := range []string{
		`"repos":[{"name":"a/b","base_branch":"main"}]`,
		`"repos":[{"name":"a/b"}],"tmux_session":"my:work"`,
		`"repos":[{"name":"a/b"}],"worktree_dir":"wt"`,
		`"repos":[{"name":"a/b"}],"worktree_links":["enterprise"]`,
	} {
		if _, err := parseConfig([]byte(`{` + base + `,` + bad + `}`)); err == nil {
			t.Errorf("config %s should be rejected", bad)
		}
	}
}

func TestParseReviewName(t *testing.T) {
	mm := RepoConfig{Name: "mattermost/mattermost", Path: "/src/mattermost"}
	weave := RepoConfig{Name: "mattermost/mattermost-plugin-weave", Path: "/src/mattermost-plugin-weave/"}
	if got := reviewName(weave, 297); got != "mattermost-plugin-weave-pr297" {
		t.Errorf("reviewName = %s", got)
	}
	for _, c := range []struct {
		repo RepoConfig
		name string
		want int
	}{
		{mm, "mattermost-pr39290", 39290},
		{mm, "mattermost-plugin-weave-pr297", 0},
		{weave, "mattermost-plugin-weave-pr297", 297},
		{mm, "mattermost-pr", 0},
		{mm, "mattermost-pr12x", 0},
		{mm, "mattermost-pr0", 0},
		{mm, "mattermost-MM-12", 0},
	} {
		if n, ok := parseReviewName(c.repo, c.name); n != c.want || ok != (c.want > 0) {
			t.Errorf("parseReviewName(%s, %s) = %d %v, want %d", c.repo.Name, c.name, n, ok, c.want)
		}
	}
}

func TestAttachReviews(t *testing.T) {
	cfg := sessionConfig(t)
	links := []PRLink{
		{Repo: "mattermost/mattermost", Number: 10, Role: RoleRequested},
		{Repo: "mattermost/mattermost", Number: 11, Role: RoleReviewed},
		{Repo: "mattermost/logr", Number: 12, Role: RoleRequested},
	}
	wts := []Worktree{
		{Repo: "mattermost/mattermost", Path: "/src/mattermost", Main: true},
		{Repo: "mattermost/mattermost", Path: "/wt/mattermost-pr10"},
		{Repo: "mattermost/mattermost", Path: "/wt/mattermost-pr99"},
		{Repo: "mattermost/mattermost", Path: "/wt/mattermost-MM-5", Branch: "MM-5-x", Key: "MM-5"},
	}
	windows := []Session{
		{Name: "mattermost-pr10", Target: "work:1"},
		{Name: "mmetl-pr3", Target: "work:2"},
		{Name: "MM-5", Key: "MM-5", Target: "work:3"},
	}
	got, leftovers := attachReviews(cfg, links, wts, windows)
	if r := got[0].Review; got[0].ReviewName != "mattermost-pr10" || r == nil || r.Worktree.Path != "/wt/mattermost-pr10" || r.Session.Target != "work:1" {
		t.Errorf("PR 10 = %+v", got[0])
	}
	if got[1].ReviewName != "mattermost-pr11" || got[1].Review != nil {
		t.Errorf("PR 11 should be startable with nothing attached: %+v", got[1])
	}
	if got[2].ReviewName != "" {
		t.Errorf("a repo without a checkout cannot be reviewed locally: %+v", got[2])
	}
	if len(leftovers) != 2 {
		t.Fatalf("leftovers = %+v", leftovers)
	}
	for _, l := range leftovers {
		if l.Role != RoleLeftover || l.Review == nil || !strings.Contains(l.URL, "/pull/") {
			t.Errorf("leftover = %+v", l)
		}
	}
	if leftovers[0].ReviewName != "mattermost-pr99" || leftovers[1].ReviewName != "mmetl-pr3" {
		t.Errorf("leftover names = %s, %s", leftovers[0].ReviewName, leftovers[1].ReviewName)
	}
}

func TestReviewCommand(t *testing.T) {
	cfg := testConfig(t)
	cfg.AgentCommand, cfg.AgentResume = "claude {key}", ""
	if got := reviewCommand(cfg, false); got != "claude " {
		t.Errorf("start: %q", got)
	}
	if got := reviewCommand(cfg, true); got != "claude " {
		t.Errorf("resume falls back to the agent command: %q", got)
	}
	cfg.AgentResume = "claude --continue"
	if got := reviewCommand(cfg, true); got != "claude --continue" {
		t.Errorf("resume: %q", got)
	}
}

func TestControls(t *testing.T) {
	local := indexData{Native: true, CanAct: true}
	remote := indexData{CanAct: true}
	sess := &Session{Target: "work:1"}
	if c := taskControls(Task{Key: "MM-1", Stage: StageTicket}, local); c.Start != "Start session" || !c.Native || !c.Web || c.Stop || c.Clean != "" {
		t.Errorf("not started task, local browser: %+v", c)
	}
	if c := taskControls(Task{Key: "MM-1", Stage: StageTicket}, remote); c.Start != "Start session" || c.Native || !c.Web {
		t.Errorf("not started task, remote browser gets only the browser terminal: %+v", c)
	}
	if c := taskControls(Task{Key: "MM-1", Stage: StageCleanup, Session: sess}, local); c.Start != "" || !c.Session || !c.Stop || !c.Native || !c.Web || c.Clean == "" {
		t.Errorf("clean-up task with a session: %+v", c)
	}
	if c := taskControls(Task{Key: "MM-1", Stage: StageCoding, Session: sess}, indexData{}); c.Native || c.Web || c.Stop || c.Start != "" {
		t.Errorf("viewer gets no actions: %+v", c)
	}
	if c := prControls(PRLink{Number: 1}, local); c.ID != "" {
		t.Errorf("PR without a local checkout: %+v", c)
	}
	if c := prControls(PRLink{ReviewName: "mm-pr1", Role: RoleRequested}, remote); c.Start != "Start review" || c.Path != "/review" || !c.Web || c.Native || c.Clean != "" {
		t.Errorf("new review, remote browser: %+v", c)
	}
	if c := prControls(PRLink{ReviewName: "mm-pr1", Role: RoleReviewed, Review: &Review{Worktree: &Worktree{}}}, local); c.Start != "Resume review" || c.Clean == "" {
		t.Errorf("review with a worktree: %+v", c)
	}
	if c := prControls(PRLink{ReviewName: "mm-pr1", Role: RoleLeftover, Review: &Review{Worktree: &Worktree{}}}, local); c.Start != "" || c.Clean == "" {
		t.Errorf("leftover review: %+v", c)
	}
	if c := taskControls(Task{Key: "MM-1", Stage: StageTicket}, indexData{}); c.Group != "MM-1" {
		t.Errorf("task tab group: %+v", c)
	}
	if c := prControls(PRLink{Repo: "o/r", Number: 1, Key: "MM-2", ReviewName: "r-pr1", Role: RoleRequested}, remote); c.Group != "MM-2" {
		t.Errorf("review of a PR with a ticket joins the ticket's tab group: %+v", c)
	}
	if c := prControls(PRLink{Repo: "o/r", Number: 1, ReviewName: "r-pr1", Role: RoleRequested}, remote); c.Group != "o/r#1" {
		t.Errorf("review of a PR without a ticket: %+v", c)
	}
}

func TestReviewEndpointGuards(t *testing.T) {
	cfg := testConfig(t)
	cfg.AgentCommand = "claude"
	board := NewBoard(cfg)
	board.snap.Reviews = []PRLink{
		{Repo: "mattermost/mattermost", Number: 1, ReviewName: "mattermost-pr1", Role: RoleRequested},
		{Repo: "mattermost/mattermost", Number: 2, ReviewName: "mattermost-pr2", Role: RoleLeftover, Review: &Review{}},
	}
	h := newServer(board, "")
	post := func(path, remote string, header bool) int {
		req := httptest.NewRequest("POST", path, nil)
		req.Host = "localhost"
		req.RemoteAddr = remote
		if header {
			req.Header.Set("X-Taskboard", "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	const local = "127.0.0.1:1234"
	for _, c := range []struct {
		path, remote string
		header       bool
		want         int
	}{
		{"/review/start/mattermost-pr1", local, false, http.StatusForbidden},
		{"/review/start/mattermost-pr1", "192.0.2.1:1234", true, http.StatusForbidden},
		{"/review/cleanup/mattermost-pr1", "192.0.2.1:1234", true, http.StatusForbidden},
		{"/review/start/mattermost-pr9", local, true, http.StatusNotFound},
		{"/review/start/mattermost-pr2", local, true, http.StatusConflict},
		{"/review/stop/mattermost-pr1", local, true, http.StatusNotFound},
		{"/review/cleanup/mattermost-pr1", local, true, http.StatusNotFound},
		{"/review/attach/mattermost-pr1", local, true, http.StatusForbidden},
	} {
		if got := post(c.path, c.remote, c.header); got != c.want {
			t.Errorf("POST %s from %s (header %v): %d, want %d", c.path, c.remote, c.header, got, c.want)
		}
	}
}

type testPKI struct {
	dir    string
	ca     *x509.Certificate
	caKey  crypto.Signer
	client tls.Certificate
	serial string
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()
	now := time.Now()
	ca, caKey, err := newCA(now)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCertAndKey(dir, "ca", ca, caKey); err != nil {
		t.Fatal(err)
	}
	server, serverKey, err := issue(ca, caKey, serverTemplate([]string{"devbox", "100.64.0.5"}, now))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCertAndKey(dir, "server", server, serverKey); err != nil {
		t.Fatal(err)
	}
	cert, key, err := issue(ca, caKey, clientTemplate("laptop", now))
	if err != nil {
		t.Fatal(err)
	}
	return testPKI{dir: dir, ca: ca, caKey: caKey, client: tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}, serial: serialHex(cert.SerialNumber)}
}

func TestCertificates(t *testing.T) {
	pki := newTestPKI(t)
	server, err := tls.LoadX509KeyPair(pki.dir+"/server.crt", pki.dir+"/server.key")
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(server.Certificate[0])
	roots := x509.NewCertPool()
	roots.AddCert(pki.ca)
	for _, host := range []string{"devbox", "100.64.0.5", "localhost", "127.0.0.1"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host}); err != nil {
			t.Errorf("server certificate for %s: %v", host, err)
		}
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > serverValidity+2*time.Hour {
		t.Errorf("server certificate valid for %v, longer than Apple allows", leaf.NotAfter.Sub(leaf.NotBefore))
	}
	client, _ := x509.ParseCertificate(pki.client.Certificate[0])
	if _, err := client.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client certificate: %v", err)
	}
	p12, err := encodeP12(pki.client.PrivateKey.(*ecdsa.PrivateKey), client, pki.ca, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, cert, chain, err := pkcs12.DecodeChain(p12, "secret"); err != nil || !cert.Equal(client) || len(chain) != 1 || !chain[0].Equal(pki.ca) {
		t.Errorf("p12 round trip: %v", err)
	}
	if err := certsInit(pki.dir, []string{"devbox"}, time.Now()); err == nil {
		t.Error("certs init must not replace an existing CA")
	}
	for _, bad := range []string{"", "../x", ".hidden", "a/b"} {
		if err := certsClient(pki.dir, bad, time.Now()); err == nil {
			t.Errorf("client name %q should be rejected", bad)
		}
	}
}

func TestTLSListener(t *testing.T) {
	pki := newTestPKI(t)
	cfg := testConfig(t)
	board := NewBoard(cfg)
	tlsConfig, err := serverTLS(pki.dir, func() []string { return board.Config().RevokedCerts })
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(newServer(board, ""))
	srv.TLS = tlsConfig
	srv.StartTLS()
	defer srv.Close()

	roots := x509.NewCertPool()
	roots.AddCert(pki.ca)
	get := func(withCert bool) (int, error) {
		tc := &tls.Config{RootCAs: roots}
		if withCert {
			tc.Certificates = []tls.Certificate{pki.client}
		}
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}}
		res, err := client.Get(srv.URL + "/settings")
		if err != nil {
			return 0, err
		}
		res.Body.Close()
		return res.StatusCode, nil
	}
	if code, err := get(true); err != nil || code != http.StatusOK {
		t.Errorf("with a client certificate: %d %v, want 200 (remote access to settings)", code, err)
	}
	if _, err := get(false); err == nil {
		t.Error("without a client certificate the handshake should fail")
	}
	revoked := *cfg
	revoked.RevokedCerts = []string{pki.serial}
	board.SetConfig(&revoked)
	if _, err := get(true); err == nil {
		t.Error("a revoked certificate should be refused")
	}
}

func TestAccessOf(t *testing.T) {
	cfg := testConfig(t)
	s := &server{board: NewBoard(cfg)}
	cert := &x509.Certificate{SerialNumber: big.NewInt(0xabc)}
	req := func(remote string, state *tls.ConnectionState) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		r.TLS = state
		return r
	}
	verified := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
	cases := []struct {
		name string
		r    *http.Request
		want access
	}{
		{"plain loopback", req("127.0.0.1:5000", nil), local},
		{"plain LAN", req("192.0.2.1:5000", nil), viewer},
		{"TLS with a verified client certificate", req("192.0.2.1:5000", verified), remote},
		{"TLS on loopback is still remote", req("127.0.0.1:5000", verified), remote},
		{"TLS without a verified chain", req("192.0.2.1:5000", &tls.ConnectionState{}), viewer},
	}
	for _, c := range cases {
		if got := s.accessOf(c.r); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	revoked := *cfg
	revoked.RevokedCerts = []string{"abc"}
	s.board.SetConfig(&revoked)
	if got := s.accessOf(req("192.0.2.1:5000", verified)); got != viewer {
		t.Errorf("revoked certificate on a kept-alive connection: %d, want viewer", got)
	}
}

func TestTerminalEndpoints(t *testing.T) {
	cfg := testConfig(t)
	board := NewBoard(cfg)
	board.snap.Tasks = []Task{{Key: "MM-1", Summary: "Fix it", Stage: StageTicket}}
	board.snap.Reviews = []PRLink{{Repo: "mattermost/mattermost", Number: 7, ReviewName: "mattermost-pr7"}}
	srv := httptest.NewServer(newServer(board, ""))
	defer srv.Close()
	get := func(path string) (int, string) {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}
	if code, body := get("/term/MM-1?start=1"); code != http.StatusOK || !strings.Contains(body, `"/start/MM-1"`) || !strings.Contains(body, "start();") {
		t.Errorf("task terminal page: %d", code)
	}
	if code, body := get("/term/mattermost-pr7"); code != http.StatusOK || !strings.Contains(body, `"/review/start/mattermost-pr7"`) || !strings.Contains(body, "connect();") {
		t.Errorf("review terminal page: %d", code)
	}
	if code, _ := get("/term/MM-9"); code != http.StatusNotFound {
		t.Errorf("unknown id: %d, want 404", code)
	}
	if code, _ := get("/static/xterm.js"); code != http.StatusOK {
		t.Errorf("xterm.js: %d", code)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/term/MM-1/ws"
	_, res, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example.com"}}})
	if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin upgrade: %v, want 403", err)
	}
	_, res, err = websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/term/MM-9/ws", nil)
	if err == nil || res == nil || res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id upgrade: %v, want 404", err)
	}

	viewer := httptest.NewRequest("GET", "/term/MM-1", nil)
	viewer.Host = "localhost"
	viewer.RemoteAddr = "192.0.2.1:5000"
	rec := httptest.NewRecorder()
	newServer(board, "").ServeHTTP(rec, viewer)
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer terminal page: %d, want 403", rec.Code)
	}
}

func TestAgentCommandDefault(t *testing.T) {
	base := `"project_keys":["MM"],"jira":{"base_url":"https://x.atlassian.net"},"repos":[{"name":"a/b"}]`
	for _, c := range []struct{ extra, agent, resume string }{
		{``, "claude", "claude --continue"},
		{`,"agent_command":"aider"`, "aider", "aider"},
		{`,"agent_resume_command":"claude -c"`, "claude", "claude -c"},
	} {
		cfg, err := parseConfig([]byte(`{` + base + c.extra + `}`))
		if err != nil {
			t.Fatal(err)
		}
		if got, gotResume := cfg.agent(false), cfg.agent(true); got != c.agent || gotResume != c.resume {
			t.Errorf("%s: agent %q resume %q, want %q %q", c.extra, got, gotResume, c.agent, c.resume)
		}
	}
}

func TestCanResume(t *testing.T) {
	if got, _ := claudeProjectName("/home/me/src/my_repo.v2-MM-1"); got != "-home-me-src-my-repo-v2-MM-1" {
		t.Errorf("project name %q", got)
	}
	if _, ok := claudeProjectName("/" + strings.Repeat("a", 200)); ok {
		t.Error("over-long project name accepted")
	}

	base := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", base)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.AgentCommand, cfg.AgentResume = "", ""
	if canResume(cfg, dir) {
		t.Error("resumed with no conversation")
	}
	cfg.AgentResume = "aider --restore"
	if !canResume(cfg, dir) {
		t.Error("custom resume command not trusted")
	}
	cfg.AgentResume = ""
	name, _ := claudeProjectName(dir)
	if err := os.MkdirAll(filepath.Join(base, "projects", name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "projects", name, "s.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !canResume(cfg, dir) {
		t.Error("conversation not found")
	}
}

func TestDefaultsAreNotSaved(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"project_keys":["MM"],"jira":{"base_url":"https://x.atlassian.net","jql":"  "},"repos":[{"name":"a/b"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.jql() != defaultJQL || cfg.tmuxSession() != "taskboard" || cfg.agent(false) != "claude" || cfg.Repos[0].plansDir() != "plans" {
		t.Errorf("defaults not applied: jql %q, session %q, agent %q, plans %q", cfg.jql(), cfg.tmuxSession(), cfg.agent(false), cfg.Repos[0].plansDir())
	}
	path := filepath.Join(t.TempDir(), "taskboard.json")
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, def := range []string{"claude", `"taskboard"`, `"plans"`, "statusCategory", "done_jql"} {
		if strings.Contains(string(raw), def) {
			t.Errorf("saved config contains the default %s; defaults belong in placeholders:\n%s", def, raw)
		}
	}
}

func TestDoneJQL(t *testing.T) {
	base := `"project_keys":["MM"],"done_days":7,"repos":[{"name":"a/b"}]`
	cfg, err := parseConfig([]byte(`{` + base + `,"jira":{"base_url":"https://x.atlassian.net"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.doneJQL(), "(assignee = currentUser()) AND statusCategory = Done AND resolved >= -7d"; got != want {
		t.Errorf("default doneJQL = %q, want %q", got, want)
	}
	cfg, err = parseConfig([]byte(`{` + base + `,"jira":{"base_url":"https://x.atlassian.net","done_jql":" assignee = currentUser() OR reporter = currentUser() "}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.doneJQL(), "(assignee = currentUser() OR reporter = currentUser()) AND statusCategory = Done AND resolved >= -7d"; got != want {
		t.Errorf("custom doneJQL = %q, want %q", got, want)
	}
	for _, order := range []string{"Order By updated", "order  BY updated", "ORDER\\nBY updated"} {
		_, err = parseConfig([]byte(`{` + base + `,"jira":{"base_url":"https://x.atlassian.net","done_jql":"assignee = currentUser() ` + order + `"}}`))
		if err == nil || !strings.Contains(err.Error(), "jira.done_jql") {
			t.Errorf("done_jql with %q: error = %v, want one naming jira.done_jql", order, err)
		}
	}
}

func TestHighlightsStore(t *testing.T) {
	path := highlightsPath(filepath.Join(t.TempDir(), "taskboard.json"))
	h, err := loadHighlights(path)
	if err != nil || len(h.list()) != 0 {
		t.Fatalf("missing file: %v %v, want empty", h.list(), err)
	}
	if _, err := h.set("MM-1", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.set("o/r#5", true, nil); err != nil {
		t.Fatal(err)
	}
	h, err = loadHighlights(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.list(); !slices.Equal(got, []string{"MM-1", "o/r#5"}) {
		t.Errorf("after reload: %v", got)
	}
	got, err := h.set("MM-2", true, map[string]bool{"MM-2": true, "o/r#5": true})
	if err != nil || !slices.Equal(got, []string{"MM-2", "o/r#5"}) {
		t.Errorf("set with a complete board: %v %v, want MM-1 pruned", got, err)
	}
	if got, _ := h.set("MM-2", false, nil); !slices.Equal(got, []string{"o/r#5"}) {
		t.Errorf("unset: %v", got)
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHighlights(path); err == nil {
		t.Error("invalid JSON should be an error")
	}
}

func TestHighlightEndpoint(t *testing.T) {
	board := NewBoard(testConfig(t))
	var err error
	if board.highlights, err = loadHighlights(highlightsPath(filepath.Join(t.TempDir(), "taskboard.json"))); err != nil {
		t.Fatal(err)
	}
	board.snap = Snapshot{
		Tasks:   []Task{{Key: "MM-1", Stage: StageTicket}, {Key: "MM-2", Stage: StageTicket}},
		Reviews: []PRLink{{Repo: "mattermost/logr", Number: 5, Role: RoleRequested}},
		Updated: time.Now(),
	}
	h := newServer(board, "")
	do := func(method, path, remote string, header bool, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Host = "localhost"
		req.RemoteAddr = remote
		if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if header {
			req.Header.Set("X-Taskboard", "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	const local = "127.0.0.1:1234"
	if rec := do("POST", "/highlight", local, false, "id=MM-1&on=1"); rec.Code != http.StatusForbidden {
		t.Errorf("without the header: %d, want 403", rec.Code)
	}
	if rec := do("POST", "/highlight", "192.0.2.1:1234", true, "id=MM-1&on=1"); rec.Code != http.StatusForbidden {
		t.Errorf("viewer: %d, want 403", rec.Code)
	}
	if rec := do("POST", "/highlight", local, true, "id=MM-9&on=1"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: %d, want 404", rec.Code)
	}
	board.highlights.ids["MM-gone"] = true
	board.snap.Errors = []string{"jira: down"}
	do("POST", "/highlight", local, true, "id=MM-2&on=1")
	if !board.highlights.ids["MM-gone"] {
		t.Error("an incomplete board should not prune highlights")
	}
	board.snap.Errors = nil
	rec := do("POST", "/highlight", local, true, "id=mattermost%2Flogr%235&on=1")
	var res struct{ Highlights []string }
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("highlight: %d %s", rec.Code, rec.Body.String())
	}
	if want := []string{"MM-2", "mattermost/logr#5"}; !slices.Equal(res.Highlights, want) {
		t.Errorf("highlights %v, want %v", res.Highlights, want)
	}

	rec = do("GET", "/version", local, false, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Highlights) != 2 {
		t.Errorf("version highlights: %s", rec.Body.String())
	}
	page := do("GET", "/", local, false, "").Body.String()
	if !strings.Contains(page, `highlighted" data-id="MM-2"`) || !strings.Contains(page, `highlighted" data-id="mattermost/logr#5"`) {
		t.Error("index should mark highlighted task and review rows")
	}
	if strings.Contains(page, `highlighted" data-id="MM-1"`) {
		t.Error("MM-1 is not highlighted")
	}
	if !strings.Contains(page, "data-highlight") {
		t.Error("a local browser should get highlight toggles")
	}
	if page := do("GET", "/", "192.0.2.1:1234", false, "").Body.String(); strings.Contains(page, "data-highlight ") {
		t.Error("a viewer should not get highlight toggles")
	}
}

func TestDecodeEpicFields(t *testing.T) {
	out := []byte(`[{"key":"MM-10","fields":{"summary":"Epic","status":{"name":"Open"},"issuetype":{"name":"Epic","hierarchyLevel":1}}},
{"key":"MM-11","fields":{"summary":"Story","status":{"name":"Open"},"issuetype":{"name":"Story","hierarchyLevel":0},
 "parent":{"key":"MM-10","fields":{"summary":"Epic","issuetype":{"name":"Epic","hierarchyLevel":1}}},"assignee":{"displayName":"Jack"}}},
{"key":"MM-12","fields":{"summary":"Subtask","status":{"name":"Open"},"issuetype":{"name":"Sub-task","hierarchyLevel":-1},
 "parent":{"key":"MM-11","fields":{"summary":"Story","issuetype":{"name":"Story","hierarchyLevel":0}}},"assignee":null}}]`)
	issues, err := decodeIssues(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []JiraIssue{
		{Key: "MM-10", Summary: "Epic", Status: "Open", Epic: true},
		{Key: "MM-11", Summary: "Story", Status: "Open", Parent: "MM-10", ParentSummary: "Epic", Assignee: "Jack"},
		{Key: "MM-12", Summary: "Subtask", Status: "Open"},
	}
	if !slices.Equal(issues, want) {
		t.Errorf("issues = %+v, want %+v", issues, want)
	}
}

func TestBuildTasksEpicFields(t *testing.T) {
	cfg := testConfig(t)
	tasks := buildTasks(cfg, sources{issues: map[string]JiraIssue{
		"MM-10": {Key: "MM-10", Summary: "Epic", Epic: true},
		"MM-11": {Key: "MM-11", Summary: "Story", Parent: "MM-10", ParentSummary: "Epic"},
	}}, time.Now())
	byKey := map[string]Task{}
	for _, task := range tasks {
		byKey[task.Key] = task
	}
	if !byKey["MM-10"].Epic || byKey["MM-10"].Parent != "" {
		t.Errorf("epic = %+v", byKey["MM-10"])
	}
	if c := byKey["MM-11"]; c.Epic || c.Parent != "MM-10" || c.ParentSummary != "Epic" || c.ParentURL != cfg.issueURL("MM-10") {
		t.Errorf("child = %+v", c)
	}
}

func TestBuildEpics(t *testing.T) {
	cfg := testConfig(t)
	now := time.Now()
	tasks := buildTasks(cfg, sources{
		issues: map[string]JiraIssue{
			"MM-10": {Key: "MM-10", Summary: "Epic", Epic: true},
			"MM-11": {Key: "MM-11", Summary: "Coding", Parent: "MM-10", Updated: now},
			"MM-12": {Key: "MM-12", Summary: "Failing", Parent: "MM-10", Updated: now},
			"MM-13": {Key: "MM-13", Summary: "Not a child"},
			"MM-14": {Key: "MM-14", Summary: "Done mine", Parent: "MM-10", Done: true, Updated: now},
			"MM-20": {Key: "MM-20", Summary: "Done epic", Epic: true, Done: true, Updated: now},
		},
		prs:       []PR{{Key: "MM-12", State: "OPEN", CI: CIFailing}},
		worktrees: []Worktree{{Key: "MM-11", Path: "/src/mm-MM-11"}},
	}, now)
	children := map[string][]JiraIssue{
		"MM-10": {
			{Key: "MM-12", Summary: "Failing"},
			{Key: "MM-30", Summary: "Done theirs", Status: "Closed", Done: true, Assignee: "Jack"},
			{Key: "MM-31", Summary: "Open theirs", Status: "Open", Assignee: "Jack"},
		},
		"MM-20": {{Key: "MM-32", Summary: "Never shown"}},
	}
	groups := buildEpics(cfg, tasks, children)
	if len(groups) != 1 || groups[0].Task.Key != "MM-10" {
		t.Fatalf("groups = %+v, want only the open epic MM-10", groups)
	}
	g := groups[0]
	var keys []string
	for _, c := range g.Children {
		keys = append(keys, c.Key)
	}
	if want := []string{"MM-12", "MM-11", "MM-31", "MM-14", "MM-30"}; !slices.Equal(keys, want) {
		t.Errorf("children = %v, want %v", keys, want)
	}
	if g.Open != 3 || g.Done != 2 {
		t.Errorf("open %d done %d, want 3 and 2", g.Open, g.Done)
	}
	if c := g.Children[0]; !c.OnBoard || c.StageLabel != "CI failing" || c.Tone != "fail" {
		t.Errorf("on-board child = %+v", c)
	}
	if c := g.Children[2]; c.OnBoard || c.Assignee != "Jack" || c.JiraURL != cfg.issueURL("MM-31") {
		t.Errorf("off-board child = %+v", c)
	}
}

func TestTaskGroup(t *testing.T) {
	cases := []struct {
		task Task
		want string
	}{
		{Task{Key: "MM-1", Epic: true, Stage: StageTicket}, groupEpics},
		{Task{Key: "MM-2", Epic: true, Stage: StageInReview, PR: &PR{}}, groupWaiting},
		{Task{Key: "MM-3", Epic: true, Stage: StageDone, JiraDone: true}, groupDone},
		{Task{Key: "MM-4", Stage: StageTicket}, groupNotStarted},
	}
	for _, c := range cases {
		if got := taskGroup(c.task); got != c.want {
			t.Errorf("%s: group %q, want %q", c.task.Key, got, c.want)
		}
	}
}

func TestSectionOrder(t *testing.T) {
	cfg := testConfig(t)
	board := NewBoard(cfg)
	board.snap = Snapshot{
		Tasks: []Task{
			{Key: "MM-1", Stage: StageCIFailing},
			{Key: "MM-2", Stage: StageInReview},
			{Key: "MM-3", Stage: StageCoding},
			{Key: "MM-4", Stage: StageCleanup},
			{Key: "MM-5", Stage: StageTicket},
			{Key: "MM-6", Stage: StageDone},
		},
		Epics:    []EpicGroup{{Task: Task{Key: "MM-7", Epic: true, Stage: StageTicket}}},
		Reviews:  []PRLink{{Repo: "a/b", Number: 1}},
		OtherPRs: []PRLink{{Repo: "c/d", Number: 2}},
		Updated:  time.Now(),
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "localhost"
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	newServer(board, "").ServeHTTP(rec, req)
	page := rec.Body.String()
	order := []string{"Needs you", "Reviews", "Waiting on others", "In progress", "Needs clean-up", "Epics", "Not started", "Other repos", "Done"}
	last := -1
	for _, name := range order {
		i := strings.Index(page, `<section id="`+slug(name)+`"`)
		if i < last {
			t.Errorf("section %s is out of order", name)
		}
		last = i
	}
	for _, name := range order {
		sec := page[strings.Index(page, `<section id="`+slug(name)+`"`):]
		sec = strings.TrimSpace(sec[strings.Index(sec, ">")+1:])
		want := "<details open><summary>"
		switch name {
		case "Done":
			want = "<details><summary>"
		case "Epics":
			want = "<h2>"
		}
		if !strings.HasPrefix(sec, want) {
			t.Errorf("section %s should start with %s", name, want)
		}
	}
	nav := page[:strings.Index(page, "</nav>")]
	last = -1
	for _, name := range order {
		i := strings.Index(nav, `href="#`+slug(name)+`"`)
		if i < last {
			t.Errorf("nav link %s is out of order", name)
		}
		last = i
	}
}

func TestIndexEpics(t *testing.T) {
	cfg := testConfig(t)
	board := NewBoard(cfg)
	tasks := buildTasks(cfg, sources{
		issues: map[string]JiraIssue{
			"MM-10": {Key: "MM-10", Summary: "Big epic", Epic: true},
			"MM-11": {Key: "MM-11", Summary: "My story", Parent: "MM-10", ParentSummary: "Big epic"},
			"MM-12": {Key: "MM-12", Summary: "Orphan story", Parent: "MM-99", ParentSummary: "Someone's epic"},
		},
	}, time.Now())
	board.snap = Snapshot{
		Tasks:   tasks,
		Epics:   buildEpics(cfg, tasks, map[string][]JiraIssue{"MM-10": {{Key: "MM-40", Summary: "Their story", Status: "Open", Assignee: "Jack"}}}),
		Updated: time.Now(),
	}
	h := newServer(board, "")
	get := func(remote string) string {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = "localhost"
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	page := get("127.0.0.1:1234")
	for _, want := range []string{
		`id="epic-MM-10"`,
		`<span class="badge">Epic</span>Big epic`,
		`href="#epic-MM-10" data-to-epic="MM-10"`,
		`href="` + cfg.issueURL("MM-99") + `"`,
		`data-goto="MM-11"`,
		`Open, Jack`,
		`href="#epics"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index is missing %s", want)
		}
	}
	notStarted := page[strings.Index(page, `id="not-started"`):]
	notStarted = notStarted[:strings.Index(notStarted, "</section>")]
	if strings.Contains(notStarted, `data-id="MM-10"`) {
		t.Error("an epic with no work of its own should not be in Not started")
	}
	if strings.Count(page, `data-id="MM-11"`) != 1 {
		t.Error("an on-board child should have one full row")
	}
	if viewer := get("192.0.2.1:1234"); !strings.Contains(viewer, `id="epic-MM-10"`) || strings.Contains(viewer, "data-highlight ") {
		t.Error("a viewer should see the epics read-only")
	}
}

func TestRenderMarkdown(t *testing.T) {
	got, err := renderMarkdown([]byte("# MM-1 plan\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n<script>alert(1)</script>\n\n[x](javascript:alert(1))\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{"<h1", "MM-1 plan</h1>", "<table>", "<td>1</td>"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	for _, bad := range []string{"<script", "javascript:"} {
		if strings.Contains(s, bad) {
			t.Errorf("unexpected %q in %s", bad, s)
		}
	}
}

func TestSplitFrontMatter(t *testing.T) {
	for _, tc := range []struct {
		in     string
		fields []planField
		body   string
	}{
		{"---\nstatus: approved\n---\nbody\n", []planField{{"status", "approved"}}, "body\n"},
		{"---\r\nstatus: \"draft\"\r\nreviewers:\r\n  - alice\r\n  - bob\r\n---\r\nbody", []planField{{"status", "draft"}, {"reviewers", "- alice - bob"}}, "body"},
		{"---\n---\nbody", nil, "body"},
		{"# title\n---\n", nil, "# title\n---\n"},
		{"---\nstatus: draft\n", nil, "---\nstatus: draft\n"},
	} {
		fields, body := splitFrontMatter([]byte(tc.in))
		if !slices.Equal(fields, tc.fields) || string(body) != tc.body {
			t.Errorf("splitFrontMatter(%q) = %q, %q, want %q, %q", tc.in, fields, body, tc.fields, tc.body)
		}
	}
}

func TestExtensionDownload(t *testing.T) {
	h := newServer(NewBoard(testConfig(t)), "")
	get := func(path, host string, tlsReq bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.Host = host
		if tlsReq {
			req.TLS = &tls.ConnectionState{}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s on %s: %d", path, host, rec.Code)
		}
		return rec
	}
	matches := func(manifest string) []string {
		t.Helper()
		var m struct {
			Version        string
			ContentScripts []struct{ Matches []string } `json:"content_scripts"`
		}
		if err := json.Unmarshal([]byte(manifest), &m); err != nil {
			t.Fatal(err)
		}
		if m.Version != extensionVersion() {
			t.Errorf("manifest version %q, want %q", m.Version, extensionVersion())
		}
		return m.ContentScripts[0].Matches
	}
	defaults := []string{"http://127.0.0.1/*", "http://localhost/*"}

	for _, c := range []struct {
		host string
		tls  bool
		want []string
	}{
		{"localhost:8080", false, defaults},
		{"127.0.0.1:8080", false, defaults},
		{"board.example:8443", true, append(slices.Clone(defaults), "https://board.example/*")},
		{"localhost:8443", true, append(slices.Clone(defaults), "https://localhost/*")},
		{"[::1]:8443", true, defaults},
	} {
		rec := get("/"+extensionZipName, c.host, c.tls)
		if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
			t.Fatalf("%s: Content-Type %q", c.host, ct)
		}
		zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
		if err != nil {
			t.Fatal(err)
		}
		zipped := map[string]string{}
		for _, f := range zr.File {
			if f.Modified.Year() > time.Now().Year() {
				t.Errorf("%s dated %v", f.Name, f.Modified)
			}
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			zipped[f.Name] = string(data)
		}
		for _, want := range []string{"manifest.json", "background.js", "content.js"} {
			if _, ok := zipped[want]; !ok {
				t.Errorf("%s: zip lacks %s at its root", c.host, want)
			}
		}
		if got := matches(zipped["manifest.json"]); !slices.Equal(got, c.want) {
			t.Errorf("%s: zip matches %v, want %v", c.host, got, c.want)
		}

		var files map[string]string
		if err := json.Unmarshal(get("/extension/files.json", c.host, c.tls).Body.Bytes(), &files); err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(files, zipped) {
			t.Errorf("%s: files.json differs from the zip", c.host)
		}
	}

	for _, path := range []string{"/", "/extension"} {
		body := get(path, "localhost", false).Body.String()
		if !strings.Contains(body, `<meta name="taskboard-extension-version" content="`+extensionVersion()+`">`) {
			t.Errorf("%s lacks the extension version meta", path)
		}
	}
	if !strings.Contains(get("/extension", "localhost", false).Body.String(), "/"+extensionZipName) {
		t.Error("install page lacks the download link")
	}
}
