package mcp_server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	_ "modernc.org/sqlite"

	instance_service "github.com/evolution-foundation/evolution-go/pkg/instance/service"
)

// OAuth 2.1 authorization server for the MCP endpoint.
//
// Hosted assistants (Claude.ai, ChatGPT) cannot send a custom header; they
// discover an authorization server from the 401 on /mcp, register themselves
// (RFC 7591), and run an authorization-code + PKCE flow. The "login" step here
// is the user pasting an instance token, so an OAuth grant is exactly as
// powerful as that token and is bound to that one instance.
//
// Clients and token hashes live in a small SQLite file so grants survive
// restarts; authorization codes are short-lived and kept in memory.

const (
	envPublicURL   = "MCP_PUBLIC_URL"
	envOAuthDBPath = "MCP_OAUTH_DB_PATH"

	accessTokenPrefix  = "evo_at_"
	refreshTokenPrefix = "evo_rt_"

	accessTokenTTL  = time.Hour
	refreshTokenTTL = 90 * 24 * time.Hour
	authCodeTTL     = 5 * time.Minute

	pathResourceMetadata = "/.well-known/oauth-protected-resource"
	pathServerMetadata   = "/.well-known/oauth-authorization-server"
	pathRegister         = "/oauth/register"
	pathAuthorize        = "/oauth/authorize"
	pathToken            = "/oauth/token"
)

const oauthSchema = `
CREATE TABLE IF NOT EXISTS oauth_clients (
	client_id     TEXT PRIMARY KEY,
	secret_hash   TEXT NOT NULL DEFAULT '',
	name          TEXT NOT NULL DEFAULT '',
	redirect_uris TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS oauth_tokens (
	token_hash  TEXT PRIMARY KEY,
	kind        TEXT NOT NULL,
	client_id   TEXT NOT NULL,
	instance_id TEXT NOT NULL,
	expires_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_expires ON oauth_tokens (expires_at);
`

type oauthServer struct {
	db        *sql.DB
	instances instance_service.InstanceService
	publicURL string // optional fixed external base URL, no trailing slash

	mu    sync.Mutex
	codes map[string]authCode
}

type authCode struct {
	clientID    string
	redirectURI string
	challenge   string
	instanceID  string
	expires     time.Time
}

type oauthClient struct {
	ID           string
	SecretHash   string
	Name         string
	RedirectURIs []string
}

