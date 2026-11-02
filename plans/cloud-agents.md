# Cursor and cloud agents

Status: planned, not started.

## Why

taskboard assumes the agent runs on this machine: a session is a tmux window (`local.go` `Session`), Start creates a worktree and types `agent_command` into a window (`session.go` `startPlan`/`openWindow`), and Open attaches a terminal. Teammates working differently fall into two groups:

1. **Cursor in the IDE (local).** Worktrees and branches exist locally, but there is no tmux window, so the task shows no session and Open/Start mean nothing to them.
2. **Cloud agents** (Claude Code on the web / `claude --remote`, Cursor cloud/background agents). No local worktree or window at all; the work is visible only as a pushed branch, eventually a PR, and a session page in the provider's UI.

Goal: the board stays one row per ticket, but a row can carry a *remote* session as well as a tmux one, with Start/Open/Stop meaning the right thing for each.

## What already works (no code)

- A cloud agent's PR joins its ticket if the key is in the branch or PR title (`github.go:88-90`), so CI failing / changes requested / ready to merge already surface.
- Repos with no `path` are allowed (`localRepos` filters them), so a cloud-only developer can run taskboard as a viewer of Jira + GitHub today.
- Gap: between "agent started" and "PR opened" the ticket sits in *Not started*, and nothing says an agent is idle waiting on you.

Cheapest first step: document in README a convention — tell the cloud agent to put the ticket key in its branch name and open a **draft PR early** with the key in the title. That alone moves the task to Coding/In review.

## Recommended design

### 1. Provider-neutral remote session on `Task`

Add a `RemoteSession` (provider, ID, URL, status, branch, repo, updated) alongside `Task.Session`; keep tmux `Session` as is. Join on key from branch name, else agent title/prompt (same `findKey` fallback pattern as PRs). Plans still attach only to tasks found elsewhere.

Stage changes (`tasks.go`): a task with a running remote session is Coding even with no worktree; a remote session reporting finished/waiting/error and no PR yet gets a new Needs-you stage, e.g. "Agent finished" (insert in the `Stage` const order, add `stageInfo`). Done tasks with a remote session that is still running go to clean-up, like tmux windows.

### 2. Sources (one file each, run concurrently in `Board.refresh`, failures to `Snapshot.Errors`)

- **Remote branches (provider-agnostic, phase 1).** `git ls-remote --heads origin` per local repo, or `gh api repos/{r}/branches` for repos without a path; any branch containing a key with no PR → task is Coding with a branch link. Catches every cloud agent that pushes, Cursor or Claude, with no API keys.
- **Cursor cloud agents (phase 2).** Cursor's Cloud Agents API (`GET /v0/agents`: status, repo, branch, PR URL; `POST /v0/agents` to launch; stop/follow-up endpoints — verify current shape). API key from env `CURSOR_API_KEY`, never written to the config file.
- **Claude Code on the web (phase 3).** Launch via `claude --remote "<prompt>"` and bring local via `claude --teleport <id>` (verify flags/output). Listing sessions depends on what the CLI/API exposes; until then rely on the remote-branch source and store the session URL returned at launch.

### 3. Actions (decided in Go, `taskControls` in `server.go`)

- **Start in cloud** (new mode alongside browser/terminal): prompt template config, e.g. `"Work on {key}: {summary}. Use a branch name containing {key} and open a draft PR early."` (`{summary}` must be passed as data, not shell — the provider call is an API request/argv, never typed into a shell). Needs local or remote access, same in-flight claim and `X-Taskboard` rules.
- **Open**: link to the provider's session URL (no attach).
- **Stop**: provider stop call where supported; otherwise hidden.
- **Pull locally**: fetch the agent's branch and reuse `startPlan`'s StartBranch path (extend branch lookup to `origin/<branch>`), then optionally `claude --teleport` in the window. This is the bridge from cloud to the existing tmux workflow.
- **Clean up**: unchanged locally; remote branches are left to GitHub's delete-on-merge.

### 4. Cursor in the IDE (local)

- `editor_command` config (argv, `{path}` placeholder, e.g. `["cursor", "{path}"]`), local-only like `attach_command`. **Open in editor** button on any row with a worktree; **Start in editor** = create the worktree via existing `createWorktree`, then run `editor_command` instead of opening a tmux window.
- tmux becomes optional for these users: no window means no Session, which is fine — the worktree alone already makes the task Coding.

## Files touched

- `tasks.go` — `RemoteSession`, stage logic, `buildTasks` join.
- `board.go` — new sources in `sources`/refresh; include in `LocalChanged` path only for local data.
- New `remote.go` (branches), `cursor.go`, `claudecloud.go` — parse functions pure and tested.
- `session.go` — start modes, Pull locally (reuse `startPlan`, `createWorktree`, `openWindow`).
- `server.go`, `templates/actions.html` — controls and routes.
- `config.go`, `templates/settings.html`, `config.example.json`, README, CLAUDE.md — new settings and docs.

## Verification

- Unit tests in `taskboard_test.go` with `testConfig(t)`: parse fixtures for `ls-remote`, Cursor agent JSON, and `buildTasks` cases (branch-only task → Coding; finished agent with no PR → Agent finished; agent + PR → PR stage wins).
- `go vet ./... && go test ./...`.
- Manual: launch a Cursor cloud agent and a `claude --remote` session on a test ticket; confirm the row moves Not started → Coding → Agent finished → In review, Open goes to the provider page, Pull locally produces a worktree + tmux window.

## Suggested order

1. README convention (zero code) + remote-branch source + `editor_command`. Covers both groups with no credentials.
2. Cursor API source and Start in cloud.
3. Claude Code cloud launch/teleport once the CLI surface is confirmed.
