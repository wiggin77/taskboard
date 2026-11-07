(() => {
  const api = globalThis.browser ?? globalThis.chrome;
  if (!document.querySelector('meta[name="application-name"][content="taskboard"]')) return;
  document.documentElement.dataset.taskboardExtension = api.runtime.getManifest().version;

  // Capture on window runs before the page's own data-tab handler on document, which then stands down.
  addEventListener("click", e => {
    if (!e.isTrusted || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = e.target instanceof Element ? e.target.closest("a[data-group]") : null;
    if (!a || !a.dataset.group) return;
    let url;
    try { url = new URL(a.href); } catch { return; }
    if (url.protocol !== "http:" && url.protocol !== "https:") return;
    // Undefined once the extension is reloaded or removed; the page then opens the link itself.
    if (!api?.runtime?.id) return;
    e.preventDefault();
    e.stopImmediatePropagation();
    const slot = a.target || url.href;
    api.runtime.sendMessage({ type: "open", url: url.href, group: a.dataset.group, slot })
      .catch(() => window.open(url.href, slot));
  }, true);
})();