func openOAuth(instances instance_service.InstanceService) (*oauthServer, error) {
	path := strings.TrimSpace(os.Getenv(envOAuthDBPath))
	if path == "" {
		dir := "."
		if exe, err := os.Executable(); err == nil {
			dir = filepath.Dir(exe)
		}
		path = filepath.Join(dir, "dbdata", "mcp_oauth.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o751); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(oauthSchema); err != nil {
		db.Close()
		return nil, err
	}

	return &oauthServer{
		db:        db,
		instances: instances,
		publicURL: strings.TrimRight(strings.TrimSpace(os.Getenv(envPublicURL)), "/"),
		codes:     map[string]authCode{},
	}, nil
}

func (o *oauthServer) routes(r *gin.Engine) {
	r.GET(pathResourceMetadata, o.resourceMetadata)
	r.GET(pathResourceMetadata+Path, o.resourceMetadata)
	r.GET(pathServerMetadata, o.serverMetadata)
	r.POST(pathRegister, o.register)
	r.GET(pathAuthorize, o.authorizePage)
	r.POST(pathAuthorize, o.authorizeSubmit)
	r.POST(pathToken, o.token)
}

// baseURL is the externally visible origin. Behind a tunnel or reverse proxy
// (cloudflared, nginx) it is taken from the forwarded headers unless
// MCP_PUBLIC_URL pins it.
func (o *oauthServer) baseURL(c *gin.Context) string {
	if o.publicURL != "" {
		return o.publicURL
	}

	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if proto := firstForwarded(c.GetHeader("X-Forwarded-Proto")); proto == "http" || proto == "https" {
		scheme = proto
	}
	host := c.Request.Host
	if fwd := firstForwarded(c.GetHeader("X-Forwarded-Host")); fwd != "" {
		host = fwd
	}
	return scheme + "://" + host
}

func firstForwarded(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.ToLower(strings.TrimSpace(first))
}

// ---------------------------------------------------------------- discovery

func (o *oauthServer) resourceMetadata(c *gin.Context) {
	base := o.baseURL(c)
	c.JSON(http.StatusOK, gin.H{
		"resource":                 base + Path,
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Evolution GO (WhatsApp)",
	})
}

func (o *oauthServer) serverMetadata(c *gin.Context) {
	base := o.baseURL(c)
	c.JSON(http.StatusOK, gin.H{
		"issuer":                                base,
		"authorization_endpoint":                base + pathAuthorize,
		"token_endpoint":                        base + pathToken,
		"registration_endpoint":                 base + pathRegister,
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
	})
}

// challengeHeader is the WWW-Authenticate value that points clients at the
// discovery document (RFC 9728 §5.1).
func (o *oauthServer) challengeHeader(c *gin.Context) string {
	return fmt.Sprintf(`Bearer realm="evolution-go", resource_metadata="%s%s"`, o.baseURL(c), pathResourceMetadata)
}

// ---------------------------------------------------------------- registration

type registrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

func (o *oauthServer) register(c *gin.Context) {
	var req registrationRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		oauthError(c, http.StatusBadRequest, "invalid_client_metadata", "body must be a JSON object")
		return
	}

	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthError(c, http.StatusBadRequest, "invalid_redirect_uri", "between 1 and 10 redirect_uris are required")
		return
	}
	for _, uri := range req.RedirectURIs {
		if !validRedirectURI(uri) {
			oauthError(c, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris must be https, or http on a loopback address")
			return
		}
	}

	method := req.TokenEndpointAuthMethod
	if method == "" {
		method = "none"
	}
	if method != "none" && method != "client_secret_post" && method != "client_secret_basic" {
		oauthError(c, http.StatusBadRequest, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}

	name := truncate(strings.TrimSpace(req.ClientName), 120)
	clientID := randomToken("")
	secret, secretHash := "", ""
	if method != "none" {
		secret = randomToken("")
		secretHash = hashToken(secret)
	}

	uris, _ := json.Marshal(req.RedirectURIs)
	now := time.Now().Unix()
	if _, err := o.db.Exec(`INSERT INTO oauth_clients (client_id, secret_hash, name, redirect_uris, created_at) VALUES (?, ?, ?, ?, ?)`,
		clientID, secretHash, name, string(uris), now); err != nil {
		oauthError(c, http.StatusInternalServerError, "server_error", "could not store client")
		return
	}

	resp := gin.H{
		"client_id":                  clientID,
		"client_id_issued_at":        now,
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
	}
	if secret != "" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, resp)
}

func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}

func (o *oauthServer) client(id string) *oauthClient {
	if id == "" {
		return nil
	}
	cl := oauthClient{ID: id}
	var uris string
	if err := o.db.QueryRow(`SELECT secret_hash, name, redirect_uris FROM oauth_clients WHERE client_id = ?`, id).
		Scan(&cl.SecretHash, &cl.Name, &uris); err != nil {
		return nil
	}
	_ = json.Unmarshal([]byte(uris), &cl.RedirectURIs)
	return &cl
}

// ---------------------------------------------------------------- authorize

type authRequest struct {
	ClientID     string
	RedirectURI  string
	State        string
	Challenge    string
	Method       string
	ResponseType string
	Scope        string
	Resource     string
}

func readAuthRequest(get func(string) string) authRequest {
	return authRequest{
		ClientID:     get("client_id"),
		RedirectURI:  get("redirect_uri"),
		State:        get("state"),
		Challenge:    get("code_challenge"),
		Method:       get("code_challenge_method"),
		ResponseType: get("response_type"),
		Scope:        get("scope"),
		Resource:     get("resource"),
	}
}

