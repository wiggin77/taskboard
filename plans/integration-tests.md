# Integration tests

Status: planned, not started.

## Why

`taskboard_test.go` covers the pure logic and the HTTP and TLS boundaries without running any external tool. Everything that drives git or tmux has no automated tests: about 40 `runCmd`/`exec.Command` call sites plus the PTY bridge. So far it has only been checked by hand, with throwaway repos, a separate tmux server, Node WebSocket scripts and Chrome. That's how the dropped-last-window bug in `parseWindows` shipped in phase 2: only an end-to-end run with several windows would have caught it. These tests turn those hand checks into code that runs locally and in CI.

## Shape

- A new `integration_test.go` behind `//go:build integration`, run with `go test -tags integration ./...`. Plain `go test ./...` stays tool-free, as CLAUDE.md requires.
- The tests need `git` and `tmux` on `PATH`, and skip with a clear message when either is missing. `gh` and `acli` are never called: `gh` is faked (below), and tests set `board.snap` directly rather than running `Board.refresh`.
- They don't use `t.Parallel()`, since the harness uses `t.Setenv` and one tmux server per test.

## Harness

`newEnv(t)` returns a struct with the config, board, server handler and helpers:

- **Isolation from the real machine.**
  - `t.Setenv("TMUX", "")` (unset), and `t.Setenv("TMUX_TMPDIR", dir)`, where `dir` comes from `os.MkdirTemp("/tmp", "tbt")`. Unix socket paths are limited to about 104 bytes, and on macOS `t.TempDir()` paths are too long for tmux's socket.
  - A cleanup hook runs `tmux kill-server` and removes the directory.
  - Git identity comes from `GIT_AUTHOR_*`/`GIT_COMMITTER_*` env vars, so CI runners need no git config.
  - Worktree and link paths are absolute temp paths, so `HOME` is never touched.
- **Repos.** A bare `origin.git` with `main`, a clone as the repo checkout, `origin/HEAD` set, and a sibling folder to use as a `worktree_links` target. Helpers:
  - `pushPR(n, files)` creates a commit and pushes it to `refs/pull/<n>/head` on origin, the way GitHub exposes PR heads;
  - `commit(dir, msg)` and `dirty(dir)` make worktrees unclean.
- **Fake `gh`.** A shell script prepended to `PATH` that implements only `gh pr checkout <n> --repo <r> [--branch <b>]`: fetch `refs/pull/<n>/head`, create the branch (default: the head ref name, recorded by `pushPR`), and set its upstream like real `gh` does for a same-repo PR (`branch.<b>.remote origin`, `branch.<b>.merge refs/heads/<head>`). Any other invocation exits non-zero, so an unexpected `gh` call fails the test loudly.
- **Agent command.** `printf 'agent %s\n' {key}` and `printf 'resumed\n'`, checked with `tmux capture-pane -p`.
- **Fake terminal client.** Tests attach with `creack/pty` from Go (`tmux attach-session -t =<name>` under a PTY) instead of `script`, whose arguments differ between macOS and Linux. Detaching uses `tmux detach-client -s <name>`.
- **`eventually(t, timeout, cond)`** polls for asynchronous tmux and hook effects, never using fixed sleeps.

## Small code changes to allow testing

- `linkedGrace` becomes a package variable, so the sweep test can shorten it instead of waiting two minutes. `staleLinked` already takes `now`.
- Nothing else is expected. Functions are exercised either directly (`startSession`, `cleanup`, `startReview`, `cleanupReview`, `openLinked`, `sweepLinked`, `listWindows`) or through the HTTP handler with a hand-built snapshot.

## Tests

### tmux and linked sessions

- `listWindows` with several windows returns all of them, including the last, and skips windows of a session marked `@taskboard`. This is the regression for the phase 2 bug, run against real tmux output.
- `openLinked` creates a marked session showing the requested window, and the original session's current window doesn't move.
- A linked session is destroyed when its attached client detaches; this checks the `client-attached` hook.
- `sweepLinked` kills a marked, never-attached session older than the grace period, and leaves attached and unmarked sessions alone.
- `attach_command` via `POST /attach/{key}`: a recording command (`sh -c 'echo "$0" > file' {target}`) receives a marked linked session's name, not the window target.

