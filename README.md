# taskboard

[![Build](https://github.com/wiggin77/taskboard/actions/workflows/go.yml/badge.svg?branch=main)](https://github.com/wiggin77/taskboard/actions/workflows/go.yml)
[![Release](https://img.shields.io/badge/release-v0.1.0-blue)](https://github.com/wiggin77/taskboard/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

A local dashboard for multitasking across many coding-agent sessions.

With a dozen coding-agent sessions running in parallel, each in its own worktree on its own ticket, the bottleneck is no longer writing code but keeping track of it: which session is waiting on you, which plan needs review, whose CI broke, what merged and still needs its ticket closed, which worktrees can be cleaned up. taskboard collects that state from where it already lives (the ticket tracker, GitHub PRs, git worktrees, plan files and tmux windows) and shows one row per task, with the work blocked on you at the top. Each row links to the ticket, PR, plan and session, so you can go straight back to the terminal that needs you.

taskboard can start a session for a ticket (creating its worktree and tmux window), stop it, and clean up after the work merges. Sessions you start yourself work too: the only convention it needs is the ticket key in branch and tmux window names. Jira is the only ticket tracker supported today; others, such as GitHub issues, may follow.

Stages: Ticket, Plan, Code, Review, Ready, Merged, Done. Tasks are grouped so the ones waiting on you come first: CI failing, changes requested, ready to merge, plan needs review, and merged but Jira not yet updated.

Done tasks that still have a worktree or a tmux window go to "Needs clean-up" instead of Done. Done shows tasks whose PR merged in the last `done_days` days (default 30) or whose ticket was resolved in that window (`assignee = currentUser() AND statusCategory = Done`).

## Requirements

- Go 1.27+, only to build from source
- `gh`, authenticated (`gh auth login`)
- `acli`, logged in to your Jira site (`acli jira auth login`)
- `git`, and `tmux` if you want sessions
- Linux or macOS. On Windows, run taskboard and its tools inside WSL, since tmux has no native Windows build.

## Run

Download the archive for your platform (Linux or macOS, amd64 or arm64) from the [latest release](https://github.com/wiggin77/taskboard/releases/latest) and unpack it, or build from source with `go build -o taskboard .`. Then:

```sh
cp config.example.json taskboard.json   # edit jira, project_keys and repos
./taskboard -config taskboard.json
```

The macOS builds aren't signed. If macOS refuses to open a binary downloaded with a browser, clear the quarantine flag with `xattr -d com.apple.quarantine taskboard`.

Open http://127.0.0.1:8484. While the page has been viewed in the last 15 minutes, the server polls every `poll_interval` (default 2m). While its tab is visible, the page checks every 15 seconds whether the board changed and reloads only if it did (and at least hourly, so ages stay current). Otherwise the server polls every `idle_poll_interval`, or not at all when it is 0 (the default), and the first view after that refreshes right away.

The Refresh button refreshes on demand, so a long `poll_interval` works if you would rather refresh by hand. Pressing it during a refresh waits for that one instead of starting another. A refresh that takes longer than `refresh_timeout` (default 5m) is cut short and reports the sources it could not read.

To keep track of the items you're working on right now, highlight them with the bookmark in the left edge of a row; it appears when you hover over the row. Highlighted rows are tinted, and the Highlighted chip at the top jumps to each one in turn. Highlights are saved on the server in `taskboard-highlights.json`, next to the config file, so they survive restarts and show on every device you use the board from; other open pages pick up a change within 15 seconds. A highlight is dropped once its item leaves the board. Like the session buttons, highlighting needs a browser on the taskboard machine or a client certificate; other browsers see highlights but can't change them.

Click a section's heading to collapse or expand it; the browser remembers your choice. Done starts collapsed and the others start expanded. The section chips at the top jump to their section and expand it.

Epics are marked with an Epic badge and get their own Epics section, just above Not started, listing each open epic's child tickets, including ones assigned to other people. An epic with no plan, PR, worktree or session of its own shows only there; one with work of its own also stays in its stage section, and a Done epic stays in Done with no group. Your own children keep their full rows, with their buttons, in the stage sections; under the epic they are compact rows, and clicking one jumps to its full row. Other people's children are read-only and show their Jira status and assignee. A row whose ticket belongs to an epic links to the epic's group, or to the epic in Jira when it has none. Epic groups start collapsed, and the browser remembers which ones you expanded.

The theme button (on every page) cycles between matching the system's light or dark setting (the default), light and dark. The choice is saved in the browser, so each browser keeps its own.

## Settings

The Settings link in the header opens a form for every config option. Saving checks the config, writes it back to the `-config` file and applies it right away, then refreshes the board; only `listen` waits for a restart. Saving rewrites the file in a standard layout, so hand formatting and unknown keys are not kept. A field left empty uses the default shown as its placeholder, and the default isn't written to the file; placeholders starting with "e.g." are only examples.

Like the session buttons, Settings is only available to a browser on the machine running taskboard or one with a client certificate (see [Remote access](#remote-access)), since `attach_command` and `agent_command` run commands there.

## Conventions

Everything is joined on the ticket key, currently any Jira key matching `project_keys` (case-insensitive):

| Source | Where the key must appear |
|---|---|
| Jira | Tickets returned by `jira.jql`, plus any key found below |
| PR | Branch name, or PR title as a fallback. A PR without a key becomes its own task, shown as `repo#number` with no Jira link |
| Worktree | Branch name |
| Plan | File name in `plans_dir`, e.g. `plans/MM-1234.md` or `plans/2026-10-07-MM-1234-history-sync.md` |
| Session | tmux window name |

The key can be anywhere in a name, as long as no letter or digit comes right before it and no digit right after it. `fix-mm-1234-login` matches MM-1234, but `xMM-1234` matches nothing and `MM-12345` is a different ticket. When a name has more than one key, the first one counts.

Plans are read from every worktree of each configured repo. A plan counts as approved when its front matter has `status: approved`:

```markdown
---
status: approved
---
# MM-1234 plan
```

The front matter must start on the file's first line. The value is case-insensitive and may be quoted. Any other plan, including one with no front matter, shows as **Plan needs review** until a PR is opened or the ticket is done. When a ticket has several plans, the most recently modified one counts.

This is taskboard's own convention, and coding agents don't know it. The plan page has an **Approve** button that writes it for you: it sets `status: approved` in the file's front matter, adding the field or the whole front matter block if they're missing, and leaves the rest of the file as it is. If the agent has changed the plan since you opened the page, it refuses and asks you to reload and review it again. Like the session buttons, Approve is only offered to a browser on the taskboard machine or a [remote](#remote-access) one. The change is an ordinary edit in the worktree, so it lands in the branch if the plan is committed.

To have the agent mark plans itself as well, tell it in each repo's instructions (`CLAUDE.md`, `AGENTS.md` or similar) where to put plans and how to mark them, for example:

```markdown
Write plans to `plans/<ticket key>-<short-name>.md`, starting with front matter `status: draft`.
Change it to `status: approved` once the plan is approved.
```

Plan files only attach to tasks found through Jira, a PR, a worktree or a session, so old plans for closed tickets stay off the board. A row's Plan link opens the plan in a tab of its own, rendered as GitHub-flavored Markdown. The plan's status is shown at the top (green when approved, orange otherwise), along with any other front matter fields; raw HTML in the file is not shown.

## Sessions

Every task that hasn't merged and has no session gets start buttons. They start your coding agent, `agent_command`, which defaults to `claude`:

- **Start in the browser** (the monitor icon) opens a new tab with the session in a terminal in the browser. It's offered to a browser on the taskboard machine and to [remote](#remote-access) browsers.
- **Start in a terminal** (the play icon) opens the session in a terminal window on the taskboard machine, using `attach_command`. It's only offered to a browser on that machine, and only when `attach_command` is set.

Either way, taskboard works out where the work lives:

1. If the task already has a worktree, it opens a window there and types `agent_resume_command` (default `claude --continue`, or `agent_command` if you changed that). With the default, a worktree Claude has never run in, such as one another agent created, gets `claude` instead, since there is no conversation to continue.
2. Otherwise, if a local branch contains the ticket key, it adds a worktree for that branch.
3. Otherwise, if the task has an open PR in a repo with a local checkout, it adds a worktree and runs `gh pr checkout`, which also handles PRs from forks.
4. Otherwise it asks for the repo (when more than one has a local checkout) and a branch name, suggesting `MM-1234-summary-words`. It then fetches the repo's `base_branch` (default: whatever `origin/HEAD` points at) and creates the branch from it, without tracking it.

New worktrees go in `worktree_dir` as `<repo>-<KEY>`, for example `~/worktrees/mattermost-MM-1234`. When `worktree_dir` is empty, each goes next to its repo's checkout. The window is named after the ticket and opened in the `tmux_session` session (default `taskboard`), which is created if it does not exist. The agent command is typed into the window's shell, so quitting the agent leaves you at a prompt in the worktree. `{key}` in either command becomes the ticket key. If `attach_command` is set, a terminal attached to the new window opens right away.

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

Rows with a session get matching open buttons, plus Stop:

- **Open in the browser** (the window icon) opens `/term/<key>` in a tab of its own, one per session. The terminal shows the full tmux window, and closing the tab or detaching (Ctrl-b d) leaves the session running. While its tab is hidden the terminal pauses, and tmux redraws it when you come back. If the connection drops, the page offers Reconnect. When the session itself ends, for example after you quit the agent and `exit` the shell, the board refreshes right away and the page offers Resume, which starts a new window in the same worktree. With tmux mouse mode on, hold Option while dragging to select text on a Mac, or Shift elsewhere.
- **Open in a terminal** (the terminal icon) runs `attach_command` on the taskboard machine, with the same rules as Start in a terminal.

Both attach through a new linked tmux session, which shares the windows of the task's session but has its own current window. Several terminals can then be attached at once, each on a different task, without switching each other's window. The linked session closes when its terminal detaches, and the board ignores linked sessions. For `attach_command`, taskboard creates the linked session first and sets `{target}` to its name, so the usual `tmux attach -t {target}` works (by default in a new gnome-terminal). A linked session whose terminal never attaches is removed on a later refresh.

## Remote access

To use the board from another machine, for example a browser on a Mac with taskboard running on an Ubuntu box, turn on the HTTPS listener. It requires a client certificate that taskboard issued, so only your devices get in, and it covers the board, the session buttons, Settings and the browser terminal. Native terminals stay local: a remote browser never gets Start or Open in a terminal, since those would open on the taskboard machine.

1. Create a certificate authority and a server certificate for every name and address browsers will use. Certificates go in `tls_dir` (default `certs`, next to the config file):

   ```sh
   ./taskboard certs init -host ubuntu,ubuntu.tailnet.ts.net,100.64.0.5
   ```

2. Issue a client certificate for each device. It's written as a password-protected `.p12` file, and the command prints the password and the certificate's serial:

   ```sh
   ./taskboard certs client mac-laptop
   ```

3. Copy `ca.crt` and `mac-laptop.p12` to the Mac and add both to your **login** keychain. Chrome, Edge and Safari all read it. Don't use System, which would trust the CA for every account on the Mac, or iCloud, which doesn't sync trust settings. `ca.crt` is taskboard's certificate authority: trusting it lets the browser accept taskboard's HTTPS certificate. The `.p12` is this device's client certificate, which proves to taskboard that the browser is yours.

   - **In Keychain Access:** double-click `ca.crt` and choose the login keychain. Then find "taskboard CA" in login > Certificates, open it, expand Trust, set "When using this certificate" to **Always Trust**, and close the window (macOS asks for your password). Double-click `mac-laptop.p12`, choose login, and enter the password `certs client` printed.
   - **Or in Terminal:**

     ```sh
     security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db ca.crt
     security import mac-laptop.p12 -k ~/Library/Keychains/login.keychain-db -P '<password>'
     ```

   The first time a browser uses the certificate, macOS asks whether it may use the key. Choose Always Allow. Firefox and iOS have their own certificate stores.
4. Set `tls_listen` (for example `0.0.0.0:8443`, or the Tailscale address) and restart taskboard. Then open `https://ubuntu:8443`. The browser asks which certificate to use the first time.

A browser without a valid certificate gets a TLS error rather than a page. Names the server certificate covers are accepted, so `allowed_hosts` only matters for the plain `listen` address. When the box gets a new name, re-issue the server certificate with `./taskboard certs server -host ...` and restart; client certificates stay valid. To cut off a lost device, add its serial to `revoked_certs`; that takes effect immediately, even on open connections.

Keep `listen` on 127.0.0.1. It has no login, so a browser that reaches it from another machine can only view the board.

## Tab groups

The board opens each Jira ticket, PR, plan and browser terminal in a tab of its own, and clicking a link again goes back to its tab. With the taskboard tab groups extension in `extension/` (also included in the release archives), those tabs open in a browser tab group named after the ticket, so a ticket's Jira issue, PR, plan and terminal sit together. It works in Chrome, Edge and Firefox 140 or later.

- A task's links, its plan and its terminal go in the ticket's group. A PR without a ticket gets a group named `repo#number`.
- A Reviews or Other repos row joins the group of the ticket its PR mentions, so reviewing a PR for MM-1234 puts the PR and the review terminal next to the MM-1234 tabs.
- An epic link opens in the epic's group, and the Jira links on the Epics section's child rows open in each child's group.
- Groups are found by name, so groups you make yourself or that the browser restores after a restart are reused. A tab you move to another group stays there.
- Only plain clicks are grouped. Middle-click and Ctrl/Cmd-click open a tab as usual. Without the extension, links open exactly as they always have.
- Native terminals (`attach_command`) aren't browser tabs, so they can't be grouped.

To install, click the puzzle-piece button in the board's header. It works the same whether the browser runs on the taskboard machine or on another one over the [HTTPS listener](#remote-access). The extension isn't in any web store, so Chrome and Edge need Developer mode once:

- **Chrome or Edge:** choose a folder on the install page and the page writes a `taskboard-tab-groups` folder there (this uses the browser's File System Access API, so the board must be on `http://127.0.0.1`, `http://localhost` or HTTPS). Then open `chrome://extensions` (`edge://extensions`), turn on Developer mode, choose Load unpacked and select that folder.
- **Firefox:** release Firefox removes unsigned extensions when it restarts. For a quick try, download `taskboard-tab-groups.zip` from the install page and pick it under Load Temporary Add-on in `about:debugging#/runtime/this-firefox`. To keep it installed, sign it as an unlisted add-on with your own [addons.mozilla.org API key](https://addons.mozilla.org/developers/addon/api/key/) and install the `.xpi` it produces: `npx web-ext sign -s extension --channel=unlisted --api-key=... --api-secret=...`.
- **Anything else:** the install page also offers the zip, which Chrome and Edge load unpacked once it's unzipped. The `extension` folder in the repo and the release archives works too.

The header button disappears once the extension is active on the board. When taskboard is upgraded with a newer extension, the board rewrites the folder it installed into and the extension reloads itself; the button turns orange if that couldn't happen (the browser wants permission to edit the folder again, or the extension was loaded from somewhere else) and leads to the install page's Update button. A zip install has to be downloaded and loaded again.

The extension works on taskboard pages at `http://127.0.0.1` and `http://localhost` on any port, and on the address it was installed from (the board writes it into the extension's manifest). To use it at any other address, add that address in the extension's options and allow access when the browser asks, then reload the board.

## Notes

- Jira is read through `acli`, so taskboard uses whatever account acli is logged in as. `jira.base_url` is only used to build ticket links.
- Within each section, items are sorted by stage, then most recently updated first. A task's date is the later of its Jira ticket's and its PR's last update. Dates turn orange after 2 days and red after a week.
- `acli` search cannot return a ticket's updated date, so the first refresh fetches each ticket with `acli jira workitem view` (about 30s for 100 tickets). Later refreshes only re-fetch tickets that a search reports as changed since the last check.
- Each refresh runs one extra Jira search per open epic to list its children. Ticket parents come from the same `view` call as updated dates, since `acli` search cannot return them either.
- Jira tickets in a done status category are cached for 24 hours, since merged PRs are rechecked every poll. So are tickets acli cannot find (deleted, or not visible to you); a merged PR whose ticket cannot be found counts as Done.
- The Reviews section lists open PRs in the configured repos where your review is requested, then ones you have already reviewed (GitHub drops the request once you submit a review). Other repos lists the same for every repo not in `repos`, plus open PRs you authored there. `github.organizations` limits Other repos to the organizations (or user accounts) it lists, for example `"github": {"organizations": ["mattermost", "mattermost-community"]}`; PRs in any other organization are hidden, including direct review requests. An empty list shows every repo, and repos in `repos` are never filtered. Neither section is joined on Jira keys, but each row links to the ticket whose key appears in the PR title, or failing that the description.
- `merged_pr_limit` controls how many recent merged PRs per repo are checked for the "merged, update Jira" stage. It is independent of `done_days`, so a merged PR older than the Done window still shows when its ticket needs updating.
- Source failures show in a banner at the top of the page and in the log; the rest of the board still renders.

## Releasing

Push a tag like `v0.2.0`. The Release workflow runs [GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`), which runs the tests, builds Linux and macOS binaries for amd64 and arm64, and publishes them with a checksums file and generated notes as a GitHub release. To try the build locally without publishing, run `goreleaser release --snapshot --clean`; the archives land in `dist/`.

After a release, update the version in the Release badge at the top of this file.

## License

MIT. See [LICENSE](LICENSE).
