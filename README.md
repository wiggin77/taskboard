# taskboard

A local dashboard for multitasking across many coding-agent sessions.

With a dozen coding-agent sessions running in parallel, each in its own worktree on its own ticket, the bottleneck is no longer writing code but keeping track of it: which session is waiting on you, which plan needs review, whose CI broke, what merged and still needs its ticket closed, which worktrees can be cleaned up. taskboard collects that state from where it already lives (the ticket tracker, GitHub PRs, git worktrees, plan files and tmux windows) and shows one row per task, with the work blocked on you at the top. Each row links to the ticket, PR, plan and session, so you can go straight back to the terminal that needs you.

taskboard can start a session for a ticket (creating its worktree and tmux window), stop it, and clean up after the work merges. Sessions you start yourself work too: the only convention it needs is the ticket key in branch and tmux window names. Jira is the only ticket tracker supported today; others, such as GitHub issues, may follow.

Stages: Ticket, Plan, Code, Review, Ready, Merged, Done. Tasks are grouped so the ones waiting on you come first: CI failing, changes requested, ready to merge, plan needs review, and merged but Jira not yet updated.

Done tasks that still have a worktree or a tmux window go to "Needs clean-up" instead of Done. Done shows tasks whose PR merged in the last `done_days` days (default 30) or whose ticket was resolved in that window (`assignee = currentUser() AND statusCategory = Done`).

## Requirements

- Go 1.27+
- `gh`, authenticated (`gh auth login`)
- `acli`, logged in to your Jira site (`acli jira auth login`)
- `git`, and `tmux` if you want sessions
- Linux or macOS. On Windows, run taskboard and its tools inside WSL, since tmux has no native Windows build.

## Run

```sh
go build -o taskboard .
cp config.example.json taskboard.json   # edit jira, project_keys and repos
./taskboard -config taskboard.json
```

Open http://127.0.0.1:8484. While the page has been viewed in the last 15 minutes, the server polls every `poll_interval` (default 2m). While its tab is visible, the page checks every 15 seconds whether the board changed and reloads only if it did (and at least hourly, so ages stay current). Otherwise the server polls every `idle_poll_interval`, or not at all when it is 0 (the default), and the first view after that refreshes right away.

The Refresh button refreshes on demand, so a long `poll_interval` works if you would rather refresh by hand. Pressing it during a refresh waits for that one instead of starting another. A refresh that takes longer than `refresh_timeout` (default 5m) is cut short and reports the sources it could not read.

To keep track of the items you're working on right now, highlight them with the bookmark in the left edge of a row; it appears when you hover over the row. Highlighted rows are tinted, and the Highlighted chip at the top jumps to each one in turn. Highlights are saved on the server in `taskboard-highlights.json`, next to the config file, so they survive restarts and show on every device you use the board from; other open pages pick up a change within 15 seconds. A highlight is dropped once its item leaves the board. Like the session buttons, highlighting needs a browser on the taskboard machine or a client certificate; other browsers see highlights but can't change them.

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

Every task that hasn't merged and has no session gets start buttons. They start your coding agent, `agent_command`, which defaults to `claude`:

- **Start in the browser** (the monitor icon) opens a new tab with the session in a terminal in the browser. It's offered to a browser on the taskboard machine and to [remote](#remote-access) browsers.
- **Start in a terminal** (the play icon) opens the session in a terminal window on the taskboard machine, using `attach_command`. It's only offered to a browser on that machine, and only when `attach_command` is set.

Either way, taskboard works out where the work lives:

1. If the task already has a worktree, it opens a window there and types `agent_resume_command` (default `claude --continue`, or `agent_command` if you changed that).
2. Otherwise, if a local branch contains the ticket key, it adds a worktree for that branch.
3. Otherwise, if the task has an open PR in a repo with a local checkout, it adds a worktree and runs `gh pr checkout`, which also handles PRs from forks.
4. Otherwise it asks for the repo (when more than one has a local checkout) and a branch name, suggesting `MM-1234-summary-words`. It then fetches the repo's `base_branch` (default: whatever `origin/HEAD` points at) and creates the branch from it, without tracking it.

