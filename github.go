package main

import (
	"cmp"
	"context"
	"encoding/json"
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

type ghViewPR struct {
	Title  string `json:"title"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Body   string `json:"body"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func viewPR(ctx context.Context, cfg *Config, link *PRLink) error {
	out, err := runCmd(ctx, "", "gh", "pr", "view", strconv.Itoa(link.Number), "--repo", link.Repo, "--json", "title,url,state,body,author,updatedAt")
	if err != nil {
		return err
	}
	var v ghViewPR
	if err := json.Unmarshal(out, &v); err != nil {
		return err
	}
	link.Title, link.URL, link.State, link.Author, link.UpdatedAt = v.Title, v.URL, v.State, v.Author.Login, v.UpdatedAt
	link.setKey(cfg, v.Body)
	return nil
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
var prFragments = [...]struct{ name, fields string }{
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
	query := "query(" + strings.Join(params, ", ") + ") { viewer { login }\n" + strings.Join(fields, "\n") + " }"
	for kind, ok := range used {
		if ok {
			f := prFragments[kind]
			query += "\nfragment " + f.name + " on PullRequest { " + f.fields + " }"
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
	return runCmd(ctx, "", "gh", args...)
}

var graphQLRetryDelay = 2 * time.Second

// postGraphQL retries once, since GitHub sometimes answers a large search with an HTML error page.
func postGraphQL(ctx context.Context, post graphQLFunc, query string, vars map[string]string) (map[string]json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		out, err := post(ctx, query, vars)
		if err == nil {
			var resp struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if err = json.Unmarshal(out, &resp); err == nil {
				return resp.Data, nil
			}
		}
		if attempt > 0 {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(graphQLRetryDelay):
		}
	}
}

func runSearches(ctx context.Context, post graphQLFunc, searches []prSearch) (string, [][]ghPRNode, error) {
	found := make([][]ghPRNode, len(searches))
	var pending []pageRequest
	for i, s := range searches {
		pending = append(pending, pageRequest{search: i, first: min(100, s.limit)})
	}
	var login string
	for len(pending) > 0 {
		query, vars := searchQuery(searches, pending)
		data, err := postGraphQL(ctx, post, query, vars)
		if err != nil {
			return "", nil, err
		}
		var viewer struct {
			Login string `json:"login"`
		}
		if err := json.Unmarshal(data["viewer"], &viewer); err != nil {
			return "", nil, fmt.Errorf("viewer: %w", err)
		}
		login = viewer.Login
		var next []pageRequest
		for _, p := range pending {
			i := p.search
			var page struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []ghPRNode `json:"nodes"`
			}
			if err := json.Unmarshal(data[fmt.Sprintf("s%d", i)], &page); err != nil {
				return "", nil, fmt.Errorf("search %q: %w", searches[i].query, err)
			}
			found[i] = append(found[i], page.Nodes...)
			left := searches[i].limit - len(found[i])
			// A page that adds nothing or doesn't move the cursor would otherwise repeat forever.
			moved := len(page.Nodes) > 0 && page.PageInfo.EndCursor != "" && page.PageInfo.EndCursor != p.after
			if page.PageInfo.HasNextPage && moved && left > 0 {
				next = append(next, pageRequest{search: i, first: min(100, left), after: page.PageInfo.EndCursor})
			}
		}
		pending = next
	}
	return login, found, nil
}

type ghResult struct {
	login           string
	prs             []PR
	reviews, others []PRLink
}

func fetchGitHub(ctx context.Context, cfg *Config) (ghResult, error) {
	login, found, err := runSearches(ctx, ghGraphQL, prSearches(cfg, time.Now()))
	if err != nil {
		return ghResult{}, err
	}
	return githubResult(cfg, login, found), nil
}

// githubResult keeps task PRs to the configured repos, named as the config names them, since sessions and
// worktrees are matched on that name.
func githubResult(cfg *Config, login string, found [][]ghPRNode) ghResult {
	res := ghResult{login: login}
	seen := map[prRef]bool{}
	for _, i := range []int{searchMineOpen, searchMineMerged, searchAssignedOpen, searchAssignedMerged} {
		assigned := i == searchAssignedOpen || i == searchAssignedMerged
		for _, n := range found[i] {
			repo, ok := cfg.repo(n.Repository.NameWithOwner)
			if !ok || seen[refOf(repo.Name, n.Number)] {
				continue
			}
			seen[refOf(repo.Name, n.Number)] = true
			pr := n.pr(cfg, repo.Name)
			if assigned {
				pr.Assigned = true
				pr.ReviewRequested = n.requests(login)
			}
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
