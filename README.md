# taskboard

A local dashboard for multitasking across many coding-agent sessions.

With a dozen coding-agent sessions running in parallel, each in its own worktree on its own ticket, the bottleneck is no longer writing code but keeping track of it: which session is waiting on you, which plan needs review, whose CI broke, what merged and still needs its ticket closed, which worktrees can be cleaned up. taskboard collects that state from where it already lives (the ticket tracker, GitHub PRs, git worktrees, plan files and tmux windows) and shows one row per task, with the work blocked on you at the top. Each row links to the ticket, PR, plan and session, so you can go straight back to the terminal that needs you.

taskboard doesn't start or drive sessions; it watches them. The only convention it needs is the ticket key in branch and tmux window names. Jira is the only ticket tracker supported today; others, such as GitHub issues, may follow.

Stages: Ticket, Plan, Code, Review, Ready, Merged, Done. Tasks are grouped so the ones waiting on you come first: CI failing, changes requested, ready to merge, plan needs review, and merged but Jira not yet updated.

Done tasks that still have a worktree or a tmux window go to "Needs clean-up" instead of Done. Done shows tasks whose PR merged in the last `done_days` days (default 30) or whose ticket was resolved in that window (`assignee = currentUser() AND statusCategory = Done`).

## Requirements

- Go 1.22+
- `gh`, authenticated (`gh auth login`)
- `acli`, logged in to your Jira site (`acli jira auth login`)
- `git`, and `tmux` if you want session links

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

Like Open session, Settings is only available from the machine running taskboard (or through an SSH tunnel to it), since `attach_command` runs commands there.

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

## Starting sessions in tmux

Name the window after the ticket and start your coding agent in the task's worktree, replacing `<agent>` with its command:

```sh
git -C ~/src/mattermost worktree add ../mattermost-MM-1234 -b MM-1234-short-name
tmux new-window -n MM-1234 -c ~/src/mattermost-MM-1234 <agent>
```

If no tmux server is running yet, use `tmux new-session -d -s work` first.

Each session gets two links:

- **Copy session command** copies `ssh -t <ssh_host> 'tmux attach -t work:3'`. Paste it on the Mac or phone. Leave `ssh_host` empty to copy a plain `tmux attach`.
- **Open session** appears only when the page is opened from the box itself and `attach_command` is set. It runs that command on the box (by default a new gnome-terminal attached to the window). Through an SSH tunnel the browser also looks local, so this button would open the terminal on the box, not on the remote machine.

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
