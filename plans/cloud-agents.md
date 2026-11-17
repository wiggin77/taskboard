---
status: approved
---
# Cursor agents

Status: approved, not started.

## Why

taskboard assumes the agent runs on this machine: a session is a tmux window (`local.go` `Session`), Start creates a worktree and types the agent command into a window (`session.go` `startPlan`/`openWindow`), and Open attaches a terminal. Teammates using Cursor work differently:

1. **Cursor in the IDE.** Worktrees and branches exist locally, but there is no tmux window, so Open/Start mean nothing to them.
2. **Cursor cloud agents.** No local worktree or window. The work exists as an agent in Cursor, a `cursor/...` branch and eventually a PR, and today the ticket sits in Not started until a PR that taskboard can find appears.

Goal: one row per ticket. Each row can show either the tmux buttons or the Cursor buttons, picked with a toggle on the row. The toggle starts at a board-wide default, and it can be switched even after work has started.

**Cursor only, not "cloud agents" in general.** Cursor's API lists the key owner's agents with their status, branch and PR, which gives an authoritative per-user source. A provider-neutral design would have to scan remote branches instead. In shared repos that means every teammate's branch, it costs GitHub budget, and it still can't tell running from idle. Claude Code on the web has no listing API, so it gets only the README convention below. The `Task` field and the stage logic stay provider-neutral where that's free (CLAUDE.md's rule for Jira). There is no provider interface until a second provider exists.

## What the Cursor API gives us

Checked against the v1 docs and OpenAPI spec (Nov 2026). v0 is legacy and its reference page now returns 404, so use v1 only.

- **Auth.** `Authorization: Bearer <key>`. Only user API keys are supported: `GET /v1/agents` lists "agents for the authenticated user". Agents have no creator field, and the docs don't say what a service-account key lists.
- **List.** `GET /v1/agents?limit=100&cursor=...`, newest first. `includeArchived` defaults to true. Items carry only `id`, `name`, `status`, `url` (the agent in Cursor Web), `createdAt`, `updatedAt` and `latestRunId`.
- **Agent status.** `ACTIVE` (a turn is running or pending), `IDLE` (the last turn finished and follow-ups are accepted) and `ARCHIVED`. The spec's enum lists only `ACTIVE`/`ARCHIVED`, while the endpoints page documents `IDLE`, so verify against the live API.
- **Runs.** `GET /v1/agents/{id}/runs/{runId}` has `status` (`CREATING`, `RUNNING`, `WAITING_FOR_BACKGROUND_WORK`, `FINISHED`, `ERROR`, `CANCELLED`, `EXPIRED`) and `git.branches[]` `{repoUrl, branch, prUrl}`. That `git` value is per-agent state, the same on every run, and the only place the branch and PR appear.
- **Launch.** `POST /v1/agents` takes:
  - `prompt.text`, `name` (max 100), `repos[] {url, startingRef}`, `autoCreatePR`, `skipReviewerRequest` and `workOnCurrentBranch`;
  - an optional client-supplied `agentId` (`bc-<uuid>`).

  The branch name can't be set: Cursor pushes to an auto-generated `cursor/...` branch unless `workOnCurrentBranch` makes it commit to `startingRef`.
- **Stop and clean-up.** No agent-level stop exists:
  - `POST /v1/agents/{id}/runs/{runId}/cancel` cancels the active run, and returns 409 `run_not_cancellable` when there is nothing to cancel.
  - `POST /v1/agents/{id}/archive` is reversible and idempotent.
  - `DELETE` is permanent, so taskboard never calls it.
- **Follow-up.** `POST /v1/agents/{id}/runs` returns 409 `agent_busy` while a run is active.
- **No v1 webhooks yet**, so this is polling only. List and get rate limits aren't documented. A 429 carries `Retry-After`.
- **PR author is unresolved.** The docs don't say whether PRs from IDE, web or API agents open as you or as the `cursor` app. A Cursor reply on the forum in Oct 2026 says automation PRs show as `cursor` regardless of the documented setting. The design below doesn't depend on the answer.
- **Opening agents.** An agent opens in the browser at its `url`. The desktop deep link (`cursor://anysphere.cursor-deeplink/background-agent?bcId=...`) isn't usable: in May 2026 Cursor said that workflow is "not currently supported".

