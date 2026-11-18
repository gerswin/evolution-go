package mcp_server

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mau.fi/whatsmeow/types"

	group_service "github.com/evolution-foundation/evolution-go/pkg/group/service"
	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	message_service "github.com/evolution-foundation/evolution-go/pkg/message/service"
	"github.com/evolution-foundation/evolution-go/pkg/msgstore"
	send_service "github.com/evolution-foundation/evolution-go/pkg/sendMessage/service"
	user_service "github.com/evolution-foundation/evolution-go/pkg/user/service"
	"github.com/evolution-foundation/evolution-go/pkg/utils"
)

// tools holds the per-request state: shared services plus the instance the
// caller authenticated as.
type tools struct {
	Deps
	instance *instance_model.Instance
}

func (t *tools) register(s *mcp.Server) {
	addTool(s, "get_status", "Connection status",
		"Report whether this WhatsApp account is connected and logged in, and which number it is.",
		readOnly, t.getStatus)

	if t.Store != nil {
		addTool(s, "list_chats", "List chats",
			"List conversations ordered by most recent activity, with a preview of the last message. Use it to find a chat's JID.",
			readOnly, t.listChats)
		addTool(s, "get_messages", "Read chat messages",
			"Read the messages of one chat, oldest first. Returns the most recent messages by default; pass 'before' to page further back.",
			readOnly, t.getMessages)
		addTool(s, "search_messages", "Search messages",
			"Find messages containing a text fragment (case-insensitive), across all chats or within one chat. Newest first.",
			readOnly, t.searchMessages)
	}

	addTool(s, "list_contacts", "List contacts",
		"List the contacts saved on this WhatsApp account, optionally filtered by name or number.",
		readOnly, t.listContacts)
	addTool(s, "check_numbers", "Check numbers on WhatsApp",
		"Check which phone numbers are registered on WhatsApp and get their JIDs.",
		readOnly, t.checkNumbers)
	addTool(s, "list_groups", "List groups",
		"List the groups this account belongs to.",
		readOnly, t.listGroups)
	addTool(s, "get_group_info", "Group details",
		"Get a group's name, description and participants.",
		readOnly, t.getGroupInfo)

	addTool(s, "send_text", "Send text message",
		"Send a text message to a person or group. Optionally reply to (quote) a previous message.",
		sends, t.sendText)
	addTool(s, "send_media", "Send media from URL",
		"Send an image, video, audio or document that is available at a public URL.",
		sends, t.sendMedia)
	addTool(s, "send_location", "Send location",
		"Send a location pin to a person or group.",
		sends, t.sendLocation)
	addTool(s, "send_poll", "Send poll",
		"Send a poll to a person or group.",
		sends, t.sendPoll)
	addTool(s, "react_to_message", "React to message",
		"Add an emoji reaction to a message, or remove it by passing an empty emoji.",
		sends, t.react)
	addTool(s, "mark_read", "Mark messages as read",
		"Mark messages in a chat as read (sends read receipts to the other side).",
		sends, t.markRead)
}

type toolKind int

const (
	readOnly toolKind = iota
	sends
)

// addTool registers a typed tool, hiding the request/result plumbing the
// handlers here never need.
func addTool[In, Out any](s *mcp.Server, name, title, description string, kind toolKind, fn func(context.Context, In) (Out, error)) {
	notDestructive := false
	openWorld := true
	annotations := &mcp.ToolAnnotations{Title: title, OpenWorldHint: &openWorld}
	if kind == readOnly {
		annotations.ReadOnlyHint = true
	} else {
		// These add messages/receipts; they never modify or delete data.
		annotations.DestructiveHint = &notDestructive
	}

	mcp.AddTool(s, &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: description,
		Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := fn(ctx, in)
		return nil, out, err
	})
}

// ---------------------------------------------------------------- status

