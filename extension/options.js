const api = globalThis.browser ?? globalThis.chrome;
const list = document.getElementById("origins");
const input = document.getElementById("origin");
const status = document.getElementById("status");

const patternFor = origin => {
  const u = new URL(origin);
  return `${u.protocol}//${u.hostname}/*`;
};

async function load() {
  const { origins } = await api.storage.local.get("origins");
  return Array.isArray(origins) ? origins : [];
}

function render(origins) {
  list.replaceChildren(...origins.map(origin => {
    const li = document.createElement("li");
    const name = document.createElement("code");
    name.textContent = origin;
    const remove = document.createElement("button");
    remove.type = "button";
    remove.textContent = "Remove";
    remove.addEventListener("click", () => removeOrigin(origin));
    li.append(name, remove);
    return li;
  }));
}

async function removeOrigin(origin) {
  const rest = (await load()).filter(o => o !== origin);
  await api.storage.local.set({ origins: rest });
  const pattern = patternFor(origin);
  if (!rest.some(o => patternFor(o) === pattern)) {
    try { await api.permissions.remove({ origins: [pattern] }); } catch {}
  }
  render(rest);
  status.textContent = `Removed ${origin}.`;
}

document.getElementById("add").addEventListener("submit", e => {
  e.preventDefault();
  const u = URL.parse(input.value.trim());
  if (!u || (u.protocol !== "http:" && u.protocol !== "https:")) {
    status.textContent = "Enter an http or https address.";
    return;
  }
  // Firefox only shows the permission prompt when request is called straight from the click.
  api.permissions.request({ origins: [patternFor(u)] }).then(async granted => {
    if (!granted) {
      status.textContent = "Permission was not granted.";
      return;
    }
    const origins = await load();
    if (!origins.includes(u.origin)) origins.push(u.origin);
    await api.storage.local.set({ origins });
    input.value = "";
    render(origins);
    status.textContent = `Added ${u.origin}. Reload any taskboard tab already open there.`;
  }, err => { status.textContent = String(err); });
});

load().then(render);