## Design

### 1. The tool toggle

**What's available.** Cursor has two independent parts:
- **Cloud**: needs `TASKBOARD_CURSOR_API_KEY`.
- **Editor**: needs `editor_command` and local access, like Open in a terminal, since it opens an app on this machine.

A Cursor IDE user needs no API key. Availability is decided per request, since the editor part depends on the client: an editor-only setup has nothing on the Cursor side for a remote client. When the Cursor side is empty for this client, the row has no toggle and shows the tmux side, whatever is stored or set as default.

**The toggle.** It's a two-state control on each row, labelled with the agent command's name (`claude` by default) and `Cursor`. It picks which tool's buttons the row shows. It's shown for local and remote access when the Cursor side has anything for this client. Viewers have no buttons, so they get no toggle.

**Which side a row shows**, in order:
1. The row's stored choice.
2. Otherwise, if only one tool has something running on the ticket (a tmux window, or a non-archived agent), that tool. This way an agent started from Cursor's UI shows Cursor buttons without any toggling.
3. Otherwise, `default_tool` (`tmux` by default, or `cursor`).

Go works out steps 2 and 3 as the row's default when it renders the page. The page applies step 1 over it.

**Storage, shared across machines.** Choices live in `taskboard-tools.json` next to the config. The file and the highlights file use a shared helper, `idStore[V]` in a new `store.go`, extracted from `highlights.go` before the tools store is written. It covers:
- a mutex;
- loading, with a missing file read as empty and an empty path meaning in-memory only;
- pruning to the IDs on the board (`present`, where nil means the board may be incomplete);
- `writeFileAtomic`.

Each store passes its own encode/decode functions, because the highlights file is a sorted list (`{"highlights": [...]}`) rather than a map. `highlights.go` becomes an `idStore[bool]` with its file format unchanged, so existing `taskboard-highlights.json` files keep working. The tools store is an `idStore[string]`. The rules for the tools file:
- the file maps key → tool, and a missing file reads as empty;
- `POST /tool` sets rather than toggles, so two devices clicking at once can't undo each other;
- setting a choice also drops entries for keys no longer on the board (the same `present` rule as highlights), so the file doesn't grow;
- a choice is stored even when it equals the default, because the row's default changes as sessions start and stop.

**Updating open pages in place.** The page renders both tools' buttons, as decided in Go (`taskControls`), and CSS hides the side that isn't selected. `/version` returns the choices next to the highlights, and the page applies them without reloading, as it does for highlights. The choices stay out of `Snapshot.Version`. Go still decides every button; the toggle only picks which of Go's two sets is visible.

The Highlighted section is built in the page from copies of rows (`showHighlights`), so choices are applied to every row with the key, copies included. The toggle works the same on a copy.

**Switching while the other tool is running.** If the side you leave has something running, the toggle asks first:
- **Switch and stop it**: tmux closes its window (the existing `/stop`); Cursor cancels the agent's run and archives the agent (`/agent/archive`).
- **Switch only**: leaves it running.

Either way the worktree stays, because both tools use it. Both tools' tokens always show, so anything left running stays visible, and Clean up handles both. The page stops first and saves the switch only if the stop succeeded, so a failed stop leaves the row as it was. Until Archive agent exists (phase 2), the prompt offers a stop only when leaving tmux.

Reviews (`/review/`) stay tmux-only. Cursor review agents would be a separate feature.

### 2. Cursor source (`cursor.go`)

