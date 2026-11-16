package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

type CIState int

const (
	CINone CIState = iota
	CIPending
	CIPassing
	CIFailing
)

func (s CIState) String() string {
	switch s {
	case CIPending:
		return "checks running"
	case CIPassing:
		return "checks passing"
	case CIFailing:
		return "checks failing"
	}
	return ""
}

type PR struct {
	Repo            string
	Key             string
	Number          int
	Title           string
	URL             string
	Branch          string
	HeadSHA         string
	State           string
	Draft           bool
	Review          string
	CI              CIState
	UpdatedAt       time.Time
	MergedAt        time.Time
	Assigned        bool
	ReviewRequested bool
	// A person, not a team, is still requested for review.
	ReviewerPending bool
}

type ghCheck struct {
	TypeName   string `json:"__typename"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

type ghPRNode struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	Body           string    `json:"body"`
	HeadRefName    string    `json:"headRefName"`
	HeadRefOid     string    `json:"headRefOid"`
	State          string    `json:"state"`
	IsDraft        bool      `json:"isDraft"`
	ReviewDecision string    `json:"reviewDecision"`
	UpdatedAt      time.Time `json:"updatedAt"`
	MergedAt       time.Time `json:"mergedAt"`
	Repository     struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	ReviewRequests struct {
		Nodes []ghReviewRequest `json:"nodes"`
	} `json:"reviewRequests"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup struct {
					State    string `json:"state"`
					Contexts struct {
						PageInfo struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
						Nodes []ghCheck `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

type ghReviewRequest struct {
	RequestedReviewer struct {
		TypeName string `json:"__typename"`
		Login    string `json:"login"`
	} `json:"requestedReviewer"`
}

func (n ghPRNode) pr(cfg *Config, repo string) PR {
	key := cfg.findKey(n.HeadRefName)
	if key == "" {
		key = cfg.findKey(n.Title)
	}
	var ci CIState
	if len(n.Commits.Nodes) > 0 {
		rollup := n.Commits.Nodes[0].Commit.StatusCheckRollup
		ci = rollupCI(rollup.Contexts.Nodes)
		// Only the first 100 checks are fetched; GitHub's overall state covers the rest.
		if rollup.Contexts.PageInfo.HasNextPage {
			ci = cmp.Or(rollupStateCI(rollup.State), ci)
		}
	}
	return PR{
		Repo:      repo,
		Key:       key,
		Number:    n.Number,
		Title:     n.Title,
		URL:       n.URL,
		Branch:    n.HeadRefName,
		HeadSHA:   n.HeadRefOid,
		State:     n.State,
		Draft:     n.IsDraft,
		Review:    n.ReviewDecision,
		CI:        ci,
		UpdatedAt: n.UpdatedAt,
		MergedAt:  n.MergedAt,
		ReviewerPending: slices.ContainsFunc(n.ReviewRequests.Nodes, func(r ghReviewRequest) bool {
			return r.RequestedReviewer.TypeName == "User"
		}),
	}
}

// requests reports whether login is requested by name; a team's request, such as one CODEOWNERS adds, doesn't count.
func (n ghPRNode) requests(login string) bool {
	return login != "" && slices.ContainsFunc(n.ReviewRequests.Nodes, func(r ghReviewRequest) bool {
		return r.RequestedReviewer.TypeName == "User" && strings.EqualFold(r.RequestedReviewer.Login, login)
	})
}

type prRef struct {
	repo   string
	number int
}

func refOf(repo string, number int) prRef { return prRef{strings.ToLower(repo), number} }

// withoutTaskPRs keeps every row while login is unknown, since ReviewRequested can't be trusted then, and keeps
// the row for a draft that requests you by name, since a draft's stage never shows the request.
func withoutTaskPRs(links []PRLink, tasks []Task, login string) []PRLink {
	if login == "" {
		return links
	}
	shown := map[prRef]bool{}
	for _, t := range tasks {
		if t.PR != nil && !(t.PR.Draft && t.PR.ReviewRequested) {
			shown[refOf(t.PR.Repo, t.PR.Number)] = true
		}
	}
	return slices.DeleteFunc(links, func(l PRLink) bool { return l.Review == nil && shown[refOf(l.Repo, l.Number)] })
}

func rollupCI(checks []ghCheck) CIState {
	if len(checks) == 0 {
		return CINone
	}
	pending := false
	for _, c := range checks {
		switch {
		case c.Conclusion == "FAILURE", c.Conclusion == "TIMED_OUT",
			c.Conclusion == "ACTION_REQUIRED", c.Conclusion == "STARTUP_FAILURE",
			c.State == "FAILURE", c.State == "ERROR":
			return CIFailing
		case c.TypeName == "CheckRun" && c.Status != "COMPLETED",
			c.State == "PENDING", c.State == "EXPECTED":
			pending = true
		}
	}
	if pending {
		return CIPending
	}
	return CIPassing
}

func rollupStateCI(state string) CIState {
	switch state {
	case "SUCCESS":
		return CIPassing
	case "FAILURE", "ERROR":
		return CIFailing
	case "PENDING", "EXPECTED":
		return CIPending
	}
	return CINone
}

// Declared in display order; sorting relies on it.
type Role int

const (
	RoleRequested Role = iota
	RoleAuthor
	RoleReviewed
	RoleLeftover
)

type PRLink struct {
	Repo      string
	Number    int
	Title     string
	URL       string
	Author    string
	Draft     bool
	Role      Role
	State     string
	UpdatedAt time.Time
	Key       string
	JiraURL   string

	ReviewName string
	Review     *Review
	// NoCheckout is true when the repo has a configured folder that does not exist on disk.
	NoCheckout bool
}

func (p *PRLink) setKey(cfg *Config, body string) {
	p.Key = cfg.findKey(p.Title)
	if p.Key == "" {
		p.Key = cfg.findKey(body)
	}
	if p.Key != "" {
		p.JiraURL = cfg.issueURL(p.Key)
	}
}

func (p PRLink) RepoName() string { return path.Base(p.Repo) }

func (p PRLink) ID() string { return p.Repo + "#" + strconv.Itoa(p.Number) }

// Group is the browser tab group for the PR's links; using the ticket puts a review with the ticket's tabs.
func (p PRLink) Group() string {
	if p.Key != "" {
		return p.Key
	}
	return p.ID()
}

func (p PRLink) Meta() string {
	var parts []string
	if p.Role != RoleAuthor {
		parts = append(parts, "by "+p.Author)
	}
	if p.Draft {
		parts = append(parts, "draft")
	}
	return strings.Join(parts, ", ")
}

func (p PRLink) StageLabel() string {
	switch p.Role {
	case RoleRequested:
		return "Review requested"
	case RoleAuthor:
		return "Your PR"
	case RoleLeftover:
		switch p.State {
		case "MERGED":
			return "Merged, clean up"
		case "CLOSED":
			return "Closed, clean up"
		}
		return "Not awaiting you, clean up"
	}
	return "Reviewed, waiting on author"
}

func (p PRLink) Tone() string {
	if p.Role == RoleRequested || p.Role == RoleLeftover {
		return "needs"
	}
	return "active"
}

func mergePRLinks(cfg *Config, found [3][]ghPRNode) []PRLink {
	seen := map[string]bool{}
	var links []PRLink
	for role, prs := range found {
		for _, r := range prs {
			if seen[r.URL] {
				continue
			}
			seen[r.URL] = true
			l := PRLink{
				Repo:      r.Repository.NameWithOwner,
				Number:    r.Number,
				Title:     r.Title,
				URL:       r.URL,
				Author:    r.Author.Login,
				Draft:     r.IsDraft,
				Role:      Role(role),
				UpdatedAt: r.UpdatedAt,
			}
			l.setKey(cfg, r.Body)
			links = append(links, l)
		}
	}
	slices.SortStableFunc(links, func(a, b PRLink) int {
		return cmp.Or(
			cmp.Compare(a.Role, b.Role),
			b.UpdatedAt.Compare(a.UpdatedAt),
		)
	})
	return links
}

// Authored PRs in configured repos are left out because they already appear as tasks.
// Repos outside the configured ones go to others only when github.organizations allows their owner.
func splitPRLinks(cfg *Config, links []PRLink) (reviews, others []PRLink) {
	for _, l := range links {
		switch {
		case !cfg.hasRepo(l.Repo):
			if cfg.showsOther(l.Repo) {
				others = append(others, l)
			}
		case l.Role != RoleAuthor:
			reviews = append(reviews, l)
		}
	}
	return reviews, others
}

type prKind int

const (
	prOpen prKind = iota
	prMerged
	prLink
)

// One fragment per kind keeps CI and review requests off searches that never show them.
type prFragment struct{ name, fields string }

func (f prFragment) text() string {
	return "fragment " + f.name + " on PullRequest { " + f.fields + " }"
}

var prFragments = [...]prFragment{
	prOpen: {"openPR", `number title url body headRefName headRefOid state isDraft reviewDecision updatedAt mergedAt
  repository { nameWithOwner } author { login }
  reviewRequests(first: 50) { nodes { requestedReviewer { __typename ... on User { login } } } }
  commits(last: 1) { nodes { commit { statusCheckRollup { state contexts(first: 100) { pageInfo { hasNextPage } nodes {
    __typename ... on CheckRun { status conclusion } ... on StatusContext { state } } } } } } }`},
	prMerged: {"mergedPR", `number title url headRefName headRefOid state isDraft reviewDecision updatedAt mergedAt repository { nameWithOwner }`},
	prLink:   {"linkPR", `number title url body isDraft updatedAt state repository { nameWithOwner } author { login }`},
}

type prSearch struct {
	query string
	limit int
	kind  prKind
}

const (
	searchMineOpen = iota
	searchAssignedOpen
	searchMineMerged
	searchAssignedMerged
	searchRequested
	searchReviewed
)

// GitHub search returns at most 1000 results per query.
const searchMax = 1000

func prSearches(cfg *Config, now time.Time) []prSearch {
	merged := " merged:>=" + now.AddDate(0, 0, -max(cfg.DoneDays, cfg.MergedPRDays)).Format("2006-01-02")
	assigned := cfg.assignedSearch()
	return []prSearch{
		searchMineOpen:       {"is:pr is:open author:@me", searchMax, prOpen},
		searchAssignedOpen:   {"is:pr is:open " + assigned, searchMax, prOpen},
		searchMineMerged:     {"is:pr is:merged author:@me" + merged, searchMax, prMerged},
		searchAssignedMerged: {"is:pr is:merged " + assigned + merged, searchMax, prMerged},
		searchRequested:      {"is:pr is:open review-requested:@me", 100, prLink},
		// GitHub drops a review request once you submit a review, so reviewed-by covers PRs you have already looked at.
		searchReviewed: {"is:pr is:open reviewed-by:@me -author:@me", 100, prLink},
	}
}

type pageRequest struct {
	search int
	first  int
	after  string
}

func searchQuery(searches []prSearch, pages []pageRequest) (string, map[string]string) {
	var params, fields []string
	vars := map[string]string{}
	var used [len(prFragments)]bool
	for _, p := range pages {
		s := searches[p.search]
		q := fmt.Sprintf("q%d", p.search)
		vars[q] = s.query
		params = append(params, "$"+q+": String!")
		after := ""
		if p.after != "" {
			a := fmt.Sprintf("a%d", p.search)
			vars[a] = p.after
			params = append(params, "$"+a+": String!")
			after = ", after: $" + a
		}
		fields = append(fields, fmt.Sprintf("s%d: search(type: ISSUE, query: $%s, first: %d%s) { pageInfo { hasNextPage endCursor } nodes { ...%s } }",
			p.search, q, p.first, after, prFragments[s.kind].name))
		used[s.kind] = true
	}
	query := "query(" + strings.Join(params, ", ") + ") { viewer { login } rateLimit { limit remaining resetAt }\n" + strings.Join(fields, "\n") + " }"
	for kind, ok := range used {
		if ok {
			query += "\n" + prFragments[kind].text()
		}
	}
	return query, vars
}

type graphQLFunc func(ctx context.Context, query string, vars map[string]string) ([]byte, error)

func ghGraphQL(ctx context.Context, query string, vars map[string]string) ([]byte, error) {
	args := []string{"api", "graphql", "-f", "query=" + query}
	for k, v := range vars {
		args = append(args, "-f", k+"="+v)
	}
	return runCmdOutput(ctx, "", "gh", args...)
}

type graphQLResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// field reports whether name came back, since a part of the query that failed comes back as null.
func (r graphQLResponse) field(name string) (json.RawMessage, bool) {
	raw, ok := r.Data[name]
	return raw, ok && string(raw) != "null"
}

func (r graphQLResponse) errs() error {
	var msgs []string
	for _, e := range r.Errors {
		if !slices.Contains(msgs, e.Message) {
			msgs = append(msgs, e.Message)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(msgs, "; "))
}

var graphQLRetryDelay = 2 * time.Second

// postGraphQL keeps the data gh prints even when it exits non-zero because part of the query failed, and
// retries once when nothing usable came back, since GitHub sometimes answers a large search with an HTML page.
func postGraphQL(ctx context.Context, post graphQLFunc, query string, vars map[string]string) (graphQLResponse, error) {
	for attempt := 0; ; attempt++ {
		out, err := post(ctx, query, vars)
		var resp graphQLResponse
		jsonErr := json.Unmarshal(out, &resp)
		if jsonErr == nil && resp.Data != nil {
			return resp, cmp.Or(resp.errs(), err)
		}
		if err == nil {
			err = cmp.Or(resp.errs(), jsonErr, errors.New("GitHub returned no data"))
		}
		if attempt > 0 || isRateLimited(err) {
			return resp, err
		}
		select {
		case <-ctx.Done():
			return resp, err
		case <-time.After(graphQLRetryDelay):
		}
	}
}

type ghRate struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"resetAt"`
}

type searchResult struct {
	login  string
	rate   ghRate
	found  [][]ghPRNode
	failed []bool
}

// A search that failed or didn't finish paging is marked failed, so its last good results can stand in.
func runSearches(ctx context.Context, post graphQLFunc, searches []prSearch) (searchResult, error) {
	res := searchResult{found: make([][]ghPRNode, len(searches)), failed: make([]bool, len(searches))}
	var pending []pageRequest
	for i, s := range searches {
		pending = append(pending, pageRequest{search: i, first: min(100, s.limit)})
	}
	var errs []error
	for len(pending) > 0 {
		query, vars := searchQuery(searches, pending)
		resp, err := postGraphQL(ctx, post, query, vars)
		if err != nil {
			errs = append(errs, err)
		}
		if raw, ok := resp.field("viewer"); ok {
			var viewer struct {
				Login string `json:"login"`
			}
			if json.Unmarshal(raw, &viewer) == nil {
				res.login = viewer.Login
			}
		}
		if raw, ok := resp.field("rateLimit"); ok {
			json.Unmarshal(raw, &res.rate)
		}
		var next []pageRequest
		for _, p := range pending {
			i := p.search
			var page struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []*ghPRNode `json:"nodes"`
			}
			raw, ok := resp.field(fmt.Sprintf("s%d", i))
			if !ok {
				res.failed[i] = true
				continue
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				res.failed[i] = true
				errs = append(errs, fmt.Errorf("search %q: %w", searches[i].query, err))
				continue
			}
			// A PR the token may not see, such as one in an org enforcing SAML, comes back as null.
			for _, n := range page.Nodes {
				if n != nil {
					res.found[i] = append(res.found[i], *n)
				}
			}
			left := searches[i].limit - len(res.found[i])
			// A page that adds nothing or doesn't move the cursor would otherwise repeat forever.
			moved := len(page.Nodes) > 0 && page.PageInfo.EndCursor != "" && page.PageInfo.EndCursor != p.after
			if page.PageInfo.HasNextPage && moved && left > 0 {
				next = append(next, pageRequest{search: i, first: min(100, left), after: page.PageInfo.EndCursor})
			}
		}
		pending = next
	}
	return res, errors.Join(errs...)
}

type ghData struct {
	login string
	found [][]ghPRNode
	at    []time.Time
}

func (res searchResult) withFallback(prev ghData, now time.Time) ghData {
	d := ghData{login: cmp.Or(res.login, prev.login), found: slices.Clone(res.found), at: make([]time.Time, len(res.found))}
	for i := range d.found {
		if !res.failed[i] {
			d.at[i] = now
			continue
		}
		if i < len(prev.found) && !prev.at[i].IsZero() {
			d.found[i], d.at[i] = prev.found[i], prev.at[i]
		}
	}
	return d
}

// staleSince returns when the oldest results still shown were fetched, if that was before t.
func (d ghData) staleSince(t time.Time) time.Time {
	var oldest time.Time
	for _, at := range d.at {
		if !at.IsZero() && at.Before(t) && (oldest.IsZero() || at.Before(oldest)) {
			oldest = at
		}
	}
	return oldest
}

func isRateLimited(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "rate limit")
}

