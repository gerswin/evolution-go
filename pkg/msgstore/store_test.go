package msgstore

import (
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "messages.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func at(minute int) time.Time {
	return time.Date(2026, 1, 1, 12, minute, 0, 0, time.UTC)
}

func TestStoreSaveListAndPage(t *testing.T) {
	s := newStore(t)
	const alice, bob = "111@s.whatsapp.net", "222@s.whatsapp.net"

	err := s.Save("inst",
		Message{ID: "a1", ChatJID: alice, SenderJID: alice, Type: "text", Text: "hola", Timestamp: at(1)},
		Message{ID: "a2", ChatJID: alice, FromMe: true, Type: "text", Text: "qué tal", Timestamp: at(2)},
		Message{ID: "a3", ChatJID: alice, SenderJID: alice, Type: "image", Text: "foto", Timestamp: at(3)},
		Message{ID: "b1", ChatJID: bob, SenderJID: bob, Type: "text", Text: "100% listo", Timestamp: at(5)},
	)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	// A replay must not duplicate or overwrite.
	if err := s.Save("inst", Message{ID: "a1", ChatJID: alice, Type: "text", Text: "OVERWRITTEN", Timestamp: at(9)}); err != nil {
		t.Fatalf("Save replay: %v", err)
	}
	// Another instance's data must stay invisible.
	if err := s.Save("other", Message{ID: "x1", ChatJID: alice, Type: "text", Text: "hola secreto", Timestamp: at(8)}); err != nil {
		t.Fatalf("Save other: %v", err)
	}
	if err := s.SetChatName("inst", alice, "Alice"); err != nil {
		t.Fatalf("SetChatName: %v", err)
	}

	chats, err := s.ListChats("inst", "", 10)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 2 || chats[0].JID != bob || chats[1].JID != alice {
		t.Fatalf("chats not ordered by recency: %+v", chats)
	}
	if c := chats[1]; c.Name != "Alice" || c.MessageCount != 3 || c.LastMessageText != "foto" || c.LastMessageType != "image" || !c.LastMessageAt.Equal(at(3)) {
		t.Fatalf("unexpected alice summary: %+v", c)
	}

	if chats, _ = s.ListChats("inst", "ali", 10); len(chats) != 1 || chats[0].JID != alice {
		t.Fatalf("name filter failed: %+v", chats)
	}

	msgs, err := s.Messages("inst", alice, time.Time{}, 2)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].ID != "a2" || msgs[1].ID != "a3" {
		t.Fatalf("expected latest two, oldest first: %+v", msgs)
	}
	older, _ := s.Messages("inst", alice, msgs[0].Timestamp, 2)
	if len(older) != 1 || older[0].ID != "a1" || older[0].Text != "hola" {
		t.Fatalf("paging back failed: %+v", older)
	}
}

func TestStoreSearchEditDeletePurge(t *testing.T) {
	s := newStore(t)
	const alice, bob = "111@s.whatsapp.net", "222@s.whatsapp.net"

	if err := s.Save("inst",
		Message{ID: "a1", ChatJID: alice, Type: "text", Text: "Reunión mañana", Timestamp: at(1)},
		Message{ID: "b1", ChatJID: bob, Type: "text", Text: "100% listo", Timestamp: at(2)},
		Message{ID: "b2", ChatJID: bob, Type: "text", Text: "1000 listo", Timestamp: at(3)},
	); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got, _ := s.Search("inst", "reuni", "", 10); len(got) != 1 || got[0].ID != "a1" {
		t.Fatalf("case-insensitive search failed: %+v", got)
	}
	// "%" must match literally, not act as a wildcard.
	if got, _ := s.Search("inst", "100%", "", 10); len(got) != 1 || got[0].ID != "b1" {
		t.Fatalf("LIKE wildcard was not escaped: %+v", got)
	}
	if got, _ := s.Search("inst", "listo", alice, 10); len(got) != 0 {
		t.Fatalf("chat filter ignored: %+v", got)
	}

	if err := s.EditText("inst", alice, "a1", "Reunión pasado mañana"); err != nil {
		t.Fatalf("EditText: %v", err)
	}
	if m, _ := s.Get("inst", alice, "a1"); m == nil || m.Text != "Reunión pasado mañana" {
		t.Fatalf("edit not applied: %+v", m)
	}

	if err := s.MarkDeleted("inst", bob, "b1"); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}
	if m, _ := s.Get("inst", bob, "b1"); m == nil || !m.Deleted || m.Text != "" {
		t.Fatalf("delete not applied: %+v", m)
	}

	if n, err := s.Prune(at(2)); err != nil || n != 1 {
		t.Fatalf("Prune removed %d (err %v), want 1", n, err)
	}

	if err := s.PurgeInstance("inst"); err != nil {
		t.Fatalf("PurgeInstance: %v", err)
	}
	if chats, _ := s.ListChats("inst", "", 10); len(chats) != 0 {
		t.Fatalf("purge left chats behind: %+v", chats)
	}
}

