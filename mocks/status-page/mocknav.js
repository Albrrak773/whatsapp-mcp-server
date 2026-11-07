// The bar at the bottom of every variant page: index, previous, next. Reads window.MOCK from
// variants.js. Tooling, not design: don't restyle it with the project's tokens.
(function () {
  const mock = window.MOCK;
  if (!mock || !Array.isArray(mock.variants) || mock.variants.length === 0) return;

  const here = decodeURIComponent(location.pathname.split("/").pop() || "");
  const at = mock.variants.findIndex((v) => v.file === here);
  if (at === -1) return;

  const count = mock.variants.length;
  const hrefAt = (step) => mock.variants[(at + step + count) % count].file + location.search;

  const style = document.createElement("style");
  style.textContent = `
    .mocknav { position: fixed; bottom: 16px; left: 50%; transform: translateX(-50%);
      z-index: 2147483647; display: flex; align-items: center; gap: 2px; padding: 4px;
      border-radius: 999px; background: rgba(12, 12, 14, 0.88); color: #f2f2f2;
      box-shadow: 0 0 0 1px rgba(255, 255, 255, 0.12) inset, 0 8px 24px rgba(0, 0, 0, 0.35);
      font: 500 13px/1 system-ui, -apple-system, "Segoe UI", sans-serif;
      direction: ltr; user-select: none; }
    .mocknav a { display: flex; align-items: center; height: 28px; padding: 0 10px;
      border-radius: 999px; color: inherit; text-decoration: none; opacity: 0.75; }
    .mocknav a:hover, .mocknav a:focus-visible { opacity: 1; background: rgba(255, 255, 255, 0.12);
      outline: none; }
    .mocknav b { padding: 0 8px; font-weight: 600; white-space: nowrap; unicode-bidi: plaintext; }
    .mocknav i { font-style: normal; opacity: 0.55; padding-inline-end: 8px; }
    .mocknav hr { width: 1px; height: 16px; margin: 0 2px; border: 0;
      background: rgba(255, 255, 255, 0.16); }
    @media print { .mocknav { display: none; } }
  `;
  document.head.appendChild(style);

  const nav = document.createElement("nav");
  nav.className = "mocknav";
  nav.setAttribute("aria-label", "Mock variants");

  const link = (href, text, label) => {
    const a = document.createElement("a");
    a.href = href;
    a.textContent = text;
    a.setAttribute("aria-label", label);
    a.title = label;
    return a;
  };
  const name = document.createElement("b");
  name.textContent = mock.variants[at].name;
  const position = document.createElement("i");
  position.textContent = `${at + 1}/${count}`;

  nav.append(link("index.html", "☰", "All variants"), document.createElement("hr"));
  if (count > 1) nav.append(link(hrefAt(-1), "←", "Previous variant (left arrow)"));
  nav.append(name, position);
  if (count > 1) nav.append(link(hrefAt(1), "→", "Next variant (right arrow)"));
  document.body.appendChild(nav);

  if (count < 2) return;
  document.addEventListener("keydown", (e) => {
    if (e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
    if (e.target instanceof Element && e.target.closest("input, textarea, select, [contenteditable]")) return;
    if (e.key === "ArrowLeft") location.href = hrefAt(-1);
    if (e.key === "ArrowRight") location.href = hrefAt(1);
  });
})();
