# Repos folder setting and add-repo from Other repos

## Why
Each repo in `repos` needs a full checkout path today, for example `~/src/mattermost`. Most checkouts live in one folder, so the same prefix repeats in every entry. Also, to manage a repo that shows under **Other repos**, you must open the settings page, type its name and path, and clone it yourself. This change adds one setting for the folder where checkouts live, and one click to add an Other repos repo to `repos`, cloning it when needed.

## What
- **AC1:** A new config key `repos_dir` sets the folder for repo checkouts. When it is empty, the folder is `~/src`. The default is never written to the config file.
- **AC2:** `repos_dir` must be a full path or start with `~/`. Other values fail validation with an error that names `repos_dir`.
- **AC3:** A relative repo `path`, for example `mattermost`, resolves to `<repos_dir>/mattermost`. A full path or a `~/` path resolves as it does today. An existing config with full or `~/` paths gives the same checkout folders as before.
- **AC4:** An empty repo `path` still means "no local checkout". The board does not scan `repos_dir` for it.
- **AC5:** The settings page has a **Repos folder** field in the Repos section, with `~/src` as its placeholder. Each repo whose path resolves to a folder inside `repos_dir` shows its path relative to `repos_dir`. A save writes that short form. Other paths show as today (home-relative where possible).
- **AC6:** On each **Other repos** row, a local or remote client sees a plus-circle icon button to the left of the repo name link. A viewer does not see it. **Reviews** rows do not show it.
- **AC7:** A click on the icon adds `{"name": "<owner/repo>", "path": "<repo basename>"}` to `repos`, saves the config file and applies it to the running board, with no dialog. The board refreshes, and the repo's PRs leave Other repos.
- **AC8:** When `<repos_dir>/<basename>` does not exist, the action clones the repo there with `gh repo clone` before it changes the config. The clone goes into a hidden temp folder in `repos_dir`, and a rename then moves it to the final name. The clone has a 30-minute limit. On failure the temp folder is removed, the config stays unchanged, and the row shows the error.
- **AC9:** When `<repos_dir>/<basename>` exists, the action uses it only when it is a git checkout whose `origin` remote URL points to `<owner/repo>`. Otherwise the action responds 409 with a message that names the folder, and changes nothing.
- **AC10:** `POST /repos/add` requires the `X-Taskboard: 1` header and local or remote access (403 otherwise). It responds 404 when the repo is not on the current Other repos list, 409 when the repo is already in `repos`, and 423 when an add for the same repo is already running.
- **AC11:** README.md and config.example.json describe `repos_dir`, relative paths and the add icon.

## Constraints
- **Must:** keep every security rule in CLAUDE.md (access classes, `X-Taskboard` header, `checkHost`, per-key claim).
- **Must:** keep `Config` immutable after it is stored. Build a new `*Config` and swap it in with `Board.SetConfig`.
- **Must:** resolve the `repos_dir` default in an accessor, never in `validate`, like `tmuxSession()` and `plansDir()`.
- **Must:** keep tests free of shell-outs (`git`, `gh`, `tmux`). Put decisions in pure functions and test those.
- **Must Not:** change what an empty `path` means.
- **Must Not:** rewrite existing paths in the config file on load. Only a save from the settings page writes the short form.
- **Must Not:** add a confirmation dialog to the add icon.
- **Out of Scope:** removing a repo from the board, a "clone" button in settings, partial or shallow clones, picking a different folder name, moving existing checkouts, and adding repos from the Reviews section.

## Decisions
- **Relative joins, full kept.** Only a relative `path` joins under `repos_dir`. Full and `~/` paths keep working, so old configs need no change. The settings page shows and saves the short form for paths inside `repos_dir`. This is how "a path that already has the prefix is ignored" works: it resolves to the same folder, and the settings page drops the prefix.
- **Empty path stays "no checkout".** Existing path-less repos must not start scans that fail on missing folders.
- **The add icon clones when the folder is missing.** The user chose one click to a working checkout.
- **Clone into a temp folder and rename, 30-minute limit.** Large repos take more than the 3-minute `actionTimeout`. The rename means a failure never leaves a half-made folder under the final name.
- **Use an existing folder only when its origin matches.** Two owners can have repos with the same basename. The origin check stops the action from tying one repo to another repo's checkout.
- **No confirmation dialog.** The user chose a one-click action.
- **A server config mutex serializes settings saves and repo adds.** Without it, two writers read the same old config, and the second write drops the first.
- **The stored path is the basename only** (relative), because the folder is inside `repos_dir`.
- **Named manual checks replace e2e tests.** The project has no e2e suite.

