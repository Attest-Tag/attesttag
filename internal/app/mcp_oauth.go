package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The MCP server's authorization server: OAuth 2.1 as the MCP authorization spec profiles it, so
// that connecting Claude, Cursor or VS Code to /mcp is signing in to this console and pressing
// Allow, rather than pasting a developer key into the client. A key still works there too
// (mcp_server.go); this is the other door.
//
// Standard by standard:
//   - RFC 9728 protected resource metadata at /.well-known/oauth-protected-resource[/mcp], which a
//     401 from /mcp points at, naming this deployment as its own authorization server;
//   - RFC 8414 metadata at /.well-known/oauth-authorization-server;
//   - RFC 7591 dynamic registration at /oauth/register, open, because that is how MCP clients
//     introduce themselves;
//   - the authorization-code grant with PKCE — S256 only, and required — the RFC 8707 resource
//     parameter, and RFC 9207's iss on every redirect back to the client;
//   - rotating refresh tokens, where presenting one a refresh already replaced ends the grant;
//   - RFC 7009 revocation.
//
// What a grant is: a developer key made in a browser. It acts as the person who approved it, in the
// organisation they were signed in to, re-resolved on every request — the same rule as a key
// (api_keys.go) — and approving one takes what minting a key takes. Its tokens open /mcp and
// nothing else: /v1 still takes keys only, and the console sessions only.

const (
	mcpAccessPrefix  = "ato1."
	mcpRefreshPrefix = "atr1."
	mcpCodePrefix    = "atc1."
	mcpClientPrefix  = "mcpc_"
	mcpSecretPrefix  = "mcps_"

	mcpAccessTTL  = time.Hour
	mcpRefreshTTL = 30 * 24 * time.Hour
	mcpCodeTTL    = 10 * time.Minute

	// mcpScope is the only scope there is: a grant is its person, as a key is, with no narrower
	// shape to ask for. offline_access is accepted and handed back to the clients that ask for it
	// before they will expect a refresh token.
	mcpScope = "mcp"

	// mcpConsentPage is the console page an authorization request lands on.
	mcpConsentPage = "/admin/oauth/"
)

// mcpRegisters counts registrations per address. Registration is open by design, and this is what
// keeps it from being a way to fill a table.
var mcpRegisters = newRateLimiter()

// mcpTokenCalls counts token-endpoint calls per address: the one place a guessed code or a stolen
// refresh token would be tried.
var mcpTokenCalls = newRateLimiter()

func mintMCPSecret(prefix string) string {
	buf := make([]byte, 32)
	rand.Read(buf)
	return prefix + base64.RawURLEncoding.EncodeToString(buf)
}

// mcpOrigin is this deployment as MCP clients know it: the issuer, and the host of the resource.
func (b *Bot) mcpOrigin(ctx context.Context) string {
	return strings.TrimRight(publicBaseURL(ctx, b.store, b.cfg), "/")
}

func mcpResourceOf(origin string) string { return origin + "/mcp" }

// mcpResourceFor reads an RFC 8707 resource parameter. Either name for this server is accepted:
// the MCP endpoint, which is what clients send, or the origin, which is what the metadata document
// at the root names. A missing one means this server. Anything else is another server, and a
// token for it is not this one's to issue.
func mcpResourceFor(origin, asked string) (string, bool) {
	if asked == "" {
		return mcpResourceOf(origin), true
	}
	a := strings.TrimRight(asked, "/")
	if strings.EqualFold(a, origin) || strings.EqualFold(a, mcpResourceOf(origin)) {
		return mcpResourceOf(origin), true
	}
	return "", false
}

// ---- redirect addresses ----

var (
	mcpSchemeRe = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)
	// Schemes a browser would run, read or hand to another program with the code in it.
	mcpBlockedSchemes = map[string]bool{"javascript": true, "data": true, "file": true, "vbscript": true,
		"about": true, "blob": true, "filesystem": true, "ftp": true, "ws": true, "wss": true,
		"mailto": true, "tel": true, "sms": true, "view-source": true, "chrome": true, "resource": true}
)