### Task sessions

- **Start, new branch:** the worktree is at `worktree_dir/<repo>-<KEY>`, the branch has no upstream, the link exists, there's a window named after the key in `tmux_session` (created when no server is running), and the agent output appears.
- **Start from an existing local branch,** and **from an open PR** via fake `gh`.
- **Resume** in an existing worktree types the resume command. A second start returns the existing window rather than creating another.
- **Stop** kills the window and keeps the worktree.
- **Clean up:**
  - clean: the worktree and branch are removed, with no prompt;
  - dirty or unpushed: `errUnsafe`, with the file and commit listed;
  - forced: the worktree is removed and a branch with unpushed commits is kept;
  - merged PR head: when `PR.HeadSHA` equals HEAD after the remote branch is deleted, it counts as pushed and the branch is deleted;
  - detached worktree with commits: forcing saves them on a branch first.
- **HTTP:** the new-branch 409 then 200 flow on `POST /start`; 423 while a start is in flight (hold the claim, fire a second request).

### Reviews

- **Start review:** `origin/pr/<n>` is fetched, the worktree is on `review/<n>` tracking the PR's branch, and the window is named `<checkout>-pr<n>`.
- **Clean up:** a clean review removes the worktree, the branch and the ref.
- **Local commit:** 409; forcing it keeps `review/<n>`; starting again reuses that branch, still ahead by one.
- **Pulled commits count as pushed:** after `pushPR` adds a commit and the worktree pulls it, clean-up re-fetches the PR head and doesn't treat the pulled commit as unpushed.
- **Leftovers:** `attachReviews` reports a review worktree whose PR isn't in the list, and clean-up works on it.

### Browser terminal (HTTP + WebSocket + PTY)

- Connect to `/term/<key>/ws`, send a resize, check `#{client_width}`, send input, see the output.
- `exit` in the shell gives close `1000 "ended"` and requests a board refresh (`board.wake`).
- Opening with no window gives `4404 "resume"` when a worktree exists and `4404 "start"` when nothing exists locally.
- Detaching the linked client gives `1000 "detached"`, and the window keeps running.
- Closing the WebSocket removes its linked session.
- **Over TLS:** with an `httptest` TLS server, a client certificate and the CA from `newTestPKI`, `wss://` connects as a remote browser. Without the certificate, the handshake fails.

### Certificates CLI

- `runCerts([]string{"init", "-dir", d, "-host", "devbox,192.0.2.1"})`, then `client` and `server`: the expected files exist, keys are mode `0600`, a second `init` is refused, and the server certificate covers the given names.

## Not covered

- Real browsers. Playwright could check the board's buttons and dialogs and the xterm.js page, but it's a heavier dependency; revisit if UI regressions become a problem.
- Real GitHub, `gh`, `acli` and Jira.
- Importing certificates into macOS Keychain, and browsers' certificate prompts.

## CI

`.github/workflows/ci.yml`, on push and pull request:

- **Job `test` (ubuntu-latest):** `actions/checkout@v4`, `actions/setup-go@v5` with `go-version-file: go.mod`, then `go vet ./...` and `go test -race ./...`.
- **Job `integration` (ubuntu-latest):** the same setup plus `sudo apt-get install -y tmux`, then `go test -race -tags integration ./...`.
- A macOS job (`brew install tmux`) is optional. The socket-path workaround above exists so the suite also runs locally on a Mac.

## Order

1. The harness, and the tmux and linked-session tests. These are the smallest, and they prove the isolation works.
2. Task sessions.
3. Reviews and fake `gh`.
4. Browser terminal, then the TLS variant.
5. Certificates CLI.
6. The CI workflow. Add the `test` job as soon as step 1 lands, since it needs nothing new.
7. Note the integration command in CLAUDE.md's Commands section and in the README.