// check validates an authorization request. A non-empty fatal means the
// client or redirect URI cannot be trusted, so the error is shown to the user
// and never redirected. A non-empty redirectErr is reported back to the
// (trusted) redirect URI.
func (o *oauthServer) check(req authRequest) (client *oauthClient, fatal, redirectErr string) {
	client = o.client(req.ClientID)
	if client == nil {
		return nil, "Aplicación desconocida (client_id inválido). Vuelve a conectar desde tu asistente.", ""
	}
	registered := false
	for _, uri := range client.RedirectURIs {
		if uri == req.RedirectURI {
			registered = true
			break
		}
	}
	if !registered {
		return nil, "La dirección de retorno (redirect_uri) no coincide con la registrada por la aplicación.", ""
	}
	if req.ResponseType != "code" {
		return client, "", "unsupported_response_type"
	}
	if req.Challenge == "" || req.Method != "S256" {
		return client, "", "invalid_request"
	}
	return client, "", ""
}

func (o *oauthServer) authorizePage(c *gin.Context) {
	req := readAuthRequest(c.Query)
	client, fatal, redirectErr := o.check(req)
	switch {
	case fatal != "":
		o.render(c, http.StatusBadRequest, authPage{Fatal: fatal})
	case redirectErr != "":
		redirectWith(c, req.RedirectURI, url.Values{"error": {redirectErr}, "state": {req.State}})
	default:
		o.render(c, http.StatusOK, newAuthPage(client, req, ""))
	}
}

func (o *oauthServer) authorizeSubmit(c *gin.Context) {
	req := readAuthRequest(c.PostForm)
	client, fatal, redirectErr := o.check(req)
	if fatal != "" {
		o.render(c, http.StatusBadRequest, authPage{Fatal: fatal})
		return
	}
	if redirectErr != "" {
		redirectWith(c, req.RedirectURI, url.Values{"error": {redirectErr}, "state": {req.State}})
		return
	}

	if c.PostForm("action") == "deny" {
		redirectWith(c, req.RedirectURI, url.Values{"error": {"access_denied"}, "state": {req.State}})
		return
	}

	token := strings.TrimSpace(c.PostForm("instance_token"))
	instance, err := o.instances.GetInstanceByToken(token)
	if token == "" || err != nil || instance == nil {
		o.render(c, http.StatusUnauthorized, newAuthPage(client, req, "Ese token no corresponde a ninguna instancia. Revísalo e inténtalo de nuevo."))
		return
	}

	code := randomToken("")
	o.mu.Lock()
	now := time.Now()
	for k, v := range o.codes {
		if now.After(v.expires) {
			delete(o.codes, k)
		}
	}
	o.codes[hashToken(code)] = authCode{
		clientID:    client.ID,
		redirectURI: req.RedirectURI,
		challenge:   req.Challenge,
		instanceID:  instance.Id,
		expires:     now.Add(authCodeTTL),
	}
	o.mu.Unlock()

	redirectWith(c, req.RedirectURI, url.Values{"code": {code}, "state": {req.State}})
}

func redirectWith(c *gin.Context, redirectURI string, params url.Values) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		c.String(http.StatusBadRequest, "invalid redirect_uri")
		return
	}
	q := u.Query()
	for k, v := range params {
		if len(v) > 0 && v[0] != "" {
			q.Set(k, v[0])
		}
	}
	u.RawQuery = q.Encode()
	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, u.String())
}

// ---------------------------------------------------------------- token

