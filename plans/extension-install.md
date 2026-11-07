# Easier tab groups extension install for Chrome and Edge (no web stores, local or remote browser)

## Context

PR #15 added an install page (`/extension`) that asks users to:
1. download a zip and unzip it into a folder they keep;
2. open `chrome://extensions`, turn on Developer mode and Load unpacked;
3. for a remote browser on `tls_listen`, also add the board's address in the extension's options and grant access.

The user wants less friction, with no web stores. It must work for a browser on another machine, so taskboard can't simply write a folder on the server.

Without a store, Chrome and Edge only take extensions unpacked in Developer mode, so the one-time Developer-mode step stays. Everything else can go, on whichever machine the browser runs:

- **No download or unzip.** The page writes the extension straight into a folder the user picks on their own machine, using the File System Access API (`showDirectoryPicker`). It works in Chrome and Edge on secure origins (`http://127.0.0.1` and `localhost`, and the `https` TLS listener).
- **No options step for remote boards.** The files the server hands out have the board's own origin added to the manifest's `content_scripts.matches`, so the extension works on that address as soon as it's loaded.
- **Self-update.** The page keeps the folder handle (IndexedDB). When the board ships a newer extension version, it rewrites the files and asks the extension to `runtime.reload()`, which re-reads an unpacked extension from disk.

The zip download stays as the fallback (Firefox, plain-http LAN hosts, browsers without the API). Work continues on branch `extension-install-link` (PR #15).

## Changes

### Server (`extension.go`, `server.go`)
- `extensionFiles(origin string) (map[string][]byte, error)`: the embedded files, with `manifest.json` changed so `content_scripts[0].matches` also has `scheme://hostname/*` for the request's origin, unless it's a default (`http` on 127.0.0.1/localhost). The origin comes from `r.Host`/`r.TLS`, the same as `extensionPage` does now; factor that into `requestOrigin(r)`.
- `GET /extension/files.json`: `{name: text}` for this origin, which the page writes into the folder. All the files are small text files.
- `GET /taskboard-tab-groups.zip`: built from `extensionFiles(origin)` per request, replacing the `sync.OnceValues` cache. It's tiny. This keeps the zip's file dates fix.
- `extensionVersion`: the embedded manifest version, parsed once, available to templates as `extVersion`.
- Both routes are read-only GETs, open to viewers like the rest. No new POSTs, and nothing is written on the server.

### Extension (`extension/`)
- `background.js` `allowedSender`: build the allowed patterns from `api.runtime.getManifest().content_scripts[0].matches` plus the options-page origins, instead of `DEFAULT_HOSTS`. A board origin baked into the manifest then counts as allowed.
- `content.js`: as now, mark the page with `data-taskboard-extension`. Also listen for a `taskboard-extension-updated` DOM event from the page and send `{type: "reload", version}`.
- `background.js` `reload` handler: check `allowedSender`. Skip it if `storage.local.reloadedFor === version` (the loop guard), otherwise store it and call `api.runtime.reload()`.
- `manifest.json`: version 0.2.0.

### Page script (new partial `templates/extension-install.html`, defining `extensioninstall`, used by `index.html` and `extension.html`)
- Every board page has `<meta name="taskboard-extension-version" content="{{extVersion}}">`.
- `install()` runs on a click:
  - `showDirectoryPicker({mode: "readwrite", id: "taskboard-extension"})`, then `getDirectoryHandle("taskboard-tab-groups", {create: true})`. A subfolder, so picking Documents or similar works.
  - Fetch `/extension/files.json` and write each file.
  - Merge `matches` with the folder's existing `manifest.json`, so two boards at different origins don't overwrite each other.
  - Remove files no longer shipped, and save the handle in IndexedDB.
- `update()` runs when the installed version (marker) is older than the meta version and a saved handle exists:
  - Call `queryPermission({mode: "readwrite"})`. If it's `granted`, which Chrome's "Allow on every visit" makes persistent, update silently. Otherwise show an **Update extension** button, since `requestPermission` needs a click.
  - Rewrite the files, dispatch `taskboard-extension-updated`, then reload the page after about a second.
- Board header (`index.html`):
  - The puzzle button stays hidden when the marker is current.
  - It shows with an "update" style and title when the marker is older; clicking it runs `update()`, or goes to `/extension` if there's no saved handle.
  - The silent update runs on board load.

### Install page (`templates/extension.html`)
For Chrome or Edge with `showDirectoryPicker`, three steps:
1. **Choose a folder** runs `install()`. It confirms with "Wrote taskboard-tab-groups into <chosen folder name>". The API doesn't expose full paths, so the page says to remember where it put the folder.
2. **Copy `chrome://extensions`** (`edge://extensions` for an `Edg/` user agent) copies with `navigator.clipboard.writeText`. Paste it into the address bar and turn on Developer mode.
3. **Load unpacked** and pick the `taskboard-tab-groups` folder. Say that later updates happen by themselves.

Also:
- **Status** from the marker: installed and current, or installed but older with an Update button. If the version is still old after an update, say the extension was loaded from a different folder and to Load unpacked from the chosen one.
- **Fallback section:** the zip download and the existing steps, shown first in Firefox, on insecure origins, or without the API. Since the zip now carries this origin, drop the "This address" section. The extension's options page stays for origins added later.

### Docs
- README "Tab groups": choose-a-folder install, self-update, remote boards needing no options step, and the zip fallback.
- CLAUDE.md "Tab groups" paragraph: origin-patched manifest, page-driven writes via File System Access, and the version meta, update event and reload handshake with its loop guard.

## Verification

- `go vet ./...`, `go test ./...`. Update `TestExtensionDownload`, and add tests that:
  - check the patched manifest for a TLS host (has `https://board.example/*`) and a default host (unchanged);
  - check `files.json` matches the zip;
  - check the version meta on `/` and `/extension`.
- `node --check` on the extension JS files.
- Chrome against a scratch build on port 8485:
  - Choose a folder in the scratchpad and confirm the files are there with the patched manifest.
  - The user does the one-time Load unpacked (I can't change browser settings).
  - Confirm the header button hides and links group.
  - Bump `manifest.json` in a scratch build, restart, and open the board. Confirm the folder is rewritten, the extension reloads to the new version once, and there's no loop when it was loaded from a different folder.
- Remote path: the user tries it from a second machine over `tls_listen`. I can't simulate that, but the patched-manifest unit test covers the origin.
- Firefox: the zip fallback still loads as a temporary add-on.

## Unverified assumptions to check while implementing
- Chrome won't let `showDirectoryPicker` pick some system and home folders; the page reports the error and asks for another folder.
- Chrome keeps the write permission with "Allow on every visit" (Chrome 122+). Without it, updates take one click.