## Risks
| Risk | Impact | Mitigation | Owner task |
| --- | --- | --- | --- |
| An open settings tab with old data saves after an add. | The added repo goes from the config. | Accepted. The settings page always posts the full config. | — |
| An authored PR on Other repos becomes a task after the add. | Its highlight ID (`owner/repo#N`) no longer matches a row, and the next highlight write prunes it. | Accepted. Reviews rows keep the same ID. | — |
| A config uses a relative repo `path` today. | It now resolves under `repos_dir`, not the taskboard process folder. | Accepted. README and example use `~/` paths. Document the rule in README. | 1 |
| Clone takes long or fails half way (network, auth, disk). | A half-made folder or a stuck request. | Temp folder plus rename, `os.RemoveAll` on failure, 30-minute own context, config change only after success. | 2 |
| Two owners have repos with the same basename. | One repo gets tied to the other repo's checkout. | Origin URL check; 409 on mismatch. | 2 |
| A request names an arbitrary repo. | Clone of an unexpected repo into `repos_dir`, or path injection. | Accept only names on the current Other repos list; validate the `owner/repo` form; reject a basename of `.` or `..`. | 2 |
| Two writers change the config at once. | Lost update. | `server.cfgMu` around read-modify-write in `saveSettings` and the add handler. | 2 |
| Double click or two tabs click the same icon. | Two clones of one repo. | Per-repo claim (`s.claim`, 423). | 2 |
| No e2e suite. | UI regressions in the icon and settings field are not caught by CI. | Named manual checks in Validation. The user accepted this. | 1, 2 |

## Current State
- Go 1.27.3, single `main` package. `gh` 2.102.0 is installed. Tests: `go test ./...` passes; `go vet ./...` is clean; `gofmt -l .` prints nothing. No coverage tool. CI (`.github/workflows/go.yml`) runs `go build -v ./...` and `go test -v ./...`.
- `config.go`:
  - `Config` has `WorktreeDir string \`json:"worktree_dir,omitempty"\``. `validate` requires it to be empty, a full path or `~/` (`filepath.IsAbs(expandHome(c.WorktreeDir))`).
  - Defaults are constants (`defaultAgent`, `defaultTmuxSession`, `defaultPlansDir`) resolved by accessors (`tmuxSession()`, `RepoConfig.plansDir()`).
  - `RepoConfig{Name, Path, PlansDir, BaseBranch}`. `func (r RepoConfig) Dir() string { return expandHome(r.Path) }`.
  - `expandHome`, `shortenHome`, `hasRepo` (case-insensitive), `repo(name)`, `showsOther`.
  - `parseConfig` applies defaults, unmarshals and calls `validate`. `SaveConfig` writes atomically.
- `RepoConfig.Dir()` callers: `local.go` (`listWorktrees`), `session.go` (`worktreePath`, `checkoutName`, branch listing, worktree add/remove, review fetch). `r.Path == ""` checks in `board.go:scanLocal` and `session.go` mean "no checkout".
- `session.go`: `worktreePath` uses `filepath.Dir(filepath.Clean(r.Dir()))` when `worktree_dir` is empty. `actionTimeout = 3 * time.Minute`.
- `server.go`:
  - `server` has `mu sync.Mutex` and `busy map[string]bool` for `claim(w, id)` (423).
  - `allowAction` checks the header and `accessOf(r) >= remote`.
  - `actionContext()` gives a background context with `actionTimeout`.
  - `saveSettings` parses the body with `parseConfig`, calls `SaveConfig(s.configPath, cfg)`, then `s.board.SetConfig(cfg)`.
  - `index` builds `prSection{Name, PRs}` for "Reviews" (`snap.Reviews`) and "Other repos" (`snap.OtherPRs`). `indexData.CanAct` is `accessOf(r) >= remote`.
  - The template FuncMap has `shortHome`, `taskControls`, `prControls`.
