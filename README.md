# taskboard

A local dashboard that shows every in-flight task by stage, keyed by Jira ticket, with links to the ticket, PR, implementation plan and Claude Code session.

Stages: Ticket, Plan, Code, Review, Ready, Merged, Done. Tasks are grouped so the ones waiting on you come first: CI failing, changes requested, ready to merge, plan needs review, and merged but Jira not yet updated.

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

Open http://127.0.0.1:8484. The page refreshes every `poll_interval`.

## Conventions

Everything is joined on the Jira key (any key matching `project_keys`, case-insensitive):

| Source | Where the key must appear |
|---|---|
| Jira | Tickets returned by `jira.jql`, plus any key found below |
| PR | Branch name, or PR title as a fallback |
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

Name the window after the ticket and run Claude Code in the task's worktree:

```sh
git -C ~/src/mattermost worktree add ../mattermost-MM-1234 -b MM-1234-short-name
tmux new-window -n MM-1234 -c ~/src/mattermost-MM-1234 claude
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
- Jira tickets in a done status category are cached for the life of the process, since merged PRs are rechecked every poll.
- `merged_pr_limit` controls how many recent merged PRs per repo are checked for the "merged, update Jira" stage.
- Source failures show in a banner at the top of the page and in the log; the rest of the board still renders.
