package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

var (
	alice  = types.NewJID("491511111111", types.DefaultUserServer)
	bob    = types.NewJID("491522222222", types.DefaultUserServer)
	bobLID = types.NewJID("99887766", types.HiddenUserServer)
	group  = types.NewJID("120363000000000001", types.GroupServer)
	t0     = time.Date(2026, 9, 20, 10, 0, 0, 0, time.Local)
)

func textMsg(chat, sender types.JID, id string, at time.Time, text string) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: chat.Server == types.GroupServer},
			ID:            id, Timestamp: at, PushName: "push-" + sender.User,
		},
		Message: &waE2E.Message{Conversation: proto.String(text)},
	}
}

func setup(t *testing.T) (*mcp.ClientSession, *WA) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "whatsapp.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := NewStore(db)
	wa, err := newWA(ctx, db, st, dir, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	own := types.NewADJID("491500000000", 0, 7)
	wa.Client.Store.ID = &own
	wa.Client.Store.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{0}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := wa.Client.Store.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := wa.Client.Store.LIDs.PutLIDMapping(ctx, bobLID, bob); err != nil {
		t.Fatal(err)
	}
	if err := wa.Client.Store.Contacts.PutContactName(ctx, alice, "Alice", "Alice Anders"); err != nil {
		t.Fatal(err)
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(wa.ingestMessage(ctx, textMsg(alice, alice, "A1", t0, "hi, lunch tomorrow?")))
	must(wa.ingestMessage(ctx, textMsg(alice, alice, "A2", t0.Add(time.Minute), "at the usual place")))
	reply := textMsg(alice, alice, "A3", t0.Add(2*time.Minute), "typo")
	reply.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("sure, noon works"), ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("A1")}}}
	must(wa.ingestMessage(ctx, reply))
	// Bob writes from his LID address; it must land in the phone-number chat.
	fromLID := textMsg(bobLID, bobLID, "B1", t0.Add(3*time.Minute), "invoice attached")
	fromLID.Message = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("invoice.pdf"),
		Mimetype: proto.String("application/pdf"), FileLength: proto.Uint64(1234), MediaKey: []byte{1}}}
	must(wa.ingestMessage(ctx, fromLID))
	// Group message, a reaction, an edit and a revoke.
	mine := textMsg(alice, own.ToNonAD(), "A0", t0.Add(-time.Minute), "hey")
	mine.Info.IsFromMe = true
	must(wa.ingestMessage(ctx, mine))
	must(wa.ingestMessage(ctx, textMsg(group, bob, "G1", t0.Add(4*time.Minute), "Lunch poll anyone?")))
	must(wa.store.SetChatName(ctx, group.String(), true, "Team Lunch"))
	react := textMsg(group, alice, "G2", t0.Add(5*time.Minute), "")
	react.Message = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("G1")}, Text: proto.String("👍")}}
	must(wa.ingestMessage(ctx, react))
	edit := textMsg(alice, alice, "A4", t0.Add(6*time.Minute), "")
	edit.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key: &waCommon.MessageKey{ID: proto.String("A2")}, EditedMessage: &waE2E.Message{Conversation: proto.String("at the new place")}}}
	must(wa.ingestMessage(ctx, edit))
	must(wa.ingestMessage(ctx, textMsg(group, alice, "G3", t0.Add(7*time.Minute), "oops wrong chat")))
	revoke := textMsg(group, alice, "G4", t0.Add(8*time.Minute), "")
	revoke.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key: &waCommon.MessageKey{ID: proto.String("G3")}}}
	must(wa.ingestMessage(ctx, revoke))

	server := newMCPServer(&App{store: st, wa: wa, mode: "test"})
	ct, stt := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, stt, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, wa
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned error: %s", name, res.Content[0].(*mcp.TextContent).Text)
	}
	if out != nil {
		reflect.ValueOf(out).Elem().SetZero()
		data, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
	}
	return res
}