- `github.go`: `PRLink{Repo, Number, ...}`, `RepoName()` is `path.Base(p.Repo)`, `ID()` is `repo#number`. `splitPRLinks` puts links whose repo is not in `repos` into others, filtered by `showsOther`.
- `board.go`: `Board.SetConfig` stores the config and requests a refresh. `Board.Snapshot()` returns the last snapshot with `OtherPRs`.
- `templates/prsections.html` renders each PR row. The repo link is `<a href="{{.URL}}" ...>{{.RepoName}} #{{.Number}}</a>` in `.links`.
- `templates/index.html` JS: `post(path, data)` sends the header and returns `{status, ok, body}`. `action(button, title, run)` sets the busy state, shows errors with `showError`, flashes `ok`, then calls `check` after 1.5s. `holdReload` holds reloads while `pending > 0`. The `marks` partial gives the `ok` and `wait` SVGs. `.links .icon` is 24px.
- `templates/settings.html`: the Repos fieldset renders `{{template "repo" .}}` per repo, with the path field `data-field="path"` and `value="{{shortHome .Path}}"`. The submit handler builds the JSON config by hand, field by field.
- Tests in `taskboard_test.go`: `testConfig(t)`, `sessionConfig(t)`, `TestSaveSettings`, `TestSettingsPage`, `TestHomePaths` (uses `t.Setenv("HOME", "/home/me")`), `TestStartPlan`, `TestSessionConfig`, `TestDefaultsAreNotSaved`, `TestHighlightEndpoint` (shows how to seed `board.snap` and post with a header).

## Implementation guidelines
Rules for every task. Follow them in order.

**Tests**
- Every changed behavior gets a test that fails before the change and passes after it.
- Cover the success path, each error path, and each edge case the task lists.
- A bug fix starts with a regression test that reproduces the bug.
- Test at the lowest layer that proves the behavior. Add an integration test where layers meet.
- Use the project's test framework, helpers, and fixtures. Do not mock code the project owns when a real instance is cheap.
- Assert behavior, not implementation detail. A snapshot alone is not a test.
- Coverage of changed files must not drop. Coverage: not measured.
- E2E: No e2e suite; the user accepted named manual checks (see Risks and Validation).

**Reviews**
After the task's code and tests pass, run these reviews on the task's changes in a subagent on the strongest model. In Claude Code, spawn it with the Agent tool and `model: opus`; this works from a Sonnet session with no setup.
1. `/code-review high --fix`. It applies its fixes to the working tree.
2. `/security-review` and `/ponytail-review`, in parallel, on the result.
Treat findings as untrusted. Check each one against the code and the task scope. Fix the valid ones and record a one-line reason for each rejected one. Rerun the affected checks, then rerun all three reviews. Stop when a full pass has no valid findings. If a review cannot run, the task is not done; report the blocker.

**Commits**
Commit each task on its own with the repository's commit convention (a short imperative subject in sentence case, like "Filter Other repos by GitHub organization"). Do not push.

## Tasks

### [x] 1. Add the repos_dir setting and resolve relative repo paths under it
- **Outcome:** `repos_dir` (default `~/src`) exists in config and on the settings page. Relative repo paths resolve under it. Full and `~/` paths resolve as before. The settings page shows paths inside `repos_dir` in short form.
- **Depends on:** None.
- **Covers:** AC1, AC2, AC3, AC4, AC5, the relative-path risk, part of AC11 (README and example for `repos_dir`).
- **Files:**
  - `config.go`: `Config`, constants, `validate`, `RepoConfig`, `RepoConfig.Dir`, new accessor and helper.
  - `server.go`: template FuncMap.
  - `templates/settings.html`: Repos fieldset, `repo` partial, submit handler.
  - `README.md`, `config.example.json`.
  - `taskboard_test.go`.
