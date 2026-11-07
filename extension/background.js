const api = globalThis.browser ?? globalThis.chrome;

const COLORS = ["blue", "red", "yellow", "green", "pink", "purple", "cyan", "orange", "grey"];
const NO_GROUP = -1;
const SCRIPT_ID = "taskboard-extra-origins";

function colorFor(title) {
  let h = 0;
  for (const c of title) h = (h * 31 + c.codePointAt(0)) >>> 0;
  return COLORS[h % COLORS.length];
}

async function extraOrigins() {
  const { origins } = await api.storage.local.get("origins");
  return Array.isArray(origins) ? origins : [];
}

// Match patterns can't hold a port, so origins are compared by scheme and host only.
function patternFor(origin) {
  const u = new URL(origin);
  return `${u.protocol}//${u.hostname}/*`;
}

// The manifest's own matches include the address the extension was installed from.
const manifestPatterns = api.runtime.getManifest().content_scripts.flatMap(c => c.matches);

async function allowedSender(sender) {
  const u = sender.tab && URL.parse(sender.url ?? "");
  if (!u) return false;
  const pattern = patternFor(u);
  if (manifestPatterns.includes(pattern)) return true;
  return (await extraOrigins()).some(o => patternFor(o) === pattern);
}

function newer(a, b) {
  const x = a.split(".").map(Number), y = b.split(".").map(Number);
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    const d = (x[i] ?? 0) - (y[i] ?? 0);
    if (d) return d > 0;
  }
  return false;
}

// The board rewrote this unpacked extension's folder; reloading reads the new files from disk.
// Once per version, so an extension loaded from some other, stale folder can't reload forever.
async function reloadFor(sender, version) {
  if (!await allowedSender(sender) || !newer(version, api.runtime.getManifest().version)) return false;
  const { reloadedFor } = await api.storage.local.get("reloadedFor");
  if (reloadedFor === version) return false;
  await api.storage.local.set({ reloadedFor: version });
  return true;
}

const slotKey = slot => "slot:" + slot;

async function findTab(slot, url) {
  const key = slotKey(slot);
  const id = (await api.storage.session.get(key))[key];
  if (id !== undefined) {
    try { return await api.tabs.get(id); } catch {}
  }
  const tab = (await api.tabs.query({})).find(t => t.url === url);
  if (tab) await api.storage.session.set({ [key]: tab.id });
  return tab;
}

async function findGroup(title, windowId, anyWindow) {
  const groups = (await api.tabGroups.query({})).filter(g => g.title === title);
  return groups.find(g => g.windowId === windowId) ?? (anyWindow ? groups[0] : undefined);
}

async function joinGroup(tab, title, group) {
  if (group) {
    await api.tabs.group({ groupId: group.id, tabIds: [tab.id] });
    return;
  }
  const id = await api.tabs.group({ tabIds: [tab.id], createProperties: { windowId: tab.windowId } });
  await api.tabGroups.update(id, { title, color: colorFor(title) });
}

async function open({ url, group: title, slot }, board) {
  const existing = await findTab(slot, url);
  if (existing) {
    await api.tabs.update(existing.id, { active: true });
    await api.windows.update(existing.windowId, { focused: true });
    // A tab the user moved to another group stays there.
    if (existing.groupId === NO_GROUP) {
      await joinGroup(existing, title, await findGroup(title, existing.windowId, false));
    }
    return;
  }
  const group = await findGroup(title, board.windowId, true);
  const windowId = group?.windowId ?? board.windowId;
  const props = { url, windowId, active: true };
  if (group) {
    const members = (await api.tabs.query({ windowId })).filter(t => t.groupId === group.id);
    props.index = Math.max(...members.map(t => t.index)) + 1;
  }
  const tab = await api.tabs.create(props);
  await api.storage.session.set({ [slotKey(slot)]: tab.id });
  await joinGroup(tab, title, group);
  if (windowId !== board.windowId) await api.windows.update(windowId, { focused: true });
}

let opening = Promise.resolve();

api.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  if (msg?.type === "reload" && typeof msg.version === "string") {
    reloadFor(sender, msg.version).then(ok => {
      sendResponse({ ok });
      if (ok) setTimeout(() => api.runtime.reload(), 100);
    }, err => sendResponse({ ok: false, error: String(err) }));
    return true;
  }
  if (msg?.type !== "open" || typeof msg.url !== "string" || typeof msg.group !== "string" || typeof msg.slot !== "string") {
    return false;
  }
  const u = URL.parse(msg.url);
  if (!u || (u.protocol !== "http:" && u.protocol !== "https:")) return false;
  const req = { url: u.href, group: msg.group.slice(0, 100), slot: msg.slot };
  // One at a time, so two quick clicks for a new ticket don't make two groups.
  opening = opening.catch(() => {}).then(async () => {
    if (!await allowedSender(sender)) throw new Error("not a taskboard page: " + sender.url);
    await open(req, sender.tab);
  });
  opening.then(() => sendResponse({ ok: true }), err => {
    console.error("taskboard tab groups:", err);
    sendResponse({ ok: false, error: String(err) });
  });
  return true;
});

api.tabs.onRemoved.addListener(async tabId => {
  const all = await api.storage.session.get(null);
  const stale = Object.keys(all).filter(k => k.startsWith("slot:") && all[k] === tabId);
  if (stale.length) await api.storage.session.remove(stale);
});

// Origins added in the options page get the content script here, on every start, so it never depends
// on the browser persisting dynamic registrations.
let syncing = Promise.resolve();

function syncScripts() {
  syncing = syncing.catch(() => {}).then(registerScripts);
  return syncing;
}

async function registerScripts() {
  const registered = await api.scripting.getRegisteredContentScripts({ ids: [SCRIPT_ID] });
  if (registered.length) await api.scripting.unregisterContentScripts({ ids: [SCRIPT_ID] });
  const matches = [];
  for (const origin of await extraOrigins()) {
    const pattern = patternFor(origin);
    if (!matches.includes(pattern) && await api.permissions.contains({ origins: [pattern] })) matches.push(pattern);
  }
  if (matches.length) {
    await api.scripting.registerContentScripts([{ id: SCRIPT_ID, matches, js: ["content.js"], runAt: "document_idle" }]);
  }
}

api.runtime.onStartup.addListener(syncScripts);
api.runtime.onInstalled.addListener(syncScripts);
api.storage.onChanged.addListener((changes, area) => {
  if (area === "local" && "origins" in changes) syncScripts();
});
api.permissions.onRemoved.addListener(syncScripts);
