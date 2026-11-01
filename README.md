# taskboard

A local dashboard for multitasking across many coding-agent sessions.

With a dozen coding-agent sessions running in parallel, each in its own worktree on its own ticket, the bottleneck is no longer writing code but keeping track of it: which session is waiting on you, which plan needs review, whose CI broke, what merged and still needs its ticket closed, which worktrees can be cleaned up. taskboard collects that state from where it already lives (the ticket tracker, GitHub PRs, git worktrees, plan files and tmux windows) and shows one row per task, with the work blocked on you at the top. Each row links to the ticket, PR, plan and session, so you can go straight back to the terminal that needs you.

taskboard can start a session for a ticket (creating its worktree and tmux window), stop it, and clean up after the work merges. Sessions you start yourself work too: the only convention it needs is the ticket key in branch and tmux window names. Jira is the only ticket tracker supported today; others, such as GitHub issues, may follow.

Stages: Ticket, Plan, Code, Review, Ready, Merged, Done. Tasks are grouped so the ones waiting on you come first: CI failing, changes requested, ready to merge, plan needs review, and merged but Jira not yet updated.

Done tasks that still have a worktree or a tmux window go to "Needs clean-up" instead of Done. Done shows tasks whose PR merged in the last `done_days` days (default 30) or whose ticket was resolved in that window (`assignee = currentUser() AND statusCategory = Done`).

## Requirements

- Go 1.22+
- `gh`, authenticated (`gh auth login`)
- `acli`, logged in to your Jira site (`acli jira auth login`)
- `git`, and `tmux` if you want session links
- Linux or macOS. On Windows, run taskboard and its tools inside WSL, since tmux has no native Windows build.

## Run

```sh
go build -o taskboard .
cp config.example.json taskboard.json   # edit repos and ssh_host
./taskboard -config taskboard.json
```

Open http://127.0.0.1:8484. While the page has been viewed in the last 15 minutes, the server polls every `poll_interval` (default 2m). While its tab is visible, the page checks every 15 seconds whether the board changed and reloads only if it did (and at least hourly, so ages stay current). Otherwise the server polls every `idle_poll_interval`, or not at all when it is 0 (the default), and the first view after that refreshes right away.

The Refresh button refreshes on demand, so a long `poll_interval` works if you would rather refresh by hand. Pressing it during a refresh waits for that one instead of starting another. A refresh that takes longer than `refresh_timeout` (default 5m) is cut short and reports the sources it could not read.

The theme button (on every page) cycles between matching the system's light or dark setting (the default), light and dark. The choice is saved in the browser, so each browser keeps its own.

## Settings

The Settings link in the header opens a form for every config option. Saving checks the config, writes it back to the `-config` file and applies it right away, then refreshes the board; only `listen` waits for a restart. Saving rewrites the file in a standard layout, so hand formatting and unknown keys are not kept.

Like the session buttons, Settings is only available from the machine running taskboard (or through an SSH tunnel to it), since `attach_command` and `agent_command` run commands there.

## Conventions

Everything is joined on the ticket key, currently any Jira key matching `project_keys` (case-insensitive):

| Source | Where the key must appear |
|---|---|
| Jira | Tickets returned by `jira.jql`, plus any key found below |
| PR | Branch name, or PR title as a fallback. A PR without a key becomes its own task, shown as `repo#number` with no Jira link |
| Worktree | Branch name |
| Plan | File name in `plans_dir`, e.g. `plans/MM-1234.md` |
| Session | tmux window name |

Plans are read from every worktree of each configured repo. A plan counts as approved when its front matter has `status: approved`:

```markdown
---
status: approved
---
# MM-1234 plan
```

Plan files only attach to tasks found through Jira, a PR, a worktree or a session, so old plans for closed tickets stay off the board.

## Sessions

Set `agent_command` (for example `claude`) to get a **Start session** button (the play icon) on every task that hasn't merged and has no session. Pressing it works out where the work lives:

1. If the task already has a worktree, it opens a window there and types `agent_resume_command` (for example `claude --continue`; defaults to `agent_command`).
2. Otherwise, if a local branch contains the ticket key, it adds a worktree for that branch.
3. Otherwise, if the task has an open PR in a repo with a local checkout, it adds a worktree and runs `gh pr checkout`, which also handles PRs from forks.
4. Otherwise it asks for the repo (when more than one has a local checkout) and a branch name, suggesting `MM-1234-summary-words`. It then fetches the repo's `base_branch` (default: whatever `origin/HEAD` points at) and creates the branch from it, without tracking it.

New worktrees go in `worktree_dir` as `<repo>-<KEY>`, for example `~/worktrees/mattermost-MM-1234`. When `worktree_dir` is empty, each goes next to its repo's checkout. The window is named after the ticket and opened in the `tmux_session` session (default `work`), which is started if no tmux server is running. The agent command is typed into the window's shell, so quitting the agent leaves you at a prompt in the worktree. `{key}` in either command becomes the ticket key. If `attach_command` is set, a terminal attached to the new window opens right away.

A tmux server that taskboard starts inherits taskboard's environment, so start taskboard from a shell that has the agent on its `PATH`.