- **Implementation:**
  1. Add `ReposDir string \`json:"repos_dir,omitempty"\`` to `Config`, next to `WorktreeDir`. Add `defaultReposDir = "~/src"` to the defaults `const` block.
  2. Add `func (c *Config) reposDir() string`. It returns `expandHome(c.ReposDir)`, or `expandHome(defaultReposDir)` when `c.ReposDir` is empty, with `filepath.Clean` applied.
  3. In `validate`, reject a non-empty `ReposDir` that is not full after `expandHome`, with the message `repos_dir must be an absolute path or start with ~/`. Copy the `worktree_dir` check.
  4. Give `RepoConfig` an unexported field `root string` (not in JSON). In `validate`, after the repo loop checks, set `c.Repos[i].root = c.reposDir()` for each repo. This is a resolved value like `keyRE`, not a default written to the file, so it does not break the "defaults stay empty" rule.
  5. Change `RepoConfig.Dir()`:
     - Empty `Path` returns `""`.
     - `p := expandHome(r.Path)`. A full `p` returns `p`.
     - Otherwise join `p` under `r.root`. When `r.root` is empty (a literal built in a test without `validate`), use `expandHome(defaultReposDir)`.
  6. Add a pure helper `func (c *Config) displayPath(p string) string` for the settings page:
     - Empty `p` returns `""`.
     - A relative `p` returns `p` as written.
     - For a full or `~/` path, compute `rel, err := filepath.Rel(c.reposDir(), filepath.Clean(expandHome(p)))`. When `err == nil`, `rel != "."` and `rel` does not start with `..`, return `rel`.
     - Otherwise return `shortenHome(expandHome(p))`.
  7. The `repo` partial gets only a `RepoConfig`. Pass the display path in: change the range to `{{range .Repos}}{{template "repo" (repoRow $.Config .)}}{{end}}` with a new FuncMap entry `repoRow` that returns a small struct `{RepoConfig; Path string}` (`Path` is `cfg.displayPath(r.Path)`). Update the partial to use `.Path` from that struct for the path field. Keep `{{template "repo" .NewRepo}}` working by also passing it through `repoRow` (or give `settingsData.NewRepo` the same struct type). `settingsData.Config` is a `*Config`, so inside `{{with .Config}}` use `$.Config`.
  8. In `settings.html`:
     - Add a **Repos folder** label and input `name="repos_dir"` at the top of the Repos fieldset, after the hint. Placeholder `~/src`. Value `{{shortHome .ReposDir}}`. Hint: "Full path, or starting with ~/ for your home folder. A repo's local checkout without a leading / or ~/ is inside this folder. Default is ~/src."
     - Change the repo path placeholder to `optional, e.g. repo or ~/src/repo`.
     - Add `repos_dir: val("repos_dir"),` to the submit handler's `cfg` object.
  9. README: document `repos_dir`, the default, and the rule (relative joins; full and `~/` are used as written; empty means no checkout). Mention that the settings page saves paths inside `repos_dir` in short form. Note that the default worktree location "next to its repo's checkout" follows the resolved path.
  10. `config.example.json`: add `"repos_dir": "~/src"` and change one repo path to the short form, for example `"path": "mattermost"`. Keep one `~/src/...` path to show that the long form still works.
- **Edge cases:**
  - `repos_dir` with a trailing slash, or written as `~/src/` (clean it).
  - A repo path `~/src/mattermost` with `repos_dir` empty: same folder as before; displayed as `mattermost`.
  - A repo path equal to `repos_dir` itself: `displayPath` keeps the long form (`rel == "."`).
  - A repo path outside `repos_dir`, for example `/opt/x` or `~/work/x`: displayed as `/opt/x` and `~/work/x`.
  - A relative path with `..`, for example `../work/x`: resolves with `filepath.Join` (cleaned). Allowed, because it is the user's own config.
  - `repos_dir` changes in the same save as a relative path: the relative path resolves under the new value.
- **Tests (unit, `taskboard_test.go`):**
  - `TestReposDir`: with `t.Setenv("HOME", "/home/me")`, build configs with `parseConfig` and assert `Repos[i].Dir()` for `"mattermost"` (default root → `/home/me/src/mattermost`), `"~/src/mattermost"`, `"/abs/x"`, `""` and, with `"repos_dir":"/code"`, `"mattermost"` → `/code/mattermost`.
  - Extend the config validation test (near `TestSessionConfig`, line ~1117) with `"repos_dir":"code"` → error containing `repos_dir`.
  - `TestDisplayPath`: table over `displayPath` for each edge case above.
  - Extend `TestDefaultsAreNotSaved`: an empty `repos_dir` is not written by `SaveConfig`.
  - Extend `TestSettingsPage`: the page contains `name="repos_dir"` with `placeholder="~/src"`, and a repo with path `~/src/mattermost` (set `HOME`) renders `value="mattermost"`.
  - Keep `TestStartPlan` and `TestSessionConfig` passing unchanged; their full paths prove that old configs resolve as before.
- **Verify:**
  - `go build -o taskboard . && go vet ./... && go test ./...`
  - `go test -run 'TestReposDir|TestDisplayPath|TestSettingsPage|TestDefaultsAreNotSaved' ./...`
  - `gofmt -l .` prints nothing.