func isSecondaryLimit(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "secondary rate limit")
}

const (
	minBackoff = time.Minute
	maxBackoff = 30 * time.Minute
)

// ghPause decides when GitHub may next be asked. It leaves a tenth of the hourly budget for everything
// else using the same gh login, such as agents in taskboard's sessions.
type ghPause struct {
	until   time.Time
	backoff time.Duration
	reason  string
}

// reset is needed only when err says the hourly budget ran out.
func (p ghPause) next(err error, rate ghRate, reset, now time.Time) ghPause {
	// The backoff resets only once GitHub answers, so a failed request between secondary limits doesn't undo it.
	var kept time.Duration
	if err != nil && rate.Limit == 0 {
		kept = p.backoff
	}
	switch {
	case isSecondaryLimit(err):
		backoff := min(max(2*p.backoff, minBackoff), maxBackoff)
		return ghPause{until: now.Add(backoff), backoff: backoff, reason: "GitHub asked taskboard to slow down"}
	case isRateLimited(err):
		if !reset.After(now) {
			reset = now.Add(15 * time.Minute)
		}
		return ghPause{until: reset, backoff: kept, reason: "this hour's GitHub API points are used up"}
	case rate.Limit > 0 && rate.Remaining < rate.Limit/10 && rate.ResetAt.After(now):
		return ghPause{until: rate.ResetAt, reason: fmt.Sprintf("%s of %s points left this hour", thousands(rate.Remaining), thousands(rate.Limit))}
	}
	return ghPause{backoff: kept}
}