// mcpRedirectAllowed decides whether a client may register a redirect address: https anywhere;
// http only to this machine (RFC 8252 §7.3), which is how Claude Code and other command-line
// clients receive the code; or an app's own scheme (§7.1), which is how Cursor and VS Code do.
func mcpRedirectAllowed(raw string) error {
	if len(raw) > 2000 {
		return errors.New("is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return errors.New("is not an absolute URL")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return errors.New("must not carry a fragment")
	}
	if u.User != nil {
		return errors.New("must not carry credentials")
	}
	switch scheme := strings.ToLower(u.Scheme); {
	case scheme == "https":
		if u.Host == "" {
			return errors.New("has no host")
		}
	case scheme == "http":
		if !isLoopbackHost(u.Hostname()) {
			return errors.New("uses http, which is only allowed to localhost")
		}
	case mcpBlockedSchemes[scheme]:
		return fmt.Errorf("uses %s:, which is not allowed", scheme)
	case !mcpSchemeRe.MatchString(scheme):
		return errors.New("has an invalid scheme")
	}
	return nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// mcpRedirectMatches is whether an authorization request's redirect address is one the client
// registered: exactly, except that a loopback address may come back on another port (RFC 8252
// §7.3), because a command-line client listens wherever a port is free that day.
func mcpRedirectMatches(registered []string, given string) bool {
	if slices.Contains(registered, given) {
		return true
	}
	g, err := url.Parse(given)
	if err != nil || g.Scheme != "http" || !isLoopbackHost(g.Hostname()) {
		return false
	}
	for _, r := range registered {
		u, err := url.Parse(r)
		if err != nil || u.Scheme != "http" || !isLoopbackHost(u.Hostname()) {
			continue
		}
		if strings.EqualFold(u.Hostname(), g.Hostname()) && u.Path == g.Path && u.RawQuery == g.RawQuery {
			return true
		}
	}
	return false
}

// mcpReturnsTo is what a person can check a client by: the host its redirect address names, or
// for an app's own scheme, the scheme. Its name is whatever it chose to call itself.
func mcpReturnsTo(redirect string) string {
	u, err := url.Parse(redirect)
	if err != nil {
		return ""
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return u.Host
	}
	return u.Scheme + "://" + u.Host
}

// cleanClientName keeps a self-chosen name to something a consent screen can print: no control or
// direction-changing characters, one line, sixty characters.
func cleanClientName(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r), r == 0x200b, r == 0xfeff: // zero-width space, byte-order mark
			continue
		case unicode.IsSpace(r):
			sb.WriteRune(' ')
		default:
			sb.WriteRune(r)
		}
	}
	name := strings.Join(strings.Fields(sb.String()), " ")
	if name == "" {
		return "An MCP client"
	}
	name, _ = cutRunes(name, 60)
	return name
}

// ---- metadata ----

// mcpCORS opens the discovery, registration and token endpoints to browser-based clients. Nothing
// behind them reads a cookie — every request carries its own proof — so any origin may call them.
func mcpCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version, Mcp-Session-Id, Last-Event-ID")
		h.Set("Access-Control-Expose-Headers", "Mcp-Session-Id, WWW-Authenticate")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func onlyGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET")
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	return false
}

// handleMCPResourceMeta is RFC 9728's document. At the MCP endpoint's own path it describes that
// endpoint; at the root it describes the origin, because a client that reads it there checks the
// resource it names against the origin.
func (b *Bot) handleMCPResourceMeta(w http.ResponseWriter, r *http.Request) {
	if !onlyGET(w, r) {
		return
	}
	origin := b.mcpOrigin(r.Context())
	resource := mcpResourceOf(origin)
	if r.URL.Path == "/.well-known/oauth-protected-resource" {
		resource = origin
	}
	writeJSON(w, 200, map[string]any{
		"resource":                 resource,
		"authorization_servers":    []string{origin},
		"scopes_supported":         []string{mcpScope},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "attest_tag",
		"resource_documentation":   origin + "/admin/developer/mcp/",
	})
}

