# Report attach_command failures

Status: planned, not started.

## Why

"Open in a terminal" can do nothing while the board reports success. `runAttach` (`server.go`) only checks `cmd.Start()` and then calls `go cmd.Wait()`, so `/attach` returns 204 and the icon flashes "Opened" even when the command fails at once. Its stderr is discarded, and the linked `tb-...` session is left for `sweepLinked` to remove two minutes later.

The case that showed this: taskboard was restarted by a wrapper script inside a tmux session first created over SSH. That shell kept the SSH login's environment (`SSH_CONNECTION` set, no `DISPLAY`), so every restart inherited it, and `gnome-terminal` could not open a window. Attaching to the session later from the desktop updates the tmux session's environment, but only for windows created afterwards, never for a shell that is already running.

## Design

**Wait briefly for the attach command.** `runAttach` captures stderr in a small bounded buffer and waits up to about 2 seconds:
- **Exits non-zero in that time:** close the linked session and return an error built from the exit status and the last line or two of stderr (shortened with `truncate`, as in `terminal.go`).
- **Exits zero:** success. This is the usual gnome-terminal case, since it hands off to `gnome-terminal-server` and exits.
- **Still running:** success, and keep waiting in a goroutine as now. This covers xterm-style commands that live as long as the terminal.

The request must not be held much longer than that grace period. The existing 10s context covers only `openLinked`.

**Show the error.** The attach click handler in `templates/index.html` reads the response text on a non-OK response and uses it as the flash label, falling back to "Could not open session". The error label should stay longer than the 1500ms used for success, or until the next click, so it can be read.

**Start in a terminal.** `startDone` already treats a `runAttach` error as `opened: false`, so it picks up the new detection with no change. It could optionally return the error text alongside `opened`.

**Docs.** One README sentence next to `attach_command`: the command runs with taskboard's environment. A taskboard started over SSH, or from a shell inside a tmux session started over SSH, has no display, so start it from the desktop session or export `DISPLAY` first.

## Verification

- If the wait logic is split into a pure helper, test it. Tests never shell out, so the process handling itself is not tested.
- `go vet ./...`, `go test ./...`, gofmt on edited files.
- Run without `DISPLAY` (for example `env -u DISPLAY -u WAYLAND_DISPLAY ./taskboard -config taskboard.json`) and click Open in a terminal.
  - Expect the error icon and a tooltip with gnome-terminal's error.
  - Expect no `tb-...` session left behind in `tmux ls`.
- Run from the desktop session and click again. Expect a gnome-terminal window on the task's window and the success icon.
- Start in a terminal (the play icon) in both environments. It says "Started and opened" only when a window actually opened.
