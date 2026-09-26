package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// The whole state lives in one SQLite file: whatsmeow's own session/contact tables
// (whatsmeow_*) plus this server's message archive (mcp_*). Keeping them together
// lets the archive resolve names from whatsmeow_contacts / whatsmeow_lid_map.

const schema = `
CREATE TABLE IF NOT EXISTS mcp_chats (
	jid             TEXT PRIMARY KEY,
	name            TEXT NOT NULL DEFAULT '',
	is_group        INTEGER NOT NULL DEFAULT 0,
	last_message_at INTEGER NOT NULL DEFAULT 0,
	unread_count    INTEGER NOT NULL DEFAULT 0,
	archived        INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS mcp_messages (
	chat_jid       TEXT    NOT NULL,
	id             TEXT    NOT NULL,
	sender_jid     TEXT    NOT NULL DEFAULT '',
	from_me        INTEGER NOT NULL DEFAULT 0,
	timestamp      INTEGER NOT NULL,
	kind           TEXT    NOT NULL,
	text           TEXT    NOT NULL DEFAULT '',
	push_name      TEXT    NOT NULL DEFAULT '',
	media_mime     TEXT    NOT NULL DEFAULT '',
	media_filename TEXT    NOT NULL DEFAULT '',
	media_size     INTEGER NOT NULL DEFAULT 0,
	media_seconds  INTEGER NOT NULL DEFAULT 0,
	quoted_id      TEXT    NOT NULL DEFAULT '',
	edited         INTEGER NOT NULL DEFAULT 0,
	deleted        INTEGER NOT NULL DEFAULT 0,
	raw            BLOB,
	PRIMARY KEY (chat_jid, id)
);
CREATE INDEX IF NOT EXISTS mcp_messages_chat_ts ON mcp_messages (chat_jid, timestamp);
CREATE INDEX IF NOT EXISTS mcp_messages_ts ON mcp_messages (timestamp);
CREATE INDEX IF NOT EXISTS mcp_messages_sender ON mcp_messages (sender_jid);

CREATE TABLE IF NOT EXISTS mcp_reactions (
	chat_jid   TEXT    NOT NULL,
	message_id TEXT    NOT NULL,
	sender_jid TEXT    NOT NULL,
	emoji      TEXT    NOT NULL,
	timestamp  INTEGER NOT NULL,
	PRIMARY KEY (chat_jid, message_id, sender_jid)
);

CREATE TABLE IF NOT EXISTS mcp_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

type Store struct {
	db *sql.DB

	namesMu      sync.Mutex
	names        map[string]string
	namesExpires time.Time
}

func openDB(path string, readOnly bool) (*sql.DB, error) {
	dsn := "file:" + path + "?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL"
	if readOnly {
		dsn += "&mode=ro"
	}
	return sql.Open("sqlite3", dsn)
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

// ---------------------------------------------------------------------------
// Writes (only the instance that owns the WhatsApp connection writes)

type StoredMessage struct {
	ChatJID   string
	ID        string
	SenderJID string
	FromMe    bool
	Timestamp time.Time
	PushName  string
	Content
	Raw []byte
}

func (s *Store) UpsertMessage(ctx context.Context, m *StoredMessage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mcp_messages (chat_jid, id, sender_jid, from_me, timestamp, kind, text, push_name,
			media_mime, media_filename, media_size, media_seconds, quoted_id, raw)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (chat_jid, id) DO UPDATE SET
			sender_jid = excluded.sender_jid,
			from_me = excluded.from_me,
			timestamp = excluded.timestamp,
			kind = CASE WHEN deleted THEN kind ELSE excluded.kind END,
			text = CASE WHEN deleted THEN '' ELSE excluded.text END,
			push_name = CASE WHEN excluded.push_name <> '' THEN excluded.push_name ELSE push_name END,
			media_mime = excluded.media_mime,
			media_filename = excluded.media_filename,
			media_size = excluded.media_size,
			media_seconds = excluded.media_seconds,
			quoted_id = excluded.quoted_id,
			raw = CASE WHEN deleted THEN NULL ELSE excluded.raw END`,
		m.ChatJID, m.ID, m.SenderJID, m.FromMe, m.Timestamp.Unix(), m.Kind, m.Text, m.PushName,
		m.Mime, m.FileName, m.Size, m.Seconds, m.QuotedID, m.Raw)
	return err
}

