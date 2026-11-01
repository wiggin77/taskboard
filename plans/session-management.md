# Session management

Status: phase 1 implemented.

Taskboard goes from watching sessions to managing them: start work on a ticket (creating or reusing a worktree and opening a tmux window running the agent), stop a session, clean up finished work, and later attach to a session from the browser.

Phases ship independently. Phase 1 is the core; phase 3 builds on the linked-session attach from phase 2.

## Phase 1: start, stop and clean up

### Config

Top level:

| Field | Default | Purpose |
|---|---|---|
| `tmux_session` | `work` | Session that new windows go into; created if missing |
| `agent_command` | empty (feature off) | Typed into a new window, e.g. `claude` |
| `agent_resume_command` | `agent_command` | Typed when continuing in an existing worktree, e.g. `claude --continue` |
| `worktree_dir` | parent of each repo's `path` | Where new worktrees go, flat, as `<dir>/<basename(path)>-<KEY>`; `~` is expanded |
| `worktree_links` | empty | Paths to link into `worktree_dir` as `<worktree_dir>/<basename>`; `~` is expanded |

New worktrees all go in one flat directory, one level deep, so a single symlink per sibling satisfies sibling-relative build paths for every worktree. For example, mattermost/mattermost's `server/Makefile` has `BUILD_ENTERPRISE_DIR ?= ../../enterprise`, and the plugins use `../mattermost-utilities`. Nothing project-specific is hardcoded; the user lists the siblings in `worktree_links`.

Before each worktree is created, `ensureLinks` checks every listed path:

- A missing link is created.
- A link that already points at the right target is left alone. This is checked with `os.SameFile` on both resolved paths rather than by comparing `os.Readlink` text, so macOS `/tmp` → `/private/tmp`, case-insensitive names and relative targets don't cause false conflicts.
- Any other file or link with that name stops the start with an error naming the conflict. It is never overwritten.

`ensureLinks` uses only the `os` package and is tested against a temp dir. Linux and macOS are supported. Windows is supported only through WSL, since tmux has no native Windows build; the README requirements should say so. The README gives the Mattermost pair as an example. Where existing worktrees live doesn't matter, since they're found through `git worktree list`.

Both commands accept `{key}`. It's the only placeholder, because keys match the project-key regex and are shell-safe; summaries and branch names are not.

Per repo (optional): `base_branch`, defaulting to the `origin/HEAD` target, as the base for new branches.

Add these to the settings page and `config.example.json`. They run commands locally, so they inherit the settings page's loopback-only rule.

### Deciding how to start (`startPlan`)

A pure function from the task, the configured repos and the local branches to one of four modes, in this order:

1. **resume**: the task has a worktree. Open a window there with `agent_resume_command`.
2. **branch**: a local branch whose name contains the key exists in a repo with a `path`, preferring the PR's repo and then the most recent commit. Run `git worktree add <path> <branch>`.
3. **pr**: the task has an open PR in a repo with a `path`. Run `git worktree add --detach <path> <base>`, then `gh pr checkout <n>` inside it, which handles fork branches and tracking.
4. **new**: there's nothing local and no PR. Run `git fetch origin <base>` and `git worktree add -b <branch> <path> <base>`. The default branch is `<KEY>-<slug of summary>`, at most about 50 characters.

The branch, pr and new modes start with `agent_command`. Only resume uses `agent_resume_command`, since a resume command such as `claude --continue` can fail when there is no history.

The new mode needs input when several repos have a `path`, and it always shows the branch name for confirmation. The other three modes need no input.

Local branches come from `git for-each-ref --sort=-committerdate --format=%(refname:short) refs/heads`, read at action time rather than on every refresh.

### Starting the window

```
tmux has-session -t <session>   # if missing: tmux new-session -d -s <session> -n <KEY> -c <path> -P -F ...
tmux new-window -d -t <session>: -n <KEY> -c <path> -P -F '#{session_name}:#{window_index}'
tmux send-keys -t <target> -l <command>
tmux send-keys -t <target> Enter
```

The command is typed into the window's shell instead of passed as the window command. That way, quitting the agent leaves a shell in the worktree instead of closing the window and losing its output.

