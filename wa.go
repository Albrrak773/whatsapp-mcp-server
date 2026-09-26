package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// WA owns the linked-device connection and mirrors everything it receives into the Store.
type WA struct {
	Client  *whatsmeow.Client
	store   *Store
	log     zerolog.Logger
	dataDir string

	loggedOut       atomic.Bool
	lastHistoryEvt  atomic.Int64 // unix nanos of the last history sync blob processed
	historyMessages atomic.Int64 // messages ingested from history sync since start
}

func init() {
	// Shown in the phone's "Linked devices" list.
	store.SetOSInfo("WhatsApp MCP", [3]uint32{1, 0, 0})
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
}

func newWA(ctx context.Context, db *sql.DB, st *Store, dataDir string, log zerolog.Logger) (*WA, error) {
	container := sqlstore.NewWithDB(db, "sqlite3", waLog.Zerolog(log.With().Str("component", "whatsmeow-store").Logger()))
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade whatsmeow store: %w", err)
	}
	if err := st.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate archive: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("load device: %w", err)
	}
	client := whatsmeow.NewClient(device, waLog.Zerolog(log.With().Str("component", "whatsmeow").Logger()))
	w := &WA{Client: client, store: st, log: log, dataDir: dataDir}
	client.AddEventHandler(w.handleEvent)
	return w, nil
}

func (w *WA) LoggedIn() bool { return w.Client.Store.ID != nil && !w.loggedOut.Load() }

func (w *WA) handleEvent(rawEvt any) {
	ctx := context.Background()
	switch evt := rawEvt.(type) {
	case *events.Message:
		if err := w.ingestMessage(ctx, evt); err != nil {
			w.log.Warn().Err(err).Str("id", evt.Info.ID).Msg("failed to store message")
		}
	case *events.HistorySync:
		w.ingestHistory(ctx, evt.Data)
	case *events.GroupInfo:
		if evt.Name != nil {
			_ = w.store.SetChatName(ctx, evt.JID.String(), true, evt.Name.Name)
		}
	case *events.JoinedGroup:
		_ = w.store.SetChatName(ctx, evt.JID.String(), true, evt.GroupName.Name)
	case *events.Connected:
		w.log.Info().Msg("connected to WhatsApp")
		go w.refreshGroups()
	case *events.LoggedOut:
		w.loggedOut.Store(true)
		w.log.Error().Str("reason", evt.Reason.String()).Msg("logged out by WhatsApp; run `whatsapp-mcp login` again")
	case *events.Disconnected:
		w.log.Warn().Msg("disconnected from WhatsApp (will auto-reconnect)")
	}
}

func (w *WA) refreshGroups() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	groups, err := w.Client.GetJoinedGroups(ctx)
	if err != nil {
		w.log.Warn().Err(err).Msg("failed to fetch joined groups")
		return
	}
	for _, g := range groups {
		_ = w.store.SetChatName(ctx, g.JID.String(), true, g.Name)
	}
}

// canonical maps an address to the form used as the archive key: device suffix dropped and
// LID ("hidden user") addresses mapped to phone-number JIDs when the mapping is known, so a
// person's DMs don't split into two chats.
func (w *WA) canonical(ctx context.Context, jid types.JID, alt types.JID) types.JID {
	jid = jid.ToNonAD()
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	if alt.Server == types.DefaultUserServer {
		return alt.ToNonAD()
	}
	if w.Client.Store.LIDs == nil {
		return jid
	}
	if pn, err := w.Client.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
		return pn.ToNonAD()
	}
	return jid
}

func (w *WA) ownJID() types.JID {
	if w.Client.Store.ID == nil {
		return types.EmptyJID
	}
	return w.Client.Store.ID.ToNonAD()
}

func (w *WA) ingestMessage(ctx context.Context, evt *events.Message) error {
	info := evt.Info
	if info.Chat.Server == types.BroadcastServer && info.Chat.User == "status" {
		return nil // status updates ("stories") are not chat history
	}
	var chatAlt types.JID
	if !info.IsGroup {
		if info.IsFromMe {
			chatAlt = info.RecipientAlt
		} else {
			chatAlt = info.SenderAlt
		}
	}
	chat := w.canonical(ctx, info.Chat, chatAlt)
	sender := w.canonical(ctx, info.Sender, info.SenderAlt)
	if info.IsFromMe {
		if own := w.ownJID(); !own.IsEmpty() {
			sender = own
		}
	}
	msg := evt.Message
	if msg == nil {
		return nil
	}

	if pm := msg.GetProtocolMessage(); pm != nil {
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			return w.store.MarkDeleted(ctx, chat.String(), pm.GetKey().GetID(), sender.String(), info.IsFromMe, info.Timestamp)
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			return w.store.ApplyEdit(ctx, chat.String(), pm.GetKey().GetID(), extractContent(pm.GetEditedMessage()).Text)
		}
		return nil
	}
	if r := msg.GetReactionMessage(); r != nil {
		return w.store.SetReaction(ctx, chat.String(), r.GetKey().GetID(), sender.String(), r.GetText(), info.Timestamp)
	}

	content := extractContent(msg)
	if content.Kind == "" {
		return nil
	}
	var raw []byte
	if content.Downloadable {
		raw, _ = proto.Marshal(msg)
	}
	if err := w.store.TouchChat(ctx, chat.String(), info.IsGroup, info.Timestamp); err != nil {
		return err
	}
	return w.store.UpsertMessage(ctx, &StoredMessage{
		ChatJID:   chat.String(),
		ID:        info.ID,
		SenderJID: sender.String(),
		FromMe:    info.IsFromMe,
		Timestamp: info.Timestamp,
		PushName:  info.PushName,
		Content:   content,
		Raw:       raw,
	})
}

