# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
go build -o taskboard .
./taskboard -config taskboard.json   # taskboard.json is gitignored; start from config.example.json
go vet ./...
go test ./...
go test -run TestBuildTasks ./...    # single test
```

Single `main` package, standard library only (Go 1.22, uses `ServeMux` method/path patterns). All tests are in `taskboard_test.go` and use `testConfig(t)`; they never shell out, so they run without `gh`/`acli`. The binary requires `acli`, `gh` and `git` on PATH at startup.

## Architecture

A local dashboard for multitasking across many parallel coding-agent sessions. It joins Jira tickets, GitHub PRs, git worktrees, plan files and tmux sessions on the ticket key; Jira is the only tracker today, but others (e.g. GitHub issues) may be added, so avoid baking in Jira-only assumptions where it's cheap. README.md documents user-facing behaviour and config; keep it in sync when behaviour changes.

Data flow:

1. **Sources** shell out via `runCmd` (`local.go`): `jira.go` (`acli jira workitem search/view`), `github.go` (`gh pr list`, `gh search prs`), `local.go` (`git worktree list`, plan files in each worktree's `plans_dir`, `tmux list-windows`). Keys are extracted with `Config.findKey` (regex built from `project_keys` in `config.go`).
2. **`Board.refresh`** (`board.go`) runs the sources concurrently, then looks up any keys seen in PRs/worktrees/sessions but not returned by the JQL (`unknownKeys` → `jiraGet`), and fills Jira updated dates (`fillJiraUpdated`; acli search can't return `updated`, so it's one `view` per issue, cached and re-fetched only for issues a search reports as changed). Board keeps caches for done/missing issues (24h TTL). Source failures become strings in `Snapshot.Errors` rather than aborting the refresh.
3. **`buildTasks`** (`tasks.go`) is a pure function from `sources` to sorted `[]Task`. Plans attach only to tasks already found via another source. PRs without a key become `NoTicket` tasks keyed `repo#number`.
4. **Stage logic** (`progressStage`/`stageFor` in `tasks.go`): the `Stage` constants are declared in display-priority order and sorting depends on that order. `stageInfo` maps each stage to its label, tone, group and track station. Done tasks with a worktree or session become `StageCleanup`; Done tasks outside the `done_days` window are dropped.
5. **Session actions** (`session.go`) are the only code that changes anything: start (worktree + tmux window), stop and clean-up. `startPlan` and `removalCheck` are pure and tested; the git/tmux calls around them are not. Actions re-read sessions and worktrees with `liveTask` because the snapshot can be minutes old, run on their own context so a page reload can't abort a half-made worktree, and call `Board.Invalidate` (unlike `Refresh`, it isn't dropped during a running refresh). Clean-up must never remove a main checkout (`Worktree.Main`) and keeps any branch with unpushed commits, even when forced. Reviews work the same way on `PRLink`s: `attachReviews` matches worktrees and windows named `<checkout>-pr<N>` (no ticket key, so they never become tasks) and returns leftovers for PRs no longer listed; their routes are under `/review/`. Which buttons a row shows is decided in Go (`taskControls`/`prControls`), not in the templates. Attaching (Open session, the copied command) goes through a linked tmux session marked `@taskboard`, so terminals don't move each other's current window; `listWindows` skips marked sessions, and each refresh sweeps never-attached ones (`staleLinked`).
6. **Server** (`server.go`) renders the snapshot with embedded `html/template`s (`templates/*.html`; `tokens`, `updated`, `prsections` are partials). Reviews/Other repos PR sections (`PRLink`) are separate from tasks and not joined on Jira keys; review sessions are matched by worktree/window name instead.

Config: the settings page saves the config file (`SaveConfig`) and swaps it into the running board (`Board.SetConfig`). Read it through `Board.Config()` once per refresh or request and treat it as immutable; only `listen` is fixed at startup.

Polling: `Board.Run` polls every `poll_interval` while the page was viewed in the last 15 minutes (`idleAfter`), otherwise `idle_poll_interval`; a view after idling wakes it immediately via `Board.Viewed`. The page polls `GET /version` and reloads only when `Snapshot.Version` (hash of tasks/PRs/errors, excluding `Updated`) changes.

Security: there is no auth. `checkHost` rejects hostnames not in `allowed_hosts` (DNS-rebinding guard); `POST /attach/{key}` runs `attach_command` locally and requires a loopback client plus the `X-Taskboard: 1` header (forces a CORS preflight that is never answered). `POST /start|stop|cleanup/{key}` and `POST /review/{attach,start,stop,cleanup}/{name}` have the same guards and a per-key in-flight claim (423 while one runs). `/settings` can rewrite `attach_command` and `agent_command`, so it is loopback-only and its `POST` needs the same header; `POST /refresh` needs only the header. Preserve these when touching the server.