- Enabled when `TASKBOARD_CURSOR_API_KEY` is set in taskboard's environment. The key is read once at startup, so changing it needs a restart. It's never written to the config file. With no key, the source is skipped silently.
- After reading the key, taskboard removes it from its environment (`os.Unsetenv`). Child processes inherit taskboard's environment: `git`, `gh`, `acli`, and the tmux server when taskboard starts it. Without this, every agent in a window started from the board could read the key. This only helps when the key is set for taskboard alone. The README says to set it that way (`TASKBOARD_CURSOR_API_KEY=… taskboard`, or in a launchd/systemd unit), not in a shell profile, where every shell and window would have it anyway.
- **Why not `CURSOR_API_KEY`.** It's deliberately not Cursor's own variable, which taskboard never reads or removes. A user who already has `CURSOR_API_KEY` set, for example for `cursor-agent` as their `agent_command` in tmux, neither gets the Cursor source turned on unasked nor loses the variable in their windows.
- It runs with the other sources in `Board.refresh` and stores its result in `fetched`. On failure it keeps the last good agents, like `ghData`/`withFallback`, and reports the error in `Snapshot.Errors`. A 429 pauses Cursor requests until `Retry-After` and shows a `Snapshot.Notices` entry, like `ghPause`.
- **Listing.** Each refresh lists agents including archived ones (the API's default), at most two pages (200).
  - Agents not updated within `done_days` are ignored. Otherwise turning the source on would put every old agent's ticket into Done, clean up.
  - The exception is an agent whose PR was open at the last lookup, kept whatever its age. A long review mustn't drop a PR opened by `cursor` from its ticket. The PR's state comes from the previous refresh's cached lookup.
  - **Archived agents** are kept only to find their PR. Archiving after a switch must not drop an open PR opened by `cursor` from its ticket. They have no status, buttons, token or clean-up.
- **Runs.** It fetches the latest run only for agents whose `updatedAt` or `latestRunId` changed since the last refresh, plus every `ACTIVE` agent, in case `updatedAt` doesn't move with run status (verify). An archived agent's run is fetched once, for its PR. Runs are cached by agent ID in `Board`, like the Jira detail cache.
- It's plain `net/http`: no new dependency, and no `runCmd`. The JSON parsing (`parseAgents`, `parseRun`) is pure and tested. The HTTP calls around it aren't, like the git/tmux calls today.

`Agent` (in `tasks.go`, provider-neutral):

| Field | From |
|---|---|
| `Provider` | `"cursor"` |
| `ID`, `Name`, `URL` | the list item |
| `Status` | the agent's status |
| `RunID`, `RunStatus` | the latest run |
| `Repo`, `Branch`, `PRURL` | `git.branches[0]`. `Repo` is the configured repo name matched from `repoUrl`. |
| `Key` | `findKey(Name)`, else `findKey(Branch)` |

Agents for repos that aren't configured are dropped, like task PRs. Agents with no key are dropped: they aren't ticket work, and an agent with a PR authored by you still appears as a `NoTicket` task through the PR. Volatile timestamps stay off `Task`, so a running agent doesn't change `Snapshot.Version` (and reload the page) on every poll.

### 3. Joining (`buildTasks`)

- `sources` gains `agents []Agent`. An agent creates a task like a worktree does, since it's yours by construction. Its key goes into `unknownKeys`, so a ticket outside the JQL still gets its Jira details.
- `Task.Agent` holds one unarchived agent per key: active before idle, then the newest. Clean-up and archiving act on every unarchived agent for the key, not just the one shown.
- An archived agent contributes only its PR (below). It doesn't set `Task.Agent`.
- **The agent's PR.** When an agent's `PRURL` (archived or not) names a PR not already in `prs`, look it up by repo and number.
  - Use the `prOpen` fragment, so the PR has CI, review and draft state.
  - Put it in the same request `viewPRs` already makes for review checkouts, after the sources finish, so there's no extra GitHub request in the common case. It follows `ghPause` and keeps the last good result on failure, like the other GitHub data.
- The agent's PR becomes the task's PR whether it's authored by you or by `cursor`, with `Assigned` false (it's your work). `withoutTaskPRs` then drops any Reviews row for it.
- Plans attach as today: an agent counts as "found elsewhere".

### 4. Stages (`tasks.go`)

- A PR wins, as now (a draft PR is Coding).
- With no PR:
  - a tmux window, a worktree or an `ACTIVE` agent is **Coding**;
  - an `IDLE` agent with no worktree and no window is a new **Agent waiting** stage in Needs you. This covers a finished, failed, cancelled or expired last run. Station 2 (Code), tone `needs`. In the `Stage` order it goes after `StagePlanning` and before `StageMergedJiraPending`. The row's agent token shows the run status, with `ERROR` in the `fail` tone.

  The "no worktree, no window" condition matters after switching tools or opening the agent's work locally: an idle agent you've stopped watching mustn't keep the ticket in Needs you.

  An unapproved plan still wins: Plan needs review comes before Agent waiting, as the `Stage` order says.
