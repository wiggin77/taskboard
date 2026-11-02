# Highlights

Status: implemented.

## Why

With many parallel sessions the board gets long, and it's easy to lose track of which few items you're actively working on. Highlighting lets you mark one or more cards (tasks and PR rows) so they stand out. The marks must survive page reloads, the 15s version-poll reloads, and browser restarts.

**Stored on the server, not in the browser.** taskboard already keeps its state on the taskboard machine, and the board is reached from several devices (loopback plus mTLS remote). With browser storage each device would have its own set, and the set would be lost whenever site data is cleared. A server file costs one small JSON file and one route. Other devices pick up changes through the existing `/version` poll, with no page reload.

## Design

**Identity.**
- A task is identified by `Task.Key` (a Jira key, or `repo#number` for `NoTicket` tasks).
- A PR row (`PRLink`) is identified by `repo#number`.
- Neither depends on Jira, so these IDs will still work when other trackers are added.

**Storage.** `taskboard-highlights.json` lives next to the config file and contains `{"highlights": ["MM-123", "mattermost/mattermost#456"]}`.
- A missing file means an empty set. Invalid JSON is a startup error, just as a bad config is.
- If `configPath` is empty (as in tests), the set is kept in memory only.
- Writes use temp file + rename. That logic is pulled out of `SaveConfig` into a shared helper.
- The file is added to `.gitignore`.

**Pruning.** On each write, drop IDs that aren't on the current board, but only when the snapshot is loaded and has no `Errors`. A source outage therefore can't wipe the set, and stale keys don't pile up.

**Who can change them.** Toggling uses `allowAction` (local or remote, plus the `X-Taskboard` header). Viewers see highlights but get no toggle.

**Sync.**
- The page embeds the current list when it renders.
- `/version` returns `highlights`, and the page applies it in place. It skips this while a toggle request is still in flight.
- Highlights are **not** part of `Snapshot.Version`, so marking a card never forces a reload.

**Interaction.** A dedicated toggle in the card's left gutter does the highlighting; clicking elsewhere on the card does nothing new.
- The toggle is the gutter left of the key (about 1.35rem), running the full height of the card, so it's easy to hit without aiming at a small icon.
- It is a real `<button>` with `aria-pressed`, so it works from the keyboard and with screen readers.
- It shows a faint bookmark outline on hover or focus, and a filled bookmark whenever the card is highlighted, so the indicator is also the control.

Why not the whole card:
- Clicking to bring the browser to the front, or just missing an icon button, would toggle a card without you noticing.
- On phones, a tap that starts a scroll can still land as a click.
- A plain card gives no hint that clicking it does anything.
- It would leave no room for a future click-to-expand on cards.

**Look.**
- A highlighted card gets a tinted background, `color-mix(in srgb,var(--link) 9%,var(--surface))`, which stays tinted on hover.
- The bookmark icon is filled in `--link`.
- The existing 3px tone bar is left alone, so stage colour still reads.

**Finding them.** A "Highlighted N" chip in the `.jump` nav jumps to the next highlighted card on each click, wrapping around. It opens the Done `<details>` if that's where the card is, and is hidden when N is 0.

## Changes

- **New `highlights.go`**
  - A `highlights` struct (mutex, path, set) with `loadHighlights(path)`, `list()` (sorted) and `set(id, on, present)` (applies the change, prunes, saves).
  - A pure helper `boardIDs(Snapshot) map[string]bool` that collects task keys and `PRLink.ID()`s.
- **`github.go`**: add `func (p PRLink) ID() string` returning `Repo + "#" + Number`.
- **`config.go`**: extract `writeFileAtomic(path, data)` from `SaveConfig`, which keeps the existing file mode, and reuse it.
- **`server.go`**
  - `Board` holds the `highlights` store (in memory by default, which suits tests). `main.go` loads the file from beside the config, so a bad file fails at startup.
  - Add a new route `POST /highlight` (form `id`, `on=1|0`) that calls `allowAction`, sets the value rather than toggling it so two devices can't race, and returns the list as JSON.
  - `indexData` gets `Highlights []string`.
  - `version()` adds `"highlights"`.
- **`templates/index.html` and `templates/prsections.html`**
  - Cards get `data-id` and the gutter toggle button (`data-highlight`), which is shown only when `CanAct`.
  - Add CSS for `.highlighted` and the toggle. The toggle is absolutely positioned over the gutter and must not cover the 3px tone bar.
  - Add JS:
    - `applyHighlights(list)`
    - A `[data-highlight]` branch in the existing delegated `click` handler
    - An optimistic POST that reverts and flashes on failure
    - Wiring into `check()`
    - The nav chip
- **`README.md`**: a short "Highlights" note covering what it does, where the file lives, that it syncs across devices, and that viewers can't change it.
- **`CLAUDE.md`**: one line under Security noting that `/highlight` is an action needing local or remote access.

## Verification

- `go vet ./... && go test ./...`
- New tests in `taskboard_test.go`:
  - Store round trip in `t.TempDir()`: set, reload, list.
  - Pruning: IDs that are absent are dropped on a clean snapshot and kept when the snapshot has errors.
  - `POST /highlight`: 403 without the header or from a viewer remote address; 200 from loopback, and the list then shows up in `/version` and in the rendered index.
- Manual check:
  - Run `go build -o taskboard . && ./taskboard -config taskboard.json`.
  - Highlight a task and a review PR, reload, restart the binary, and confirm both are still highlighted.
  - Open the board in a second browser and confirm the highlight appears within about 15s without a reload.
  - Confirm that only the gutter toggles: clicking elsewhere on the card, its links or its buttons doesn't.
  - Confirm the toggle works with Tab plus Enter or Space.
  - Confirm the nav chip cycles through highlighted cards and opens Done.