If the request is loopback and `attach_command` is set, run it with the new target so a terminal opens straight away.

### Stopping a session

`tmux kill-window -t <target>`, behind a confirmation dialog, since it kills a running agent. The worktree is left alone.

### Clean-up (Needs clean-up rows)

**Safety check (`removalCheck`).** The raw output is collected by thin git wrappers and parsed by pure functions:

- **Uncommitted changes**: `git -C <wt> status --porcelain`, which includes untracked files but not ignored ones.
- **Unpushed commits**: `git -C <wt> log --oneline HEAD --not --remotes`. There's one exception. If the task's PR is merged and its head SHA equals the worktree's HEAD, the work is already in the merged PR, even after GitHub deleted the branch and a `fetch --prune` removed it locally. This needs `headRefOid` added to the `gh pr list` fields and a `HeadSHA` field on `PR`.

**Flow.** `POST /cleanup/{key}`:

1. If the check is clean: kill the tmux window, run `git worktree remove <path>`, and delete the branch with `git branch -D`. `-D` is needed because squash merges make `-d` refuse. Respond 204, with no dialog.
2. If the check isn't clean: respond 409 with `{uncommitted: [...], unpushed: [...]}`, each capped at about 20 lines. The page opens a `<dialog>` listing them, with a "Remove anyway" button that resends with `force=1`.
3. Forced: the server re-runs the check, then uses `git worktree remove --force`. It **keeps the branch if there are unpushed commits**, so an override only ever loses uncommitted changes. The response says which branch was kept.

**Invariants:**

- Never remove a repo's main worktree, which is the first entry of `git worktree list` (or `path` itself), even if its branch contains the key.
- Kill the window before removing the worktree, so the agent isn't writing into a directory that's being deleted.
- If there's a session but no worktree, clean-up just kills the window.

### Server

New routes, all needing a loopback client plus `X-Taskboard: 1`, like `/attach`:

| Route | Purpose |
|---|---|
| `POST /start/{key}` | In new mode without a `branch` field, responds 409 with the `startPlan` as JSON (`mode`, `repo`, `path`, suggested `branch`, selectable `repos`), and the page resends with `repo` and `branch`. Otherwise starts (or reuses an existing window) and responds `{target, opened}` |
| `POST /stop/{key}` | Kills the task's window |
| `POST /cleanup/{key}` | Clean-up as above, `force=1` to override |

Validation and robustness:

- The key must be a task in the current snapshot. `repo` must be configured and have a `path`.
- `branch` must contain the key (so the board finds it again), must not start with `-`, and must pass `git check-ref-format --branch`.
- Every command is passed as exec args, never through a shell, except the configured agent command, which is the user's own.
- A per-key in-flight guard (a mutex-protected set on `server`) turns double clicks into 423 Locked instead of two `worktree add` runs.
- Commands run with their own timeout of about 3 minutes, because a worktree checkout of mattermost/mattermost can take a while.
- `NoTicket` tasks get no start button, since a `repo#123` window name wouldn't match `findKey`.

After any action, call a new `Board.Invalidate()`, which requests a refresh unconditionally the way `SetConfig` does. `Refresh()` is ignored while a refresh is running, so a new worktree could otherwise stay off the board until the next poll.

### UI (`index.html`)

Buttons appear only when `.Local` is true and `agent_command` is set:

| Rows | Button | Behaviour |
|---|---|---|
| No session, not Done or clean-up | Play icon: "Start session" (new/branch/pr) or "Resume session" (resume) `POST /start`; a 409 (new mode) opens a dialog with a repo select (last choice per project remembered in localStorage) and an editable branch; otherwise POSTs directly. The button spins while working |
| Session | Stop icon next to copy/open | Confirmation dialog, then `POST /stop` |
| Needs clean-up | Clean-up icon | `POST /cleanup`; a 409 opens the override dialog listing files and commits, with a danger-styled "Remove anyway" |

- Use one shared `<dialog>` element with `showModal()`, never `confirm()` or `prompt()`.
- Errors show inside the dialog or as the existing `flashIcon` err state, with the server's message as the tooltip.