func TestTools(t *testing.T) {
	cs, _ := setup(t)

	var chats ListChatsOut
	call(t, cs, "list_chats", nil, &chats)
	if len(chats.Chats) != 3 {
		t.Fatalf("want 3 chats, got %+v", chats.Chats)
	}
	if chats.Chats[0].Name != "Team Lunch" || chats.Chats[0].LastMessage == nil {
		t.Errorf("most recent chat should be the group with a preview: %+v", chats.Chats[0])
	}
	for _, c := range chats.Chats {
		if strings.HasSuffix(c.JID, "@lid") {
			t.Errorf("LID chat leaked into chat list: %s", c.JID)
		}
	}

	var msgs MessagesOut
	call(t, cs, "list_messages", map[string]any{"chat": "alice"}, &msgs)
	if len(msgs.Messages) != 4 {
		t.Fatalf("want 4 messages with Alice, got %d", len(msgs.Messages))
	}
	if m := msgs.Messages[0]; m.Sender != "me" || !m.FromMe {
		t.Errorf("own message: %+v", m)
	}
	msgs.Messages = msgs.Messages[1:]
	if msgs.Messages[0].Sender != "Alice Anders" {
		t.Errorf("sender name not resolved: %q", msgs.Messages[0].Sender)
	}
	if m := msgs.Messages[1]; m.Text != "at the new place" || !m.Edited {
		t.Errorf("edit not applied: %+v", m)
	}
	if m := msgs.Messages[2]; m.ReplyTo == nil || m.ReplyTo.Text != "hi, lunch tomorrow?" {
		t.Errorf("reply context missing: %+v", m.ReplyTo)
	}

	call(t, cs, "list_messages", map[string]any{"chat": "alice", "limit": 1}, &msgs)
	if !msgs.HasMore || len(msgs.Messages) != 1 || msgs.Messages[0].ID != "A3" {
		t.Errorf("paging: %+v", msgs)
	}
	call(t, cs, "list_messages", map[string]any{"chat": "alice", "before": msgs.Messages[0].Time}, &msgs)
	if len(msgs.Messages) != 3 {
		t.Errorf("before-paging returned %d", len(msgs.Messages))
	}

	call(t, cs, "list_messages", map[string]any{"chat": "+49 1522 222222"}, &msgs)
	if len(msgs.Messages) != 1 || msgs.Messages[0].Media == nil || msgs.Messages[0].Media.FileName != "invoice.pdf" {
		t.Errorf("LID DM not merged into phone chat: %+v", msgs.Messages)
	}

	call(t, cs, "list_messages", map[string]any{"chat": "team"}, &msgs)
	if len(msgs.Messages) != 2 || msgs.Messages[0].Reactions[0].Emoji != "👍" || !msgs.Messages[1].Deleted || msgs.Messages[1].Text != "" {
		t.Errorf("group reactions/revoke wrong: %+v %+v", msgs.Messages[0], msgs.Messages[1])
	}

	call(t, cs, "search_messages", map[string]any{"query": "LUNCH"}, &msgs)
	if len(msgs.Messages) != 2 || msgs.Messages[1].ChatName != "Team Lunch" {
		t.Errorf("search: %+v", msgs.Messages)
	}
	call(t, cs, "search_messages", map[string]any{"query": "100%"}, &msgs)
	if len(msgs.Messages) != 0 {
		t.Errorf("LIKE wildcards not escaped")
	}

	var ctxOut ContextOut
	call(t, cs, "get_message_context", map[string]any{"message_id": "A2", "before": 1, "after": 1}, &ctxOut)
	if len(ctxOut.Messages) != 3 || ctxOut.Messages[0].ID != "A1" || ctxOut.Messages[2].ID != "A3" {
		t.Errorf("context: %+v", ctxOut.Messages)
	}

	var contacts SearchContactsOut
	call(t, cs, "search_contacts", map[string]any{"query": "anders"}, &contacts)
	if len(contacts.Contacts) != 1 || contacts.Contacts[0].Phone != "+491511111111" {
		t.Errorf("contacts: %+v", contacts)
	}

	var st StatusOut
	call(t, cs, "whatsapp_status", nil, &st)
	if st.Messages != 7 || !st.LoggedIn || st.Connected || st.Account != "491500000000@s.whatsapp.net" {
		t.Errorf("status: %+v", st)
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "download_media", Arguments: map[string]any{"message_id": "B1"}})
	if err != nil || !res.IsError {
		t.Errorf("download_media should fail cleanly while disconnected: %v %+v", err, res)
	}

	rr, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "whatsapp://chat/" + group.String()})
	if err != nil {
		t.Fatal(err)
	}
	if text := rr.Contents[0].Text; !strings.Contains(text, "Lunch poll anyone? [👍 Alice Anders]") || !strings.Contains(text, "<deleted>") {
		t.Errorf("transcript:\n%s", text)
	}
	rr, err = cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "whatsapp://chat/Alice%20Anders"})
	if err != nil || !strings.Contains(rr.Contents[0].Text, "at the new place (edited)") {
		t.Errorf("transcript by name: %v %+v", err, rr)
	}
	rr, err = cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "whatsapp://chats"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Contents[0].Text, "push-491522222222 (direct") {
		t.Errorf("push name fallback missing:\n%s", rr.Contents[0].Text)
	}
}