// MarkDeleted mirrors "delete for everyone": the content is dropped, a tombstone stays.
// If the revoke arrives before the original message, the tombstone is created first and
// the later upsert will not resurrect the content.
func (s *Store) MarkDeleted(ctx context.Context, chat, id, sender string, fromMe bool, ts time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mcp_messages (chat_jid, id, sender_jid, from_me, timestamp, kind, deleted)
		VALUES (?, ?, ?, ?, ?, 'deleted', 1)
		ON CONFLICT (chat_jid, id) DO UPDATE SET deleted = 1, text = '', raw = NULL`,
		chat, id, sender, fromMe, ts.Unix())
	return err
}

func (s *Store) ApplyEdit(ctx context.Context, chat, id, text string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE mcp_messages SET text = ?, edited = 1 WHERE chat_jid = ? AND id = ? AND NOT deleted`, text, chat, id)
	return err
}

func (s *Store) SetReaction(ctx context.Context, chat, msgID, sender, emoji string, ts time.Time) error {
	if emoji == "" {
		_, err := s.db.ExecContext(ctx,
			`DELETE FROM mcp_reactions WHERE chat_jid = ? AND message_id = ? AND sender_jid = ?`, chat, msgID, sender)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mcp_reactions (chat_jid, message_id, sender_jid, emoji, timestamp) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (chat_jid, message_id, sender_jid) DO UPDATE SET emoji = excluded.emoji, timestamp = excluded.timestamp
		WHERE excluded.timestamp >= mcp_reactions.timestamp`,
		chat, msgID, sender, emoji, ts.Unix())
	return err
}

// TouchChat makes sure a chat row exists and bumps its last activity time.
func (s *Store) TouchChat(ctx context.Context, jid string, isGroup bool, lastMessage time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mcp_chats (jid, is_group, last_message_at) VALUES (?, ?, ?)
		ON CONFLICT (jid) DO UPDATE SET last_message_at = MAX(last_message_at, excluded.last_message_at)`,
		jid, isGroup, lastMessage.Unix())
	return err
}

func (s *Store) SetChatName(ctx context.Context, jid string, isGroup bool, name string) error {
	if name == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mcp_chats (jid, is_group, name) VALUES (?, ?, ?)
		ON CONFLICT (jid) DO UPDATE SET name = excluded.name`, jid, isGroup, name)
	s.invalidateNames()
	return err
}

func (s *Store) SetChatState(ctx context.Context, jid string, unread uint32, archived bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE mcp_chats SET unread_count = ?, archived = ? WHERE jid = ?`, unread, archived, jid)
	return err
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mcp_meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---------------------------------------------------------------------------
// Name resolution

// Names returns jid -> display name, built from the address book and push names that
// whatsmeow keeps, mirrored across phone-number and LID addresses, overlaid with chat names.
func (s *Store) Names(ctx context.Context) map[string]string {
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	if s.names != nil && time.Now().Before(s.namesExpires) {
		return s.names
	}
	names := make(map[string]string)
	lidToPN := make(map[string]string)
	if rows, err := s.db.QueryContext(ctx, `SELECT lid, pn FROM whatsmeow_lid_map`); err == nil {
		for rows.Next() {
			var lid, pn string
			if rows.Scan(&lid, &pn) == nil {
				lidToPN[lid+"@lid"] = pn + "@s.whatsapp.net"
			}
		}
		rows.Close()
	}
	if rows, err := s.db.QueryContext(ctx, `
		SELECT their_jid, COALESCE(NULLIF(full_name, ''), NULLIF(first_name, ''),
			NULLIF(business_name, ''), NULLIF(push_name, ''), '')
		FROM whatsmeow_contacts`); err == nil {
		for rows.Next() {
			var jid, name string
			if rows.Scan(&jid, &name) == nil && name != "" {
				names[jid] = name
			}
		}
		rows.Close()
	}
	for lid, pn := range lidToPN {
		if n, ok := names[lid]; ok && names[pn] == "" {
			names[pn] = n
		} else if n, ok := names[pn]; ok && names[lid] == "" {
			names[lid] = n
		}
	}
	// Chat names (group subjects, history-sync display names) win for chats that have one,
	// except that a saved address-book name wins for direct chats.
	if rows, err := s.db.QueryContext(ctx, `SELECT jid, name, is_group FROM mcp_chats WHERE name <> ''`); err == nil {
		for rows.Next() {
			var jid, name string
			var isGroup bool
			if rows.Scan(&jid, &name, &isGroup) == nil && (isGroup || names[jid] == "") {
				names[jid] = name
			}
		}
		rows.Close()
	}
	// Last resort: the WhatsApp profile name people sent their messages with.
	if rows, err := s.db.QueryContext(ctx, `SELECT sender_jid, push_name, MAX(timestamp) FROM mcp_messages
		WHERE push_name <> '' AND NOT from_me GROUP BY sender_jid`); err == nil {
		for rows.Next() {
			var jid, name string
			var ts int64 // SQLite takes push_name from the row with MAX(timestamp): the latest one
			if rows.Scan(&jid, &name, &ts) == nil && names[jid] == "" {
				names[jid] = name
			}
		}
		rows.Close()
	}
	s.names = names
	s.namesExpires = time.Now().Add(30 * time.Second)
	return names
}

