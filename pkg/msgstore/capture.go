package msgstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gomessguii/logger"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Attach registers an event handler on client that persists messages for
// instanceID. It is a no-op when message persistence is disabled.
func Attach(client *whatsmeow.Client, instanceID string) {
	s := Default()
	if s == nil || client == nil {
		return
	}

	client.AddEventHandler(func(rawEvt any) {
		defer func() {
			if r := recover(); r != nil {
				logger.LogError("[%s] message store: panic while handling event: %v", instanceID, r)
			}
		}()

		switch evt := rawEvt.(type) {
		case *events.Message:
			s.captureMessage(client, instanceID, evt)
		case *events.HistorySync:
			s.captureHistory(client, instanceID, evt)
		}
	})
}

// RecordSent persists a message sent through the API. WhatsApp does not echo
// our own sends back as events, so the send path has to report them.
func RecordSent(instanceID string, info types.MessageInfo, msg *waE2E.Message) {
	s := Default()
	if s == nil {
		return
	}

	m, ok := convert(nil, &events.Message{Info: info, Message: msg})
	if !ok {
		return
	}
	if err := s.Save(instanceID, m); err != nil {
		logger.LogError("[%s] message store: failed to save sent message %s: %v", instanceID, info.ID, err)
	}
}

// PurgeInstance drops everything stored for an instance (used when the
// instance is deleted). No-op when persistence is disabled.
func PurgeInstance(instanceID string) {
	s := Default()
	if s == nil {
		return
	}
	if err := s.PurgeInstance(instanceID); err != nil {
		logger.LogError("[%s] message store: failed to purge instance: %v", instanceID, err)
	}
}

func (s *Store) captureMessage(client *whatsmeow.Client, instanceID string, evt *events.Message) {
	if evt == nil || evt.Message == nil {
		return
	}

	// Edits and "delete for everyone" arrive as protocol messages that point at
	// the original message instead of carrying a new one.
	if proto := evt.Message.GetProtocolMessage(); proto != nil {
		chat := resolveChat(client, &evt.Info)
		targetID := proto.GetKey().GetID()
		if chat == "" || targetID == "" {
			return
		}
		switch proto.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			if err := s.MarkDeleted(instanceID, chat, targetID); err != nil {
				logger.LogError("[%s] message store: failed to mark %s deleted: %v", instanceID, targetID, err)
			}
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			if _, text := describe(proto.GetEditedMessage()); text != "" {
				if err := s.EditText(instanceID, chat, targetID, text); err != nil {
					logger.LogError("[%s] message store: failed to apply edit to %s: %v", instanceID, targetID, err)
				}
			}
		}
		return
	}

	m, ok := convert(client, evt)
	if !ok {
		return
	}
	if err := s.Save(instanceID, m); err != nil {
		logger.LogError("[%s] message store: failed to save message %s: %v", instanceID, m.ID, err)
		return
	}

	if evt.Info.IsGroup {
		if s.ChatName(instanceID, m.ChatJID) == "" {
			// Group names are not in the message; fetch once, off the event loop.
			go s.fetchGroupName(client, instanceID, evt.Info.Chat)
		}
	} else if !evt.Info.IsFromMe && evt.Info.PushName != "" {
		_ = s.SetChatName(instanceID, m.ChatJID, evt.Info.PushName)
	}
}

func (s *Store) captureHistory(client *whatsmeow.Client, instanceID string, evt *events.HistorySync) {
	if evt == nil || evt.Data == nil {
		return
	}

	total := 0
	for _, conv := range evt.Data.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}

		var batch []Message
		for _, hm := range conv.GetMessages() {
			parsed, err := client.ParseWebMessage(chatJID, hm.GetMessage())
			if err != nil || parsed == nil || parsed.Message == nil {
				continue
			}
			if m, ok := convert(client, parsed); ok {
				batch = append(batch, m)
			}
		}
		if len(batch) == 0 {
			continue
		}

		if err := s.Save(instanceID, batch...); err != nil {
			logger.LogError("[%s] message store: failed to save history for %s: %v", instanceID, chatJID, err)
			continue
		}
		total += len(batch)

		name := conv.GetName()
		if name == "" {
			name = conv.GetDisplayName()
		}
		if name != "" && s.ChatName(instanceID, batch[0].ChatJID) == "" {
			_ = s.SetChatName(instanceID, batch[0].ChatJID, name)
		}
	}

	if total > 0 {
		logger.LogInfo("[%s] message store: saved %d messages from history sync", instanceID, total)
	}
}

func (s *Store) fetchGroupName(client *whatsmeow.Client, instanceID string, group types.JID) {
	defer func() { _ = recover() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	info, err := client.GetGroupInfo(ctx, group)
	if err != nil || info == nil {
		return
	}
	_ = s.SetChatName(instanceID, group.ToNonAD().String(), info.Name)
}

// convert turns a WhatsApp message event into a storable Message. ok is false
// for things we do not keep: status broadcasts, channels, reactions, poll
// votes and other content-less protocol traffic.
func convert(client *whatsmeow.Client, evt *events.Message) (Message, bool) {
	info := &evt.Info
	if info.ID == "" {
		return Message{}, false
	}
	if info.Chat.Server == types.BroadcastServer || info.Chat.Server == types.NewsletterServer {
		return Message{}, false
	}

	msgType, text := describe(evt.Message)
	if msgType == "" {
		return Message{}, false
	}

	chat := resolveChat(client, info)
	if chat == "" {
		return Message{}, false
	}

	ts := info.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	return Message{
		ID:         info.ID,
		ChatJID:    chat,
		SenderJID:  resolveSender(client, info),
		SenderName: info.PushName,
		FromMe:     info.IsFromMe,
		Type:       msgType,
		Text:       text,
		QuotedID:   quotedID(evt.Message),
		Timestamp:  ts,
	}, true
}

// resolveChat returns the canonical chat JID. WhatsApp increasingly addresses
// 1:1 chats by LID (xxx@lid); we key them by phone number whenever it can be
// resolved so one conversation does not split in two.
func resolveChat(client *whatsmeow.Client, info *types.MessageInfo) string {
	chat := info.Chat.ToNonAD()
	if chat.Server != types.HiddenUserServer {
		return chat.String()
	}

	alt := info.SenderAlt
	if info.IsFromMe {
		alt = info.RecipientAlt
	}
	if alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.ToNonAD().String()
	}
	if pn, ok := pnForLID(client, chat); ok {
		return pn.String()
	}
	return chat.String()
}

