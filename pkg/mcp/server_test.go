package mcp_server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"

	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	instance_service "github.com/evolution-foundation/evolution-go/pkg/instance/service"
	"github.com/evolution-foundation/evolution-go/pkg/msgstore"
	send_service "github.com/evolution-foundation/evolution-go/pkg/sendMessage/service"
)

const (
	tokenA = "token-a"
	tokenB = "token-b"
	alice  = "5511911112222@s.whatsapp.net"
)

// Stubs embed the real interfaces so only the methods under test need bodies;
// anything else panics loudly if a tool unexpectedly reaches for it.
type stubInstances struct {
	instance_service.InstanceService
}

func (stubInstances) GetInstanceByToken(token string) (*instance_model.Instance, error) {
	switch token {
	case tokenA:
		return &instance_model.Instance{Id: "inst-a", Name: "Ventas", Token: tokenA}, nil
	case tokenB:
		return &instance_model.Instance{Id: "inst-b", Name: "Soporte", Token: tokenB}, nil
	}
	return nil, errors.New("not found")
}

func (stubInstances) Info(id string) (*instance_model.Instance, error) {
	switch id {
	case "inst-a":
		return &instance_model.Instance{Id: "inst-a", Name: "Ventas", Token: tokenA}, nil
	case "inst-b":
		return &instance_model.Instance{Id: "inst-b", Name: "Soporte", Token: tokenB}, nil
	}
	return nil, errors.New("not found")
}

func (stubInstances) Status(*instance_model.Instance) (*instance_service.StatusStruct, error) {
	return &instance_service.StatusStruct{Connected: true, LoggedIn: false, Name: "Ventas WA"}, nil
}

type stubSender struct {
	send_service.SendService
	texts []*send_service.TextStruct
}

func (s *stubSender) SendText(data *send_service.TextStruct, instance *instance_model.Instance) (*send_service.MessageSendStruct, error) {
	s.texts = append(s.texts, data)
	return &send_service.MessageSendStruct{Info: types.MessageInfo{
		ID:            "SENT1",
		MessageSource: types.MessageSource{Chat: types.NewJID("5511911112222", types.DefaultUserServer)},
		Timestamp:     time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}}, nil
}

type headerTransport struct {
	header, value string
}

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(h.header, h.value)
	return http.DefaultTransport.RoundTrip(req)
}

func startServer(t *testing.T, store *msgstore.Store) (*httptest.Server, *stubSender) {
	t.Helper()
	t.Setenv(envEnabled, "true")
	t.Setenv(envOAuthDBPath, filepath.Join(t.TempDir(), "oauth.db"))
	t.Setenv(envPublicURL, "")
	gin.SetMode(gin.TestMode)

	sender := &stubSender{}
	r := gin.New()
	Register(r, Deps{
		InstanceService: stubInstances{},
		SendService:     sender,
		Clients:         map[string]*whatsmeow.Client{},
		Store:           store,
		Version:         "test",
	})

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, sender
}

