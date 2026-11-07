// Mock tooling, not design: a state switcher pinned to the bottom-left corner so every variant can
// be seen in every state. Keys 1-5 switch too. The variant defines window.render(stateName).
(function () {
  const names = ["connected", "reconnecting", "logged_out", "linking", "syncing"];
  const bar = document.createElement("div");
  bar.className = "mock-states";
  bar.innerHTML =
    `<span>state</span>` +
    names.map((n, i) => `<button data-s="${n}" title="key ${i + 1}">${n.replace("_", " ")}</button>`).join("");
  const css = document.createElement("style");
  css.textContent = `
    .mock-states{position:fixed;bottom:64px;left:12px;z-index:9999;display:flex;gap:2px;align-items:center;
      padding:4px;border:1px dashed #888;border-radius:8px;background:#222c;backdrop-filter:blur(6px);
      font:11px/1 ui-monospace,monospace;color:#ccc}
    .mock-states span{padding:0 6px;opacity:.6;text-transform:uppercase;letter-spacing:.06em}
    .mock-states button{all:unset;cursor:pointer;padding:5px 7px;border-radius:5px}
    .mock-states button[aria-pressed=true]{background:#fff;color:#111}`;
  document.head.append(css);

  function set(name) {
    if (!names.includes(name)) name = "logged_out";
    bar.querySelectorAll("button").forEach((b) => b.setAttribute("aria-pressed", b.dataset.s === name));
    history.replaceState(null, "", "#" + name);
    window.render(name);
  }
  window.setMockState = set;
  bar.addEventListener("click", (e) => e.target.dataset.s && set(e.target.dataset.s));
  addEventListener("keydown", (e) => {
    if (e.target.matches("input")) return;
    const i = Number(e.key) - 1;
    if (names[i]) set(names[i]);
  });
  addEventListener("DOMContentLoaded", () => {
    document.body.append(bar);
    set(location.hash.slice(1) || window.MOCK_DEFAULT_STATE || "logged_out");
  });
})();