- **Clean-up.** A done task with an unarchived agent goes to **Done, clean up**, like a worktree or window. This rule arrives in phase 2 together with archiving. In phase 1, Clean up couldn't remove the agent, so the row could never leave clean-up.

### 5. Rows and controls (`taskControls`)

Controls are still decided in Go. `controls` gains a Cursor set alongside the existing tmux fields, plus the row's default side for §1. The templates only render them.

- **Tokens.** The tmux session token as today. The agent token whenever a task has an agent, on either side: run status plus a link to the agent's `url`, with `data-group` set to the key so the tab-groups extension files it. Viewers see the tokens too: the agent URL is useless without the owner's Cursor login.
- **tmux side:** unchanged (Start/Resume session, Open in a terminal or the browser, Stop).
- **Cursor side** (each part only when available, §1):
  - **No agent:** these follow the stage part of `CanStart`, like tmux starts, so tickets with no key and merged, cleaned-up or done tasks get none.
    - `CanStart` (`tasks.go`) also returns false while a tmux window is running, which would hide Start in Cursor after Switch only. So it splits in two:
      - the stage and no-key check, used by both sides;
      - the running-session check, used only by the tmux side.
    - **Start in Cursor** (cloud).
    - **Start in editor**, or **Resume in editor** when a worktree exists.
  - **With an agent:**
    - **Open agent** (its `url`, in a new tab).
    - **Stop agent**, when the latest run is active.
    - **Open locally**, when there's no worktree yet. It brings the agent's work into a worktree and opens the editor (§6).
  - **Open in editor**, once there's a worktree.
- **Clean up**, the same on both sides: archives agents as well as removing the worktree and window. Its label says which of these it will do.

### 6. Actions (`session.go`, `server.go`)

All actions follow the existing rules: local or remote access, the `X-Taskboard: 1` header, and the per-key in-flight claim (423 while one runs). They run on their own context, and they call `Invalidate` afterwards, so the next refresh lists the change. Cursor state lives in `fetched`, which is never modified once stored, so there's no `LocalChanged` path for it. Local worktree and window changes still call `LocalChanged`.

Like `liveTask` for sessions and worktrees, agent actions don't trust the snapshot, which can be minutes old. Stop and Archive fetch the agent first (`GET /v1/agents/{id}`) for its current `latestRunId`, then that run's status. A follow-up may have started a newer run, so the snapshot's run can be the wrong one to cancel.

- **Start in Cursor** (`POST /agent/start/{key}`):
  - **Form.**
    - With no worktree and no open PR, it picks any configured repo (no local path needed). With either, the repo is fixed.
    - It shows the prompt for editing. The prompt starts from `cursor_prompt`, default `"Work on {key}: {summary}"`. Placeholders are filled in Go, and the result is sent as JSON, never through a shell.
  - **Request.**
    - `name` is `"{key}: {summary}"`, cut to 100 characters so the key always joins.
    - `autoCreatePR: true`.
    - `skipReviewerRequest: true`, which keeps Cursor from requesting you as a reviewer on your own work.
  - **Where the agent starts**, in order:
    1. With a worktree (handing off from tmux or the editor): the worktree's branch, using `workOnCurrentBranch: true` and that branch as `startingRef`. Cursor can only see what's pushed, so the start is refused unless the conditions below hold.
    2. With an open PR and no worktree, for example to address Changes requested: the PR, using `repos[0].prUrl` and `workOnCurrentBranch: true`. The agent commits to the PR's branch instead of opening a second PR.
    3. Otherwise: `repos[0].startingRef` is the repo's base branch when one is configured, and Cursor makes a `cursor/...` branch.

    The conditions for starting from a worktree:
      - the branch exists on origin, checked live with `git ls-remote origin refs/heads/<branch>` because `origin/<branch>` can be stale; a branch with no commits of its own passes the unpushed check but has never been pushed;
      - the worktree has no uncommitted or unpushed work, listed with `removalCheck`'s checks.
    - If the ticket's tmux window is still running (after Switch only), the form offers to stop it first, since you and the agent pushing to the same branch would conflict.
  - **Double submits.** A `bc-<uuid>` `agentId` is generated per form render, so a double submit can't launch two agents.
  - **On success** the result dialog shows an **Open agent** link to its `url`, with `data-group` set to the key. Opening the tab by script after the request finishes would usually be blocked as a pop-up, and the tab-groups extension only acts on link clicks.
  - **Trust.** The ticket summary becomes agent instructions. Anyone who can edit the ticket can change it, and the agent can push. That's the same trust as pasting the ticket into a prompt yourself. The README says so, and the prompt is shown before sending.