Each repo's `path` is its local checkout. `repos_dir` (default `~/src`) is the folder that holds checkouts, so a `path` without a leading `/` or `~/`, such as `mattermost`, means `<repos_dir>/mattermost`. A full path or a `~/` path is used as written, and an empty `path` means the repo has no local checkout; the board never scans `repos_dir` for it. `repos_dir` must be a full path or start with `~/`. The settings page shows, and saves, the short form for any checkout inside `repos_dir`. When `worktree_dir` is empty, "next to its repo's checkout" means next to the resolved path.

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

Rows with a session get matching open buttons, plus Stop:

- **Open in the browser** (the window icon) opens `/term/<key>` in a tab of its own, one per session. The terminal shows the full tmux window, and closing the tab or detaching (Ctrl-b d) leaves the session running. If the connection drops, the page offers Reconnect. When the session itself ends, for example after you quit the agent and `exit` the shell, the board refreshes right away and the page offers Resume, which starts a new window in the same worktree. With tmux mouse mode on, hold Option while dragging to select text on a Mac, or Shift elsewhere.
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

## Notes

- Jira is read through `acli`, so taskboard uses whatever account acli is logged in as. `jira.base_url` is only used to build ticket links.
- Within each section, items are sorted by stage, then most recently updated first. A task's date is the later of its Jira ticket's and its PR's last update. Dates turn orange after 2 days and red after a week.
- `acli` search cannot return a ticket's updated date, so the first refresh fetches each ticket with `acli jira workitem view` (about 30s for 100 tickets). Later refreshes only re-fetch tickets that a search reports as changed since the last check.
- Jira tickets in a done status category are cached for 24 hours, since merged PRs are rechecked every poll. So are tickets acli cannot find (deleted, or not visible to you); a merged PR whose ticket cannot be found counts as Done.
- The Reviews section lists open PRs in the configured repos where your review is requested, then ones you have already reviewed (GitHub drops the request once you submit a review). Other repos lists the same for every repo not in `repos`, plus open PRs you authored there. `github.organizations` limits Other repos to the organizations (or user accounts) it lists, for example `"github": {"organizations": ["mattermost", "mattermost-community"]}`; PRs in any other organization are hidden, including direct review requests. An empty list shows every repo, and repos in `repos` are never filtered. The filter runs after the search, and each search returns at most 100 PRs, so hidden PRs still count toward that limit. On Other repos, a local browser or a client certificate also shows a plus-circle icon left of each repo name. One click adds the repo to `repos` with its checkout in `repos_dir` (saved as the repo's folder name), and the repo's PRs leave Other repos. When `<repos_dir>/<name>` is missing, taskboard clones it there with `gh repo clone`, for up to 30 minutes, into a hidden `.<name>.clone-*` folder that is renamed on success and removed on failure; a later add also removes such folders left by a killed taskboard once they are older than 30 minutes. When the folder exists, taskboard uses it only if it is a git checkout whose `origin` is that repo on github.com; otherwise it refuses and changes nothing. On Reviews, a PR in a repo whose configured folder is missing shows a **Clone repo** button (with a GitHub icon) instead of the review buttons. A task row that has a PR in such a repo, with no worktree or session, shows the same button instead of the start buttons, including a PR row without a ticket; it runs the same clone as the add action into that folder, and a folder that exists must be a checkout of the repo. Neither section is joined on Jira keys, but each row links to the ticket whose key appears in the PR title, or failing that the description.
- A task row for a PR without a ticket (not done) shows a **Create Jira ticket** button for a local browser or a client certificate. It runs `acli jira workitem create` in the first `project_keys` project (type Task, assigned to you, summary from the PR title), then prefixes the PR title with the new key using `gh pr edit`. The next refresh joins the PR to the ticket, so the start buttons appear. If the rename fails, the ticket still exists and the error names its key.
- `merged_pr_limit` controls how many recent merged PRs per repo are checked for the "merged, update Jira" stage. It is independent of `done_days`, so a merged PR older than the Done window still shows when its ticket needs updating.
- Source failures show in a banner at the top of the page and in the log; the rest of the board still renders.

## License

MIT. See [LICENSE](LICENSE).
