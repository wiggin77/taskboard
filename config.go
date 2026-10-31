package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	Listen           string       `json:"listen"`
	AllowedHosts     []string     `json:"allowed_hosts"`
	PollInterval     Duration     `json:"poll_interval"`
	IdlePollInterval Duration     `json:"idle_poll_interval"`
	RefreshTimeout   Duration     `json:"refresh_timeout"`
	ProjectKeys      []string     `json:"project_keys"`
	MergedPRLimit    int          `json:"merged_pr_limit"`
	DoneDays         int          `json:"done_days"`
	SSHHost          string       `json:"ssh_host"`
	AttachCommand    []string     `json:"attach_command"`
	Jira             JiraConfig   `json:"jira"`
	Repos            []RepoConfig `json:"repos"`

	keyRE *regexp.Regexp
}

type JiraConfig struct {
	BaseURL string `json:"base_url"`
	JQL     string `json:"jql"`
}

type RepoConfig struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	PlansDir string `json:"plans_dir"`
}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Listen:         "127.0.0.1:8484",
		PollInterval:   Duration{2 * time.Minute},
		RefreshTimeout: Duration{5 * time.Minute},
		MergedPRLimit:  20,
		DoneDays:       30,
		Jira: JiraConfig{
			JQL: "assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC",
		},
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Jira.BaseURL == "" {
		return errors.New("jira.base_url is required")
	}
	c.Jira.BaseURL = strings.TrimRight(c.Jira.BaseURL, "/")
	if len(c.ProjectKeys) == 0 {
		return errors.New("project_keys must list at least one Jira project key")
	}
	if len(c.Repos) == 0 {
		return errors.New("repos must list at least one repository")
	}
	for i := range c.Repos {
		r := &c.Repos[i]
		if r.Name == "" {
			return fmt.Errorf("repos[%d].name is required", i)
		}
		if r.PlansDir == "" {
			r.PlansDir = "plans"
		}
	}
	if c.DoneDays < 1 {
		return errors.New("done_days must be at least 1")
	}
	if c.PollInterval.Duration < 10*time.Second {
		return errors.New("poll_interval must be at least 10s")
	}
	if c.RefreshTimeout.Duration < 10*time.Second {
		return errors.New("refresh_timeout must be at least 10s")
	}
	if idle := c.IdlePollInterval.Duration; idle != 0 && idle < c.PollInterval.Duration {
		return errors.New("idle_poll_interval must be 0 (no idle polling) or at least poll_interval")
	}

	quoted := make([]string, len(c.ProjectKeys))
	for i, k := range c.ProjectKeys {
		quoted[i] = regexp.QuoteMeta(k)
	}
	re, err := regexp.Compile(`(?i)(?:^|[^a-z0-9])((?:` + strings.Join(quoted, "|") + `)-[0-9]+)(?:[^0-9]|$)`)
	if err != nil {
		return err
	}
	c.keyRE = re
	return nil
}

func (c *Config) findKey(s string) string {
	m := c.keyRE.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

func (c *Config) issueURL(key string) string {
	return c.Jira.BaseURL + "/browse/" + key
}

func (c *Config) hasRepo(name string) bool {
	for _, r := range c.Repos {
		if strings.EqualFold(r.Name, name) {
			return true
		}
	}
	return false
}
