package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mau.fi/whatsmeow/types"
)

// version is overridden at build time: go build -ldflags "-X main.version=..." (see Makefile).
var version = "0.1.0-dev"

const instructions = `Read-only access to the user's WhatsApp account (a linked device, like WhatsApp Web).
Chats and messages come from a local archive that is filled by live sync plus the history the phone shared at link time.
Typical flow: list_chats (or search_contacts) to find a chat -> list_messages to read it -> page back with "before".
"chat"/"sender" arguments accept a JID, a phone number, or a (partial) contact/group name.
If a chat's history looks cut off, request_older_history asks the phone for more.
This data is private. Quote only what the user needs and never forward it elsewhere without being asked.`

// App holds what the MCP handlers need. wa is nil when this instance runs read-only
// (another process owns the WhatsApp connection, or no account is linked yet).
type App struct {
	store *Store
	wa    *WA
	mode  string
}

func (a *App) online() error {
	if a.wa == nil {
		return errors.New("this server instance is read-only (another whatsapp-mcp process owns the WhatsApp connection); only the archive can be queried")
	}
	if !a.wa.LoggedIn() {
		return errors.New("no WhatsApp account linked: run `whatsapp-mcp login`")
	}
	if !a.wa.Client.IsConnected() {
		return errors.New("not connected to WhatsApp right now (reconnecting); try again shortly")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Output shapes

type ChatOut struct {
	JID           string        `json:"jid"`
	Name          string        `json:"name"`
	IsGroup       bool          `json:"is_group"`
	LastMessageAt string        `json:"last_message_at,omitempty"`
	UnreadCount   int           `json:"unread_count,omitempty"`
	Archived      bool          `json:"archived,omitempty"`
	MessageCount  int           `json:"archived_messages"`
	LastMessage   *MessageOut   `json:"last_message,omitempty"`
	Topic         string        `json:"topic,omitempty"`
	Participants  []Participant `json:"participants,omitempty"`
	FirstMessage  string        `json:"oldest_archived_message_at,omitempty"`
}

type Participant struct {
	JID   string `json:"jid"`
	Name  string `json:"name,omitempty"`
	Admin bool   `json:"admin,omitempty"`
}

type MessageOut struct {
	ID        string        `json:"id"`
	ChatJID   string        `json:"chat_jid"`
	ChatName  string        `json:"chat_name,omitempty"`
	Time      string        `json:"time"`
	Sender    string        `json:"sender"`
	SenderJID string        `json:"sender_jid"`
	FromMe    bool          `json:"from_me,omitempty"`
	Kind      string        `json:"kind"`
	Text      string        `json:"text,omitempty"`
	Media     *MediaOut     `json:"media,omitempty"`
	ReplyTo   *QuoteOut     `json:"reply_to,omitempty"`
	Reactions []ReactionOut `json:"reactions,omitempty"`
	Edited    bool          `json:"edited,omitempty"`
	Deleted   bool          `json:"deleted,omitempty"`
}

type MediaOut struct {
	Mime         string `json:"mime,omitempty"`
	FileName     string `json:"file_name,omitempty"`
	Size         int64  `json:"size_bytes,omitempty"`
	Seconds      int    `json:"duration_seconds,omitempty"`
	Downloadable bool   `json:"downloadable"`
}

type QuoteOut struct {
	ID     string `json:"id"`
	Sender string `json:"sender,omitempty"`
	Text   string `json:"text,omitempty"`
}

type ReactionOut struct {
	Emoji  string `json:"emoji"`
	Sender string `json:"sender"`
}

type MessagesOut struct {
	Messages []*MessageOut `json:"messages"`
	HasMore  bool          `json:"has_more"`
	Hint     string        `json:"hint,omitempty"`
}

func (a *App) messageOut(ctx context.Context, m *MessageRow, withChatName bool) *MessageOut {
	names := a.store.Names(ctx)
	out := &MessageOut{
		ID:        m.ID,
		ChatJID:   m.ChatJID,
		Time:      m.Timestamp.Format(time.RFC3339),
		Sender:    a.personName(names, m.SenderJID, m.PushName, m.FromMe),
		SenderJID: m.SenderJID,
		FromMe:    m.FromMe,
		Kind:      m.Kind,
		Text:      m.Text,
		Edited:    m.Edited,
		Deleted:   m.Deleted,
	}
	if withChatName {
		out.ChatName = names[m.ChatJID]
	}
	if m.Mime != "" || m.FileName != "" || m.HasMedia {
		out.Media = &MediaOut{Mime: m.Mime, FileName: m.FileName, Size: m.Size, Seconds: m.Seconds, Downloadable: m.HasMedia}
	}
	if m.QuotedID != "" {
		out.ReplyTo = &QuoteOut{ID: m.QuotedID, Text: truncate(m.QuotedText, 200)}
		if m.QuotedSender != "" {
			out.ReplyTo.Sender = a.personName(names, m.QuotedSender, "", m.QuotedSender == a.ownJID())
		}
	}
	for _, r := range m.Reactions {
		out.Reactions = append(out.Reactions, ReactionOut{Emoji: r.Emoji, Sender: a.personName(names, r.SenderJID, "", r.SenderJID == a.ownJID())})
	}
	return out
}

func (a *App) ownJID() string {
	if a.wa == nil || a.wa.Client.Store.ID == nil {
		return ""
	}
	return a.wa.Client.Store.ID.ToNonAD().String()
}

func (a *App) personName(names map[string]string, jid, pushName string, fromMe bool) string {
	if fromMe {
		return "me"
	}
	if n := names[jid]; n != "" {
		return n
	}
	if pushName != "" {
		return pushName
	}
	if u, ok := strings.CutSuffix(jid, "@s.whatsapp.net"); ok {
		return "+" + u
	}
	return jid
}

func (a *App) chatOut(ctx context.Context, c *ChatRow, withLast bool) *ChatOut {
	out := &ChatOut{
		JID: c.JID, Name: c.Name, IsGroup: c.IsGroup, UnreadCount: c.UnreadCount,
		Archived: c.Archived, MessageCount: c.MessageCount,
	}
	if out.Name == "" {
		out.Name = a.personName(nil, c.JID, "", false)
	}
	if !c.LastMessageAt.IsZero() {
		out.LastMessageAt = c.LastMessageAt.Format(time.RFC3339)
	}
	if withLast {
		if msgs, _, err := a.store.Messages(ctx, MessageFilter{ChatJID: c.JID, Limit: 1}); err == nil && len(msgs) > 0 {
			out.LastMessage = a.messageOut(ctx, msgs[0], false)
			out.LastMessage.Text = truncate(out.LastMessage.Text, 160)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Argument resolution

// resolveChat turns a JID, phone number or name fragment into an archived chat JID.
func (a *App) resolveChat(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("chat is required")
	}
	if strings.Contains(ref, "@") {
		jid, err := types.ParseJID(ref)
		if err != nil {
			return "", fmt.Errorf("invalid JID %q: %w", ref, err)
		}
		return a.lidToPN(ctx, jid.ToNonAD().String()), nil
	}
	if digits := phoneDigits(ref); digits != "" {
		return digits + "@s.whatsapp.net", nil
	}
	chats, err := a.store.Chats(ctx)
	if err != nil {
		return "", err
	}
	var matches []*ChatRow
	for _, c := range chats {
		if strings.EqualFold(c.Name, ref) {
			return c.JID, nil
		}
		if containsFold(c.Name, ref) {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no chat matches %q; use list_chats or search_contacts to find it", ref)
	case 1:
		return matches[0].JID, nil
	}
	var cands []string
	for i, c := range matches {
		if i == 10 {
			cands = append(cands, fmt.Sprintf("... and %d more", len(matches)-10))
			break
		}
		cands = append(cands, fmt.Sprintf("%s (%s)", c.Name, c.JID))
	}
	return "", fmt.Errorf("%q is ambiguous, pass one of these JIDs instead: %s", ref, strings.Join(cands, "; "))
}

// resolvePerson turns a JID, phone number or contact name into a user JID.
func (a *App) resolvePerson(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if strings.EqualFold(ref, "me") && a.ownJID() != "" {
		return a.ownJID(), nil
	}
	if strings.Contains(ref, "@") {
		jid, err := types.ParseJID(ref)
		if err != nil {
			return "", fmt.Errorf("invalid JID %q: %w", ref, err)
		}
		return a.lidToPN(ctx, jid.ToNonAD().String()), nil
	}
	if digits := phoneDigits(ref); digits != "" {
		return digits + "@s.whatsapp.net", nil
	}
	contacts := a.contacts(ctx, ref)
	switch len(contacts) {
	case 0:
		return "", fmt.Errorf("no contact matches %q", ref)
	case 1:
		return contacts[0].JID, nil
	}
	for _, c := range contacts {
		if strings.EqualFold(c.Name, ref) {
			return c.JID, nil
		}
	}
	var cands []string
	for i, c := range contacts {
		if i == 10 {
			break
		}
		cands = append(cands, fmt.Sprintf("%s (%s)", c.Name, c.JID))
	}
	return "", fmt.Errorf("%q is ambiguous, pass one of these JIDs instead: %s", ref, strings.Join(cands, "; "))
}

func (a *App) lidToPN(ctx context.Context, jid string) string {
	lid, ok := strings.CutSuffix(jid, "@lid")
	if !ok {
		return jid
	}
	var pn string
	if a.store.db.QueryRowContext(ctx, `SELECT pn FROM whatsmeow_lid_map WHERE lid = ?`, lid).Scan(&pn) == nil {
		return pn + "@s.whatsapp.net"
	}
	return jid
}

type ContactOut struct {
	JID   string `json:"jid"`
	Name  string `json:"name"`
	Phone string `json:"phone,omitempty"`
}

func (a *App) contacts(ctx context.Context, query string) []ContactOut {
	names := a.store.Names(ctx)
	var out []ContactOut
	qDigits := phoneDigits(query)
	for jid, name := range names {
		if !strings.HasSuffix(jid, "@s.whatsapp.net") && !strings.HasSuffix(jid, "@lid") {
			continue
		}
		if strings.HasSuffix(jid, "@lid") && names[a.lidToPN(ctx, jid)] != "" && a.lidToPN(ctx, jid) != jid {
			continue // shown under the phone-number JID
		}
		user := strings.Split(jid, "@")[0]
		if query != "" && !containsFold(name, query) && (qDigits == "" || !strings.Contains(user, qDigits)) {
			continue
		}
		c := ContactOut{JID: jid, Name: name}
		if strings.HasSuffix(jid, "@s.whatsapp.net") {
			c.Phone = "+" + user
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// ---------------------------------------------------------------------------
// Tools

type StatusIn struct{}

type StatusOut struct {
	Mode            string `json:"mode"`
	LoggedIn        bool   `json:"logged_in"`
	Connected       bool   `json:"connected"`
	Account         string `json:"account,omitempty"`
	Chats           int    `json:"archived_chats"`
	Messages        int    `json:"archived_messages"`
	Oldest          string `json:"oldest_message_at,omitempty"`
	Newest          string `json:"newest_message_at,omitempty"`
	LastHistorySync string `json:"last_history_sync,omitempty"`
}

func (a *App) status(ctx context.Context, _ *mcp.CallToolRequest, _ StatusIn) (*mcp.CallToolResult, StatusOut, error) {
	st, err := a.store.Stats(ctx)
	if err != nil {
		return nil, StatusOut{}, err
	}
	out := StatusOut{Mode: a.mode, Chats: st.Chats, Messages: st.Messages, LastHistorySync: st.LastHistorySync}
	if !st.Oldest.IsZero() {
		out.Oldest, out.Newest = st.Oldest.Format(time.RFC3339), st.Newest.Format(time.RFC3339)
	}
	if a.wa != nil {
		out.LoggedIn = a.wa.LoggedIn()
		out.Connected = a.wa.Client.IsConnected()
		out.Account = a.ownJID()
	} else {
		var jid string
		if a.store.db.QueryRowContext(ctx, `SELECT jid FROM whatsmeow_device LIMIT 1`).Scan(&jid) == nil {
			out.LoggedIn = true
			if parsed, err := types.ParseJID(jid); err == nil {
				out.Account = parsed.ToNonAD().String()
			}
		}
	}
	return nil, out, nil
}

type ListChatsIn struct {
	Query           string `json:"query,omitempty" jsonschema:"filter by chat/contact name, phone number or JID (case-insensitive substring)"`
	Type            string `json:"type,omitempty" jsonschema:"'all' (default), 'direct' or 'group'"`
	IncludeArchived *bool  `json:"include_archived,omitempty" jsonschema:"include archived chats (default true)"`
	UnreadOnly      bool   `json:"unread_only,omitempty" jsonschema:"only chats with unread messages (as of the last history sync)"`
	Limit           int    `json:"limit,omitempty" jsonschema:"max chats to return (default 30, max 200)"`
	Offset          int    `json:"offset,omitempty" jsonschema:"pagination offset"`
}

type ListChatsOut struct {
	Chats   []*ChatOut `json:"chats"`
	Total   int        `json:"total_matching"`
	HasMore bool       `json:"has_more"`
}

func (a *App) listChats(ctx context.Context, _ *mcp.CallToolRequest, in ListChatsIn) (*mcp.CallToolResult, ListChatsOut, error) {
	chats, err := a.store.Chats(ctx)
	if err != nil {
		return nil, ListChatsOut{}, err
	}
	limit := clamp(in.Limit, 30, 200)
	qDigits := phoneDigits(in.Query)
	var filtered []*ChatRow
	for _, c := range chats {
		switch strings.ToLower(in.Type) {
		case "direct", "dm", "dms":
			if c.IsGroup {
				continue
			}
		case "group", "groups":
			if !c.IsGroup {
				continue
			}
		}
		if c.Archived && in.IncludeArchived != nil && !*in.IncludeArchived {
			continue
		}
		if in.UnreadOnly && c.UnreadCount == 0 {
			continue
		}
		if in.Query != "" && !containsFold(c.Name, in.Query) && !strings.Contains(c.JID, in.Query) &&
			(qDigits == "" || !strings.Contains(c.JID, qDigits)) {
			continue
		}
		filtered = append(filtered, c)
	}
	out := ListChatsOut{Chats: []*ChatOut{}, Total: len(filtered)}
	start := min(max(in.Offset, 0), len(filtered))
	end := min(start+limit, len(filtered))
	for _, c := range filtered[start:end] {
		out.Chats = append(out.Chats, a.chatOut(ctx, c, true))
	}
	out.HasMore = end < len(filtered)
	return nil, out, nil
}

type GetChatIn struct {
	Chat string `json:"chat" jsonschema:"chat JID, phone number, or contact/group name"`
}

func (a *App) getChat(ctx context.Context, _ *mcp.CallToolRequest, in GetChatIn) (*mcp.CallToolResult, *ChatOut, error) {
	jid, err := a.resolveChat(ctx, in.Chat)
	if err != nil {
		return nil, nil, err
	}
	c, err := a.store.Chat(ctx, jid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("no archived chat %s", jid)
	} else if err != nil {
		return nil, nil, err
	}
	out := a.chatOut(ctx, c, true)
	if oldest, err := a.store.OldestMessage(ctx, jid); err == nil {
		out.FirstMessage = oldest.Timestamp.Format(time.RFC3339)
	}
	if c.IsGroup && a.online() == nil {
		gjid, _ := types.ParseJID(jid)
		if info, err := a.wa.Client.GetGroupInfo(ctx, gjid); err == nil {
			names := a.store.Names(ctx)
			out.Topic = info.Topic
			for _, p := range info.Participants {
				pj := p.JID
				if !p.PhoneNumber.IsEmpty() {
					pj = p.PhoneNumber
				}
				out.Participants = append(out.Participants, Participant{
					JID: pj.String(), Name: a.personName(names, pj.String(), "", false), Admin: p.IsAdmin || p.IsSuperAdmin,
				})
			}
		}
	}
	return nil, out, nil
}

type ListMessagesIn struct {
	Chat   string `json:"chat" jsonschema:"chat JID, phone number, or contact/group name"`
	Before string `json:"before,omitempty" jsonschema:"only messages strictly before this time (RFC3339 or YYYY-MM-DD[ HH:MM]); pass the oldest returned 'time' to page back"`
	After  string `json:"after,omitempty" jsonschema:"only messages strictly after this time (RFC3339 or YYYY-MM-DD[ HH:MM])"`
	Sender string `json:"sender,omitempty" jsonschema:"only messages from this person (JID, phone, name, or 'me')"`
	Kind   string `json:"kind,omitempty" jsonschema:"only this message kind: text, image, video, gif, voice, audio, document, sticker, location, contact, poll, event"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max messages (default 50, max 500)"`
	Oldest bool   `json:"oldest_first,omitempty" jsonschema:"take the earliest matching messages instead of the most recent (e.g. to read forward from 'after')"`
}

func (a *App) listMessages(ctx context.Context, _ *mcp.CallToolRequest, in ListMessagesIn) (*mcp.CallToolResult, MessagesOut, error) {
	chat, err := a.resolveChat(ctx, in.Chat)
	if err != nil {
		return nil, MessagesOut{}, err
	}
	f := MessageFilter{ChatJID: chat, Kind: in.Kind, Limit: clamp(in.Limit, 50, 500), Oldest: in.Oldest}
	if err := a.fillCommonFilter(ctx, &f, in.Sender, in.After, in.Before); err != nil {
		return nil, MessagesOut{}, err
	}
	out, err := a.queryMessages(ctx, f, false)
	if err == nil && len(out.Messages) == 0 {
		out.Hint = "no archived messages match; the chat may predate the linked device's history. Try request_older_history."
	} else if err == nil && out.HasMore && !in.Oldest {
		out.Hint = fmt.Sprintf("older messages exist: call again with before=%q", out.Messages[0].Time)
	}
	return nil, out, err
}

type SearchMessagesIn struct {
	Query  string `json:"query" jsonschema:"text to search for (case-insensitive substring of message text, captions and file names)"`
	Chat   string `json:"chat,omitempty" jsonschema:"restrict to one chat (JID, phone number, or name)"`
	Sender string `json:"sender,omitempty" jsonschema:"restrict to one sender (JID, phone, name, or 'me')"`
	After  string `json:"after,omitempty" jsonschema:"only messages after this time (RFC3339 or YYYY-MM-DD)"`
	Before string `json:"before,omitempty" jsonschema:"only messages before this time (RFC3339 or YYYY-MM-DD)"`
	Kind   string `json:"kind,omitempty" jsonschema:"only this message kind (text, image, document, ...)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max results (default 30, max 200)"`
	Offset int    `json:"offset,omitempty" jsonschema:"skip this many of the newest matches (pagination)"`
}

func (a *App) searchMessages(ctx context.Context, _ *mcp.CallToolRequest, in SearchMessagesIn) (*mcp.CallToolResult, MessagesOut, error) {
	if strings.TrimSpace(in.Query) == "" && in.Sender == "" && in.Kind == "" {
		return nil, MessagesOut{}, errors.New("give at least one of query, sender or kind")
	}
	f := MessageFilter{Query: strings.TrimSpace(in.Query), Kind: in.Kind, Limit: clamp(in.Limit, 30, 200), Offset: max(in.Offset, 0)}
	if in.Chat != "" {
		chat, err := a.resolveChat(ctx, in.Chat)
		if err != nil {
			return nil, MessagesOut{}, err
		}
		f.ChatJID = chat
	}
	if err := a.fillCommonFilter(ctx, &f, in.Sender, in.After, in.Before); err != nil {
		return nil, MessagesOut{}, err
	}
	out, err := a.queryMessages(ctx, f, f.ChatJID == "")
	if err == nil && out.HasMore {
		out.Hint = fmt.Sprintf("more matches exist: call again with offset=%d", f.Offset+f.Limit)
	}
	return nil, out, err
}

func (a *App) fillCommonFilter(ctx context.Context, f *MessageFilter, sender, after, before string) error {
	var err error
	if sender != "" {
		if f.SenderJID, err = a.resolvePerson(ctx, sender); err != nil {
			return err
		}
	}
	if f.After, err = parseTime(after); err != nil {
		return fmt.Errorf("after: %w", err)
	}
	if f.Before, err = parseTime(before); err != nil {
		return fmt.Errorf("before: %w", err)
	}
	return nil
}

func (a *App) queryMessages(ctx context.Context, f MessageFilter, withChatName bool) (MessagesOut, error) {
	rows, more, err := a.store.Messages(ctx, f)
	if err != nil {
		return MessagesOut{}, err
	}
	out := MessagesOut{Messages: []*MessageOut{}, HasMore: more}
	for _, m := range rows {
		out.Messages = append(out.Messages, a.messageOut(ctx, m, withChatName))
	}
	return out, nil
}

type ContextIn struct {
	MessageID string `json:"message_id" jsonschema:"the message id"`
	Chat      string `json:"chat,omitempty" jsonschema:"chat of the message (optional, speeds up lookup)"`
	Before    int    `json:"before,omitempty" jsonschema:"messages to include before it (default 10, max 100)"`
	After     int    `json:"after,omitempty" jsonschema:"messages to include after it (default 10, max 100)"`
}

type ContextOut struct {
	Target   *MessageOut   `json:"target"`
	Messages []*MessageOut `json:"messages"`
}

func (a *App) messageContext(ctx context.Context, _ *mcp.CallToolRequest, in ContextIn) (*mcp.CallToolResult, ContextOut, error) {
	target, err := a.findMessage(ctx, in.Chat, in.MessageID)
	if err != nil {
		return nil, ContextOut{}, err
	}
	// Timestamps are second-granular, so fetch the neighbours inclusively and drop the target itself.
	before, _, err := a.store.Messages(ctx, MessageFilter{ChatJID: target.ChatJID, Before: target.Timestamp.Add(time.Second), Limit: clamp(in.Before, 10, 100) + 5})
	if err != nil {
		return nil, ContextOut{}, err
	}
	after, _, err := a.store.Messages(ctx, MessageFilter{ChatJID: target.ChatJID, After: target.Timestamp.Add(-time.Second), Limit: clamp(in.After, 10, 100) + 5, Oldest: true})
	if err != nil {
		return nil, ContextOut{}, err
	}
	idx := func(ms []*MessageRow) int {
		for i, m := range ms {
			if m.ID == target.ID {
				return i
			}
		}
		return -1
	}
	if i := idx(before); i >= 0 {
		before = before[:i]
	}
	before = before[max(0, len(before)-clamp(in.Before, 10, 100)):]
	if i := idx(after); i >= 0 {
		after = after[i+1:]
	}
	after = after[:min(len(after), clamp(in.After, 10, 100))]

	out := ContextOut{Target: a.messageOut(ctx, target, true), Messages: []*MessageOut{}}
	for _, m := range append(append(before, target), after...) {
		out.Messages = append(out.Messages, a.messageOut(ctx, m, false))
	}
	return nil, out, nil
}

func (a *App) findMessage(ctx context.Context, chatRef, id string) (*MessageRow, error) {
	if id == "" {
		return nil, errors.New("message_id is required")
	}
	var m *MessageRow
	var err error
	if chatRef != "" {
		chat, rerr := a.resolveChat(ctx, chatRef)
		if rerr != nil {
			return nil, rerr
		}
		m, err = a.store.Message(ctx, chat, id)
	} else {
		m, err = a.store.FindMessage(ctx, id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("message %s not found in the archive", id)
	}
	return m, err
}

type SearchContactsIn struct {
	Query string `json:"query" jsonschema:"name or phone number fragment"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results (default 30, max 200)"`
}

type SearchContactsOut struct {
	Contacts []ContactOut `json:"contacts"`
	HasMore  bool         `json:"has_more"`
}

func (a *App) searchContacts(ctx context.Context, _ *mcp.CallToolRequest, in SearchContactsIn) (*mcp.CallToolResult, SearchContactsOut, error) {
	all := a.contacts(ctx, strings.TrimSpace(in.Query))
	limit := clamp(in.Limit, 30, 200)
	out := SearchContactsOut{Contacts: all[:min(limit, len(all))], HasMore: len(all) > limit}
	if out.Contacts == nil {
		out.Contacts = []ContactOut{}
	}
	return nil, out, nil
}

type DownloadMediaIn struct {
	MessageID string `json:"message_id" jsonschema:"id of a message with media (media.downloadable = true)"`
	Chat      string `json:"chat,omitempty" jsonschema:"chat of the message (optional)"`
}

type DownloadMediaOut struct {
	Path     string `json:"path"`
	Mime     string `json:"mime"`
	Size     int    `json:"size_bytes"`
	Inlined  bool   `json:"inlined_as_image"`
	FileName string `json:"file_name,omitempty"`
}

const maxInlineImage = 4 << 20

func (a *App) downloadMedia(ctx context.Context, _ *mcp.CallToolRequest, in DownloadMediaIn) (*mcp.CallToolResult, DownloadMediaOut, error) {
	m, err := a.findMessage(ctx, in.Chat, in.MessageID)
	if err != nil {
		return nil, DownloadMediaOut{}, err
	}
	if !m.HasMedia {
		return nil, DownloadMediaOut{}, fmt.Errorf("message %s has no downloadable media", m.ID)
	}
	if err := a.online(); err != nil {
		return nil, DownloadMediaOut{}, err
	}
	path, data, err := a.wa.DownloadMedia(ctx, m)
	if err != nil {
		return nil, DownloadMediaOut{}, err
	}
	mimeType := strings.TrimSpace(strings.Split(m.Mime, ";")[0])
	out := DownloadMediaOut{Path: path, Mime: mimeType, Size: len(data), FileName: m.FileName}
	outJSON, _ := json.Marshal(&out)
	res := &mcp.CallToolResult{}
	if strings.HasPrefix(mimeType, "image/") && mimeType != "image/webp" && len(data) <= maxInlineImage {
		out.Inlined = true
		outJSON, _ = json.Marshal(&out)
		res.Content = []mcp.Content{&mcp.TextContent{Text: string(outJSON)}, &mcp.ImageContent{Data: data, MIMEType: mimeType}}
	} else {
		res.Content = []mcp.Content{&mcp.TextContent{Text: string(outJSON)}}
	}
	return res, out, nil
}

type RequestHistoryIn struct {
	Chat        string `json:"chat" jsonschema:"chat JID, phone number, or contact/group name"`
	Count       int    `json:"count,omitempty" jsonschema:"how many older messages to ask the phone for (default 50, max 200)"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the phone's answer (default 20, max 60)"`
}

type RequestHistoryOut struct {
	Requested   int    `json:"requested"`
	NewMessages int    `json:"new_messages"`
	OldestNow   string `json:"oldest_archived_message_at,omitempty"`
	Note        string `json:"note,omitempty"`
}

func (a *App) requestHistory(ctx context.Context, _ *mcp.CallToolRequest, in RequestHistoryIn) (*mcp.CallToolResult, RequestHistoryOut, error) {
	if err := a.online(); err != nil {
		return nil, RequestHistoryOut{}, err
	}
	chat, err := a.resolveChat(ctx, in.Chat)
	if err != nil {
		return nil, RequestHistoryOut{}, err
	}
	jid, _ := types.ParseJID(chat)
	count := clamp(in.Count, 50, 200)
	before, _ := a.store.CountMessages(ctx, chat)
	if err := a.wa.RequestOlderHistory(ctx, jid, count); err != nil {
		return nil, RequestHistoryOut{}, err
	}
	out := RequestHistoryOut{Requested: count}
	deadline := time.After(time.Duration(clamp(in.WaitSeconds, 20, 60)) * time.Second)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	stable := 0
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case <-deadline:
			break wait
		case <-tick.C:
			n, _ := a.store.CountMessages(ctx, chat)
			if n > before+out.NewMessages {
				out.NewMessages = n - before
				stable = 0
			} else if out.NewMessages > 0 {
				if stable++; stable >= 2 {
					break wait
				}
			}
		}
	}
	if oldest, err := a.store.OldestMessage(ctx, chat); err == nil {
		out.OldestNow = oldest.Timestamp.Format(time.RFC3339)
	}
	if out.NewMessages == 0 {
		out.Note = "the phone has not answered yet (it must be online; results may still arrive later) or there is no older history for this chat"
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// Resources

func (a *App) chatsResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	chats, err := a.store.Chats(ctx)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("# WhatsApp chats (most recent first)\n\n")
	for i, c := range chats {
		if i == 100 {
			fmt.Fprintf(&b, "\n... %d more (use the list_chats tool)\n", len(chats)-100)
			break
		}
		co := a.chatOut(ctx, c, false)
		kind := "direct"
		if c.IsGroup {
			kind = "group"
		}
		fmt.Fprintf(&b, "- %s (%s, %s) last activity %s, %d archived messages, resource whatsapp://chat/%s\n",
			co.Name, kind, c.JID, shortTime(c.LastMessageAt), c.MessageCount, url.PathEscape(c.JID))
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/markdown", Text: b.String()}}}, nil
}

func (a *App) chatResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	raw, ok := strings.CutPrefix(req.Params.URI, "whatsapp://chat/")
	if !ok {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	ref, err := url.PathUnescape(raw)
	if err != nil {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	chat, err := a.resolveChat(ctx, ref)
	if err != nil {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	c, err := a.store.Chat(ctx, chat)
	if err != nil {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	msgs, more, err := a.store.Messages(ctx, MessageFilter{ChatJID: chat, Limit: 200})
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	name := a.chatOut(ctx, c, false).Name
	fmt.Fprintf(&b, "# %s (%s)\n", name, chat)
	if more {
		b.WriteString("(showing the latest 200 messages; use list_messages with `before` for older ones)\n")
	}
	b.WriteString("\n")
	for _, m := range msgs {
		b.WriteString(a.transcriptLine(ctx, m))
		b.WriteString("\n")
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/markdown", Text: b.String()}}}, nil
}

func (a *App) transcriptLine(ctx context.Context, m *MessageRow) string {
	o := a.messageOut(ctx, m, false)
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s: ", shortTime(m.Timestamp), o.Sender)
	if o.ReplyTo != nil {
		fmt.Fprintf(&b, "(replying to %s: %q) ", o.ReplyTo.Sender, truncate(o.ReplyTo.Text, 80))
	}
	switch {
	case o.Deleted:
		b.WriteString("<deleted>")
	case o.Kind == "text":
		b.WriteString(o.Text)
	default:
		label := o.Kind
		if m.FileName != "" {
			label += " " + m.FileName
		}
		if m.Seconds > 0 {
			label += fmt.Sprintf(" %ds", m.Seconds)
		}
		fmt.Fprintf(&b, "<%s>", label)
		if o.Text != "" {
			b.WriteString(" " + o.Text)
		}
	}
	if o.Edited {
		b.WriteString(" (edited)")
	}
	for _, r := range o.Reactions {
		fmt.Fprintf(&b, " [%s %s]", r.Emoji, r.Sender)
	}
	fmt.Fprintf(&b, " {id:%s}", m.ID)
	return b.String()
}

func (a *App) statusResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	_, st, err := a.status(ctx, nil, StatusIn{})
	if err != nil {
		return nil, err
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(data)}}}, nil
}

// ---------------------------------------------------------------------------

func newMCPServer(a *App) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "whatsapp-mcp", Title: "WhatsApp (read-only)", Version: version},
		&mcp.ServerOptions{Instructions: instructions})
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(false)}

	mcp.AddTool(s, &mcp.Tool{Name: "whatsapp_status", Annotations: ro,
		Description: "Connection state of the linked WhatsApp account and size/time range of the local message archive."}, a.status)
	mcp.AddTool(s, &mcp.Tool{Name: "list_chats", Annotations: ro,
		Description: "List WhatsApp chats (direct and group), most recently active first, with a preview of the last message. Filter by name/number, type, unread."}, a.listChats)
	mcp.AddTool(s, &mcp.Tool{Name: "get_chat", Annotations: ro,
		Description: "Details for one chat: name, archive coverage, last message, and for groups the topic and participants (when connected)."}, a.getChat)
	mcp.AddTool(s, &mcp.Tool{Name: "list_messages", Annotations: ro,
		Description: "Read a chat's message history in chronological order (latest `limit` by default). Page back with `before` set to the oldest returned time. Filters: time range, sender, kind."}, a.listMessages)
	mcp.AddTool(s, &mcp.Tool{Name: "search_messages", Annotations: ro,
		Description: "Full-text search across all chats (or one chat) for messages containing `query`; newest matches first-page, returned chronologically with chat names. Use get_message_context to see the surrounding conversation."}, a.searchMessages)
	mcp.AddTool(s, &mcp.Tool{Name: "get_message_context", Annotations: ro,
		Description: "Show a message together with the messages right before and after it in its chat."}, a.messageContext)
	mcp.AddTool(s, &mcp.Tool{Name: "search_contacts", Annotations: ro,
		Description: "Find people by name or phone number fragment (address book names, WhatsApp profile names). Returns JIDs usable as `chat`/`sender`."}, a.searchContacts)
	mcp.AddTool(s, &mcp.Tool{Name: "download_media", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(true)},
		Description: "Download the image/video/audio/document attached to a message into the local media cache and return its file path. Images are also returned inline so you can look at them. Requires a live connection."}, a.downloadMedia)
	mcp.AddTool(s, &mcp.Tool{Name: "request_older_history", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
		Description: "Ask the user's phone to send messages older than the oldest archived one for a chat (WhatsApp on-demand history sync). Waits briefly and reports how many arrived. The phone must be online. Repeat to go further back."}, a.requestHistory)

	s.AddResource(&mcp.Resource{URI: "whatsapp://status", Name: "status", MIMEType: "application/json",
		Description: "Connection state and archive statistics"}, a.statusResource)
	s.AddResource(&mcp.Resource{URI: "whatsapp://chats", Name: "chats", MIMEType: "text/markdown",
		Description: "The 100 most recently active chats"}, a.chatsResource)
	s.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "whatsapp://chat/{+chat}", Name: "chat transcript", MIMEType: "text/markdown",
		Description: "Transcript of the latest 200 messages of a chat; {chat} is a URL-escaped JID, phone number or name"}, a.chatResource)
	return s
}

// ---------------------------------------------------------------------------
// helpers

func clamp(v, def, maxV int) int {
	if v <= 0 {
		return def
	}
	return min(v, maxV)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// phoneDigits returns the digits of s if s looks like a phone number, else "".
func phoneDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return ""
		}
	}
	if b.Len() < 6 {
		return ""
	}
	return b.String()
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q (use RFC3339 or YYYY-MM-DD[ HH:MM])", s)
}

func shortTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04")
}
