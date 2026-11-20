# taskboard

[![Build](https://github.com/wiggin77/taskboard/actions/workflows/go.yml/badge.svg?branch=main)](https://github.com/wiggin77/taskboard/actions/workflows/go.yml)
[![Release](https://img.shields.io/github/v/release/wiggin77/taskboard)](https://github.com/wiggin77/taskboard/releases/latest)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

Keep a dozen parallel coding-agent sessions on track.

Each session works in its own worktree on its own ticket, and the hard part is knowing which one needs you: a plan to review, a failing CI run, a PR ready to merge, a merged ticket to close, a worktree to clean up. taskboard joins Jira tickets, GitHub PRs, plan files, git worktrees and tmux windows into one board, one row per task, with what needs you first.

From the board you can start a session for a ticket (a worktree plus a tmux window running your agent), attach to it in a browser or native terminal, approve plans, review PRs and clean up after the work merges. That works on the taskboard machine, and from another computer or your phone with a client certificate. Sessions you start yourself show up too, as long as the ticket key is in the branch and tmux window names. Jira is the only ticket tracker supported today.

## Quick start

You need `gh` (logged in with `gh auth login`), `acli` (logged in with `acli jira auth login`), `git`, and `tmux` for sessions, on Linux or macOS. On Windows, run all of it inside WSL.

1. Download the archive for your platform from the [latest release](https://github.com/wiggin77/taskboard/releases/latest) and unpack it. Or, with Go installed, run `go install github.com/wiggin77/taskboard@latest` and use `taskboard` in place of `./taskboard` below.
2. Create a config file, `taskboard.json`:

   ```sh
   ./taskboard init              # generic starting point
   ./taskboard init mattermost   # Mattermost's Jira site and repos
   ```

   With the generic one, set your Jira site, project keys and repos. With the Mattermost one, check that the repo paths match your checkouts; its Jira queries also find tickets that name you as Matty code assigner. You can change all of this later in Settings.
3. Start taskboard and open http://127.0.0.1:8484:

   ```sh
   ./taskboard
   ```

The macOS builds aren't signed. If macOS refuses to run one, clear the quarantine flag with `xattr -d com.apple.quarantine taskboard`, or install with `go install` instead.

## Using the board

Tasks move through Ticket, Plan, Code, Review, Ready, Merged and Done. The Needs you section at the top lists the tasks waiting on you: CI failing, changes requested, review requested, ready to merge (approved, with checks done and no one else still asked to review), plan needs review, and merged but Jira not yet updated. Done tasks that still have a worktree or tmux window go to Needs clean-up (a repo's main checkout on the ticket's branch doesn't count). Done shows tasks whose PR merged, or whose ticket was resolved, in the last `done_days` days (default 30).

- **Refreshing.** While a board tab is open, taskboard refreshes every `poll_interval` (default 5m) and the page reloads only when something changed. With no tab open it stops asking Jira and GitHub. The Refresh button refreshes now. Set `manual_refresh` to refresh only when you ask.
- **Highlights.** Hover over a row and click the bookmark at its left edge to highlight it. Highlighted rows are also copied into a Highlighted section at the top of the board, which the Highlighted chip jumps to. Highlights are saved next to the config file, so every device sees them.
- **Epics** have their own section listing each open epic's child tickets, including other people's. Your own children also keep their full rows in the stage sections.
- **Sections** collapse when you click their heading, and the chips at the top jump to them. Done starts collapsed.
- **Theme.** The theme button cycles between your system setting, light and dark.
- **Settings** edits every config option and applies it straight away; only `listen` needs a restart. An empty field uses the default shown as its placeholder. Saving rewrites the file in a standard layout, dropping hand formatting and unknown keys.

Starting, stopping and cleaning up sessions, approving plans, highlighting and Settings only work from a browser on the taskboard machine or one with a client certificate (see [Remote access](#remote-access)). Any other browser can only look.

## Conventions

Everything is joined on the ticket key, which is any Jira key in one of your `project_keys` (case-insensitive):

| Source | Where the key must appear |
|---|---|
| Jira | Tickets returned by `jira.jql`, plus any key found below |
| PR | Branch name, or PR title as a fallback. A PR without a key becomes its own task, shown as `repo#number` |
| Worktree | Branch name |
| Plan | File name in the repo's `plans_dir`, e.g. `plans/MM-1234.md` or `plans/2026-10-07-MM-1234-history-sync.md` |
| Session | tmux window name |

The key can be anywhere in a name, as long as no letter or digit comes right before it and no digit right after it: `fix-mm-1234-login` matches MM-1234, `xMM-1234` matches nothing. When a name has more than one key, the first one counts.

### Plans

A plan counts as approved when its front matter, starting on the file's first line, has `status: approved`:

```markdown
---
status: approved
---
# MM-1234 plan
```

Any other plan shows as **Plan needs review** until a PR is opened. When a ticket has several plans, the most recently modified one counts. The plan page renders the Markdown and has an **Approve** button that writes `status: approved` into the file for you. It refuses if the plan changed since you opened the page.

Coding agents don't know this convention. To have them follow it, add something like this to each repo's `CLAUDE.md` or `AGENTS.md`:

```markdown
Write plans to `plans/<ticket key>-<short-name>.md`, starting with front matter `status: draft`.
Change it to `status: approved` once the plan is approved.
```

## Sessions

A task with no session gets two start buttons, which run your coding agent, `agent_command` (default `claude`):

- **Start in the browser** (monitor icon) opens the session in a terminal in a new browser tab.
- **Start in a terminal** (play icon) opens it in a terminal window on the taskboard machine, using `attach_command`.

taskboard works out where the work lives:

1. If the task has a worktree, it opens a window there and runs `agent_resume_command` (default `claude --continue`, or plain `claude` where Claude has never run).
2. Otherwise, if a local branch contains the ticket key, it adds a worktree for that branch.
3. Otherwise, if the task has an open PR, it adds a worktree and runs `gh pr checkout`.
4. Otherwise it asks for the repo and a branch name, and creates the branch from the repo's `base_branch` (default: what `origin/HEAD` points at).

New worktrees go in `worktree_dir` as `<folder>-<KEY>`, named after the repo's checkout folder (e.g. `~/worktrees/mattermost-MM-1234`), so no two repos' checkout folders can share a name. They go next to the repo's checkout when `worktree_dir` is empty, and windows open in the `tmux_session` session (default `taskboard`). The agent command is typed into the window's shell, so quitting the agent leaves you at a prompt in the worktree. Start taskboard from a shell with the agent on its `PATH`; tmux inherits its environment.

Some builds expect sibling checkouts at a relative path, such as mattermost/mattermost's server looking for `../../enterprise`. List those in `worktree_links` and taskboard links each one into `worktree_dir`:

```json
"worktree_dir": "~/worktrees",
"worktree_links": ["~/src/enterprise"]
```

Once a session is running, its row gets **Open in the browser**, **Open in a terminal** and **Stop**, which closes the tmux window and keeps the worktree. Terminals open on different tasks don't switch each other's window, and closing a browser terminal's tab or detaching (Ctrl-b d) leaves the session running.

On a phone, the board uses larger buttons. On a phone or tablet, while the on-screen keyboard is open, the browser terminal adds a row of Esc, Tab, Shift-Tab, Ctrl and arrow keys above it; with a physical keyboard attached the on-screen one stays closed, and so does the row. Ctrl applies to the next key you type or arrow you tap, so Ctrl then b, d detaches.

**Clean up** (trash icon, on Needs clean-up rows) closes the session, removes the worktree and deletes its branch. If there are uncommitted changes or unpushed commits it lists them and asks first, and it always keeps a branch with unpushed commits. A repo's main checkout is never removed or switched to another branch; clean-up only closes its session.

### Reviews

PRs in the Reviews section get the same buttons when their repo has a local checkout. **Start review** checks the PR out on a `review/<N>` branch in a worktree named `<repo>-pr<N>` and starts `agent_command` there without a prompt, so you choose how to review. `git pull` picks up the author's new commits, and `git push` updates the PR when you're allowed to. **Clean up** removes the worktree and branch, keeping the branch if you have unpushed commits. A review whose PR has merged, closed or stopped requesting you stays in the section until you clean it up.

## Remote access

To use the board from another machine or your phone, such as a Mac browser with taskboard running on a Linux box, turn on the HTTPS listener. It gives browsers with a client certificate that taskboard issued everything except native terminals, which would open on the taskboard machine. Browsers without one can only pair.

Set it up under **Settings > Remote access** on the taskboard machine:

1. Enter every name and address other devices will use, such as a Tailscale name, and choose **Create certificates**. This makes a certificate authority and a server certificate in `tls_dir` (default `certs`, next to the config file).
2. Set **Remote access address** (`tls_listen`, for example `0.0.0.0:8443` or a Tailscale address), save and restart taskboard.
3. For each device, enter a name and a passphrase of at least 12 characters and choose **Add device**. Scan the QR code with the device, or open the link shown on it. Accept the certificate warning, which appears only this first time, and install the certificate as the page describes, typing the passphrase when asked. iPhones, iPads and Macs get a single profile; other systems get `taskboard-ca.crt` and a `.p12`. Then quit and reopen the browser.

The code works once, for 10 minutes. The dialog shows the address of the device that opened it; if that wasn't yours, **Revoke** cuts its certificate off at once. The passphrase is never stored or sent to the device: it only encrypts the `.p12`, so someone who intercepts the download can't use it.

A device with a certificate can add other devices too. When the machine gets a new name, change the names and choose **Re-issue server certificate**, then restart. To cut off a lost device, add its serial to `revoked_certs`. Keep `listen` on 127.0.0.1: it has no login, so other machines that reach it can only view the board.

The same steps work from the command line: `./taskboard certs init -host ubuntu,ubuntu.tailnet.ts.net,100.64.0.5`, `./taskboard certs server -host ...` and `./taskboard certs client mac-laptop`, which writes `mac-laptop.p12` and prints its password and serial. On a Mac, add `ca.crt` and the `.p12` to the login keychain:

```sh
security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db ca.crt
security import mac-laptop.p12 -k ~/Library/Keychains/login.keychain-db -P '<password>'
```

## Tab groups

The optional tab groups extension (Chrome, Edge, Firefox 140+) opens each ticket's Jira issue, PR, plan and terminal in a browser tab group named after the ticket. Review rows join the group of the ticket their PR mentions. Only plain clicks are grouped; without the extension, links open as usual.

To install it, click the puzzle-piece button in the board's header and follow the install page:

- **Chrome or Edge:** the page writes the extension into a folder you choose. Load that folder with Load unpacked in `chrome://extensions` (or `edge://extensions`) with Developer mode on. The board keeps the folder up to date when taskboard is upgraded.
- **Firefox:** load the downloaded zip under Load Temporary Add-on in `about:debugging#/runtime/this-firefox`. Release Firefox drops unsigned extensions on restart; to keep it, [sign it as an unlisted add-on](https://extensionworkshop.com/documentation/publish/signing-and-distribution-overview/).

The extension works on `http://127.0.0.1`, `http://localhost` and the address it was installed from. Add other addresses in its options.

## Notes

- Jira is read through `acli`, as whatever account it's logged in as. `jira.base_url` is only used for links. `jira.jql` picks your open tickets (default: assigned to you and not done). `jira.done_jql` says which done tickets are yours (default `assignee = currentUser()`); taskboard adds the done status and date window, so it can't have an `ORDER BY`.
- Task PRs are PRs in your `repos` that you authored or that are assigned to you, so an agent that assigns its PR back to you (such as Matty or Cursor) shows up as your task. `github.ignore_assigned_authors` skips assigned PRs from accounts such as a cherry-pick bot; use `app/<name>` for a GitHub app.
- Reviews lists open PRs in your `repos` that request your review or that you've reviewed. Other repos lists the same for every other repo, plus your own open PRs there, limited to `github.organizations` when that is set.
- `merged_pr_days` (default 90) is how far back merged PRs are checked for "merged, update Jira".
- The first refresh views each Jira ticket once (about 30s for 100 tickets); later refreshes only view tickets that changed.
- When a source fails, a banner says so and the board keeps that source's last good data, with its age in the header. When GitHub's API budget (shared with everything using your `gh` login) runs low, taskboard pauses GitHub requests until it resets.

## License

MIT. See [LICENSE](LICENSE).