type statusOutput struct {
	Instance  string `json:"instance" jsonschema:"instance name"`
	Connected bool   `json:"connected" jsonschema:"connected to WhatsApp servers"`
	LoggedIn  bool   `json:"logged_in" jsonschema:"paired with a WhatsApp account"`
	JID       string `json:"jid,omitempty" jsonschema:"JID of the paired account"`
	Name      string `json:"name,omitempty" jsonschema:"profile name of the paired account"`
}

func (t *tools) getStatus(_ context.Context, _ struct{}) (statusOutput, error) {
	out := statusOutput{Instance: t.instance.Name}

	status, err := t.InstanceService.Status(t.instance)
	if err != nil {
		return out, err
	}
	out.Connected = status.Connected
	out.LoggedIn = status.LoggedIn
	out.Name = status.Name

	if client := t.Clients[t.instance.Id]; client != nil && status.LoggedIn && client.Store != nil && client.Store.ID != nil {
		out.JID = client.Store.ID.ToNonAD().String()
	}
	return out, nil
}

// ---------------------------------------------------------------- history

type chatOutput struct {
	JID           string `json:"jid"`
	Name          string `json:"name,omitempty"`
	IsGroup       bool   `json:"is_group"`
	MessageCount  int    `json:"message_count" jsonschema:"number of stored messages"`
	LastMessageAt string `json:"last_message_at" jsonschema:"RFC 3339 timestamp"`
	LastMessage   string `json:"last_message" jsonschema:"preview of the most recent message"`
}

type listChatsInput struct {
	Query string `json:"query,omitempty" jsonschema:"filter by chat name or number"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum chats to return (default 30, max 200)"`
}

type listChatsOutput struct {
	Chats []chatOutput `json:"chats"`
}

func (t *tools) listChats(ctx context.Context, in listChatsInput) (listChatsOutput, error) {
	chats, err := t.Store.ListChats(t.instance.Id, strings.TrimSpace(in.Query), limitOr(in.Limit, 30, 200))
	if err != nil {
		return listChatsOutput{}, err
	}

	out := listChatsOutput{Chats: make([]chatOutput, 0, len(chats))}
	for _, c := range chats {
		preview := c.LastMessageText
		if preview == "" {
			preview = "[" + c.LastMessageType + "]"
		} else if c.LastMessageType != "text" {
			preview = "[" + c.LastMessageType + "] " + preview
		}
		if c.LastFromMe {
			preview = "You: " + preview
		}

		out.Chats = append(out.Chats, chatOutput{
			JID:           c.JID,
			Name:          t.chatName(ctx, c.JID, c.Name),
			IsGroup:       isGroupJID(c.JID),
			MessageCount:  c.MessageCount,
			LastMessageAt: c.LastMessageAt.Format(time.RFC3339),
			LastMessage:   truncate(preview, 200),
		})
	}
	return out, nil
}

type messageOutput struct {
	ID         string `json:"id"`
	Chat       string `json:"chat,omitempty" jsonschema:"chat JID"`
	ChatName   string `json:"chat_name,omitempty"`
	Timestamp  string `json:"timestamp" jsonschema:"RFC 3339 timestamp"`
	FromMe     bool   `json:"from_me" jsonschema:"true if sent by this account"`
	Sender     string `json:"sender,omitempty" jsonschema:"sender JID"`
	SenderName string `json:"sender_name,omitempty"`
	Type       string `json:"type" jsonschema:"text, image, video, audio, voice_note, document, sticker, location, contact, poll, ..."`
	Text       string `json:"text,omitempty" jsonschema:"message text, or the caption / file name for media"`
	QuotedID   string `json:"quoted_id,omitempty" jsonschema:"id of the message this one replies to"`
	Deleted    bool   `json:"deleted,omitempty" jsonschema:"the sender deleted this message for everyone"`
}

type getMessagesInput struct {
	Chat   string `json:"chat" jsonschema:"chat JID (from list_chats) or phone number"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum messages to return (default 50, max 500)"`
	Before string `json:"before,omitempty" jsonschema:"only messages older than this RFC 3339 timestamp; use the oldest timestamp of the previous page to go further back"`
}