func connect(t *testing.T, srv *httptest.Server, header, value string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             srv.URL + Path,
		HTTPClient:           &http.Client{Transport: headerTransport{header, value}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// call invokes a tool and decodes its structured result into out.
func call(t *testing.T, session *mcp.ClientSession, name string, args any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	if out != nil {
		if res.IsError {
			t.Fatalf("%s: tool error: %+v", name, res.Content)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: decode %s: %v", name, raw, err)
		}
	}
	return res
}

func seededStore(t *testing.T) *msgstore.Store {
	t.Helper()
	store, err := msgstore.Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := store.Save("inst-a",
		msgstore.Message{ID: "m1", ChatJID: alice, SenderJID: alice, SenderName: "Alice", Type: "text", Text: "¿Tienen stock del modelo X?", Timestamp: base},
		msgstore.Message{ID: "m2", ChatJID: alice, FromMe: true, Type: "text", Text: "Sí, quedan 3", Timestamp: base.Add(time.Minute)},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.Save("inst-b", msgstore.Message{ID: "s1", ChatJID: alice, SenderJID: alice, Type: "text", Text: "stock privado de otra instancia", Timestamp: base}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.SetChatName("inst-a", alice, "Alice"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return store
}

func TestRejectsMissingOrInvalidToken(t *testing.T) {
	srv, _ := startServer(t, nil)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	for name, header := range map[string][2]string{
		"no token":      {"X-Nothing", "x"},
		"unknown token": {"apikey", "nope"},
		"basic scheme":  {"Authorization", "Basic " + tokenA},
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+Path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set(header[0], header[1])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: missing WWW-Authenticate header", name)
		}
	}
}

func TestDisabledByDefault(t *testing.T) {
	t.Setenv(envEnabled, "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	Register(r, Deps{InstanceService: stubInstances{}})

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Post(srv.URL+Path, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404 when MCP_ENABLED is unset", resp.StatusCode)
	}
}

func TestHistoryToolsAreScopedToTheInstance(t *testing.T) {
	srv, _ := startServer(t, seededStore(t))
	session := connect(t, srv, "apikey", tokenA)

	var status statusOutput
	call(t, session, "get_status", map[string]any{}, &status)
	if status.Instance != "Ventas" || !status.Connected || status.LoggedIn || status.Name != "Ventas WA" {
		t.Fatalf("unexpected status: %+v", status)
	}

	var chats listChatsOutput
	call(t, session, "list_chats", map[string]any{}, &chats)
	if len(chats.Chats) != 1 {
		t.Fatalf("want 1 chat, got %+v", chats.Chats)
	}
	if c := chats.Chats[0]; c.JID != alice || c.Name != "Alice" || c.IsGroup || c.MessageCount != 2 || c.LastMessage != "You: Sí, quedan 3" {
		t.Fatalf("unexpected chat: %+v", c)
	}

	// A bare phone number must resolve to the same chat as its JID.
	var msgs getMessagesOutput
	call(t, session, "get_messages", map[string]any{"chat": "+55 11 91111-2222"}, &msgs)
	if len(msgs.Messages) != 2 || msgs.Messages[0].ID != "m1" || msgs.Messages[1].ID != "m2" {
		t.Fatalf("unexpected messages: %+v", msgs.Messages)
	}
	if m := msgs.Messages[0]; m.Sender != alice || m.SenderName != "Alice" || m.FromMe {
		t.Fatalf("unexpected incoming message: %+v", m)
	}
	if m := msgs.Messages[1]; !m.FromMe || m.Sender != "" {
		t.Fatalf("unexpected outgoing message: %+v", m)
	}

	var found searchMessagesOutput
	call(t, session, "search_messages", map[string]any{"query": "STOCK"}, &found)
	if len(found.Messages) != 1 || found.Messages[0].ID != "m1" || found.Messages[0].Chat != alice || found.Messages[0].ChatName != "Alice" {
		t.Fatalf("search leaked or missed messages: %+v", found.Messages)
	}

	// The other token sees only its own instance's data.
	other := connect(t, srv, "Authorization", "Bearer "+tokenB)
	call(t, other, "search_messages", map[string]any{"query": "stock"}, &found)
	if len(found.Messages) != 1 || found.Messages[0].ID != "s1" {
		t.Fatalf("instance B got: %+v", found.Messages)
	}
}

func TestSendTextAndInputValidation(t *testing.T) {
	srv, sender := startServer(t, seededStore(t))
	session := connect(t, srv, "apikey", tokenA)

	var out sentOutput
	call(t, session, "send_text", map[string]any{"to": "5511911112222", "text": "Hola", "reply_to": "m1"}, &out)
	if out.MessageID != "SENT1" || out.Chat != alice || out.Timestamp != "2026-01-01T12:00:00Z" {
		t.Fatalf("unexpected send result: %+v", out)
	}
	if len(sender.texts) != 1 {
		t.Fatalf("SendText called %d times", len(sender.texts))
	}
	if got := sender.texts[0]; got.Text != "Hola" || !strings.HasSuffix(got.Number, "@s.whatsapp.net") || got.Quoted.MessageID != "m1" {
		t.Fatalf("unexpected SendText payload: %+v", got)
	}

	// Handler-level validation surfaces as a tool error, and nothing is sent.
	res := call(t, session, "send_text", map[string]any{"to": "5511911112222", "text": "   "}, nil)
	if !res.IsError {
		t.Fatalf("blank text should be a tool error")
	}
	// Schema-level validation: missing required argument.
	if res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_text", Arguments: map[string]any{"text": "x"}}); err == nil && !res.IsError {
		t.Fatalf("missing 'to' should be rejected")
	}
	if len(sender.texts) != 1 {
		t.Fatalf("invalid calls reached SendText (%d calls)", len(sender.texts))
	}
}

func TestToolListDependsOnStore(t *testing.T) {
	names := func(store *msgstore.Store) map[string]*mcp.Tool {
		srv, _ := startServer(t, store)
		session := connect(t, srv, "apikey", tokenA)
		res, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		out := map[string]*mcp.Tool{}
		for _, tool := range res.Tools {
			out[tool.Name] = tool
		}
		return out
	}

	without := names(nil)
	with := names(seededStore(t))

	for _, history := range []string{"list_chats", "get_messages", "search_messages"} {
		if _, ok := without[history]; ok {
			t.Errorf("%s must not be offered without a message store", history)
		}
		if _, ok := with[history]; !ok {
			t.Errorf("%s missing when the message store is enabled", history)
		}
	}
	if len(with) != 14 {
		t.Errorf("expected 14 tools with store, got %d", len(with))
	}

	if a := with["get_messages"].Annotations; a == nil || !a.ReadOnlyHint {
		t.Errorf("get_messages should be annotated read-only: %+v", a)
	}
	if a := with["send_text"].Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
		t.Errorf("send_text should be a non-destructive write: %+v", a)
	}
}