func (w *WA) ingestHistory(ctx context.Context, data *waHistorySync.HistorySync) {
	var n int
	for _, conv := range data.GetConversations() {
		rawChat, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		var alt types.JID
		if pn, err := types.ParseJID(conv.GetPnJID()); err == nil {
			alt = pn
		}
		chat := w.canonical(ctx, rawChat, alt)
		if chat.Server == types.BroadcastServer {
			continue
		}
		isGroup := chat.Server == types.GroupServer
		name := conv.GetName()
		if name == "" {
			name = conv.GetDisplayName()
		}
		_ = w.store.SetChatName(ctx, chat.String(), isGroup, name)
		if ts := conv.GetConversationTimestamp(); ts > 0 {
			_ = w.store.TouchChat(ctx, chat.String(), isGroup, time.Unix(int64(ts), 0))
		}
		for _, hm := range conv.GetMessages() {
			evt, err := w.Client.ParseWebMessage(rawChat, hm.GetMessage())
			if err != nil {
				continue
			}
			if err := w.ingestMessage(ctx, evt); err != nil {
				w.log.Warn().Err(err).Msg("failed to store history message")
				continue
			}
			n++
		}
		_ = w.store.SetChatState(ctx, chat.String(), conv.GetUnreadCount(), conv.GetArchived())
	}
	w.historyMessages.Add(int64(n))
	w.lastHistoryEvt.Store(time.Now().UnixNano())
	_ = w.store.SetMeta(ctx, "last_history_sync", time.Now().Format(time.RFC3339))
	w.log.Info().Str("type", data.GetSyncType().String()).Uint32("progress", data.GetProgress()).
		Int("conversations", len(data.GetConversations())).Int("messages", n).Msg("history sync chunk stored")
}

// RequestOlderHistory asks the phone for `count` messages older than the oldest one archived
// for the chat. The answer arrives asynchronously as an ON_DEMAND history sync.
func (w *WA) RequestOlderHistory(ctx context.Context, chat types.JID, count int) error {
	oldest, err := w.store.OldestMessage(ctx, chat.String())
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no messages archived for %s yet, so there is no anchor to page back from", chat)
	} else if err != nil {
		return err
	}
	target := chat
	// The phone keys many 1:1 threads by LID nowadays; ask using the address it knows.
	if chat.Server == types.DefaultUserServer {
		if lid, err := w.Client.Store.LIDs.GetLIDForPN(ctx, chat); err == nil && !lid.IsEmpty() {
			target = lid
		}
	}
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: target, IsFromMe: oldest.FromMe, IsGroup: chat.Server == types.GroupServer},
		ID:            oldest.ID,
		Timestamp:     oldest.Timestamp,
	}
	_, err = w.Client.SendPeerMessage(ctx, w.Client.BuildHistorySyncRequest(info, count))
	return err
}

// DownloadMedia fetches and decrypts a media message, caching it under <data-dir>/media.
func (w *WA) DownloadMedia(ctx context.Context, m *MessageRow) (string, []byte, error) {
	dir := filepath.Join(w.dataDir, "media", safeName(m.ChatJID))
	path := filepath.Join(dir, safeName(m.ID)+extensionFor(m))
	if data, err := os.ReadFile(path); err == nil {
		return path, data, nil
	}
	raw, err := w.store.RawMessage(ctx, m.ChatJID, m.ID)
	if err != nil || len(raw) == 0 {
		return "", nil, fmt.Errorf("message %s has no downloadable media", m.ID)
	}
	var msg waE2E.Message
	if err := proto.Unmarshal(raw, &msg); err != nil {
		return "", nil, fmt.Errorf("decode stored message: %w", err)
	}
	data, err := w.Client.DownloadAny(ctx, &msg)
	if err != nil {
		return "", nil, fmt.Errorf("download failed (media older than a few weeks is often gone from WhatsApp's servers): %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", nil, err
	}
	return path, data, nil
}

func extensionFor(m *MessageRow) string {
	if ext := filepath.Ext(m.FileName); ext != "" && len(ext) <= 8 {
		return ext
	}
	mt := strings.TrimSpace(strings.Split(m.Mime, ";")[0])
	switch mt {
	case "image/jpeg":
		return ".jpg"
	case "audio/ogg":
		return ".ogg"
	case "video/mp4":
		return ".mp4"
	case "image/webp":
		return ".webp"
	}
	if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 || r == ':' {
			return '_'
		}
		return r
	}, s)
}
