# Browser tab groups for related links

## Context

Clicking Jira, PR and browser-terminal links on the board opens each in its own named tab (`target="{{URL}}"` plus the `tabs` script in `templates/tokens.html:37`, which focuses an existing named tab instead of reloading it). With many parallel tasks, the window fills with unrelated tabs. The goal: tabs for the same ticket (Jira issue, PR, `/term/{key}`) land in one browser tab group titled with the ticket key, in Chrome, Edge and Firefox.

A web page can't create or join tab groups; only an extension can (`chrome.tabs.group` + `chrome.tabGroups` in Chromium 89+, `browser.tabs.group` + `browser.tabGroups` in Firefox 139+). So the plan has two parts: the server tags every tab-opening link with a group key, and a small cross-browser MV3 extension opens those links into groups. Without the extension the board behaves exactly as it does today.

Native terminals (`attach_command`) aren't browser tabs and are out of scope. Middle-click and modifier-clicks keep the browser default (no grouping), matching the current `tabs` script.

## Part 1: server (Go + templates)

**Group key** for each link:
- Task row, its Jira/PR links and its terminal links: `Task.Key` (NoTicket tasks are already keyed `repo#number`).
- PR row (Reviews / Other repos) and its review terminal: new `PRLink.Group()` in `github.go`, returning `Key` when the PR mentions a ticket, else `ID()`. A review of a PR for MM-1234 joins the MM-1234 group.
- "epic MM-1" link (when the epic has no group on the board): the epic key `.Parent`.
- Epic child link to Jira (`templates/index.html:180`): the child's `.Key`.
- Plan page links (`templates/plan.html:27-29`): `.Task.Key`.

**Changes:**
- `server.go:134` `controls`: add `Group string`; `taskControls` sets it to `t.Key`, `prControls` to `p.Group()`. This keeps the "which buttons a row shows is decided in Go" rule, and the terminal links in the `controls` template get `data-group="{{$c.Group}}"`.
- Add `data-group="..."` to every `data-tab` anchor in `templates/index.html` (taskrow links, epic child), `templates/prrow.html:12-13` and `templates/plan.html`, and to the two `/term/...` anchors in the `controls` template.
- Add `<meta name="application-name" content="taskboard">` to `index.html` and `plan.html`, so the extension only acts on taskboard pages (other dev servers on localhost are left alone).
- `templates/tokens.html` `tabs` script: add `e.defaultPrevented` to the early-return check, so it stands down when the extension has handled the click.
- Tests in `taskboard_test.go` (next to the existing control tests near line 1221): assert `Group` from `taskControls`/`prControls`, and `PRLink.Group()` with and without a key.

## Part 2: extension (`extension/` at repo root)

Plain JS, no build step, one codebase for all three browsers (`globalThis.browser ?? chrome`, promise-based APIs).

**`manifest.json`** (MV3):
- `permissions`: `tabs`, `tabGroups`, `storage`, `scripting`.
- `background`: both `"service_worker": "background.js"` (Chrome/Edge) and `"scripts": ["background.js"]` (Firefox); each browser ignores the key it doesn't use.
- `content_scripts`: `content.js` on `http://127.0.0.1/*`, `http://localhost/*` (match patterns ignore the port, so any `listen` port works).
- `optional_host_permissions`: `https://*/*`, `http://*/*`, for extra origins added in the options page (TLS listener, LAN name).
- `browser_specific_settings.gecko`: an `id` (needed for signing) and `strict_min_version: "139.0"`.
- `options_ui`: `options.html`.

**`content.js`**: capture-phase `click` listener on `window`. Acts only when the page has the taskboard meta tag, the event `isTrusted`, it's a plain left click, and the anchor has `data-group` and an http(s) `href`. Then `preventDefault()` + `stopImmediatePropagation()` and sends `{url, group, slot: a.target}` to the background. If messaging fails (extension reloaded), it does nothing and the page's own handler runs.

**`background.js`**:
- `slots` map (slot name, i.e. the anchor's `target`, to tabId) in `storage.session`, so it survives service-worker restarts; drop entries on `tabs.onRemoved`.
- On a message (check `sender.tab` and that the sender's origin is an allowed one):
  1. Find the existing tab: `slots[slot]` if the tab still exists, else adopt one via `tabs.query({url})` (catches tabs opened before the extension).
  2. Find the group: `tabGroups.query({title: group})`; prefer one in the board tab's window.
  3. Existing tab: activate it and focus its window, without reloading (today's behaviour). If it's ungrouped, add it to the group; if the user moved it to another group, leave it alone.
  4. No tab: `tabs.create` in the group's window (else the board's window), placed right after the group's last tab, then `tabs.group` into the existing group, or create one and `tabGroups.update` with `title: group` and a color hashed from the key (colors set only on creation, so user edits stick).
- Group identity is the title, so groups the user already made or that survive a browser restart are reused.

**`options.html` / `options.js`**: a list of extra taskboard origins. Adding one calls `permissions.request({origins})` then `scripting.registerContentScripts` for it; removing reverses both. Needed for the HTTPS `tls_listen` address and remote browsers.

## Part 3: docs and release

- README: a "Tab groups" section: what it does, install steps per browser (Chrome/Edge: Load unpacked from `extension/` in developer mode; Firefox: a signed `.xpi`, since release Firefox only keeps unsigned extensions until restart; `web-ext sign --channel=unlisted` produces one), adding the TLS origin in the extension's options, and that the board works the same without it.
- CLAUDE.md: one line under Architecture noting `extension/`, that links opt in with `data-group`, and that `controls.Group` sets it for terminal links.
- `.goreleaser.yaml`: optionally attach `extension/` as a zip to releases.

## Verification

- `go vet ./...`, `go test ./...`, `gofmt` on edited Go files.
- `npx web-ext lint -s extension` validates the manifest for Firefox.
- Manual, in Chrome, Edge and Firefox, against `./taskboard -config taskboard.json`:
  - From one task row, click Jira, PR and Open in the browser: all three end up in one group titled with the key. Clicking again focuses each tab without reloading.
  - A second task's links make a second group. A review row for a PR whose title has that key joins the same group; one without a key gets a `repo#N` group.
  - Close the group, click again: a fresh group. Restart the browser with the group restored: the extension reuses it by title.
  - Disable the extension: links behave exactly as before (named tabs, no groups).
  - Add the `https://host:port` TLS origin in the options page and repeat from a remote browser with a client certificate.