func (b *Bot) handleMCPAuthServerMeta(w http.ResponseWriter, r *http.Request) {
	if !onlyGET(w, r) {
		return
	}
	origin := b.mcpOrigin(r.Context())
	writeJSON(w, 200, map[string]any{
		"issuer":                                         origin,
		"authorization_endpoint":                         origin + "/oauth/authorize",
		"token_endpoint":                                 origin + "/oauth/token",
		"registration_endpoint":                          origin + "/oauth/register",
		"revocation_endpoint":                            origin + "/oauth/revoke",
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_basic", "client_secret_post"},
		"revocation_endpoint_auth_methods_supported":     []string{"none", "client_secret_basic", "client_secret_post"},
		"code_challenge_methods_supported":               []string{"S256"},
		"scopes_supported":                               []string{mcpScope, "offline_access"},
		"authorization_response_iss_parameter_supported": true,
		"service_documentation":                          origin + "/admin/developer/mcp/",
	})
}

// ---- errors, in OAuth's shape ----

func writeOAuthJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, status, v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeOAuthJSON(w, status, map[string]any{"error": code, "error_description": desc})
}

// ---- registration (RFC 7591) ----

func (b *Bot) handleMCPRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "register with a POST")
		return
	}
	if ok, _ := mcpRegisters.allow("mcp-register:"+clientIP(r), 30, time.Hour); !ok {
		oauthError(w, http.StatusTooManyRequests, "invalid_request", "too many registrations from this address; try again later")
		return
	}
	var in struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body is not client metadata in JSON")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "register between one and ten redirect_uris")
		return
	}
	for _, u := range in.RedirectURIs {
		if err := mcpRedirectAllowed(u); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", fmt.Sprintf("%q %s", u, err))
			return
		}
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", fmt.Sprintf("grant type %q is not supported", g))
			return
		}
	}
	for _, t := range in.ResponseTypes {
		if t != "code" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", fmt.Sprintf("response type %q is not supported", t))
			return
		}
	}
	method := in.TokenEndpointAuthMethod
	switch method {
	case "", "none":
		method = "none"
	case "client_secret_basic", "client_secret_post":
	default:
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata",
			"token_endpoint_auth_method must be none, client_secret_basic or client_secret_post")
		return
	}
	id := make([]byte, 16)
	rand.Read(id)
	c := MCPClient{ID: mcpClientPrefix + base64.RawURLEncoding.EncodeToString(id), Name: cleanClientName(in.ClientName),
		RedirectURIs: in.RedirectURIs, AuthMethod: method, CreatedAt: now()}
	secret := ""
	if method != "none" {
		secret = mintMCPSecret(mcpSecretPrefix)
		c.SecretHash = hashAPIKey(secret)
	}
	if err := b.store.CreateMCPClient(r.Context(), c); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not register the client")
		return
	}
	out := map[string]any{
		"client_id": c.ID, "client_id_issued_at": time.Now().Unix(), "client_name": c.Name,
		"redirect_uris": c.RedirectURIs, "grant_types": []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"}, "token_endpoint_auth_method": method,
	}
	if secret != "" {
		out["client_secret"] = secret
		out["client_secret_expires_at"] = 0
	}
	writeOAuthJSON(w, http.StatusCreated, out)
}

// ---- the authorization request ----

// mcpAuthRequest is an authorization request that checked out: a registered client, a redirect
// address it registered, PKCE, and a resource that is this server.
type mcpAuthRequest struct {
	Client      *MCPClient
	RedirectURI string
	Challenge   string
	State       string
	Scope       string
	Resource    string
}

// mcpAuthError is a request that did not. Redirect is where the client should hear about it —
// empty when the client or its redirect address is the problem, because then there is nowhere
// that can be trusted to send it.
type mcpAuthError struct {
	Code, Desc string
	Redirect   string
}

var pkceChallengeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43,128}$`)

// checkMCPAuthRequest validates an authorization request's parameters. It runs three times for
// one request — when it arrives, when the consent screen reads it, and when Allow is pressed —
// because the second and third see the parameters as the browser kept them, not as they arrived.
func (b *Bot) checkMCPAuthRequest(ctx context.Context, q url.Values) (*mcpAuthRequest, *mcpAuthError) {
	origin := b.mcpOrigin(ctx)
	c, err := b.store.MCPClient(ctx, q.Get("client_id"))
	if err != nil {
		return nil, &mcpAuthError{Code: "server_error", Desc: "could not look the app up"}
	}
	if c == nil {
		return nil, &mcpAuthError{Code: "invalid_client", Desc: "This app is not registered here. Remove it in the app and add it again."}
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" || !mcpRedirectMatches(c.RedirectURIs, redirect) {
		return nil, &mcpAuthError{Code: "invalid_request", Desc: "The address this app asked to be sent back to is not one it registered."}
	}
	state := q.Get("state")
	back := func(code, desc string) *mcpAuthError {
		return &mcpAuthError{Code: code, Desc: desc, Redirect: mcpRedirectWith(redirect, origin, state, url.Values{
			"error": {code}, "error_description": {desc}})}
	}
	if q.Get("response_type") != "code" {
		return nil, back("unsupported_response_type", "only response_type=code is supported")
	}
	if q.Get("code_challenge_method") != "S256" || !pkceChallengeRe.MatchString(q.Get("code_challenge")) {
		return nil, back("invalid_request", "PKCE is required: send code_challenge with code_challenge_method=S256")
	}
	resource, ok := mcpResourceFor(origin, q.Get("resource"))
	if !ok {
		return nil, back("invalid_target", "resource must be this server's MCP endpoint, "+mcpResourceOf(origin))
	}
	if len(state) > 2000 {
		return nil, back("invalid_request", "state is too long")
	}
	scope := mcpScope
	if slices.Contains(strings.Fields(q.Get("scope")), "offline_access") {
		scope += " offline_access"
	}
	return &mcpAuthRequest{Client: c, RedirectURI: redirect, Challenge: q.Get("code_challenge"), State: state,
		Scope: scope, Resource: resource}, nil
}

// mcpRedirectWith is the redirect address with a response added to whatever query it already had,
// and the issuer beside it (RFC 9207), so a client talking to several servers can tell which one
// answered.
func mcpRedirectWith(redirect, origin, state string, add url.Values) string {
	u, err := url.Parse(redirect)
	if err != nil {
		return ""
	}
	q := u.Query()
	for k, vs := range add {
		q[k] = vs
	}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", origin)
	u.RawQuery = q.Encode()
	return u.String()
}

// handleMCPAuthorize is the authorization endpoint. It decides nothing itself: a request that
// checks out goes on to the consent page in the console, with its query as it came, and the page
// signs the person in if it has to. One that does not goes back to the client when the client can
// be trusted to hear it, and to the consent page's error otherwise.
func (b *Bot) handleMCPAuthorize(w http.ResponseWriter, r *http.Request) {
	if _, perr := b.checkMCPAuthRequest(r.Context(), r.URL.Query()); perr != nil {
		if perr.Redirect != "" {
			http.Redirect(w, r, perr.Redirect, http.StatusFound)
			return
		}
		http.Redirect(w, r, mcpConsentPage+"?"+url.Values{"error": {perr.Code}, "error_description": {perr.Desc}}.Encode(), http.StatusFound)
		return
	}
	http.Redirect(w, r, mcpConsentPage+"?"+r.URL.RawQuery, http.StatusFound)
}

// mayConnectMCP says what stops this person approving a client, or "" when nothing does. The same
// rule as minting a key, because that is what approving one is.
func (b *Bot) mayConnectMCP(ctx context.Context, u *AdminUser) string {
	if u == nil || !u.Permissions[PermAPIKeysManage] {
		return "Connecting an app makes a credential that acts as you, like an API key, so it needs the same permission: ask an admin to give your role API keys."
	}
	return b.needsVerifiedEmail(ctx, u, PermAPIKeysManage)
}

// handleMCPConsentGet is what the consent page shows: which app, where it will send the person
// back to, and whether they may approve it. A request that does not check out is still a 200,
// with the problem in the body: the page has something to say about it — and, when the client
// can be trusted to hear it, somewhere to send the person back to — rather than a failed fetch.
func (b *Bot) handleMCPConsentGet(w http.ResponseWriter, r *http.Request) {
	req, perr := b.checkMCPAuthRequest(r.Context(), r.URL.Query())
	if perr != nil {
		writeJSON(w, 200, map[string]any{"problem": map[string]any{"error": perr.Desc, "code": perr.Code, "redirect": perr.Redirect}})
		return
	}
	u := adminFromCtx(r.Context())
	writeJSON(w, 200, map[string]any{
		"client_name":  req.Client.Name,
		"returns_to":   mcpReturnsTo(req.RedirectURI),
		"redirect_uri": req.RedirectURI,
		"org_name":     u.OrgName,
		"email":        u.Email,
		"refusal":      b.mayConnectMCP(r.Context(), u),
	})
}

// handleMCPConsentPost is Allow or Cancel. Allow writes a code for the token endpoint; either way
// the answer is the address to send the browser to.
func (b *Bot) handleMCPConsentPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query string `json:"query"`
		Allow bool   `json:"allow"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	q, err := url.ParseQuery(strings.TrimPrefix(in.Query, "?"))
	if err != nil {
		bad(w, err)
		return
	}
	ctx := r.Context()
	req, perr := b.checkMCPAuthRequest(ctx, q)
	if perr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": perr.Desc, "code": perr.Code, "redirect": perr.Redirect})
		return
	}
	origin := b.mcpOrigin(ctx)
	if !in.Allow {
		writeJSON(w, 200, map[string]any{"redirect": mcpRedirectWith(req.RedirectURI, origin, req.State,
			url.Values{"error": {"access_denied"}, "error_description": {"the request was declined"}})})
		return
	}
	u := adminFromCtx(ctx)
	if why := b.mayConnectMCP(ctx, u); why != "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": why})
		return
	}
	code := mintMCPSecret(mcpCodePrefix)
	if err := b.store.CreateMCPCode(ctx, mcpCode{Hash: hashAPIKey(code), ClientID: req.Client.ID, OrgID: u.OrgID,
		UserID: u.ID, RedirectURI: req.RedirectURI, Challenge: req.Challenge, Scope: req.Scope, Resource: req.Resource,
		ExpiresAt: time.Now().Add(mcpCodeTTL).UTC().Format(time.DateTime)}); err != nil {
		fail(w, err)
		return
	}
	b.audit(r, "mcp.approved", AuditEvent{TargetKind: "mcp_client", TargetID: req.Client.ID, TargetName: req.Client.Name,
		Details: auditDetails(map[string]any{"returns_to": mcpReturnsTo(req.RedirectURI)})})
	writeJSON(w, 200, map[string]any{"redirect": mcpRedirectWith(req.RedirectURI, origin, req.State, url.Values{"code": {code}})})
}

