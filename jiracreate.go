package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// createJiraIssue makes a ticket from a PR and puts its key at the start of the PR title, which is how the board joins them.
func createJiraIssue(ctx context.Context, cfg *Config, project string, pr PR) (string, error) {
	out, err := runCmd(ctx, "", "acli", "jira", "workitem", "create",
		"--project", project, "--type", "Task", "--summary", pr.Title, "--assignee", "@me", "--json")
	if err != nil {
		return "", err
	}
	key, err := parseCreatedKey(cfg, out)
	if err != nil {
		return "", err
	}
	if _, err := runCmd(ctx, "", "gh", "pr", "edit", strconv.Itoa(pr.Number), "--repo", pr.Repo,
		"--title", retitle(key, pr.Title)); err != nil {
		return key, fmt.Errorf("created %s but could not rename the PR: %w", key, err)
	}
	return key, nil
}

func parseCreatedKey(cfg *Config, out []byte) (string, error) {
	var raw struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(out, &raw) == nil && cfg.findKey(raw.Key) == raw.Key && raw.Key != "" {
		return raw.Key, nil
	}
	if key := cfg.findKey(string(out)); key != "" {
		return key, nil
	}
	return "", errors.New("acli did not return a ticket key")
}

func retitle(key, title string) string { return key + " " + title }