func (s *Store) invalidateNames() {
	s.namesMu.Lock()
	s.names = nil
	s.namesMu.Unlock()
}

// ---------------------------------------------------------------------------
// Reads

type ChatRow struct {
	JID           string
	Name          string
	IsGroup       bool
	LastMessageAt time.Time
	UnreadCount   int
	Archived      bool
	MessageCount  int
}

func (s *Store) Chats(ctx context.Context) ([]*ChatRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.jid, c.name, c.is_group, c.last_message_at, c.unread_count, c.archived,
			(SELECT COUNT(*) FROM mcp_messages m WHERE m.chat_jid = c.jid)
		FROM mcp_chats c ORDER BY c.last_message_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := s.Names(ctx)
	var out []*ChatRow
	for rows.Next() {
		c := &ChatRow{}
		var ts int64
		if err := rows.Scan(&c.JID, &c.Name, &c.IsGroup, &ts, &c.UnreadCount, &c.Archived, &c.MessageCount); err != nil {
			return nil, err
		}
		c.LastMessageAt = unixOrZero(ts)
		if n := names[c.JID]; n != "" {
			c.Name = n
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Chat(ctx context.Context, jid string) (*ChatRow, error) {
	chats, err := s.Chats(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range chats {
		if c.JID == jid {
			return c, nil
		}
	}
	return nil, sql.ErrNoRows
}

type MessageRow struct {
	ChatJID   string
	ID        string
	SenderJID string
	FromMe    bool
	Timestamp time.Time
	Kind      string
	Text      string
	PushName  string
	Mime      string
	FileName  string
	Size      int64
	Seconds   int
	QuotedID  string
	Edited    bool
	Deleted   bool
	HasMedia  bool

	QuotedText   string
	QuotedSender string
	Reactions    []Reaction
}

type Reaction struct {
	SenderJID string
	Emoji     string
}

type MessageFilter struct {
	ChatJID   string
	SenderJID string
	Query     string
	Kind      string
	After     time.Time
	Before    time.Time
	FromMe    *bool
	Limit     int
	Offset    int
	Oldest    bool // take the oldest matching rows instead of the newest
}

const messageColumns = `m.chat_jid, m.id, m.sender_jid, m.from_me, m.timestamp, m.kind, m.text, m.push_name,
	m.media_mime, m.media_filename, m.media_size, m.media_seconds, m.quoted_id, m.edited, m.deleted,
	m.raw IS NOT NULL, COALESCE(q.text, ''), COALESCE(q.sender_jid, '')`

// Messages returns matching messages in chronological order plus whether more matches exist
// beyond the returned window.
func (s *Store) Messages(ctx context.Context, f MessageFilter) ([]*MessageRow, bool, error) {
	var where []string
	var args []any
	if f.ChatJID != "" {
		where = append(where, "m.chat_jid = ?")
		args = append(args, f.ChatJID)
	} else {
		where = append(where, "m.chat_jid <> 'status@broadcast'")
	}
	if f.SenderJID != "" {
		where = append(where, "m.sender_jid = ?")
		args = append(args, f.SenderJID)
	}
	if f.Query != "" {
		where = append(where, `(m.text LIKE ? ESCAPE '\' OR m.media_filename LIKE ? ESCAPE '\')`)
		pat := "%" + likeEscape(f.Query) + "%"
		args = append(args, pat, pat)
	}
	if f.Kind != "" {
		where = append(where, "m.kind = ?")
		args = append(args, f.Kind)
	}
	if !f.After.IsZero() {
		where = append(where, "m.timestamp > ?")
		args = append(args, f.After.Unix())
	}
	if !f.Before.IsZero() {
		where = append(where, "m.timestamp < ?")
		args = append(args, f.Before.Unix())
	}
	if f.FromMe != nil {
		where = append(where, "m.from_me = ?")
		args = append(args, *f.FromMe)
	}
	order := "DESC"
	if f.Oldest {
		order = "ASC"
	}
	q := `SELECT ` + messageColumns + ` FROM mcp_messages m
		LEFT JOIN mcp_messages q ON q.chat_jid = m.chat_jid AND q.id = m.quoted_id AND m.quoted_id <> ''
		WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY m.timestamp ` + order + `, m.rowid ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, f.Limit+1, f.Offset)
	msgs, err := s.queryMessages(ctx, q, args...)
	if err != nil {
		return nil, false, err
	}
	more := len(msgs) > f.Limit
	if more {
		msgs = msgs[:f.Limit]
	}
	if !f.Oldest {
		reverse(msgs)
	}
	return msgs, more, s.attachReactions(ctx, msgs)
}

func (s *Store) Message(ctx context.Context, chat, id string) (*MessageRow, error) {
	msgs, err := s.queryMessages(ctx, `SELECT `+messageColumns+` FROM mcp_messages m
		LEFT JOIN mcp_messages q ON q.chat_jid = m.chat_jid AND q.id = m.quoted_id AND m.quoted_id <> ''
		WHERE m.chat_jid = ? AND m.id = ?`, chat, id)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, sql.ErrNoRows
	}
	return msgs[0], s.attachReactions(ctx, msgs)
}

// FindMessage locates a message by ID when the caller doesn't know the chat.
func (s *Store) FindMessage(ctx context.Context, id string) (*MessageRow, error) {
	var chat string
	err := s.db.QueryRowContext(ctx, `SELECT chat_jid FROM mcp_messages WHERE id = ? LIMIT 1`, id).Scan(&chat)
	if err != nil {
		return nil, err
	}
	return s.Message(ctx, chat, id)
}

func (s *Store) RawMessage(ctx context.Context, chat, id string) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT raw FROM mcp_messages WHERE chat_jid = ? AND id = ?`, chat, id).Scan(&raw)
	return raw, err
}

// OldestMessage returns the oldest stored message of a chat, the anchor for on-demand history requests.
func (s *Store) OldestMessage(ctx context.Context, chat string) (*MessageRow, error) {
	msgs, _, err := s.Messages(ctx, MessageFilter{ChatJID: chat, Limit: 1, Oldest: true})
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, sql.ErrNoRows
	}
	return msgs[0], nil
}

func (s *Store) CountMessages(ctx context.Context, chat string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_messages WHERE chat_jid = ?`, chat).Scan(&n)
	return n, err
}

type Stats struct {
	Chats, Messages int
	Oldest, Newest  time.Time
	LastHistorySync string
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	var oldest, newest sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM mcp_chats), COUNT(*), MIN(timestamp), MAX(timestamp)
		FROM mcp_messages`).Scan(&st.Chats, &st.Messages, &oldest, &newest)
	if err != nil {
		return st, err
	}
	st.Oldest, st.Newest = unixOrZero(oldest.Int64), unixOrZero(newest.Int64)
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM mcp_meta WHERE key = 'last_history_sync'`).Scan(&st.LastHistorySync)
	return st, nil
}

func (s *Store) queryMessages(ctx context.Context, q string, args ...any) ([]*MessageRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MessageRow
	for rows.Next() {
		m := &MessageRow{}
		var ts int64
		if err := rows.Scan(&m.ChatJID, &m.ID, &m.SenderJID, &m.FromMe, &ts, &m.Kind, &m.Text, &m.PushName,
			&m.Mime, &m.FileName, &m.Size, &m.Seconds, &m.QuotedID, &m.Edited, &m.Deleted, &m.HasMedia,
			&m.QuotedText, &m.QuotedSender); err != nil {
			return nil, err
		}
		m.Timestamp = time.Unix(ts, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) attachReactions(ctx context.Context, msgs []*MessageRow) error {
	if len(msgs) == 0 {
		return nil
	}
	byKey := make(map[[2]string]*MessageRow, len(msgs))
	var conds []string
	var args []any
	for _, m := range msgs {
		byKey[[2]string{m.ChatJID, m.ID}] = m
		conds = append(conds, "(chat_jid = ? AND message_id = ?)")
		args = append(args, m.ChatJID, m.ID)
	}
	// Chunk to stay well below SQLite's bound-parameter limit.
	for len(conds) > 0 {
		n := min(len(conds), 400)
		rows, err := s.db.QueryContext(ctx, `SELECT chat_jid, message_id, sender_jid, emoji FROM mcp_reactions WHERE `+
			strings.Join(conds[:n], " OR ")+` ORDER BY timestamp`, args[:2*n]...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var chat, id string
			var r Reaction
			if err := rows.Scan(&chat, &id, &r.SenderJID, &r.Emoji); err != nil {
				rows.Close()
				return err
			}
			if m := byKey[[2]string{chat, id}]; m != nil {
				m.Reactions = append(m.Reactions, r)
			}
		}
		rows.Close()
		conds, args = conds[n:], args[2*n:]
	}
	return nil
}

var errNotInitialized = errors.New("message archive not initialized yet: run `whatsapp-mcp login` first")

// checkInitialized verifies the schema exists (relevant for read-only instances).
func (s *Store) checkInitialized(ctx context.Context) error {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('mcp_messages', 'whatsmeow_contacts')`).Scan(&n)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	if n < 2 {
		return errNotInitialized
	}
	return nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func unixOrZero(ts int64) time.Time {
	if ts == 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