// ---- the token endpoint ----

// mcpTokenClient authenticates the client at the token and revocation endpoints: by its id alone
// when it registered as public, which PKCE then stands behind, and by its secret as well when it
// registered with one.
func (b *Bot) mcpTokenClient(w http.ResponseWriter, r *http.Request) *MCPClient {
	id, secret, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	refuse := func() *MCPClient {
		if basic {
			w.Header().Set("WWW-Authenticate", `Basic realm="attest_tag"`)
		}
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client, or the wrong secret for it")
		return nil
	}
	if id == "" {
		return refuse()
	}
	c, err := b.store.MCPClient(r.Context(), id)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not look the client up")
		return nil
	}
	if c == nil {
		return refuse()
	}
	if c.SecretHash != "" && subtle.ConstantTimeCompare([]byte(hashAPIKey(secret)), []byte(c.SecretHash)) != 1 {
		return refuse()
	}
	return c
}

// pkceMatches is RFC 7636's S256 check.
func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

func (b *Bot) handleMCPToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "use POST")
		return
	}
	if ok, _ := mcpTokenCalls.allow("mcp-token:"+clientIP(r), 120, time.Minute); !ok {
		oauthError(w, http.StatusTooManyRequests, "invalid_request", "too many token requests from this address; slow down")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body is not a form")
		return
	}
	client := b.mcpTokenClient(w, r)
	if client == nil {
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		b.mcpExchangeCode(w, r, client)
	case "refresh_token":
		b.mcpRefresh(w, r, client)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

// mcpTokens mints a new access and refresh token with their expiries.
func mcpTokens() (access, accessExp, refresh, refreshExp string) {
	n := time.Now().UTC()
	return mintMCPSecret(mcpAccessPrefix), n.Add(mcpAccessTTL).Format(time.DateTime),
		mintMCPSecret(mcpRefreshPrefix), n.Add(mcpRefreshTTL).Format(time.DateTime)
}

func writeMCPTokens(w http.ResponseWriter, access, refresh, scope string) {
	writeOAuthJSON(w, 200, map[string]any{"access_token": access, "token_type": "Bearer",
		"expires_in": int(mcpAccessTTL.Seconds()), "refresh_token": refresh, "scope": scope})
}

func (b *Bot) mcpExchangeCode(w http.ResponseWriter, r *http.Request, client *MCPClient) {
	ctx := r.Context()
	code, verifier := r.PostForm.Get("code"), r.PostForm.Get("code_verifier")
	if code == "" || verifier == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "code and code_verifier are both required")
		return
	}
	row, first, err := b.store.UseMCPCode(ctx, hashAPIKey(code))
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not check that code")
		return
	}
	if row == nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown authorization code")
		return
	}
	if !first {
		// A code presented twice has been copied, and the copy may be the one that got there
		// first. RFC 6749 §4.1.2: end whatever the first exchange made.
		if row.GrantID != 0 {
			b.store.RevokeMCPGrant(ctx, row.OrgID, row.GrantID)
		}
		oauthError(w, http.StatusBadRequest, "invalid_grant", "that authorization code was used already")
		return
	}
	switch {
	case row.ExpiresAt <= now():
		oauthError(w, http.StatusBadRequest, "invalid_grant", "that authorization code has expired")
		return
	case row.ClientID != client.ID:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "that authorization code was issued to another client")
		return
	case r.PostForm.Get("redirect_uri") != row.RedirectURI:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	case !pkceMatches(verifier, row.Challenge):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code_challenge")
		return
	}
	if res := r.PostForm.Get("resource"); res != "" {
		if got, ok := mcpResourceFor(b.mcpOrigin(ctx), res); !ok || got != row.Resource {
			oauthError(w, http.StatusBadRequest, "invalid_target", "resource does not match the authorization request")
			return
		}
	}
	// Ten minutes is long enough for somebody to be removed, or have their role changed, between
	// Allow and here.
	u, kerr := b.actAs(ctx, row.UserID, row.OrgID, "connection")
	if kerr != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", kerr.msg)
		return
	}
	if why := b.mayConnectMCP(ctx, u); why != "" {
		oauthError(w, http.StatusBadRequest, "invalid_grant", why)
		return
	}
	access, accessExp, refresh, refreshExp := mcpTokens()
	id, err := b.store.CreateMCPGrant(ctx, MCPGrant{OrgID: row.OrgID, UserID: row.UserID, ClientID: client.ID,
		ClientName: client.Name, RedirectURI: row.RedirectURI, Scope: row.Scope, Resource: row.Resource},
		hashAPIKey(access), accessExp, hashAPIKey(refresh), refreshExp)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not record the connection")
		return
	}
	b.store.SetMCPCodeGrant(ctx, row.Hash, id)
	b.store.TouchMCPClient(ctx, client.ID)
	writeMCPTokens(w, access, refresh, row.Scope)
}