### Code layout

- New `session.go` holds the actions (`startPlan`, `startSession`, `stopSession`, `removalCheck`, `cleanup`) and their git/tmux wrappers.
- `local.go` stays read-only sources.

### Tests (no shelling out, per the existing convention)

- `startPlan` mode selection across all four modes, including repo preference and repos without a `path`.
- Branch-name slugging and truncation, worktree path derivation, and branch validation.
- `removalCheck` parsing: clean, dirty, unpushed, and merged with a matching head SHA counted as pushed.
- The main-worktree refusal.
- Endpoint guards, modelled on `TestRefreshEndpoint`: missing header gives 403, non-loopback gives 403, an unknown key gives 404, and an in-flight key gives 423.
- Config defaults and validation for the new fields.

### Docs

- **README**:
  - Replace "Starting sessions in tmux" with the start/stop/clean-up flow and the config fields, keeping the manual naming convention for people who start sessions themselves.
  - Rewrite the "doesn't start or drive sessions; it watches them" line.
- **CLAUDE.md**: add `session.go` to the architecture section and the new routes to the security paragraph.

## Phase 1b: review sessions (implemented)

Reviews rows with a local checkout get Start/Resume review, Stop and Clean up. A review is named `<checkout>-pr<N>` for both its worktree and its tmux window, so it is never joined to a ticket. `gh pr checkout --branch review/<N>` puts the worktree on a branch tracking the PR, so pull and (where allowed) push work. The PR head is also fetched as `refs/remotes/<remote>/pr/<N>`, and re-fetched at clean-up, so its commits count as pushed even for fork PRs. Clean-up removes that ref and deletes `review/<N>` unless it has unpushed commits; any other branch in the worktree is kept. Review worktrees or windows whose PR has left the list are shown as leftovers with the PR's state from `gh pr view`. The window starts `agent_command` with no prompt; the user types the review command.

## Phase 2: linked-session attach

Two clients on the same tmux session mirror each other's current window. Attach through a linked session instead, so each client tracks its own window:

```
tmux new-session -t <session> -s tb-<KEY>-<n> \; set destroy-unattached on \; select-window -t <target>
```

- Build this in Go and pass it as a new `{attach}` placeholder (or change what `{target}` expands to) for `attach_command` and the copied ssh command.
- Filter `tb-*` sessions out of `listSessions`, so linked windows don't show up as duplicate sessions.

## Phase 3: terminal in the browser

Dependencies: `github.com/creack/pty`, `github.com/coder/websocket`, and xterm.js plus its fit addon vendored under `static/` and embedded, so the page works offline. Update the "standard library only" line in CLAUDE.md.

- **Server.** `GET /term/{key}` upgrades to a WebSocket and runs the phase 2 linked-session attach under a PTY. It copies PTY output to binary messages and input messages to the PTY. A small JSON control message (`{"cols":..,"rows":..}`) calls `pty.Setsize`. On disconnect it kills the attach process, and `destroy-unattached` removes the linked session.
- **Security**, the part that matters most:
  - Loopback only.
  - Check that `Origin` matches the request host and passes `checkHost`. Browsers can't set `X-Taskboard` on a WebSocket and WebSockets bypass CORS, so without this check any website you visit could open a shell.
  - The key must resolve to a snapshot task with a session. The server only ever attaches to that window, never runs an arbitrary command.
- **UI.** The terminal icon opens a panel: a full-height drawer on desktop, full screen on phones. It has a close button and reconnects if the page reloads after a `/version` change, which also means the reload must not kill an open terminal. Either skip auto-reload while the panel is open, or open the terminal at `/term/{key}` as its own page. The own page is simpler and recommended, so the panel can come later.
- **Tests.** The origin check, and the guards for loopback, missing session and unknown key. The PTY bridge itself is checked by hand.

## Later ideas (not planned)

- A per-repo `setup_command` run in new worktrees (`make setup`, `npm ci`).
- Moving the Jira ticket to In Progress on start. Keep it behind the tracker abstraction.
- Start buttons for `NoTicket` PRs, which would need a window-naming convention that `findKey`-style matching understands.