func ghRateReset(ctx context.Context) time.Time {
	out, err := runCmd(ctx, "", "gh", "api", "rate_limit", "--jq", ".resources.graphql.reset")
	if err != nil {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

func asOf(at, now time.Time) string {
	if now.Sub(at) > 20*time.Hour {
		return at.Format("Jan 2 15:04")
	}
	return at.Format("15:04")
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func (p ghPause) notice() string {
	return fmt.Sprintf("GitHub paused until %s (%s)", p.until.Local().Format("15:04"), p.reason)
}

// Only refresh calls fetchGitHub, and refreshes don't overlap.
func (b *Board) fetchGitHub(ctx context.Context, cfg *Config, prev ghData) (data ghData, errs []string, notice string) {
	now := time.Now()
	if now.Before(b.ghPause.until) {
		return prev, nil, b.ghPause.notice()
	}
	res, err := runSearches(ctx, ghGraphQL, prSearches(cfg, now))
	data = res.withFallback(prev, now)
	if err != nil {
		errs = append(errs, "GitHub: "+err.Error())
	}
	if res.rate.Limit > 0 {
		b.ghRate = res.rate
	}
	b.pauseAfter(ctx, err, res.rate, now)
	if !b.ghPause.until.IsZero() {
		notice = b.ghPause.notice()
	}
	return data, errs, notice
}

func (b *Board) pauseAfter(ctx context.Context, err error, rate ghRate, now time.Time) {
	var reset time.Time
	if isRateLimited(err) && !isSecondaryLimit(err) {
		// gh api rate_limit can report a later reset than the GraphQL budget itself, so it is only a fallback.
		if reset = b.ghRate.ResetAt; !reset.After(now) {
			reset = ghRateReset(ctx)
		}
	}
	b.ghPause = b.ghPause.next(err, rate, reset, now)
}

type ghResult struct {
	prs             []PR
	reviews, others []PRLink
}

// githubResult keeps task PRs to the configured repos, named as the config names them, since sessions and
// worktrees are matched on that name.
func githubResult(cfg *Config, login string, found [][]ghPRNode) ghResult {
	var res ghResult
	index := map[prRef]int{}
	for _, i := range []int{searchMineOpen, searchMineMerged, searchAssignedOpen, searchAssignedMerged} {
		assigned := i == searchAssignedOpen || i == searchAssignedMerged
		for _, n := range found[i] {
			repo, ok := cfg.repo(n.Repository.NameWithOwner)
			if !ok {
				continue
			}
			pr := n.pr(cfg, repo.Name)
			if assigned {
				pr.Assigned = true
				pr.ReviewRequested = n.requests(login)
			}
			// A search that fell back to old results can hold an older copy of a PR another search just returned.
			// The earlier search still decides whether it counts as assigned, as it does when both are fresh.
			ref := refOf(repo.Name, n.Number)
			if j, ok := index[ref]; ok {
				if pr.UpdatedAt.After(res.prs[j].UpdatedAt) {
					if !res.prs[j].Assigned {
						pr.Assigned, pr.ReviewRequested = false, false
					}
					res.prs[j] = pr
				}
				continue
			}
			index[ref] = len(res.prs)
			res.prs = append(res.prs, pr)
		}
	}
	res.reviews, res.others = splitPRLinks(cfg, mergePRLinks(cfg, [3][]ghPRNode{
		RoleRequested: found[searchRequested],
		RoleAuthor:    found[searchMineOpen],
		RoleReviewed:  found[searchReviewed],
	}))
	return res
}

func viewPRs(ctx context.Context, post graphQLFunc, cfg *Config, links []PRLink) (views map[string]PRLink, gone map[string]bool, err error) {
	var params, fields []string
	vars := map[string]string{}
	for i, l := range links {
		owner, name, _ := strings.Cut(l.Repo, "/")
		o, n := fmt.Sprintf("o%d", i), fmt.Sprintf("n%d", i)
		vars[o], vars[n] = owner, name
		params = append(params, "$"+o+": String!", "$"+n+": String!")
		fields = append(fields, fmt.Sprintf("v%d: repository(owner: $%s, name: $%s) { pullRequest(number: %d) { ...%s } }", i, o, n, l.Number, prFragments[prLink].name))
	}
	query := "query(" + strings.Join(params, ", ") + ") {\n" + strings.Join(fields, "\n") + " }\n" + prFragments[prLink].text()
	resp, err := postGraphQL(ctx, post, query, vars)
	views, gone = map[string]PRLink{}, map[string]bool{}
	for i, l := range links {
		raw, answered := resp.Data[fmt.Sprintf("v%d", i)]
		var repo struct {
			PullRequest *ghPRNode `json:"pullRequest"`
		}
		if !answered || json.Unmarshal(raw, &repo) != nil {
			continue
		}
		if repo.PullRequest == nil {
			gone[l.URL] = true
			continue
		}
		v := repo.PullRequest
		l.Title, l.URL, l.State, l.Author, l.UpdatedAt = v.Title, v.URL, v.State, v.Author.Login, v.UpdatedAt
		l.setKey(cfg, v.Body)
		views[links[i].URL] = l
	}
	return views, gone, err
}