func (b *Bot) mcpRefresh(w http.ResponseWriter, r *http.Request, client *MCPClient) {
	ctx := r.Context()
	spent := r.PostForm.Get("refresh_token")
	if spent == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	g, replayed, err := b.store.MCPGrantByRefresh(ctx, hashAPIKey(spent))
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not check that token")
		return
	}
	if g == nil || g.ClientID != client.ID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown refresh token")
		return
	}
	if replayed {
		// The client was handed a new refresh token when this one was spent, so whoever is
		// presenting the old one again is somebody else, or somebody as well. Ending the grant
		// is the only answer that is safe for both cases; the person connects again.
		if ended, _ := b.store.RevokeMCPGrant(ctx, g.OrgID, g.ID); ended {
			b.audit(r, "mcp.replay_ended", AuditEvent{OrgID: g.OrgID, Via: viaMCP, Outcome: auditDenied,
				TargetKind: "mcp_grant", TargetID: strconv.FormatInt(g.ID, 10), TargetName: client.Name})
		}
		oauthError(w, http.StatusBadRequest, "invalid_grant", "that refresh token was used already, so the connection has been ended; connect the app again")
		return
	}
	if g.RevokedAt != "" {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "this connection was revoked")
		return
	}
	if g.RefreshExpiresAt <= now() {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "this connection lapsed after 30 days unused; connect the app again")
		return
	}
	if res := r.PostForm.Get("resource"); res != "" {
		if got, ok := mcpResourceFor(b.mcpOrigin(ctx), res); !ok || got != g.Resource {
			oauthError(w, http.StatusBadRequest, "invalid_target", "resource does not match the connection")
			return
		}
	}
	if _, kerr := b.actAs(ctx, g.UserID, g.OrgID, "connection"); kerr != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", kerr.msg)
		return
	}
	access, accessExp, refresh, refreshExp := mcpTokens()
	ok, err := b.store.RotateMCPGrant(ctx, g.OrgID, g.ID, hashAPIKey(spent), hashAPIKey(access), accessExp, hashAPIKey(refresh), refreshExp)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not refresh the connection")
		return
	}
	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "that refresh token was just used by another request")
		return
	}
	b.store.TouchMCPClient(ctx, client.ID)
	writeMCPTokens(w, access, refresh, g.Scope)
}