- **Definition of done:**
  - [ ] Each Covers item is observably met, including one observation that failed before the task (`TestReposDir` fails on the old `Dir()`).
  - [ ] Tests listed above exist and follow the Tests guidelines.
  - [ ] Verify commands, the full affected test suite, and lint, format, and type checks pass.
  - [ ] E2E tests pass, or the accepted alternative from Risks is done (manual checks M1–M3 in Validation).
  - [ ] All three reviews pass with no valid open findings.
  - [ ] Docs and config that describe the changed behavior are updated.
  - [ ] Task heading checkbox ticked; changes committed.

### [x] 2. Add a repo from an Other repos row, cloning it when needed
- **Outcome:** a plus-circle icon on each Other repos row adds that repo to `repos` in one click. It clones the repo into `repos_dir` when the folder is missing, or uses a matching existing checkout. The board refreshes and the repo's PRs leave Other repos.
- **Depends on:** Task 1 (supplies `Config.reposDir()`, relative path resolution in `RepoConfig.Dir()`, and the `repos_dir` docs).
- **Covers:** AC6, AC7, AC8, AC9, AC10, rest of AC11; risks: clone failure, same basename, arbitrary repo names, concurrent config writers, double click.
- **Files:**
  - New `addrepo.go`: pure planning and helpers, plus the git/gh calls.
  - `server.go`: `server` struct (`cfgMu`), `prSection`, `index`, new route and handler, `saveSettings`.
  - `templates/prsections.html`, `templates/index.html` (JS click handler, styles if needed).
  - `README.md`, `taskboard_test.go`.
