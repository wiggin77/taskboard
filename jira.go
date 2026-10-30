package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type JiraIssue struct {
	Key     string
	Summary string
	Status  string
	Done    bool
}

type JiraClient struct {
	baseURL string
	email   string
	token   string
	http    *http.Client
}

func NewJiraClient(cfg JiraConfig, token string) *JiraClient {
	return &JiraClient{
		baseURL: cfg.BaseURL,
		email:   cfg.Email,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
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

func (c *JiraClient) Search(ctx context.Context, jql string) ([]JiraIssue, error) {
	var all []JiraIssue
	pageToken := ""
	for {
		body := map[string]any{
			"jql":        jql,
			"fields":     []string{"summary", "status"},
			"maxResults": 100,
		}
		if pageToken != "" {
			body["nextPageToken"] = pageToken
		}
		var resp struct {
			Issues        []jiraIssueJSON `json:"issues"`
			NextPageToken string          `json:"nextPageToken"`
			IsLast        bool            `json:"isLast"`
		}
		if _, err := c.do(ctx, http.MethodPost, "/rest/api/3/search/jql", body, &resp); err != nil {
			return nil, err
		}
		for _, is := range resp.Issues {
			all = append(all, is.issue())
		}
		if resp.IsLast || resp.NextPageToken == "" {
			return all, nil
		}
		pageToken = resp.NextPageToken
	}
}

// Get returns ok=false when the issue does not exist or is not visible to the user.
func (c *JiraClient) Get(ctx context.Context, key string) (JiraIssue, bool, error) {
	var is jiraIssueJSON
	status, err := c.do(ctx, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(key)+"?fields=summary,status", nil, &is)
	if status == http.StatusNotFound {
		return JiraIssue{}, false, nil
	}
	if err != nil {
		return JiraIssue{}, false, err
	}
	return is.issue(), true, nil
}

func (c *JiraClient) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth(c.email, c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(snippet))
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
}