type getMessagesOutput struct {
	Chat     string          `json:"chat"`
	ChatName string          `json:"chat_name,omitempty"`
	Messages []messageOutput `json:"messages"`
}

func (t *tools) getMessages(ctx context.Context, in getMessagesInput) (getMessagesOutput, error) {
	chat, err := chatJID(in.Chat)
	if err != nil {
		return getMessagesOutput{}, err
	}

	var before time.Time
	if in.Before != "" {
		if before, err = time.Parse(time.RFC3339, in.Before); err != nil {
			return getMessagesOutput{}, errors.New("'before' must be an RFC 3339 timestamp, e.g. 2026-01-31T15:04:05Z")
		}
	}

	msgs, err := t.Store.Messages(t.instance.Id, chat, before, limitOr(in.Limit, 50, 500))
	if err != nil {
		return getMessagesOutput{}, err
	}

	out := getMessagesOutput{
		Chat:     chat,
		ChatName: t.chatName(ctx, chat, t.Store.ChatName(t.instance.Id, chat)),
		Messages: make([]messageOutput, 0, len(msgs)),
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, toMessageOutput(m, false))
	}
	return out, nil
}

type searchMessagesInput struct {
	Query string `json:"query" jsonschema:"text to look for"`
	Chat  string `json:"chat,omitempty" jsonschema:"restrict the search to this chat JID or phone number"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum messages to return (default 30, max 200)"`
}

type searchMessagesOutput struct {
	Messages []messageOutput `json:"messages"`
}

func (t *tools) searchMessages(ctx context.Context, in searchMessagesInput) (searchMessagesOutput, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return searchMessagesOutput{}, errors.New("query is required")
	}

	chat := ""
	if strings.TrimSpace(in.Chat) != "" {
		var err error
		if chat, err = chatJID(in.Chat); err != nil {
			return searchMessagesOutput{}, err
		}
	}

	msgs, err := t.Store.Search(t.instance.Id, query, chat, limitOr(in.Limit, 30, 200))
	if err != nil {
		return searchMessagesOutput{}, err
	}

	names := map[string]string{}
	out := searchMessagesOutput{Messages: make([]messageOutput, 0, len(msgs))}
	for _, m := range msgs {
		name, ok := names[m.ChatJID]
		if !ok {
			name = t.chatName(ctx, m.ChatJID, t.Store.ChatName(t.instance.Id, m.ChatJID))
			names[m.ChatJID] = name
		}
		mo := toMessageOutput(m, true)
		mo.ChatName = name
		out.Messages = append(out.Messages, mo)
	}
	return out, nil
}

func toMessageOutput(m msgstore.Message, withChat bool) messageOutput {
	out := messageOutput{
		ID:        m.ID,
		Timestamp: m.Timestamp.Format(time.RFC3339),
		FromMe:    m.FromMe,
		Type:      m.Type,
		Text:      m.Text,
		QuotedID:  m.QuotedID,
		Deleted:   m.Deleted,
	}
	if withChat {
		out.Chat = m.ChatJID
	}
	if !m.FromMe {
		out.Sender = m.SenderJID
		out.SenderName = m.SenderName
	}
	return out
}

// chatName returns stored if set; otherwise it falls back to the WhatsApp
// address book for 1:1 chats.
func (t *tools) chatName(ctx context.Context, jid, stored string) string {
	if stored != "" || isGroupJID(jid) {
		return stored
	}

	client := t.Clients[t.instance.Id]
	if client == nil || client.Store == nil || client.Store.Contacts == nil {
		return ""
	}
	parsed, err := types.ParseJID(jid)
	if err != nil {
		return ""
	}
	contact, err := client.Store.Contacts.GetContact(ctx, parsed)
	if err != nil || !contact.Found {
		return ""
	}
	return firstNonEmpty(contact.FullName, contact.FirstName, contact.BusinessName, contact.PushName)
}