// handleMCPRevoke is RFC 7009: a client signing itself out. It answers 200 whether or not the
// token meant anything, so the endpoint cannot be used to test which tokens are live.
func (b *Bot) handleMCPRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "use POST")
		return
	}
	if ok, _ := mcpTokenCalls.allow("mcp-token:"+clientIP(r), 120, time.Minute); !ok {
		oauthError(w, http.StatusTooManyRequests, "invalid_request", "too many requests from this address; slow down")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body is not a form")
		return
	}
	client := b.mcpTokenClient(w, r)
	if client == nil {
		return
	}
	ctx := r.Context()
	h := hashAPIKey(r.PostForm.Get("token"))
	g, _ := b.store.MCPGrantByAccess(ctx, h)
	if g == nil {
		g, _, _ = b.store.MCPGrantByRefresh(ctx, h)
	}
	if g != nil && g.ClientID == client.ID {
		if ended, _ := b.store.RevokeMCPGrant(ctx, g.OrgID, g.ID); ended {
			b.audit(r, "mcp.revoked", AuditEvent{OrgID: g.OrgID, Via: viaMCP, TargetKind: "mcp_grant",
				TargetID: strconv.FormatInt(g.ID, 10), TargetName: client.Name,
				Details: auditDetails(map[string]any{"by": "the app itself"})})
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// mcpOAuthRoutes registers the authorization server. Every route but the consent API is public:
// each request carries its own proof — a client id and PKCE, a code, a refresh token — and the
// person is only ever asked in the console, behind their session.
func (b *Bot) mcpOAuthRoutes(mux *http.ServeMux) {
	meta := mcpCORS(b.handleMCPResourceMeta)
	mux.HandleFunc("/.well-known/oauth-protected-resource", meta)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", meta)
	mux.HandleFunc("/.well-known/oauth-authorization-server", mcpCORS(b.handleMCPAuthServerMeta))
	mux.HandleFunc("/oauth/register", mcpCORS(b.handleMCPRegister))
	mux.HandleFunc("/oauth/token", mcpCORS(b.handleMCPToken))
	mux.HandleFunc("/oauth/revoke", mcpCORS(b.handleMCPRevoke))
	mux.HandleFunc("GET /oauth/authorize", b.handleMCPAuthorize)
	// The consent page's own API, behind the console session like every /api route — so the
	// organisation's sign-in policy and its two-factor rule stand in front of every approval.
	mux.HandleFunc("GET /api/oauth/consent", b.requireAdmin(b.handleMCPConsentGet))
	mux.HandleFunc("POST /api/oauth/consent", b.requireAdmin(b.handleMCPConsentPost))
}
