// Mock data for the status page. Archive numbers match the real archive on 2026-11-05;
// names and messages are invented. The page states mirror what the server can actually know:
// whatsmeow connection events, the last connect error, the logout reason, history-sync
// counters, and the archive itself.
window.WA = {
  account: "+966 55 012 3456",
  pushName: "Abdulrahman",
  device: "WhatsApp MCP · Linux",
  url: "http://127.0.0.1:8765/mcp",
  archive: {
    chats: 540,
    messages: 82802,
    oldest: "2 Jul 2022",
    newest: "Today, 09:41",
    lastHistorySync: "Today, 09:12",
    media: "1.2 GB cached",
  },

  // One entry per state the page can be in.
  states: {
    connected: {
      label: "Connected", tone: "ok",
      title: "WhatsApp is connected",
      detail: "Live messages are being archived. Linked 6 days ago.",
      since: "Connected since 08:57 today",
    },
    reconnecting: {
      label: "Reconnecting", tone: "warn",
      title: "Can't reach WhatsApp",
      detail: "The network looks down. Retrying in the background; the archive is still readable.",
      error: 'dial tcp: lookup web.whatsapp.com: Temporary failure in name resolution',
      retry: { attempt: 4, nextIn: 14 },
      since: "Offline since 08:55",
    },
    logged_out: {
      label: "Logged out", tone: "bad",
      title: "WhatsApp logged this device out",
      detail: "This happens every few weeks, or when the device is removed on your phone. Link it again to resume archiving. Nothing in the archive is lost.",
      error: "401: logged out from another device",
      since: "Logged out on 5 Nov, 05:48",
    },
    linking: {
      label: "Linking", tone: "warn",
      title: "Scan with your phone",
      detail: "WhatsApp › Settings › Linked devices › Link a device",
    },
    syncing: {
      label: "Receiving history", tone: "ok",
      title: "Linked. Receiving history from your phone",
      detail: "Keep WhatsApp open on your phone. This finishes on its own once nothing new arrives for 90 seconds.",
      history: { stored: 3418, chunks: 7, quietFor: 12 },
    },
  },

  // Connection events, newest first (the server would keep these in memory).
  events: [
    { t: "08:57", tone: "ok", text: "Connected to WhatsApp", sub: "after 5 attempts" },
    { t: "08:55", tone: "warn", text: "Network unreachable at startup", sub: "retrying with backoff: 2s, 4s, 8s, 16s" },
    { t: "Yesterday 23:10", tone: "muted", text: "Service stopped", sub: "system shutdown" },
    { t: "Yesterday 18:02", tone: "warn", text: "Disconnected, reconnected in 3s", sub: "" },
    { t: "30 Oct 06:34", tone: "ok", text: "History sync finished", sub: "12,904 messages from the phone" },
  ],

  messages: [
    { chat: "Family 🏡", group: true, sender: "Noura", text: "Dinner at grandma's on Friday, everyone confirm 🙏", time: "09:41", color: "#7c5cff" },
    { chat: "Faisal Alharbi", sender: "Faisal Alharbi", text: "Sent the slides, check the last section before the meeting", time: "09:33", color: "#e0703a" },
    { chat: "GDG Qassim · Organizers", group: true, sender: "Reem", text: "Venue confirmed for the 14th. Need two more volunteers for check-in", time: "09:20", color: "#2f9e8f" },
    { chat: "Mohammed", sender: "You", text: "On my way, 10 minutes", time: "09:02", color: "#3a7be0", fromMe: true },
    { chat: "CS 499 · Graduation project", group: true, sender: "Dr. Sultan", kind: "Document", text: "Milestone-2-rubric.pdf", time: "08:47", color: "#c2410c" },
    { chat: "Sara", sender: "Sara", kind: "Voice", text: "Voice message · 0:42", time: "08:15", color: "#d6457a" },
    { chat: "Aramex", sender: "Aramex", text: "Your shipment 4471 is out for delivery today", time: "07:58", color: "#6b7280" },
    { chat: "DragPath team", group: true, sender: "Khalid", kind: "Photo", text: "Photo · new onboarding screens", time: "Yesterday", color: "#0e9f6e" },
  ],

  initials(name) {
    return name.replace(/[^\p{L}\s]/gu, "").trim().split(/\s+/).slice(0, 2).map((w) => w[0]).join("").toUpperCase();
  },

  // A QR-looking code. Not scannable; finder and timing patterns make it read as a QR.
  qr(size = 232, seed = 1) {
    const n = 33;
    let s = seed * 9301 + 49297;
    const rnd = () => ((s = (s * 9301 + 49297) % 233280) / 233280);
    const m = Array.from({ length: n }, () => Array.from({ length: n }, () => rnd() > 0.52));
    const finder = (r, c) => {
      for (let i = -1; i <= 7; i++) for (let j = -1; j <= 7; j++) {
        const y = r + i, x = c + j;
        if (y < 0 || x < 0 || y >= n || x >= n) continue;
        const edge = i === 0 || i === 6 || j === 0 || j === 6;
        const core = i >= 2 && i <= 4 && j >= 2 && j <= 4;
        m[y][x] = i >= 0 && i <= 6 && j >= 0 && j <= 6 && (edge || core);
      }
    };
    finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
    for (let i = 8; i < n - 8; i++) { m[6][i] = i % 2 === 0; m[i][6] = i % 2 === 0; }
    const cell = size / n;
    let rects = "";
    for (let y = 0; y < n; y++) for (let x = 0; x < n; x++)
      if (m[y][x]) rects += `<rect x="${(x * cell).toFixed(2)}" y="${(y * cell).toFixed(2)}" width="${(cell + 0.3).toFixed(2)}" height="${(cell + 0.3).toFixed(2)}"/>`;
    return `<svg width="${size}" height="${size}" viewBox="0 0 ${size} ${size}" fill="#0c100f" shape-rendering="crispEdges">${rects}</svg>`;
  },
};