// ---------------------------------------------------------------- contacts

type contactOutput struct {
	JID          string `json:"jid"`
	Name         string `json:"name,omitempty" jsonschema:"name saved in the address book"`
	PushName     string `json:"push_name,omitempty" jsonschema:"name the contact chose for their own profile"`
	BusinessName string `json:"business_name,omitempty"`
}

type listContactsInput struct {
	Query string `json:"query,omitempty" jsonschema:"filter by name or number"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum contacts to return (default 100, max 1000)"`
}

type listContactsOutput struct {
	Contacts []contactOutput `json:"contacts"`
	Total    int             `json:"total" jsonschema:"number of contacts matching the filter, before the limit"`
}

func (t *tools) listContacts(_ context.Context, in listContactsInput) (listContactsOutput, error) {
	contacts, err := t.UserService.GetContacts(t.instance)
	if err != nil {
		return listContactsOutput{}, err
	}

	query := strings.ToLower(strings.TrimSpace(in.Query))
	matched := make([]contactOutput, 0, len(contacts))
	for _, c := range contacts {
		// LID entries duplicate the phone-number ones and are not addressable by users.
		if strings.HasSuffix(c.Jid, "@lid") {
			continue
		}
		name := firstNonEmpty(c.FullName, c.FirstName)
		if query != "" && !containsAny(query, name, c.PushName, c.BusinessName, c.Jid) {
			continue
		}
		matched = append(matched, contactOutput{JID: c.Jid, Name: name, PushName: c.PushName, BusinessName: c.BusinessName})
	}

	sort.Slice(matched, func(i, j int) bool {
		a := strings.ToLower(firstNonEmpty(matched[i].Name, matched[i].PushName, matched[i].JID))
		b := strings.ToLower(firstNonEmpty(matched[j].Name, matched[j].PushName, matched[j].JID))
		return a < b
	})

	out := listContactsOutput{Total: len(matched)}
	if limit := limitOr(in.Limit, 100, 1000); len(matched) > limit {
		matched = matched[:limit]
	}
	out.Contacts = matched
	return out, nil
}

type checkNumbersInput struct {
	Numbers []string `json:"numbers" jsonschema:"phone numbers in international format, digits only"`
}

type checkedNumber struct {
	Query        string `json:"query"`
	OnWhatsApp   bool   `json:"on_whatsapp"`
	JID          string `json:"jid,omitempty"`
	VerifiedName string `json:"verified_name,omitempty" jsonschema:"verified business name, if any"`
}

type checkNumbersOutput struct {
	Numbers []checkedNumber `json:"numbers"`
}

