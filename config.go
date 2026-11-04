package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	Listen           string       `json:"listen"`
	TLSListen        string       `json:"tls_listen,omitempty"`
	TLSDir           string       `json:"tls_dir,omitempty"`
	RevokedCerts     []string     `json:"revoked_certs,omitempty"`
	AllowedHosts     []string     `json:"allowed_hosts,omitempty"`
	PollInterval     Duration     `json:"poll_interval"`
	IdlePollInterval Duration     `json:"idle_poll_interval"`
	RefreshTimeout   Duration     `json:"refresh_timeout"`
	ProjectKeys      []string     `json:"project_keys"`
	MergedPRLimit    int          `json:"merged_pr_limit"`
	DoneDays         int          `json:"done_days"`
	AttachCommand    []string     `json:"attach_command,omitempty"`
	TmuxSession      string       `json:"tmux_session,omitempty"`
	AgentCommand     string       `json:"agent_command,omitempty"`
	AgentResume      string       `json:"agent_resume_command,omitempty"`
	WorktreeDir      string       `json:"worktree_dir,omitempty"`
	WorktreeLinks    []string     `json:"worktree_links,omitempty"`
	Jira             JiraConfig   `json:"jira"`
	Repos            []RepoConfig `json:"repos"`
	GitHub           GitHubConfig `json:"github"`

	keyRE *regexp.Regexp
}

// Defaults apply when a field is empty and are never written back, so the settings page shows each one as
// its field's placeholder rather than as a saved value.
const (
	defaultAgent       = "claude"
	defaultAgentResume = "claude --continue"
	defaultTmuxSession = "taskboard"
	defaultPlansDir    = "plans"
)

const defaultJQL = "assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC"

type JiraConfig struct {
	BaseURL string `json:"base_url"`
	JQL     string `json:"jql"`
}

type GitHubConfig struct {
	Organizations []string `json:"organizations,omitempty"`
}

type RepoConfig struct {
	Name       string `json:"name"`
	Path       string `json:"path,omitempty"`
	PlansDir   string `json:"plans_dir,omitempty"`
	BaseBranch string `json:"base_branch,omitempty"`
}

func (r RepoConfig) Dir() string { return expandHome(r.Path) }

func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}

func shortenHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
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

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d Duration) String() string {
	s := d.Duration.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := parseConfig(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func parseConfig(b []byte) (*Config, error) {
	cfg := &Config{
		Listen:         "127.0.0.1:8484",
		PollInterval:   Duration{2 * time.Minute},
		RefreshTimeout: Duration{5 * time.Minute},
		MergedPRLimit:  20,
		DoneDays:       30,
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// MarshalJSON leaves out the github block while github.organizations is empty, so saving never writes an unset setting.
func (c Config) MarshalJSON() ([]byte, error) {
	type plain Config
	out := struct {
		plain
		GitHub *GitHubConfig `json:"github,omitempty"`
	}{plain: plain(c)}
	if len(c.GitHub.Organizations) > 0 {
		out.GitHub = &c.GitHub
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func SaveConfig(path string, cfg *Config) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	return writeFileAtomic(path, buf.Bytes())
}

// writeFileAtomic writes via a temp file and rename so a crash never leaves a truncated file. It keeps an existing file's mode.
func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
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
		if r.BaseBranch != "" && !strings.Contains(strings.Trim(r.BaseBranch, "/"), "/") {
			return fmt.Errorf("repos[%d].base_branch must name a remote branch, like origin/main", i)
		}
	}
	for i, org := range c.GitHub.Organizations {
		org = strings.TrimSpace(org)
		switch {
		case org == "":
			return fmt.Errorf("github.organizations[%d] is empty", i)
		case strings.ContainsAny(org, "/ \t"):
			return fmt.Errorf("github.organizations[%d]: %q must be an organization or user name, not owner/repo", i, org)
		}
		c.GitHub.Organizations[i] = org
	}
	for i, serial := range c.RevokedCerts {
		c.RevokedCerts[i] = normalizeSerial(serial)
	}
	if strings.ContainsAny(c.TmuxSession, ":. ") {
		return errors.New("tmux_session cannot contain ':', '.' or spaces")
	}
	if c.WorktreeDir != "" && !filepath.IsAbs(expandHome(c.WorktreeDir)) {
		return errors.New("worktree_dir must be an absolute path or start with ~/")
	}
	for _, l := range c.WorktreeLinks {
		if !filepath.IsAbs(expandHome(l)) {
			return fmt.Errorf("worktree_links: %q must be an absolute path or start with ~/", l)
		}
	}
	if c.MergedPRLimit < 1 {
		return errors.New("merged_pr_limit must be at least 1")
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

func normalizeSerial(s string) string {
	return strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(s)))
}

// tlsDir resolves tls_dir, default "certs", relative to the config file's folder.
func (c *Config) tlsDir(configPath string) string {
	dir := expandHome(c.TLSDir)
	if dir == "" {
		dir = "certs"
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(configPath), dir)
	}
	return dir
}

func (c *Config) jql() string {
	if strings.TrimSpace(c.Jira.JQL) == "" {
		return defaultJQL
	}
	return c.Jira.JQL
}

func (c *Config) tmuxSession() string {
	if c.TmuxSession == "" {
		return defaultTmuxSession
	}
	return c.TmuxSession
}

// agent returns the command typed into a new window. An empty resume command falls back to the agent
// command when that was changed, since claude --continue would be wrong for another agent.
func (c *Config) agent(resume bool) string {
	switch {
	case resume && c.AgentResume != "":
		return c.AgentResume
	case resume && c.AgentCommand == "":
		return defaultAgentResume
	case c.AgentCommand == "":
		return defaultAgent
	}
	return c.AgentCommand
}

func (r RepoConfig) plansDir() string {
	if r.PlansDir == "" {
		return defaultPlansDir
	}
	return r.PlansDir
}

func (c *Config) repo(name string) (RepoConfig, bool) {
	for _, r := range c.Repos {
		if strings.EqualFold(r.Name, name) {
			return r, true
		}
	}
	return RepoConfig{}, false
}

// showsOther reports whether a repo outside repos may appear under Other repos. An empty github.organizations shows every repo.
func (c *Config) showsOther(repo string) bool {
	if len(c.GitHub.Organizations) == 0 {
		return true
	}
	owner, _, _ := strings.Cut(repo, "/")
	for _, org := range c.GitHub.Organizations {
		if strings.EqualFold(org, owner) {
			return true
		}
	}
	return false
}

func (c *Config) hasRepo(name string) bool {
	for _, r := range c.Repos {
		if strings.EqualFold(r.Name, name) {
			return true
		}
	}
	return false
}
