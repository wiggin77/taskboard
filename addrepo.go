package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const cloneTimeout = 30 * time.Minute

var (
	errNotListed        = errors.New("repo is not under Other repos")
	errManaged          = errors.New("repo is already in your repos")
	errCheckoutMismatch = errors.New("folder is not a checkout of the repo")
)

var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)

// addRepoPlan returns the repos entry and checkout folder for a repo listed under Other repos.
func addRepoPlan(cfg *Config, snap Snapshot, repo string) (RepoConfig, string, error) {
	listed := false
	for _, p := range snap.OtherPRs {
		if p.Repo == repo {
			listed = true
			break
		}
	}
	base := path.Base(repo)
	if !listed || !repoNameRE.MatchString(repo) || base == "." || base == ".." {
		return RepoConfig{}, "", errNotListed
	}
	if cfg.hasRepo(repo) {
		return RepoConfig{}, "", errManaged
	}
	return RepoConfig{Name: repo, Path: base}, filepath.Join(cfg.reposDir(), base), nil
}

// remoteMatches reports whether a git remote URL points to owner/repo.
func remoteMatches(url, repo string) bool {
	url = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(url), "/"), ".git")
	url = strings.ToLower(url)
	repo = strings.ToLower(repo)
	return strings.HasSuffix(url, "/"+repo) || strings.HasSuffix(url, ":"+repo)
}

// withRepo returns a copy of cfg with r added. It never changes cfg.
func withRepo(cfg *Config, r RepoConfig) (*Config, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	next, err := parseConfig(b)
	if err != nil {
		return nil, err
	}
	next.Repos = append(next.Repos, r)
	if err := next.validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// ensureCheckout makes dir a checkout of repo: it uses an existing folder whose origin matches, or clones
// into a hidden folder and renames it, so a failed clone never leaves a half-made folder under the final name.
func ensureCheckout(ctx context.Context, repo, dir string) (cloned bool, err error) {
	_, err = os.Lstat(dir)
	switch {
	case err == nil:
		out, err := runCmd(ctx, dir, "git", "remote", "get-url", "origin")
		if err != nil || !remoteMatches(string(out), repo) {
			return false, fmt.Errorf("%w: %s exists but is not a checkout of %s", errCheckoutMismatch, dir, repo)
		}
		return false, nil
	case !errors.Is(err, os.ErrNotExist):
		return false, err
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return false, err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".clone-*")
	if err != nil {
		return false, err
	}
	if _, err := runCmd(ctx, parent, "gh", "repo", "clone", repo, tmp); err != nil {
		os.RemoveAll(tmp)
		return false, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		return false, err
	}
	return true, nil
}
