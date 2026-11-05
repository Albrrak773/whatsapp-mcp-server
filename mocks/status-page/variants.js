// The manifest for one mock topic. index.html and mocknav.js both read it.
// Adding a variant = one new .html file + one entry here.
window.MOCK = {
  topic: "whatsapp-mcp status & re-link page",
  question:
    "How should a local status page show whether WhatsApp is linked and archiving, and make re-linking with a QR code quick? (Switch states bottom-left or with keys 1-5.)",
  variants: [
    {
      file: "a3-status-card-cleaned.html",
      name: "Status card, cleaned",
      axis: "A as it was, minus the tells: no logo tile, glow, pill badges or stat tiles; WhatsApp ticks as the status mark",
      good: "Keeps A's button, cards and message list, which the owner liked",
      cost: "Still the dark + green look; the plain redo was too far the other way",
    },
    {
      file: "a2-status-plain.html",
      name: "Status, plain",
      axis: "A's layout redone with the frontend-design skill: no cards, one typeface (Readex Pro, Arabic + Latin), WhatsApp's message ticks as the status mark",
      good: "Reads like a calm utility page, not a dashboard template; Arabic chats render properly",
      cost: "Less 'at a glance' density than the tiles; relies on typography being right",
    },
    {
      file: "a-status-card.html",
      name: "Status card",
      axis: "Hierarchy: action first. One column, one big status card that becomes the QR when linking",
      good: "Calm and obvious: the state and the one thing to do fill the top of the page",
      cost: "Archive and messages sit below the fold; little history of past drops",
    },
    {
      file: "b-console.html",
      name: "Console",
      axis: "Layout: sidebar for status and log, main area for the message feed; linking in a dialog",
      good: "Most information at once; the feed feels like the agents' view of WhatsApp",
      cost: "Busiest of the three; the state lives in a 340px sidebar, so re-link needs a banner too",
    },
    {
      file: "c-health-timeline.html",
      name: "Health timeline",
      axis: "Hierarchy: reliability first. 30-day uptime strip and connection history; re-link as an inline 3-step flow",
      good: "Answers 'has it been working?' at a glance; the Oct 30 to Nov 5 outage would have been visible",
      cost: "Needs the server to keep connection events in the archive (new table); most to build",
    },
  ],
  // Filled in as the user decides. Dated, plain sentences.
  decisions: [
    "2026-11-05: Status card (A) won for its simple layout and flow. Its look was rejected as generic AI styling (owner: 'vomit AI look'), and so was the logo tile. Redone as 'Status, plain' (a2) with Anthropic's frontend-design skill: no cards, glow, pills, stat tiles, avatars, middle dots or uppercase labels.",
    "2026-11-05: 'Status, plain' rejected as too plain and dead. Cards are fine. Keep A's green Link device button and A's message card exactly, minus the 'Latest in the archive' subtitle. Built as 'Status card, cleaned' (a3).",
    "2026-11-05: 'Status card, cleaned' approved and built into the server as web/index.html, served at / on the HTTP transport. Dropped from the mock: 'Request older history' (it works per chat, so it belongs to the MCP tool) and 'Unlink this device' (destructive; `whatsapp-mcp logout` covers it).",
  ],
};