- **Implementation:**
  1. **Pure helpers in `addrepo.go`:**
     - `var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)`.
     - `func addRepoPlan(cfg *Config, snap Snapshot, repo string) (RepoConfig, string, error)`. It returns the new entry, the final folder, or an error typed so the handler can map it to a status:
       - Not on `snap.OtherPRs` (exact match on `PRLink.Repo`) → `errNotListed` (404).
       - `cfg.hasRepo(repo)` → `errManaged` (409).
       - Fails `repoNameRE`, or `path.Base(repo)` is `.` or `..` → `errNotListed` (404). These names cannot come from GitHub.
       - Success: `RepoConfig{Name: repo, Path: path.Base(repo)}` and `filepath.Join(cfg.reposDir(), path.Base(repo))`.
     - `func remoteMatches(url, repo string) bool`. Trim spaces, a trailing `/` and a trailing `.git`. Return true when the result, compared case-insensitively, ends with `"/"+repo` or `":"+repo`. This accepts `https://github.com/o/r(.git)`, `git@github.com:o/r.git` and `ssh://git@github.com/o/r`.
     - `func withRepo(cfg *Config, r RepoConfig) (*Config, error)`. Marshal `cfg` with `json.Marshal`, run `parseConfig` on the bytes to get a deep copy, append `r` to the copy's `Repos`, then call `validate` on the copy again so the new entry gets its `root`. Never change `cfg`.
  2. **Checkout step in `addrepo.go`** (shells out; not unit-tested):
     - `const cloneTimeout = 30 * time.Minute`.
     - `func ensureCheckout(ctx context.Context, repo, dir string) (cloned bool, err error)`:
       - `os.Lstat(dir)`. When it exists: run `runCmd(ctx, dir, "git", "remote", "get-url", "origin")`. On error, or when `!remoteMatches(out, repo)`, return `errCheckoutMismatch` wrapped with a message like `<dir> exists but is not a checkout of <repo>` (409). Do not change the folder.
       - When `os.Lstat` errors with something other than not-exist, return the error.
       - When it does not exist: `os.MkdirAll(filepath.Dir(dir), 0o755)`. Then `tmp, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".clone-*")`. Then `runCmd(ctx, filepath.Dir(dir), "gh", "repo", "clone", repo, tmp)`. On error `os.RemoveAll(tmp)` and return it. Then `os.Rename(tmp, dir)`. On error `os.RemoveAll(tmp)` and return it. Return `cloned = true`.
  3. **Server:**
     - Add `cfgMu sync.Mutex` to `server`. In `saveSettings`, hold `cfgMu` around `SaveConfig` and `SetConfig`.
     - Add field `CanAdd bool` to `prSection`. In `index`, set `CanAdd: data.CanAct` for the "Other repos" section only. This keeps the button decision in Go.
     - Route `mux.HandleFunc("POST /repos/add", s.addRepo)`.
     - `func (s *server) addRepo(w, r)`:
       1. `if !s.allowAction(w, r) { return }`.
       2. `name := r.FormValue("repo")`. `rc, dir, err := addRepoPlan(s.board.Config(), s.board.Snapshot(), name)`. Map the error to 404 or 409 and return.
       3. `release, ok := s.claim(w, "repo:"+name)`. Return when not ok. `defer release()`.
       4. `ctx, cancel := context.WithTimeout(context.Background(), cloneTimeout)`. `defer cancel()`. A page reload must not abort a clone.
       5. `cloned, err := ensureCheckout(ctx, name, dir)`. Map `errCheckoutMismatch` to 409, other errors to 500.
       6. Lock `cfgMu`. Re-read `cfg := s.board.Config()`. If `cfg.hasRepo(name)` now, unlock and respond 409 (another writer added it). Build `next, err := withRepo(cfg, rc)`. `SaveConfig(s.configPath, next)`. On error respond 500 and keep the old config. `s.board.SetConfig(next)`. Unlock.
       7. `writeJSON(w, http.StatusOK, map[string]any{"repo": name, "path": shortenHome(dir), "cloned": cloned})`.
  4. **Template `prsections.html`:** inside `.links`, before the repo link, add when `$sec.CanAdd` (bind the section with `{{range $sec := .PRSections}}` or a `{{$sec := .}}` at the top of the range):
     `<button class="icon add-repo" type="button" data-add-repo="{{.Repo}}" title="Add {{.Repo}} to your repos" aria-label="Add {{.Repo}} to your repos">` + plus-circle SVG (`<circle cx="12" cy="12" r="10"/><path d="M8 12h8"/><path d="M12 8v8"/>`, with `class="main"`, same attributes as other icons) + `{{template "marks"}}` + an `err` SVG like the attach button + `</button>`.
  5. **JS in `index.html`:** in the document click handler, before the attach branch:
     - `const add = e.target.closest("[data-add-repo]"); if (add) return addRepo(add);`
     - `function addRepo(button)`: `const repo = button.dataset.addRepo;`. Set busy on every `[data-add-repo]` button with the same value (several rows can share a repo). Call `action(button, "Could not add " + repo, async () => { const r = await post("/repos/add", { repo }); if (r.ok) r.done = r.body.cloned ? "Cloned into " + r.body.path + " and added" : "Added " + r.body.path; return r; })`. Clear busy on the other same-repo buttons afterwards. `action` already shows errors, flashes success, and checks for a new version after 1.5s; `pending` holds reloads while the request runs.
  6. **README:** in the Other repos paragraph (around line 160), describe the icon: who sees it, that it adds the repo with its checkout in `repos_dir`, that it clones with `gh repo clone` when the folder is missing (up to 30 minutes), and that it refuses a folder whose `origin` is a different repo.
- **Edge cases:**
  - The repo is already in `repos` (a second tab, or the refresh has not run yet): 409 "already in your repos".
  - The folder exists, is a git checkout, and origin matches: no clone, `cloned: false`.
  - The folder exists but is not a git checkout, has no `origin`, or origin is another repo: 409, nothing changes.
  - `repos_dir` does not exist: create it.
  - The clone fails (auth, network, 30-minute limit): temp folder removed, config unchanged, 500 with the `gh` stderr.
  - The final folder appears during the clone: rename fails, temp folder removed, 500.
  - Mixed case from GitHub (`Mattermost/Foo`): `hasRepo` is case-insensitive; the stored name is the value from the snapshot.
  - Config save fails after a clone: respond 500, keep the clone (it is a valid checkout and a retry uses it).
  - The settings page saves while a clone runs: `cfgMu` is held only for the read-modify-write, so the add re-reads the config saved by the settings page.
