# whatsapp-mcp

A **read-only** MCP server for your own WhatsApp account. It links as a companion device (like
WhatsApp Web), keeps a local SQLite archive of your chats, and exposes that archive to MCP clients
(Claude Code, Claude Desktop, openclaw, ...) as tools and resources. It deliberately has **no send
capability**.

## Design

```
 phone (primary device)
     │  multi-device protocol (end-to-end encrypted)
     ▼
 whatsmeow client ──events──▶ ingest ──▶ whatsapp.db (SQLite, WAL)
  (linked device)                          ├─ whatsmeow_*  session keys, contacts, LID↔phone map
                                           └─ mcp_*        chats, messages, reactions
                                                    ▲
 MCP client ◀── stdio / streamable HTTP ── MCP server (tools + resources, read queries)
```

- **One Go binary**: [whatsmeow](https://github.com/tulir/whatsmeow) for the WhatsApp protocol and
  the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk).
- **Archive, not live queries.** WhatsApp has no "fetch my chat history" API for linked devices.
  History arrives as pushes: an initial history sync right after linking, live messages while
  connected, and on-demand history the phone sends when asked. Everything is written to SQLite and
  the MCP tools query that. Reading therefore works offline and is fast.
- **One connection owner.** Two processes using the same device keys would fight over the socket,
  so a file lock (`connection.lock`) decides who connects. Any further `serve` instance (for example
  a second MCP client spawning its own stdio server) automatically falls back to **read-only
  archive mode**. `download_media` and `request_older_history` need the connected instance.
- **Identity normalization.** WhatsApp increasingly addresses people by LID (`…@lid`) instead of
  phone number. The ingest maps LIDs to phone-number JIDs when the mapping is known, so one person's
  DMs don't split into two chats. Names are resolved from your address book, then chat or group
  names, then WhatsApp profile ("push") names.
- **Faithful edits/deletes.** Edits update the text and set `edited`. "Delete for everyone" leaves
  a tombstone (`deleted: true`) with the content removed, the same as the phone shows it. Reactions
  are attached to the message they react to. Status updates (stories) are skipped.
- **Privacy.** All files are created owner-only (umask 077). The HTTP transport binds to loopback by
  default and refuses a non-loopback address without `--token`. Downloaded media is cached under
  `<data-dir>/media/`.

## Tools

| tool | purpose |
|---|---|
| `whatsapp_status` | Link and connection state, archive size, and time range |
| `list_chats` | Chats by recent activity with a last-message preview. Filter by `query`, `type` (direct/group), `unread_only`, `include_archived` |
| `get_chat` | Chat details and archive coverage. For groups: topic and participants (when connected) |
| `list_messages` | A chat's history in chronological order. Page back with `before`; filter by `after`, `sender`, `kind`, `oldest_first` |
| `search_messages` | Substring search over text, captions, and file names across all chats or one chat, with `sender`, date, `kind`, and `offset` filters |
| `get_message_context` | A message with N messages before and after it |
| `search_contacts` | Find people by name or number and get their JIDs |
| `download_media` | Decrypt and cache an attachment and return its path. Images are also returned inline |
| `request_older_history` | Ask the phone for messages older than the oldest archived one in a chat |

Wherever a tool takes `chat` or `sender`, you can pass a JID (`4915112345678@s.whatsapp.net`,
`…@g.us`), a phone number (`+49 151 12345678`), or part of a name (`"team lunch"`). A name that
matches several chats returns an error listing the candidate JIDs. `sender` also accepts `me`.

## Resources

- `whatsapp://status`: status as JSON
- `whatsapp://chats`: the 100 most recent chats (markdown)
- `whatsapp://chat/{+chat}`: a transcript of a chat's latest 200 messages. `{chat}` is a JID, a
  number, or a URL-escaped name

## Setup

```sh
make            # builds bin/whatsapp-mcp (needs Go and a C compiler: the SQLite driver uses cgo)

# 1. Link the account (once). Either scan the QR code...
./bin/whatsapp-mcp login
#    ...or use a pairing code (WhatsApp > Linked devices > Link with phone number instead):
./bin/whatsapp-mcp login --phone +4915112345678
#    Add --full-history (optionally --history-days 365) to ask the phone for more than the recent
#    default. This only takes effect at link time.

./bin/whatsapp-mcp status      # what's archived

# 2. Register with an MCP client, e.g. Claude Code:
claude mcp add whatsapp -- "$PWD/bin/whatsapp-mcp" serve
```