func resolveSender(client *whatsmeow.Client, info *types.MessageInfo) string {
	sender := info.Sender.ToNonAD()
	if sender.Server != types.HiddenUserServer {
		return sender.String()
	}
	if alt := info.SenderAlt; alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.ToNonAD().String()
	}
	if pn, ok := pnForLID(client, sender); ok {
		return pn.String()
	}
	return sender.String()
}

func pnForLID(client *whatsmeow.Client, lid types.JID) (types.JID, bool) {
	if client == nil || client.Store == nil || client.Store.LIDs == nil {
		return types.JID{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pn, err := client.Store.LIDs.GetPNForLID(ctx, lid)
	if err != nil || pn.User == "" {
		return types.JID{}, false
	}
	return pn.ToNonAD(), true
}

// describe maps a WhatsApp message to a short type label plus the text worth
// keeping. Media bodies are never stored, only captions and file names. An
// empty type means "do not store".
func describe(msg *waE2E.Message) (msgType, text string) {
	switch {
	case msg == nil:
		return "", ""
	case msg.GetConversation() != "":
		return "text", msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		return "text", msg.GetExtendedTextMessage().GetText()
	case msg.GetImageMessage() != nil:
		return "image", msg.GetImageMessage().GetCaption()
	case msg.GetVideoMessage() != nil:
		return "video", msg.GetVideoMessage().GetCaption()
	case msg.GetPtvMessage() != nil:
		return "video_note", ""
	case msg.GetAudioMessage() != nil:
		a := msg.GetAudioMessage()
		label := "audio"
		if a.GetPTT() {
			label = "voice_note"
		}
		return label, fmt.Sprintf("(%ds)", a.GetSeconds())
	case msg.GetDocumentMessage() != nil:
		d := msg.GetDocumentMessage()
		return "document", joinNonEmpty(" — ", d.GetFileName(), d.GetCaption())
	case msg.GetDocumentWithCaptionMessage().GetMessage().GetDocumentMessage() != nil:
		d := msg.GetDocumentWithCaptionMessage().GetMessage().GetDocumentMessage()
		return "document", joinNonEmpty(" — ", d.GetFileName(), d.GetCaption())
	case msg.GetStickerMessage() != nil:
		return "sticker", ""
	case msg.GetLocationMessage() != nil:
		l := msg.GetLocationMessage()
		coords := fmt.Sprintf("%.6f,%.6f", l.GetDegreesLatitude(), l.GetDegreesLongitude())
		return "location", joinNonEmpty(" — ", l.GetName(), l.GetAddress(), coords)
	case msg.GetLiveLocationMessage() != nil:
		l := msg.GetLiveLocationMessage()
		coords := fmt.Sprintf("%.6f,%.6f", l.GetDegreesLatitude(), l.GetDegreesLongitude())
		return "live_location", joinNonEmpty(" — ", l.GetCaption(), coords)
	case msg.GetContactMessage() != nil:
		return "contact", msg.GetContactMessage().GetDisplayName()
	case msg.GetContactsArrayMessage() != nil:
		var names []string
		for _, c := range msg.GetContactsArrayMessage().GetContacts() {
			names = append(names, c.GetDisplayName())
		}
		return "contact", strings.Join(names, ", ")
	case msg.GetButtonsResponseMessage() != nil:
		return "button_reply", msg.GetButtonsResponseMessage().GetSelectedDisplayText()
	case msg.GetTemplateButtonReplyMessage() != nil:
		return "button_reply", msg.GetTemplateButtonReplyMessage().GetSelectedDisplayText()
	case msg.GetListResponseMessage() != nil:
		return "list_reply", msg.GetListResponseMessage().GetTitle()
	}

	if poll := pollCreation(msg); poll != nil {
		var options []string
		for _, o := range poll.GetOptions() {
			options = append(options, o.GetOptionName())
		}
		return "poll", joinNonEmpty(" — ", poll.GetName(), strings.Join(options, " | "))
	}

	return "", ""
}

func pollCreation(msg *waE2E.Message) *waE2E.PollCreationMessage {
	for _, p := range []*waE2E.PollCreationMessage{
		msg.GetPollCreationMessage(),
		msg.GetPollCreationMessageV2(),
		msg.GetPollCreationMessageV3(),
		msg.GetPollCreationMessageV5(),
		msg.GetPollCreationMessageV6(),
	} {
		if p != nil {
			return p
		}
	}
	return nil
}

func quotedID(msg *waE2E.Message) string {
	switch {
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID()
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetContextInfo().GetStanzaID()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetContextInfo().GetStanzaID()
	case msg.GetAudioMessage() != nil:
		return msg.GetAudioMessage().GetContextInfo().GetStanzaID()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetContextInfo().GetStanzaID()
	case msg.GetStickerMessage() != nil:
		return msg.GetStickerMessage().GetContextInfo().GetStanzaID()
	}
	return ""
}

func joinNonEmpty(sep string, parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}
