package main

import (
	"context"
	"encoding/json"
	"strconv"
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
	State     string
	Draft     bool
	Review    string
	CI        CIState
	UpdatedAt time.Time
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
	State             string    `json:"state"`
	IsDraft           bool      `json:"isDraft"`
	ReviewDecision    string    `json:"reviewDecision"`
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

func listPRs(ctx context.Context, cfg *Config, repo, state string, limit int) ([]PR, error) {
	out, err := runCmd(ctx, "", "gh", "pr", "list",
		"--repo", repo,
		"--author", "@me",
		"--state", state,
		"--limit", strconv.Itoa(limit),
		"--json", "number,title,url,headRefName,state,isDraft,reviewDecision,statusCheckRollup,updatedAt")
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
			State:     r.State,
			Draft:     r.IsDraft,
			Review:    r.ReviewDecision,
			CI:        rollupCI(r.StatusCheckRollup),
			UpdatedAt: r.UpdatedAt,
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