func (o *oauthServer) token(c *gin.Context) {
	c.Header("Cache-Control", "no-store")

	clientID, clientSecret := c.PostForm("client_id"), c.PostForm("client_secret")
	if id, secret, ok := c.Request.BasicAuth(); ok {
		clientID, clientSecret = id, secret
	}
	client := o.client(clientID)
	if client == nil {
		oauthError(c, http.StatusUnauthorized, "invalid_client", "unknown client")
		return
	}
	if client.SecretHash != "" && !constantEqual(client.SecretHash, hashToken(clientSecret)) {
		oauthError(c, http.StatusUnauthorized, "invalid_client", "bad client credentials")
		return
	}

	var instanceID string
	switch c.PostForm("grant_type") {
	case "authorization_code":
		key := hashToken(c.PostForm("code"))
		o.mu.Lock()
		code, ok := o.codes[key]
		delete(o.codes, key) // single use, whatever the outcome
		o.mu.Unlock()

		if !ok || time.Now().After(code.expires) || code.clientID != client.ID {
			oauthError(c, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
			return
		}
		if uri := c.PostForm("redirect_uri"); uri != "" && uri != code.redirectURI {
			oauthError(c, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
			return
		}
		sum := sha256.Sum256([]byte(c.PostForm("code_verifier")))
		if !constantEqual(base64.RawURLEncoding.EncodeToString(sum[:]), strings.TrimRight(code.challenge, "=")) {
			oauthError(c, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
		instanceID = code.instanceID

	case "refresh_token":
		// Consume atomically: a refresh token can be exchanged exactly once.
		var owner string
		err := o.db.QueryRow(`DELETE FROM oauth_tokens WHERE token_hash = ? AND kind = 'refresh' AND expires_at > ? RETURNING client_id, instance_id`,
			hashToken(c.PostForm("refresh_token")), time.Now().Unix()).Scan(&owner, &instanceID)
		if err != nil || owner != client.ID {
			oauthError(c, http.StatusBadRequest, "invalid_grant", "refresh token is invalid or expired")
			return
		}

	default:
		oauthError(c, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}

	// The grant dies with the instance.
	if _, err := o.instances.Info(instanceID); err != nil {
		oauthError(c, http.StatusBadRequest, "invalid_grant", "the instance no longer exists")
		return
	}

	access, refresh := randomToken(accessTokenPrefix), randomToken(refreshTokenPrefix)
	now := time.Now()
	if err := o.storeTokens(client.ID, instanceID, access, refresh, now); err != nil {
		oauthError(c, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": refresh,
	})
}

func (o *oauthServer) storeTokens(clientID, instanceID, access, refresh string, now time.Time) error {
	tx, err := o.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM oauth_tokens WHERE expires_at <= ?`, now.Unix()); err != nil {
		return err
	}
	const insert = `INSERT INTO oauth_tokens (token_hash, kind, client_id, instance_id, expires_at) VALUES (?, ?, ?, ?, ?)`
	if _, err := tx.Exec(insert, hashToken(access), "access", clientID, instanceID, now.Add(accessTokenTTL).Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(insert, hashToken(refresh), "refresh", clientID, instanceID, now.Add(refreshTokenTTL).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// instanceForAccessToken resolves a bearer access token to its instance id.
func (o *oauthServer) instanceForAccessToken(token string) (string, error) {
	var instanceID string
	err := o.db.QueryRow(`SELECT instance_id FROM oauth_tokens WHERE token_hash = ? AND kind = 'access' AND expires_at > ?`,
		hashToken(token), time.Now().Unix()).Scan(&instanceID)
	if err != nil {
		return "", errors.New("invalid or expired access token")
	}
	return instanceID, nil
}

// ---------------------------------------------------------------- helpers

func oauthError(c *gin.Context, status int, code, description string) {
	c.Header("Cache-Control", "no-store")
	c.AbortWithStatusJSON(status, gin.H{"error": code, "error_description": description})
}

func randomToken(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is not recoverable
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// Only hashes are stored, so a leaked database does not leak usable tokens.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func constantEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---------------------------------------------------------------- consent page

type authPage struct {
	Fatal        string
	Error        string
	ClientName   string
	RedirectHost string
	Req          authRequest
}

func newAuthPage(client *oauthClient, req authRequest, errMsg string) authPage {
	name := client.Name
	if name == "" {
		name = "Una aplicación"
	}
	host := ""
	if u, err := url.Parse(req.RedirectURI); err == nil {
		host = u.Host
	}
	return authPage{Error: errMsg, ClientName: name, RedirectHost: host, Req: req}
}

func (o *oauthServer) render(c *gin.Context, status int, page authPage) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Frame-Options", "DENY")
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action *; frame-ancestors 'none'")
	c.Header("Referrer-Policy", "no-referrer")
	c.Status(status)
	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = authTemplate.Execute(c.Writer, page)
}

var authTemplate = template.Must(template.New("authorize").Parse(`<!doctype html>
<html lang="es">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Autorizar acceso · Evolution GO</title>
<style>
:root { --bg:#f4f5f7; --card:#fff; --text:#16181d; --muted:#5d6470; --line:#d9dce2; --accent:#128c4a; --danger:#b3261e; }
@media (prefers-color-scheme: dark) { :root { --bg:#111317; --card:#1b1e24; --text:#eceef2; --muted:#9aa1ad; --line:#333842; --accent:#2fbf71; --danger:#ff8a80; } }
* { box-sizing: border-box; }
body { margin:0; min-height:100vh; display:grid; place-items:center; padding:16px; background:var(--bg); color:var(--text); font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif; }
main { width:100%; max-width:420px; background:var(--card); border:1px solid var(--line); border-radius:14px; padding:28px; }
h1 { font-size:20px; margin:0 0 8px; }
p { margin:0 0 14px; color:var(--muted); }
strong { color:var(--text); }
label { display:block; font-size:14px; font-weight:600; margin:18px 0 6px; }
input[type=password] { width:100%; padding:11px 12px; font:inherit; color:var(--text); background:transparent; border:1px solid var(--line); border-radius:8px; }
input[type=password]:focus { outline:2px solid var(--accent); outline-offset:1px; }
.actions { display:flex; flex-direction:row-reverse; gap:10px; margin-top:20px; }
button { flex:1; padding:11px; font:inherit; font-weight:600; border-radius:8px; border:1px solid var(--line); background:transparent; color:var(--text); cursor:pointer; }
button.primary { background:var(--accent); border-color:var(--accent); color:#fff; }
.error { color:var(--danger); font-size:14px; margin:10px 0 0; }
ul { margin:0 0 14px; padding-left:20px; color:var(--muted); }
small { display:block; margin-top:18px; color:var(--muted); font-size:13px; }
</style>
</head>
<body>
<main>
{{if .Fatal}}
<h1>No se puede autorizar</h1>
<p class="error">{{.Fatal}}</p>
{{else}}
<h1>Autorizar acceso a WhatsApp</h1>
<p><strong>{{.ClientName}}</strong> quiere conectarse a una instancia de Evolution GO. Si autorizas, podrá:</p>
<ul>
<li>Leer tus chats, contactos y grupos</li>
<li>Enviar mensajes en tu nombre</li>
</ul>
<form method="post" action="/oauth/authorize" autocomplete="off">
<input type="hidden" name="client_id" value="{{.Req.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.Req.RedirectURI}}">
<input type="hidden" name="state" value="{{.Req.State}}">
<input type="hidden" name="code_challenge" value="{{.Req.Challenge}}">
<input type="hidden" name="code_challenge_method" value="{{.Req.Method}}">
<input type="hidden" name="response_type" value="{{.Req.ResponseType}}">
<input type="hidden" name="scope" value="{{.Req.Scope}}">
<input type="hidden" name="resource" value="{{.Req.Resource}}">
<label for="instance_token">Token de la instancia</label>
<input id="instance_token" name="instance_token" type="password" autofocus>
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
<div class="actions">
<button type="submit" name="action" value="allow" class="primary">Autorizar</button>
<button type="submit" name="action" value="deny">Cancelar</button>
</div>
</form>
<small>Al terminar volverás a <strong>{{.RedirectHost}}</strong>. Autoriza solo si tú iniciaste esta conexión.</small>
{{end}}
</main>
</body>
</html>`))