- **Stop agent** (`POST /agent/stop/{key}`): cancels the current latest run. A 409 `run_not_cancellable` counts as success only when that fresh run is already in a terminal state.
- **Archive agent** (`POST /agent/archive/{key}`): cancels any active run, then archives every unarchived agent for the key. It's used by the toggle's **Switch and stop it** and by Clean up. A cancelled run may still report as active for a moment, so a refused archive right after a cancel is retried once after a short wait.
- **Start, Resume or Open in editor** (`POST /editor/{key}`, local only):
  - With no worktree, it creates one with `startPlan`/`createWorktree`, as Start session does, including the new-branch form.
  - It then runs `editor_command` with `{path}` in place, as argv, never through a shell, and opens no tmux window.
- **Agent work in a worktree.** This is Open locally on the Cursor side, and Start session on the tmux side for a ticket with an agent. `startPlan` picks, in order:
  1. a local branch containing the key;
  2. the PR (`StartPR`, as today);
  3. a new `StartAgentBranch` mode, which fetches the agent's branch and runs `git worktree add --track -b <branch> <path> origin/<branch>`, keeping the same name so a plain push goes back to it. If a local branch with that name already exists (an earlier clean-up kept it for unpushed commits), it's used as is, with `git worktree add <path> <branch>`;
  4. a new branch.

  If the agent's run is still active, the form offers to stop it first, since the agent and you pushing to the same branch would conflict. Until Stop agent exists (phase 2), it only warns.

  Resume opens an existing worktree as it is, without pulling. After a hand-off where the agent committed more to the branch, you pull yourself. The README says so.
- **Clean up** (`POST /cleanup/{key}`, extended): does the existing worktree/window clean-up first, then archives the key's agents as above. That clean-up can stop to ask about uncommitted work, so if you cancel there, the agent is left as it was. Archiving is reversible, and remote branches are left to GitHub's delete-on-merge.
- **Tool toggle** (`POST /tool`, with `id` and `tool`; local or remote): sets the stored choice and returns the full map, like `/highlight`. Stopping the other tool is a separate call the page makes after the prompt in §1.

### 7. Worktrees whose branch has no key (`local.go`)

`Worktree.Key` comes only from the branch name, but Cursor branches are `cursor/...` with no key. So an agent branch opened locally, or a `StartPR` checkout of any PR whose key is only in its title, never joins its task. That second case is an existing bug. Fall back to `findKey` on the worktree directory, which taskboard names `<checkout>-<KEY>`, when the branch has no key.

**This changes the board for every user, not only Cursor users.**
- **Rows that move.** Worktrees that used to be ignored now attach to their tickets. Some rows move from Not started to Coding, and some done tickets move to Done, clean up.
- **The hazard.** Clean-up runs `git branch -D` on the worktree's branch when it has no unpushed commits. A folder like `mattermost-MM-1234` that is now checked out on `master` or `release-10.5` would join MM-1234, and cleaning it up would delete that local branch.
- **The fix.** A worktree joined through its folder name is marked (`Worktree.KeyFromDir`). Clean-up removes its folder but never deletes its branch, and reports the kept branch the way `Removal.Kept` does today.
- **The README** notes the new matching.

### 8. Everyone else

README convention, no code: tell any agent to put the ticket key in its branch name and open a draft PR early with the key in the title. That moves the task to Coding/In review through the existing PR join, as long as the PR is authored by you or assigned to you.

## Settings

| Setting | Default | Notes |
|---|---|---|
| `default_tool` | `tmux` | `tmux` or `cursor`: the toggle's starting side when nothing is running. `cursor` falls back to tmux for a client with nothing on the Cursor side. |
| `cursor_prompt` | `"Work on {key}: {summary}"` | |
| `editor_command` | none | argv with `{path}`, e.g. `["cursor", "{path}"]`. Like `attach_command`, it only runs for local access. |

