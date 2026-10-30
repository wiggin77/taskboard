package main

import (
	"context"
	"encoding/json"
	"strings"
)

type JiraIssue struct {
	Key     string
	Summary string
	Status  string
	Done    bool
}

type jiraIssueJSON struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
		Status  struct {
			Name           string `json:"name"`
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
	} `json:"fields"`
}

func (j jiraIssueJSON) issue() JiraIssue {
	return JiraIssue{
		Key:     j.Key,
		Summary: j.Fields.Summary,
		Status:  j.Fields.Status.Name,
		Done:    j.Fields.Status.StatusCategory.Key == "done",
	}
}

func jiraSearch(ctx context.Context, jql string) ([]JiraIssue, error) {
	out, err := runCmd(ctx, "", "acli", "jira", "workitem", "search",
		"--jql", jql, "--fields", "key,summary,status", "--paginate", "--json")
	if err != nil {
		return nil, err
	}
	return decodeIssues(out)
}

func decodeIssues(out []byte) ([]JiraIssue, error) {
	var raw []jiraIssueJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	issues := make([]JiraIssue, len(raw))
	for i, r := range raw {
		issues[i] = r.issue()
	}
	return issues, nil
}

// jiraGet returns ok=false when the issue does not exist or is not visible to the user.
func jiraGet(ctx context.Context, key string) (JiraIssue, bool, error) {
	out, err := runCmd(ctx, "", "acli", "jira", "workitem", "view", key,
		"--fields", "summary,status", "--json")
	if err != nil {
		if isNotFound(err) {
			return JiraIssue{}, false, nil
		}
		return JiraIssue{}, false, err
	}
	var raw jiraIssueJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return JiraIssue{}, false, err
	}
	return raw.issue(), true, nil
}

func isNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") || strings.Contains(msg, "404")
}
