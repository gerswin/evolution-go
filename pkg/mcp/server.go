// Package mcp_server exposes an instance's WhatsApp account as a Model Context
// Protocol server (Streamable HTTP) so assistants such as Claude or ChatGPT
// can read chats and send messages.
//
// It is opt-in (MCP_ENABLED=true) and mounted at /mcp. Every request must
// carry either an instance token ("apikey" header or "Authorization: Bearer")
// or an OAuth access token issued by the built-in authorization server (see
// oauth.go), and the session only ever sees that one instance.
package mcp_server

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gomessguii/logger"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mau.fi/whatsmeow"

	group_service "github.com/evolution-foundation/evolution-go/pkg/group/service"
	instance_model "github.com/evolution-foundation/evolution-go/pkg/instance/model"
	instance_service "github.com/evolution-foundation/evolution-go/pkg/instance/service"
	message_service "github.com/evolution-foundation/evolution-go/pkg/message/service"
	"github.com/evolution-foundation/evolution-go/pkg/msgstore"
	send_service "github.com/evolution-foundation/evolution-go/pkg/sendMessage/service"
	user_service "github.com/evolution-foundation/evolution-go/pkg/user/service"
)

const (
	envEnabled = "MCP_ENABLED"
	Path       = "/mcp"
)

type Deps struct {
	InstanceService instance_service.InstanceService
	SendService     send_service.SendService
	UserService     user_service.UserService
	GroupService    group_service.GroupService
	MessageService  message_service.MessageService
	// Clients is the shared instanceId -> WhatsApp client map.
	Clients map[string]*whatsmeow.Client
	// Store is nil when message persistence is disabled; the history tools
	// are not registered in that case.
	Store   *msgstore.Store
	Version string
}

type instanceKey struct{}

// endpoint is the mounted MCP endpoint: shared dependencies plus the OAuth
// server, which is nil if its database could not be opened (header tokens
// keep working in that case).
type endpoint struct {
	Deps
	oauth *oauthServer
}

// Register mounts the MCP endpoint on r when MCP_ENABLED=true.
func Register(r *gin.Engine, d Deps) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(envEnabled)), "true") {
		return
	}

	// A server is built per request (each one is bound to the authenticated
	// instance); the cache keeps tool schema reflection a one-time cost.
	schemas := mcp.NewSchemaCache()

	handler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		instance, _ := req.Context().Value(instanceKey{}).(*instance_model.Instance)
		if instance == nil {
			return nil
		}
		return d.newServer(instance, schemas)
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// The SDK rejects requests that reach us over loopback with a public
		// Host header, which is exactly what a same-host reverse proxy does.
		// That check guards unauthenticated local servers against DNS
		// rebinding; this endpoint requires an instance token on every call.
		DisableLocalhostProtection: true,
	})

	e := &endpoint{Deps: d}
	if oauth, err := openOAuth(d.InstanceService); err != nil {
		logger.LogError("MCP OAuth disabled, failed to open its database: %v", err)
	} else {
		e.oauth = oauth
		oauth.routes(r)
	}

	r.Any(Path, e.authenticate, gin.WrapH(handler))

	if d.Store == nil {
		logger.LogInfo("MCP server enabled at %s (message history tools disabled: set MESSAGE_STORE_ENABLED=true)", Path)
	} else {
		logger.LogInfo("MCP server enabled at %s", Path)
	}
}

func (e *endpoint) authenticate(c *gin.Context) {
	instance := e.resolveInstance(c)
	if instance == nil {
		challenge := `Bearer realm="evolution-go"`
		if e.oauth != nil {
			challenge = e.oauth.challengeHeader(c)
		}
		c.Header("WWW-Authenticate", challenge)
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "not authorized"})
		return
	}

	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), instanceKey{}, instance))
	c.Next()
}

// resolveInstance maps the request credentials to an instance, or nil.
func (e *endpoint) resolveInstance(c *gin.Context) *instance_model.Instance {
	token := strings.TrimSpace(c.GetHeader("apikey"))
	if token == "" {
		if scheme, value, ok := strings.Cut(c.GetHeader("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
			token = strings.TrimSpace(value)
		}
	}
	if token == "" {
		return nil
	}

	if e.oauth != nil && strings.HasPrefix(token, accessTokenPrefix) {
		instanceID, err := e.oauth.instanceForAccessToken(token)
		if err != nil {
			return nil
		}
		instance, err := e.InstanceService.Info(instanceID)
		if err != nil {
			return nil
		}
		return instance
	}

	instance, err := e.InstanceService.GetInstanceByToken(token)
	if err != nil {
		return nil
	}
	return instance
}

func (d Deps) newServer(instance *instance_model.Instance, schemas *mcp.SchemaCache) *mcp.Server {
	instructions := "WhatsApp account \"" + instance.Name + "\" via Evolution GO. " +
		"Recipients (\"to\", \"chat\") accept a phone number in international format (digits only, with country code) " +
		"or a WhatsApp JID (…@s.whatsapp.net for people, …@g.us for groups). " +
		"Sending tools deliver real messages to real people: confirm the recipient and content with the user before sending."
	if d.Store != nil {
		instructions += " Chat history only covers messages received or sent since history storage was enabled " +
			"(plus whatever WhatsApp synced when the device was linked); prefer the JIDs returned by list_chats."
	} else {
		instructions += " Message history is not available on this server."
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "evolution-go",
		Title:   "Evolution GO (WhatsApp)",
		Version: d.Version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
		SchemaCache:  schemas,
	})

	t := &tools{Deps: d, instance: instance}
	t.register(server)

	return server
}