func TestCaptureMessageLifecycle(t *testing.T) {
	s := newStore(t)

	lid := types.NewJID("99999", types.HiddenUserServer)
	phone := types.NewJID("5511988887777", types.DefaultUserServer)
	info := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: lid, Sender: lid, SenderAlt: phone},
		ID:            "m1",
		PushName:      "Carla",
		Timestamp:     at(1),
	}

	s.captureMessage(nil, "inst", &events.Message{Info: info, Message: &waE2E.Message{Conversation: proto.String("primer texto")}})

	// LID-addressed chats are keyed by phone number.
	m, err := s.Get("inst", phone.String(), "m1")
	if err != nil || m == nil {
		t.Fatalf("message not stored under phone JID (err %v)", err)
	}
	if m.Text != "primer texto" || m.SenderJID != phone.String() || m.SenderName != "Carla" || m.Type != "text" {
		t.Fatalf("unexpected stored message: %+v", m)
	}
	if name := s.ChatName("inst", phone.String()); name != "Carla" {
		t.Fatalf("chat name = %q, want Carla", name)
	}

	protocol := func(id string, pm *waE2E.ProtocolMessage) *events.Message {
		i := info
		i.ID = id
		pm.Key = &waCommon.MessageKey{ID: proto.String("m1")}
		return &events.Message{Info: i, Message: &waE2E.Message{ProtocolMessage: pm}}
	}

	s.captureMessage(nil, "inst", protocol("e1", &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		EditedMessage: &waE2E.Message{Conversation: proto.String("texto editado")},
	}))
	if m, _ = s.Get("inst", phone.String(), "m1"); m.Text != "texto editado" {
		t.Fatalf("edit not captured: %+v", m)
	}

	s.captureMessage(nil, "inst", protocol("r1", &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}))
	if m, _ = s.Get("inst", phone.String(), "m1"); !m.Deleted || m.Text != "" {
		t.Fatalf("revoke not captured: %+v", m)
	}

	// Protocol traffic itself is never stored as a message.
	if msgs, _ := s.Messages("inst", phone.String(), time.Time{}, 10); len(msgs) != 1 {
		t.Fatalf("expected only the original message, got %d", len(msgs))
	}
}

func TestConvertSkipsNonChatContent(t *testing.T) {
	text := &waE2E.Message{Conversation: proto.String("hi")}
	chat := types.NewJID("111", types.DefaultUserServer)

	cases := map[string]*events.Message{
		"status broadcast": {Info: types.MessageInfo{ID: "1", MessageSource: types.MessageSource{Chat: types.StatusBroadcastJID}}, Message: text},
		"newsletter":       {Info: types.MessageInfo{ID: "1", MessageSource: types.MessageSource{Chat: types.NewJID("1", types.NewsletterServer)}}, Message: text},
		"reaction":         {Info: types.MessageInfo{ID: "1", MessageSource: types.MessageSource{Chat: chat}}, Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}},
		"missing id":       {Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat}}, Message: text},
	}
	for name, evt := range cases {
		if _, ok := convert(nil, evt); ok {
			t.Errorf("%s should not be stored", name)
		}
	}

	m, ok := convert(nil, &events.Message{
		Info: types.MessageInfo{ID: "d1", MessageSource: types.MessageSource{Chat: chat, Sender: chat}, Timestamp: at(1)},
		Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			FileName:    proto.String("factura.pdf"),
			Caption:     proto.String("la de enero"),
			ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("q9")},
		}},
	})
	if !ok || m.Type != "document" || m.Text != "factura.pdf — la de enero" || m.QuotedID != "q9" {
		t.Fatalf("unexpected document conversion: ok=%v %+v", ok, m)
	}
}