The QR code is printed to the terminal and also saved as `<data-dir>/login-qr.png`, which is useful
when the terminal is remote. It refreshes about every 20 seconds.

After linking, `login` stays connected to receive the initial history. It shows a countdown and
exits by itself once no history has arrived for `--sync-idle` (default 90s). Keep the phone online
during that time. Ctrl-C is safe: everything received so far is already saved.

`login` needs the WhatsApp connection for itself. If a server instance is connected (the
`make service` daemon, or a stdio `serve` that an open MCP client session started), `login` stops
with "another whatsapp-mcp process is connected". Stop that instance first
(`systemctl --user stop whatsapp-mcp`, or close the client session), and start it again afterwards.
This also applies when you link again after a session expired.

### Long-running daemon (recommended with several clients)

stdio servers only run while a client session is open, and **messages are archived only while
some instance is connected**. To keep the archive continuously up to date, run one daemon and
point every client at it:

```sh
make service    # installs to ~/.local/bin and starts a systemd user unit serving http://127.0.0.1:8765/mcp
claude mcp add --transport http whatsapp http://127.0.0.1:8765/mcp
```

`make service ADDR=127.0.0.1:9000` changes the port. Follow the logs with
`journalctl --user -u whatsapp-mcp -f`. User services stop when you log out unless lingering is
enabled (`loginctl enable-linger $USER`). `make uninstall` removes the unit and the binary and keeps
the archive. Without systemd, run `whatsapp-mcp serve --transport http` yourself. If the daemon is
running, stdio instances started by other clients fall back to read-only mode automatically.

### Flags and environment

| | |
|---|---|
| `--data-dir` / `WHATSAPP_MCP_DATA_DIR` | State directory (default `~/.local/share/whatsapp-mcp`) |
| `--token` / `WHATSAPP_MCP_TOKEN` | Bearer token required on the HTTP transport |
| `--log-level` | `debug`, `info`, `warn`, or `error`. Logs go to stderr, never stdout |

`whatsapp-mcp logout` unlinks the device and keeps the archive. Delete the data directory to
remove all data.

## Limitations

- **Coverage.** History before linking is limited to what the phone sends. By default that is a
  limited number of recent messages per chat, not a fixed time window, so quiet chats may reach
  back years while busy ones only cover days. More batches can arrive later while an instance is
  connected. `--full-history` asks for much more, but only takes effect at link time: `logout`, then
  `login` again. Use `request_older_history` to go further back one chat at a time. It works in steps
  of up to 200 messages and needs the phone online.
- **Media expiry.** WhatsApp servers drop media after some weeks, so downloading old attachments
  can fail. Only attachments you've downloaded before are cached locally.
- **Voice notes** come back as audio files (`.ogg`) and are not transcribed.
- **Session expiry.** A linked device is logged out if the phone doesn't connect for about 14
  days. After that, run `login` again (see above about stopping the server first).
- **Unofficial protocol.** whatsmeow is a reverse-engineered client. Reading your own messages is
  low-risk, but if WhatsApp changes the protocol, update the library with
  `go get go.mau.fi/whatsmeow@latest && make`.

## Development

`make check` runs gofmt, vet, and the tests. `make install` copies the binary to `~/.local/bin`
(override with `PREFIX=`). The version comes from the last git commit touching this directory and
is shown by `whatsapp-mcp version` and in the MCP server info.

`go test ./...` feeds synthetic WhatsApp events (text, replies, LID senders, documents, reactions,
edits, and revokes) through the real ingest path. It then runs every tool and resource through an
in-memory MCP client.

## License

MIT, see [LICENSE](LICENSE). Built binaries include
[whatsmeow](https://github.com/tulir/whatsmeow), which is licensed under MPL-2.0 (unmodified; its
source is available upstream). Other dependencies are under MIT, Apache-2.0, or BSD-style licenses.