Some builds expect sibling checkouts at a relative path. For example, mattermost/mattermost's server build looks for `../../enterprise`, and the plugins look for `../mattermost-utilities`. List those checkouts in `worktree_links`, and before each new worktree is created, taskboard links each one into `worktree_dir` under its own name:

```json
"worktree_dir": "~/worktrees",
"worktree_links": ["~/src/enterprise", "~/src/mattermost-utilities"]
```

Because every worktree sits one level down in `worktree_dir`, one link per checkout serves them all. taskboard never replaces an existing file or link with that name; it stops and reports the conflict instead.

**Stop session** (the circle icon next to a session) closes the task's tmux window after you confirm. The worktree is kept.

**Clean up** (the trash icon on Needs clean-up rows) closes the session, removes the worktree with `git worktree remove` and deletes its branch. If the worktree has uncommitted changes or commits that aren't on any remote, it lists them and asks before going ahead. Even then it keeps a branch that has unpushed commits, so only the uncommitted changes are lost. Commits whose branch GitHub deleted after a merge still count as pushed when the worktree is at the merged PR's head. A repo's main checkout is never removed.

### Reviews

PRs in the Reviews section get the same buttons when their repo has a local checkout:

- **Start review** fetches the PR's head (`refs/pull/N/head`, which also covers PRs from forks) as `origin/pr/N`, adds a worktree at `<worktree_dir>/<repo>-pr<N>`, and opens a tmux window with the same name. In the worktree, `gh pr checkout --branch review/<N>` puts the PR on a `review/<N>` branch that tracks it, so `git pull` picks up the author's new commits, and `git push` updates the PR when you're allowed to (for a fork, only if the author allows edits by maintainers). It types `agent_command` into the window without asking for a review, so you can pick the model, effort and review command yourself. **Resume review** reopens an existing review worktree with `agent_resume_command`. PR heads come from the remote in the repo's `base_branch` (default `origin`).
- **Stop** and **Clean up** work as they do for tasks. Clean up re-fetches the PR head, so commits you pulled from the PR count as pushed and only your own unpushed edits trigger the confirmation. It then removes the worktree, the `origin/pr/N` ref and the `review/<N>` branch. It keeps that branch if it has unpushed commits, and keeps any other branch you switched to in the worktree. Starting the review again reuses a kept `review/<N>` branch.
- A review worktree or window whose PR has left the Reviews list (merged, closed, or no longer requesting you) stays in the section, labelled with what happened to the PR, so it can still be cleaned up.

Review windows and worktrees contain no ticket key, so they never show up as your tasks.

To start sessions by hand instead, name the window after the ticket and start your agent in the task's worktree:

```sh
git -C ~/src/mattermost worktree add ../mattermost-MM-1234 -b MM-1234-short-name
tmux new-window -n MM-1234 -c ~/src/mattermost-MM-1234 <agent>
```

Each session gets two links:

- **Copy session command** (the copy icon) copies `ssh -t <ssh_host> 'tmux attach -t work:3'`. Paste it on the Mac or phone. Leave `ssh_host` empty to copy a plain `tmux attach`.
- **Open session** (the terminal icon) appears only when the page is opened from the box itself and `attach_command` is set. It runs that command on the box (by default a new gnome-terminal attached to the window). Through an SSH tunnel the browser also looks local, so this button would open the terminal on the box, not on the remote machine.

## Reaching it from other devices

The server has no authentication, so keep `listen` on 127.0.0.1 and tunnel:

```sh
ssh -N -L 8484:127.0.0.1:8484 ubuntu
```

If you bind to a LAN or Tailscale address instead, requests by IP work as is. Add any hostname you use (for example a Tailscale MagicDNS name) to `allowed_hosts`; other hostnames are rejected to block DNS rebinding.

## Notes

- Jira is read through `acli`, so taskboard uses whatever account acli is logged in as. `jira.base_url` is only used to build ticket links.
- Within each section, items are sorted by stage, then most recently updated first. A task's date is the later of its Jira ticket's and its PR's last update. Dates turn orange after 2 days and red after a week.
- `acli` search cannot return a ticket's updated date, so the first refresh fetches each ticket with `acli jira workitem view` (about 30s for 100 tickets). Later refreshes only re-fetch tickets that a search reports as changed since the last check.
- Jira tickets in a done status category are cached for 24 hours, since merged PRs are rechecked every poll. So are tickets acli cannot find (deleted, or not visible to you); a merged PR whose ticket cannot be found counts as Done.
- The Reviews section lists open PRs in the configured repos where your review is requested, then ones you have already reviewed (GitHub drops the request once you submit a review). Other repos lists the same for every repo not in `repos`, plus open PRs you authored there. Neither section is joined on Jira keys.
- `merged_pr_limit` controls how many recent merged PRs per repo are checked for the "merged, update Jira" stage. It is independent of `done_days`, so a merged PR older than the Done window still shows when its ticket needs updating.
- Source failures show in a banner at the top of the page and in the log; the rest of the board still renders.

## License

MIT. See [LICENSE](LICENSE).