Settings with defaults are resolved by accessors, shown as placeholders, and never written by `validate`. `validate` rejects a `default_tool` other than `tmux` or `cursor`. All three go on the settings page. The page shows whether `TASKBOARD_CURSOR_API_KEY` was found, never the key, and says a change needs a restart. Add them to `configs/*.json` only if a template should set them (`TestConfigTemplates` checks they parse).

## Files touched

- New `cursor.go`: client, `parseAgents`/`parseRun`, the run cache and 429 pause.
- New `store.go`: `idStore[V]`, extracted from `highlights.go`.
- `highlights.go`: rebuilt on `idStore[bool]`, keeping its file format and `/version` output.
- New `tools.go`: the per-ticket choice store on `idStore[string]`.
- `tasks.go`: `Agent`, `Task.Agent`, the Agent waiting stage and `stageInfo`, the `buildTasks` join, and `CanStart` split into its stage and session parts.
- `board.go`: the Cursor source in refresh with its fallback; agent keys in `unknownKeys`; the tools store next to the highlights.
- `github.go`: agent PR lookup alongside `viewPRs`.
- `local.go`: worktree key fallback to the directory name, marked with `Worktree.KeyFromDir`.
- `session.go`:
  - `StartAgentBranch` in `startPlan`;
  - Start in Cursor from a worktree's branch, with the uncommitted/unpushed check;
  - the editor start;
  - agent cancel/archive in clean-up;
  - clean-up keeps the branch of a worktree with `KeyFromDir`.
- `server.go`, `templates/actions.html`, `templates/tokens.html`, `templates/index.html`:
  - routes and controls;
  - both button sets per row, and the toggle with its switch prompt;
  - choices applied from `/version`.
- `config.go`, `templates/settings.html`: new settings.
- README, CLAUDE.md:
  - the Cursor source and the `TASKBOARD_CURSOR_API_KEY` requirement, set for taskboard only;
  - the toggle and `taskboard-tools.json`;
  - the prompt trust note and the convention;
  - pulling after a hand-off;
  - the `/agent/`, `/editor/` and `/tool` routes under Security (`/editor/` is local only).

## Verification

- Unit tests in `taskboard_test.go` with `testConfig(t)`, with no network:
  - **Parsing.** `parseAgents` and `parseRun` fixtures, covering an unknown status, a missing `git`, and a repo that isn't configured.
  - **`buildTasks` stages and joins:**
    - an active agent with no PR → Coding;
    - an idle agent with no PR, worktree or window → Agent waiting;
    - an idle agent plus a worktree → Coding;
    - an agent plus an open PR → the PR stage wins;
    - a Jira-done task with an unarchived agent → clean-up;
    - an agent whose key is only in its name joins;
    - an agent for a repo that isn't configured is dropped;
    - an agent not updated within `done_days` is ignored;
    - an archived agent's PR (authored by `cursor`) still joins its ticket, with no `Task.Agent`;
    - an agent older than `done_days` whose PR was open at the last lookup is kept;
    - an unapproved plan plus an idle agent → Plan needs review;
    - two agents for one key → the active one is shown.
  - **Row default side:**
    - only a window → tmux;
    - only an agent → Cursor;
    - both or neither → `default_tool`;
    - no Cursor part available → no toggle;
    - an editor-only setup on a remote client → no toggle and the tmux side, even with a stored `cursor` choice or `default_tool: cursor`.
  - **`idStore`:**
    - set and delete;
    - pruning with a `present` map, and no pruning when it's nil;
    - a missing file read as empty;
    - an empty path kept in memory;
    - a bad file reported with its path.
  - **Highlights:** the existing tests pass unchanged, and an existing `taskboard-highlights.json` still loads.
  - **Tools store:** a choice round-trips through the file.
  - **`taskControls`:**
    - each side's buttons for not started, worktree-only, running and done rows;
    - Cursor starts hidden where the stage check fails, and shown while a tmux window is running;
    - a cloud-only setup (no `editor_command`) and an editor-only setup (no key);
    - Cursor buttons and the toggle hidden for viewers;
    - editor buttons hidden for remote access.
  - **`startPlan`:** picks `StartAgentBranch` for an agent branch with no PR or local branch, reuses an existing local branch of that name, and picks `StartPR` when the agent has a PR.
  - **Start in Cursor request:** a worktree gives its branch with `workOnCurrentBranch`; an open PR with no worktree gives `prUrl`; otherwise the base branch.
  - **Other cases:**
    - `TASKBOARD_CURSOR_API_KEY` is gone from the environment after startup;
    - Start in Cursor from a worktree is refused with uncommitted or unpushed work, and when the branch isn't on origin;
    - `validate` rejects an unknown `default_tool`;
    - `withoutTaskPRs` drops the Reviews row for an agent PR authored by `cursor`;
    - worktree key fallback to the directory name, with the branch name still winning when it has a key;
    - clean-up of a worktree joined through its folder name keeps its branch (e.g. `master`) and reports it as kept;
    - prompt placeholders are filled without interpretation (a summary containing `{key}` or quotes).
