# GitHub organization filter

Status: implemented.

## Why

The Other repos section lists every open PR that you authored, reviewed or were asked to review in any repo that is not in `repos`. GitHub search returns PRs from all organizations you can see. Repos you do not follow fill the section.

**Goal:** a new setting, `github.organizations`, lists the organizations (or user accounts) whose repos may appear under Other repos. Repos in other organizations are hidden. An empty list shows all repos.

Example: `"github": {"organizations": ["mattermost", "mattermost-community"]}` shows PRs in `mattermost/*` and `mattermost-community/*`. It hides PRs in `someone-else/*`.

## Decisions

These decisions are final. No question remains open.

- **One list, include only.** There is no exclude list and no per-repo entry. A repo in an included organization always shows.
- **Scope: Other repos only.** Repos in `repos` are not affected, even when their organization is not in `github.organizations`. Their PRs stay in Reviews and in tasks.
- **Hidden means hidden.** The filter applies to every PR in Other repos. This includes a review that GitHub requested from you directly, and a PR that you authored.
- **Filter after the search.** `splitPRLinks` drops the PRs after `gh` returns them. The `gh` queries do not change.
  - GitHub cannot join `org:` and `repo:` qualifiers with OR. A query on both returns nothing. This was tested with `gh search prs`.
  - A query-level filter would need extra searches for the configured repos outside the listed organizations.
  - Known limit: each `gh` query still returns at most 100 PRs, and hidden PRs count toward that limit.
- **Match rule.** The owner is the text before the first `/` of `PRLink.Repo`. The comparison ignores case, as `hasRepo` does. A user account name works the same as an organization name.
- **Empty list means no filter.** A config file without the setting shows every repo.

## Design

**Config.** `Config` gets `GitHub GitHubConfig` with the tag `json:"github"`. `GitHubConfig` has `Organizations []string` with the tag `json:"organizations,omitempty"`.
- `validate` trims each entry.
- `validate` returns an error for an empty entry. The message names the index: `github.organizations[1] is empty`.
- `validate` returns an error for an entry with a `/` or a space. The message is `github.organizations[0]: "a/b" must be an organization or user name, not owner/repo`.
- The setting has no default. `validate` does not fill it in, and saving does not write it when empty.

**Filter.** A new method on `Config` decides if a repo shows under Other repos:

```go
func (c *Config) showsOther(repo string) bool
```

- It returns true when `GitHub.Organizations` is empty.
- Otherwise it returns true when the owner of `repo` equals an entry, ignoring case.

`splitPRLinks` in `github.go` calls it only in the branch for repos that are not in `repos`:

```go
case !cfg.hasRepo(l.Repo):
	if cfg.showsOther(l.Repo) {
		others = append(others, l)
	}
```

The `hasRepo` branch stays first. A configured repo therefore never goes through the filter.

**Applying a change.** The settings page posts the new config. `saveSettings` calls `Board.SetConfig`, which always requests a refresh. The refresh runs `listPRLinks` with the new config, so the section changes without a restart. No change to `board.go` is needed.

**Highlights.** A highlighted PR that the filter hides leaves the board. The existing prune of `taskboard-highlights.json` then drops its ID. This is expected.

## Changes

1. **`config.go`**
   - Add the `GitHubConfig` type, the `GitHub` field and its validation.
   - Add `showsOther`.
2. **`github.go`**
   - Change the `!cfg.hasRepo` branch of `splitPRLinks` as shown above.
   - Update the comment above `splitPRLinks` to say that Other repos honors `github.organizations`.
3. **`templates/settings.html`**
   - Add a fieldset `#orgs` after the Repos fieldset. Its legend is `Organizations for Other repos`.
   - Each organization is a row with the same classes as a repo row (`repo org`): one name input (`data-field="name"`) and the same remove button with the undo state.
   - An add button `#add-org` appends a row from `<template id="org-row">`.
   - The add, remove and undo handlers run for both fieldsets. A blank new row is dropped when you remove it. A saved row is struck through until you save.
   - The submit handler posts `github.organizations` as the non-empty names of the rows that are not removed.
   - Update the Repos hint: replace `PRs elsewhere go under Other repos` with `PRs elsewhere go under Other repos, limited by the organizations below`.
4. **`config.example.json`**
   - Add `"github": {"organizations": ["mattermost", "mattermost-community"]}` after `repos`.
5. **`README.md`**
   - In Notes, change the Other repos sentence. State that Other repos lists the same PRs for repos not in `repos`, limited to the organizations in `github.organizations` when that list is not empty.
   - Add a short paragraph with the `github.organizations` example, the empty-list rule, and the 100-result limit.
6. **`CLAUDE.md`**
   - In the Server paragraph (architecture item 6), add that `splitPRLinks` also applies `github.organizations` to the Other repos section.

## Tests

All tests go in `taskboard_test.go`, use `testConfig(t)`, and do not run `gh`.

1. **`TestSplitPRLinks` (extend or add a case set).** Use `cfg.Repos = {mattermost/mattermost}` and `cfg.GitHub.Organizations = {"mattermost-community", "Acme"}`.
   - A PR in `mattermost-community/x` shows in `others`.
   - A PR in `acme/y` shows in `others` (case-insensitive).
   - A PR in `other/repo` does not show in `others`, for each of the three roles.
   - An authored PR in `other/repo` does not show.
   - A review-requested PR in `mattermost/mattermost` still shows in `reviews`, although `mattermost` is not in `GitHub.Organizations`.
2. **Empty list.** With `GitHub.Organizations` empty, `others` has the same content as before this work.
3. **`TestShowsOther`.** Table test: empty list, match, case difference, a repo name without `/`, an owner that only starts with a listed name (`mattermost-x` against `mattermost`) which must not match.
4. **Config parsing.** Use the pattern near the `worktree_links` test.
   - A valid list parses and keeps its order.
   - Entries are trimmed.
   - `"a/b"`, `"a b"` and `""` each return an error that names `github.organizations`.
5. **Config round trip.** `SaveConfig` writes `github.organizations` when set and omits that key when empty. The empty `github` object is still written. A saved config parses to the same list.
6. **Settings page.** The rendered `/settings` HTML contains the `#orgs` fieldset, the `#add-org` button, and one row for each configured organization.

Run `go vet ./...` and `go test ./...` before the change is done.

## Out of scope

- An exclude list, or per-repo entries.
- Filtering Reviews, tasks or the repos in `repos`.
- Moving the filter into the `gh` queries.
- Raising the 100-result limit of the searches.