func (t *tools) checkNumbers(_ context.Context, in checkNumbersInput) (checkNumbersOutput, error) {
	if len(in.Numbers) == 0 {
		return checkNumbersOutput{}, errors.New("numbers is required")
	}

	result, err := t.UserService.CheckUser(&user_service.CheckUserStruct{Number: in.Numbers}, t.instance)
	if err != nil {
		return checkNumbersOutput{}, err
	}

	out := checkNumbersOutput{Numbers: make([]checkedNumber, 0, len(result.Users))}
	for _, u := range result.Users {
		out.Numbers = append(out.Numbers, checkedNumber{
			Query:        u.Query,
			OnWhatsApp:   u.IsInWhatsapp,
			JID:          firstNonEmpty(u.RemoteJID, u.JID),
			VerifiedName: u.VerifiedName,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------- groups

type groupSummary struct {
	JID          string `json:"jid"`
	Name         string `json:"name"`
	Participants int    `json:"participants"`
}

type listGroupsOutput struct {
	Groups []groupSummary `json:"groups"`
}

func (t *tools) listGroups(_ context.Context, _ struct{}) (listGroupsOutput, error) {
	groups, err := t.GroupService.ListGroups(t.instance)
	if err != nil {
		return listGroupsOutput{}, err
	}

	out := listGroupsOutput{Groups: make([]groupSummary, 0, len(groups))}
	for _, g := range groups {
		if g == nil {
			continue
		}
		out.Groups = append(out.Groups, groupSummary{JID: g.JID.String(), Name: g.Name, Participants: len(g.Participants)})
	}
	sort.Slice(out.Groups, func(i, j int) bool {
		return strings.ToLower(out.Groups[i].Name) < strings.ToLower(out.Groups[j].Name)
	})
	return out, nil
}

type getGroupInfoInput struct {
	Group string `json:"group" jsonschema:"group JID (ends in @g.us)"`
}

type groupParticipant struct {
	JID     string `json:"jid"`
	Phone   string `json:"phone,omitempty"`
	IsAdmin bool   `json:"is_admin"`
}

type getGroupInfoOutput struct {
	JID          string             `json:"jid"`
	Name         string             `json:"name"`
	Description  string             `json:"description,omitempty"`
	Owner        string             `json:"owner,omitempty"`
	CreatedAt    string             `json:"created_at,omitempty" jsonschema:"RFC 3339 timestamp"`
	AdminsOnly   bool               `json:"admins_only" jsonschema:"only admins can send messages"`
	Participants []groupParticipant `json:"participants"`
}

func (t *tools) getGroupInfo(_ context.Context, in getGroupInfoInput) (getGroupInfoOutput, error) {
	if !isGroupJID(in.Group) {
		return getGroupInfoOutput{}, errors.New("group must be a group JID ending in @g.us (see list_groups)")
	}

	g, err := t.GroupService.GetGroupInfo(&group_service.GetGroupInfoStruct{GroupJID: in.Group}, t.instance)
	if err != nil {
		return getGroupInfoOutput{}, err
	}

	out := getGroupInfoOutput{
		JID:          g.JID.String(),
		Name:         g.Name,
		Description:  g.Topic,
		AdminsOnly:   g.IsAnnounce,
		Participants: make([]groupParticipant, 0, len(g.Participants)),
	}
	if owner := firstJID(g.OwnerPN, g.OwnerJID); owner != "" {
		out.Owner = owner
	}
	if !g.GroupCreated.IsZero() {
		out.CreatedAt = g.GroupCreated.Format(time.RFC3339)
	}
	for _, p := range g.Participants {
		gp := groupParticipant{JID: p.JID.String(), IsAdmin: p.IsAdmin || p.IsSuperAdmin}
		if !p.PhoneNumber.IsEmpty() {
			gp.Phone = p.PhoneNumber.User
		}
		out.Participants = append(out.Participants, gp)
	}
	return out, nil
}

// ---------------------------------------------------------------- sending

type sentOutput struct {
	MessageID string `json:"message_id"`
	Chat      string `json:"chat" jsonschema:"JID the message was delivered to"`
	Timestamp string `json:"timestamp" jsonschema:"RFC 3339 timestamp"`
}

func sent(m *send_service.MessageSendStruct, err error) (sentOutput, error) {
	if err != nil {
		return sentOutput{}, err
	}
	if m == nil {
		return sentOutput{}, errors.New("message was not sent")
	}
	return sentOutput{
		MessageID: m.Info.ID,
		Chat:      m.Info.Chat.String(),
		Timestamp: m.Info.Timestamp.Format(time.RFC3339),
	}, nil
}

type sendTextInput struct {
	To      string `json:"to" jsonschema:"recipient phone number or JID"`
	Text    string `json:"text" jsonschema:"message body; WhatsApp formatting (*bold*, _italic_) is supported"`
	ReplyTo string `json:"reply_to,omitempty" jsonschema:"id of a message in that chat to quote"`
}

func (t *tools) sendText(_ context.Context, in sendTextInput) (sentOutput, error) {
	if strings.TrimSpace(in.Text) == "" {
		return sentOutput{}, errors.New("text is required")
	}
	number, err := recipient(in.To)
	if err != nil {
		return sentOutput{}, err
	}

	data := &send_service.TextStruct{Number: number, Text: in.Text}
	if in.ReplyTo != "" {
		data.Quoted = t.quoted(in.To, in.ReplyTo)
	}
	return sent(t.SendService.SendText(data, t.instance))
}

type sendMediaInput struct {
	To       string `json:"to" jsonschema:"recipient phone number or JID"`
	URL      string `json:"url" jsonschema:"public http(s) URL of the file"`
	Type     string `json:"type" jsonschema:"one of: image, video, audio, document"`
	Caption  string `json:"caption,omitempty" jsonschema:"text shown with the media"`
	Filename string `json:"filename,omitempty" jsonschema:"file name shown to the recipient (documents)"`
}

func (t *tools) sendMedia(_ context.Context, in sendMediaInput) (sentOutput, error) {
	mediaType := strings.ToLower(strings.TrimSpace(in.Type))
	switch mediaType {
	case "image", "video", "audio", "document":
	default:
		return sentOutput{}, errors.New("type must be one of: image, video, audio, document")
	}
	if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
		return sentOutput{}, errors.New("url must be an http(s) URL")
	}
	number, err := recipient(in.To)
	if err != nil {
		return sentOutput{}, err
	}

	return sent(t.SendService.SendMediaUrl(&send_service.MediaStruct{
		Number:   number,
		Url:      in.URL,
		Type:     mediaType,
		Caption:  in.Caption,
		Filename: in.Filename,
	}, t.instance))
}

type sendLocationInput struct {
	To        string  `json:"to" jsonschema:"recipient phone number or JID"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Name      string  `json:"name,omitempty" jsonschema:"place name"`
	Address   string  `json:"address,omitempty"`
}

func (t *tools) sendLocation(_ context.Context, in sendLocationInput) (sentOutput, error) {
	if in.Latitude < -90 || in.Latitude > 90 || in.Longitude < -180 || in.Longitude > 180 {
		return sentOutput{}, errors.New("latitude must be within ±90 and longitude within ±180")
	}
	number, err := recipient(in.To)
	if err != nil {
		return sentOutput{}, err
	}

	return sent(t.SendService.SendLocation(&send_service.LocationStruct{
		Number:    number,
		Latitude:  in.Latitude,
		Longitude: in.Longitude,
		Name:      in.Name,
		Address:   in.Address,
	}, t.instance))
}

type sendPollInput struct {
	To         string   `json:"to" jsonschema:"recipient phone number or JID"`
	Question   string   `json:"question"`
	Options    []string `json:"options" jsonschema:"2 to 12 answer options"`
	MaxAnswers int      `json:"max_answers,omitempty" jsonschema:"how many options each person may pick (default 1)"`
}

func (t *tools) sendPoll(_ context.Context, in sendPollInput) (sentOutput, error) {
	if strings.TrimSpace(in.Question) == "" {
		return sentOutput{}, errors.New("question is required")
	}
	if len(in.Options) < 2 || len(in.Options) > 12 {
		return sentOutput{}, errors.New("a poll needs between 2 and 12 options")
	}
	maxAnswers := in.MaxAnswers
	if maxAnswers <= 0 {
		maxAnswers = 1
	}
	if maxAnswers > len(in.Options) {
		maxAnswers = len(in.Options)
	}
	number, err := recipient(in.To)
	if err != nil {
		return sentOutput{}, err
	}

	return sent(t.SendService.SendPoll(&send_service.PollStruct{
		Number:    number,
		Question:  in.Question,
		Options:   in.Options,
		MaxAnswer: maxAnswers,
	}, t.instance))
}

type reactInput struct {
	Chat      string `json:"chat" jsonschema:"chat JID or phone number the message belongs to"`
	MessageID string `json:"message_id" jsonschema:"id of the message to react to"`
	Emoji     string `json:"emoji" jsonschema:"a single emoji, or an empty string to remove the reaction"`
	FromMe    *bool  `json:"from_me,omitempty" jsonschema:"whether the target message was sent by this account; only needed when it is not in the stored history"`
}

type okOutput struct {
	OK bool `json:"ok"`
}

func (t *tools) react(_ context.Context, in reactInput) (okOutput, error) {
	if in.MessageID == "" {
		return okOutput{}, errors.New("message_id is required")
	}
	number, err := recipient(in.Chat)
	if err != nil {
		return okOutput{}, err
	}

	data := &message_service.ReactStruct{Number: number, Reaction: in.Emoji, Id: in.MessageID}
	if stored := t.storedMessage(in.Chat, in.MessageID); stored != nil {
		data.FromMe = stored.FromMe
		if !stored.FromMe && isGroupJID(stored.ChatJID) {
			data.Participant = stored.SenderJID
		}
	}
	if in.FromMe != nil {
		data.FromMe = *in.FromMe
	}

	if _, err := t.MessageService.React(data, t.instance); err != nil {
		return okOutput{}, err
	}
	return okOutput{OK: true}, nil
}

type markReadInput struct {
	Chat       string   `json:"chat" jsonschema:"chat JID or phone number"`
	MessageIDs []string `json:"message_ids" jsonschema:"ids of the messages to mark as read"`
}

func (t *tools) markRead(_ context.Context, in markReadInput) (okOutput, error) {
	if len(in.MessageIDs) == 0 {
		return okOutput{}, errors.New("message_ids is required")
	}
	number, err := recipient(in.Chat)
	if err != nil {
		return okOutput{}, err
	}

	if _, err := t.MessageService.MarkRead(&message_service.MarkReadStruct{Id: in.MessageIDs, Number: number}, t.instance); err != nil {
		return okOutput{}, err
	}
	return okOutput{OK: true}, nil
}

// quoted builds the quote reference for a reply. Group replies need the
// original author, which we can only know from stored history.
func (t *tools) quoted(chat, messageID string) send_service.QuotedStruct {
	q := send_service.QuotedStruct{MessageID: messageID}
	if stored := t.storedMessage(chat, messageID); stored != nil && !stored.FromMe {
		q.Participant = stored.SenderJID
	}
	return q
}

func (t *tools) storedMessage(chat, messageID string) *msgstore.Message {
	if t.Store == nil {
		return nil
	}
	jid, err := chatJID(chat)
	if err != nil {
		return nil
	}
	m, _ := t.Store.Get(t.instance.Id, jid, messageID)
	return m
}

// ---------------------------------------------------------------- helpers

// recipient normalizes a user-supplied number/JID the same way the REST
// middleware does before it reaches the services.
func recipient(to string) (string, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		return "", errors.New("recipient is required")
	}
	jid, err := utils.CreateJID(to)
	if err != nil {
		return "", errors.New("invalid recipient: use a phone number with country code or a WhatsApp JID")
	}
	return jid, nil
}

// chatJID returns the JID under which a chat is stored (no "+", no device).
func chatJID(chat string) (string, error) {
	normalized, err := recipient(chat)
	if err != nil {
		return "", err
	}
	parsed, err := types.ParseJID(strings.TrimPrefix(normalized, "+"))
	if err != nil || parsed.User == "" {
		return "", errors.New("invalid chat: use a JID from list_chats or a phone number with country code")
	}
	return parsed.ToNonAD().String(), nil
}

func isGroupJID(jid string) bool {
	return strings.HasSuffix(jid, "@"+types.GroupServer)
}

func firstJID(jids ...types.JID) string {
	for _, j := range jids {
		if !j.IsEmpty() {
			return j.ToNonAD().String()
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func containsAny(lowerQuery string, values ...string) bool {
	for _, v := range values {
		if strings.Contains(strings.ToLower(v), lowerQuery) {
			return true
		}
	}
	return false
}

func limitOr(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
