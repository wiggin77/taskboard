package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	cloneTimeout = 30 * time.Minute
	forgeHost    = "github.com"
)

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

// remoteMatches reports whether a git remote URL points to owner/repo on github.com.
func remoteMatches(remote, repo string) bool {
	remote = strings.TrimSpace(remote)
	var host, p string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return false
		}
		host, p = u.Hostname(), u.Path
	} else {
		hostPart, rest, ok := strings.Cut(remote, ":")
		if !ok {
			return false
		}
		if _, h, found := strings.Cut(hostPart, "@"); found {
			hostPart = h
		}
		host, p = hostPart, rest
	}
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	return strings.EqualFold(host, forgeHost) && strings.EqualFold(p, repo)
}

// staleClone reports whether a folder is a leftover temp clone of base. A live clone stops after cloneTimeout
// and its folder is never older than the clone, so an older folder belongs to a clone that was killed.
func staleClone(name, base string, modified, now time.Time) bool {
	return strings.HasPrefix(name, "."+base+".clone-") && now.Sub(modified) > cloneTimeout
}

// sweepClones removes leftover temp clone folders of base in parent.
func sweepClones(parent, base string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if fi, err := e.Info(); err == nil && staleClone(e.Name(), base, fi.ModTime(), now) {
			os.RemoveAll(filepath.Join(parent, e.Name()))
		}
	}
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
	sweepClones(parent, filepath.Base(dir))
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".clone-*")
	if err != nil {
		return false, err
	}
	// MkdirTemp makes the folder 0700, and git clone keeps an existing folder's mode.
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.RemoveAll(tmp)
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