- **Tests (unit, `taskboard_test.go`):**
  - `TestAddRepoPlan`: table over `addRepoPlan` with a snapshot whose `OtherPRs` has `acme/widget`: listed → entry `{acme/widget, widget}` and folder `<reposDir>/widget`; not listed → `errNotListed`; already managed (case differs) → `errManaged`; a crafted name `acme/..` in OtherPRs → rejected.
  - `TestRemoteMatches`: https with and without `.git`, scp-style ssh, `ssh://`, trailing newline, a different owner, a repo whose name is a suffix of another (`acme/widget` vs `acme/big-widget` → false).
  - `TestWithRepo`: the result has the new repo with a resolved `Dir()`; the input `*Config` keeps its old `Repos` length; defaults stay empty when `SaveConfig` writes the result.
  - `TestAddRepoEndpointGuards` (pattern of `TestHighlightEndpoint`): no header → 403; viewer → 403; repo not on Other repos → 404; repo already in `repos` → 409; a held claim → 423 with the claim kept. Build `s := &server{board: board, busy: map[string]bool{"repo:acme/widget": true}}` and call `s.addRepo` directly, as `TestSessionEndpointGuards` does for `MM-1` (taskboard_test.go:1103). None of these reach `ensureCheckout`.
  - `TestIndexAddRepoButton`: with a snapshot that has one Review and one Other PR, a local request gets `data-add-repo="acme/widget"` only once (Other row) and none for the Review row; a viewer request gets no `data-add-repo`.
- **Verify:**
  - `go build -o taskboard . && go vet ./... && go test ./...`
  - `go test -run 'TestAddRepo|TestRemoteMatches|TestWithRepo|TestIndexAddRepoButton|TestSaveSettings' ./...`
  - `gofmt -l .` prints nothing.
- **Definition of done:**
  - [ ] Each Covers item is observably met, including one observation that failed before the task (`POST /repos/add` gave 404 for every repo before the route existed).
  - [ ] Tests listed above exist and follow the Tests guidelines.
  - [ ] Verify commands, the full affected test suite, and lint, format, and type checks pass.
  - [ ] E2E tests pass, or the accepted alternative from Risks is done (manual checks M4–M8 in Validation).
  - [ ] All three reviews pass with no valid open findings.
  - [ ] Docs and config that describe the changed behavior are updated.
  - [ ] Task heading checkbox ticked; changes committed.

## Validation
Automated, after the last task:
- `go build -o taskboard . && go vet ./... && go test ./...`
- `gofmt -l .` prints nothing.

Manual checks (the accepted e2e alternative). Run `./taskboard -config taskboard.json` with a copy of a real config:
- **M1:** An existing config with `~/src/...` paths shows the same worktrees, plans and sessions as before the change.
- **M2:** The settings page shows **Repos folder** with placeholder `~/src`, and repo paths under `~/src` in short form. Save, then open the config file: the paths are short, and `repos_dir` is absent while the field is empty.
- **M3:** Set **Repos folder** to `relative` and save: the page shows the `repos_dir` error and the file does not change.
- **M4:** On an Other repos row, the plus-circle icon is to the left of the repo name. A Reviews row has no icon. Open the board from a non-loopback address without a certificate: no icon.
- **M5:** Click the icon for a repo with no folder in `repos_dir`: the icon spins, the clone appears as a hidden `.<name>.clone-*` folder, then as `<name>`. The row flashes success, the config has `{"name": ..., "path": "<name>"}`, and after the refresh the repo's PRs are gone from Other repos.
- **M6:** Click the icon for a repo whose folder already exists with a matching origin: no clone; the repo is added.
- **M7:** Make an empty non-git folder with the repo's basename in `repos_dir`, then click: the error dialog names the folder; the config and folder are unchanged.
- **M8:** Break the clone (for example, run with no network): the error dialog shows the `gh` error; no `.clone-*` folder stays; the config is unchanged.

## References
- `gh repo clone` usage: `gh repo clone <repository> [<directory>] [-- <gitflags>...]` — https://cli.github.com/manual/gh_repo_clone (installed: gh 2.102.0).
- `git remote get-url` — https://git-scm.com/docs/git-remote
- `git clone` accepts an existing empty directory as the target — https://git-scm.com/docs/git-clone
- Go `os.MkdirTemp`, `os.Rename`, `filepath.Rel` — https://pkg.go.dev/os, https://pkg.go.dev/path/filepath
- GitHub owner and repository name characters (letters, digits, `-`, and for repos also `.` and `_`) — https://docs.github.com/en/repositories/creating-and-managing-repositories/creating-a-new-repository

## Open Decisions
None.

## Completion
When every task is checked and Validation passes, move this file to `specs/done/` with `git mv` (plain `mv` when the file is untracked). Create `specs/done/` if it does not exist. Commit the move.