- **No Cursor configured: Claude Code users see no change.** With no `TASKBOARD_CURSOR_API_KEY` and no `editor_command`, the board must behave as today apart from the folder-name fallback (§7). Guards:
  - every existing test passes unchanged, in particular the `taskControls`, `startPlan`, `buildTasks` and highlights tests;
  - a test that, with no Cursor config, every row's controls equal today's tmux controls, with no toggle and no Cursor set;
  - a test that `CURSOR_API_KEY` in the environment neither enables the source nor is removed;
  - the new `Stage` value leaves the relative order of existing stages unchanged;
  - the agent PR lookup adds nothing to the GitHub request when there are no agents;
  - saving settings from an existing config writes none of the new fields;
  - manual, with no Cursor config:
    - start, open (terminal and browser), stop and clean up a tmux session;
    - start and clean up a review;
    - highlight and unhighlight a row, and check another tab follows;
    - confirm the rows and buttons look as before.
- `go vet ./... && go test ./...`.
- **Manual.**
  - **Toggle.**
    - Flip a row on one machine and see it change in place on another, including its copy in Highlighted.
    - Switch a row that has a running window and check both prompt choices.
    - Archive an agent whose PR is open and check the PR stays on the row.
  - **Lifecycle.** Launch from the board on a test ticket and confirm Not started → Coding → Agent waiting (or In review when its PR opens). The dialog's Open agent link opens Cursor Web in the ticket's tab group, Stop agent cancels the run, Open locally gives a worktree that joins the row and opens Cursor, and Clean up archives the agent.
  - **Hand-offs.**
    - Start a tmux session, push, switch to Cursor, and Start in Cursor; the agent commits to the same branch.
    - Switch back to tmux and Start session; the window opens on the agent's branch.
    - On a ticket at Changes requested with no worktree, Start in Cursor; the agent pushes to the existing PR.
    - Cancel Clean up at its uncommitted-work prompt; the agent stays unarchived.
  - **Agents launched elsewhere.** Launch one from Cursor directly with the key in its name and confirm it joins and the row defaults to Cursor.
  - **First run.** Enable the source on an account with older agents and check that only agents updated within `done_days` appear.
  - **Open questions:**
    - who authors the PR;
    - whether `IDLE` appears;
    - whether `updatedAt` moves with run status;
    - whether archive works on an active agent, and right after a cancel;
    - whether omitting `startingRef` uses the default branch;
    - whether `workOnCurrentBranch` pushes to an existing remote branch as expected, and to a PR's branch with `prUrl`.

## Suggested order

0. **Store helper.** Extract `idStore` from `highlights.go` with no change in behaviour, as its own PR, so the highlights tests prove the refactor before anything new uses it.
1. **Toggle and seeing.** The toggle with its store and `default_tool`, the read-only Cursor source, joining, stages and the agent token, the editor buttons, agent branches in `startPlan` (`StartAgentBranch`, so Open locally and starts on a ticket with an agent pick up its work instead of a fresh branch), the worktree key fallback, and the README convention. Cursor IDE users are fully served at this point, and cloud agents are visible. The new write paths are local ones (the editor start and `StartAgentBranch`) plus the toggle, whose prompt can only stop tmux until phase 2.
2. **Cloud actions.** Start in Cursor (including from a worktree), Stop agent, Archive agent with the toggle's Switch and stop it, and archive in Clean up together with the rule that sends done tasks with an unarchived agent there.
3. **Follow-up.** A follow-up box on Agent waiting rows (`POST /v1/agents/{id}/runs`, 409 shown as busy), if the first two prove useful.
