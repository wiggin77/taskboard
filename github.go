package main

import (
	"context"
	"encoding/json"
	"path"
	"sort"
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
	Repo      string
	Key       string
	Number    int
	Title     string
	URL       string
	Branch    string
	HeadSHA   string
	State     string
	Draft     bool
	Review    string
	CI        CIState
	UpdatedAt time.Time
	MergedAt  time.Time
}

type ghCheck struct {
	TypeName   string `json:"__typename"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

type ghPR struct {
	Number            int       `json:"number"`
	Title             string    `json:"title"`
	URL               string    `json:"url"`
	HeadRefName       string    `json:"headRefName"`
	HeadRefOid        string    `json:"headRefOid"`
	State             string    `json:"state"`
	IsDraft           bool      `json:"isDraft"`
	ReviewDecision    string    `json:"reviewDecision"`
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"`
	UpdatedAt         time.Time `json:"updatedAt"`
	MergedAt          time.Time `json:"mergedAt"`
}

func listPRs(ctx context.Context, cfg *Config, repo, state string, limit int, search string) ([]PR, error) {
	out, err := runCmd(ctx, "", "gh", "pr", "list",
		"--repo", repo,
		"--author", "@me",
		"--state", state,
		"--limit", strconv.Itoa(limit),
		"--search", search,
		"--json", "number,title,url,headRefName,headRefOid,state,isDraft,reviewDecision,statusCheckRollup,updatedAt,mergedAt")
	if err != nil {
		return nil, err
	}
	var raw []ghPR
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	prs := make([]PR, 0, len(raw))
	for _, r := range raw {
		key := cfg.findKey(r.HeadRefName)
		if key == "" {
			key = cfg.findKey(r.Title)
		}
		prs = append(prs, PR{
			Repo:      repo,
			Key:       key,
			Number:    r.Number,
			Title:     r.Title,
			URL:       r.URL,
			Branch:    r.HeadRefName,
			HeadSHA:   r.HeadRefOid,
			State:     r.State,
			Draft:     r.IsDraft,
			Review:    r.ReviewDecision,
			CI:        rollupCI(r.StatusCheckRollup),
			UpdatedAt: r.UpdatedAt,
			MergedAt:  r.MergedAt,
		})
	}
	return prs, nil
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

	ReviewName string
	Review     *Review
}

func (p PRLink) RepoName() string { return path.Base(p.Repo) }

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
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func viewPR(ctx context.Context, link *PRLink) error {
	out, err := runCmd(ctx, "", "gh", "pr", "view", strconv.Itoa(link.Number), "--repo", link.Repo, "--json", "title,url,state,author,updatedAt")
	if err != nil {
		return err
	}
	var v ghViewPR
	if err := json.Unmarshal(out, &v); err != nil {
		return err
	}
	link.Title, link.URL, link.State, link.Author, link.UpdatedAt = v.Title, v.URL, v.State, v.Author.Login, v.UpdatedAt
	return nil
}

type ghSearchPR struct {
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	IsDraft   bool      `json:"isDraft"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func listPRLinks(ctx context.Context, cfg *Config) (reviews, others []PRLink, err error) {
	var found [3][]ghSearchPR
	// GitHub drops a review request once you submit a review, so reviewed-by covers PRs you have already looked at.
	queries := [3][]string{
		RoleRequested: {"review-requested:@me"},
		RoleAuthor:    {"author:@me"},
		RoleReviewed:  {"reviewed-by:@me", "-author:@me"},
	}
	for role, q := range queries {
		if found[role], err = searchPRs(ctx, q...); err != nil {
			return nil, nil, err
		}
	}
	reviews, others = splitPRLinks(cfg, mergePRLinks(found))
	return reviews, others, nil
}

func searchPRs(ctx context.Context, query ...string) ([]ghSearchPR, error) {
	args := []string{"search", "prs", "--state", "open", "--limit", "100",
		"--json", "repository,number,title,url,author,isDraft,updatedAt", "--"}
	out, err := runCmd(ctx, "", "gh", append(args, query...)...)
	if err != nil {
		return nil, err
	}
	var raw []ghSearchPR
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func mergePRLinks(found [3][]ghSearchPR) []PRLink {
	seen := map[string]bool{}
	var links []PRLink
	for role, prs := range found {
		for _, r := range prs {
			if seen[r.URL] {
				continue
			}
			seen[r.URL] = true
			links = append(links, PRLink{
				Repo:      r.Repository.NameWithOwner,
				Number:    r.Number,
				Title:     r.Title,
				URL:       r.URL,
				Author:    r.Author.Login,
				Draft:     r.IsDraft,
				Role:      Role(role),
				UpdatedAt: r.UpdatedAt,
			})
		}
	}
	sort.SliceStable(links, func(i, j int) bool {
		if links[i].Role != links[j].Role {
			return links[i].Role < links[j].Role
		}
		return links[i].UpdatedAt.After(links[j].UpdatedAt)
	})
	return links
}

// Authored PRs in configured repos are left out because they already appear as tasks.
func splitPRLinks(cfg *Config, links []PRLink) (reviews, others []PRLink) {
	for _, l := range links {
		switch {
		case !cfg.hasRepo(l.Repo):
			others = append(others, l)
		case l.Role != RoleAuthor:
			reviews = append(reviews, l)
		}
	}
	return reviews, others
}
