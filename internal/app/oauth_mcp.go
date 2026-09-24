package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuth 2.0 authorization-code for remote MCP servers (e.g. mcp.mongodb.com), per the MCP
// authorization spec: discover the authorization server from the resource (RFC 9728), read
// its metadata (RFC 8414), register a client dynamically when we have none (RFC 7591), then
// PKCE (S256). Tokens live sealed in the connection secret and refresh automatically.

type OAuthState struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	AuthURL      string `json:"auth_url,omitempty"`
	TokenURL     string `json:"token_url,omitempty"`
	Scopes       string `json:"scopes,omitempty"`
	Resource     string `json:"resource,omitempty"`

	// The provider's signed assertion about who just signed in, when it sends one. Never
	// sealed — it is read once, for the address to label the account with, and then it is of
	// no further use to anybody.
	IDToken string `json:"-"`
}

type asMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

func getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: http %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// discover finds the authorization server for an MCP endpoint.
func discoverAuthServer(ctx context.Context, mcpURL string) (*asMetadata, string, error) {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return nil, "", err
	}
	origin := u.Scheme + "://" + u.Host
	resource := mcpURL
	// 1. protected resource metadata (path-aware first, then origin)
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	candidates := []string{origin + "/.well-known/oauth-protected-resource" + u.Path, origin + "/.well-known/oauth-protected-resource"}
	var servers []string
	for _, c := range candidates {
		if getJSON(ctx, c, &prm) == nil && len(prm.AuthorizationServers) > 0 {
			servers = prm.AuthorizationServers
			if prm.Resource != "" {
				resource = prm.Resource
			}
			break
		}
	}
	if len(servers) == 0 {
		servers = []string{origin} // fall back: the MCP host is its own authorization server
	}
	// 2. authorization server metadata
	for _, as := range servers {
		asu, err := url.Parse(as)
		if err != nil {
			continue
		}
		base := asu.Scheme + "://" + asu.Host
		for _, m := range []string{
			base + "/.well-known/oauth-authorization-server" + strings.TrimSuffix(asu.Path, "/"),
			base + "/.well-known/oauth-authorization-server",
			base + "/.well-known/openid-configuration" + strings.TrimSuffix(asu.Path, "/"),
			base + "/.well-known/openid-configuration",
		} {
			var md asMetadata
			if getJSON(ctx, m, &md) == nil && md.AuthorizationEndpoint != "" && md.TokenEndpoint != "" {
				return &md, resource, nil
			}
		}
	}
	return nil, "", errors.New("could not discover an OAuth authorization server for this MCP endpoint")
}

// registerClient performs dynamic client registration when the server supports it.
func registerClient(ctx context.Context, md *asMetadata, redirectURI string) (id, secret string, err error) {
	if md.RegistrationEndpoint == "" {
		return "", "", errors.New("the server needs a pre-registered client id: set it in the Advanced tab")
	}
	body, _ := json.Marshal(map[string]any{
		"client_name": "attest_tag", "redirect_uris": []string{redirectURI},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, "POST", md.RegistrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Error        string `json:"error_description"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	json.Unmarshal(raw, &out)
	if out.ClientID == "" {
		return "", "", fmt.Errorf("client registration failed: %s", truncate(out.Error+" "+string(raw), 200))
	}
	return out.ClientID, out.ClientSecret, nil
}

func pkcePair() (verifier, challenge string) {
	b := make([]byte, 32)
	rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// oauthStart builds the authorization URL for a connection and remembers the PKCE verifier.
func (b *Bot) oauthStart(ctx context.Context, orgID int64, conn *Connection, redirectURI string) (string, error) {
	sec, err := b.proxy.secret(conn)
	if err != nil {
		return "", err
	}
	if sec.MCPURL == "" {
		return "", errors.New("the connection has no MCP server url")
	}
	st := sec.OAuth
	if st == nil {
		st = &OAuthState{}
	}
	if st.AuthURL == "" || st.TokenURL == "" {
		md, resource, err := discoverAuthServer(ctx, sec.MCPURL)
		if err != nil {
			return "", err
		}
		st.AuthURL, st.TokenURL, st.Resource = md.AuthorizationEndpoint, md.TokenEndpoint, resource
		if st.Scopes == "" && len(md.ScopesSupported) > 0 {
			st.Scopes = strings.Join(md.ScopesSupported, " ")
		}
		if st.ClientID == "" {
			id, secret, err := registerClient(ctx, md, redirectURI)
			if err != nil {
				return "", err
			}
			st.ClientID, st.ClientSecret = id, secret
		}
	}
	for _, rawURL := range []string{st.AuthURL, st.TokenURL} {
		u, err := url.Parse(rawURL)
		if err != nil {
			return "", err
		}
		if err := publicURL(u); err != nil {
			return "", err
		}
	}
	sec.OAuth = st
	enc, err := b.sealSecret(sec)
	if err != nil {
		return "", err
	}
	if err := b.store.UpdateConnection(ctx, orgID, conn, enc); err != nil {
		return "", err
	}
	verifier, challenge := pkcePair()
	state := randomToken()
	me := adminFromCtx(ctx)
	var userID int64
	if me != nil {
		userID = me.ID
	}
	if err := b.store.PutOAuthPending(ctx, b.sealer, state,
		oauthPending{userID: userID, orgID: orgID, connID: conn.ID, verifier: verifier}); err != nil {
		return "", err
	}
	q := url.Values{"response_type": {"code"}, "client_id": {st.ClientID}, "redirect_uri": {redirectURI}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	if st.Scopes != "" {
		q.Set("scope", st.Scopes)
	}
	if st.Resource != "" {
		q.Set("resource", st.Resource)
	}
	sep := "?"
	if strings.Contains(st.AuthURL, "?") {
		sep = "&"
	}
	return st.AuthURL + sep + q.Encode(), nil
}

// oauthCallback exchanges the code and stores the tokens on the connection.
func (b *Bot) oauthCallback(ctx context.Context, state, code, redirectURI string) (*Connection, error) {
	p, err := b.store.TakeOAuthPending(ctx, b.sealer, state)
	if err != nil {
		return nil, err
	}
	me := &AdminUser{ID: p.userID, OrgID: p.orgID}
	b.permissionsFor(ctx, me)
	if !me.Permissions[PermConnManage] {
		return nil, errors.New("connection management authority was revoked")
	}
	conn, err := b.store.Connection(ctx, p.orgID, p.connID)
	if err != nil || conn == nil {
		return nil, errors.New("connection not found")
	}
	sec, err := b.proxy.secret(conn)
	if err != nil || sec.OAuth == nil {
		return nil, errors.New("connection has no OAuth setup")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
		"client_id": {sec.OAuth.ClientID}, "code_verifier": {p.verifier}}
	if sec.OAuth.Resource != "" {
		form.Set("resource", sec.OAuth.Resource)
	}
	if err := tokenRequest(ctx, sec.OAuth, form); err != nil {
		return nil, err
	}
	sec.Token = sec.OAuth.AccessToken
	enc, err := b.sealSecret(sec)
	if err != nil {
		return nil, err
	}
	conn.Status = "active"
	if err := b.store.UpdateConnection(ctx, p.orgID, conn, enc); err != nil {
		return nil, err
	}
	b.changed(ctx, p.orgID)
	b.agent.mcp.forget(conn.ID)
	return conn, nil
}

func tokenRequest(ctx context.Context, st *OAuthState, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, "POST", st.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if st.ClientSecret != "" {
		req.SetBasicAuth(st.ClientID, st.ClientSecret)
	}
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	json.Unmarshal(raw, &tr)
	if tr.AccessToken == "" {
		return fmt.Errorf("token request failed (%d): %s", resp.StatusCode, truncate(tr.Error+" "+tr.ErrorDesc+" "+string(raw), 300))
	}
	st.AccessToken = tr.AccessToken
	st.IDToken = tr.IDToken
	// What was actually granted, which is not always what was asked for: a person may untick
	// parts on the way through, and a provider may hand back less than was requested. Storing
	// the answer rather than the question is what lets the bot say "you did not connect mail"
	// instead of guessing at a 403.
	if tr.Scope != "" {
		st.Scopes = tr.Scope
	}
	if tr.RefreshToken != "" {
		st.RefreshToken = tr.RefreshToken
	}
	if tr.ExpiresIn > 0 {
		st.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	} else {
		st.ExpiresAt = 0
	}
	return nil
}

// freshMCPToken returns a valid access token for an OAuth-backed MCP connection, refreshing
// (and persisting) when it is within two minutes of expiry.
func (b *Bot) freshMCPToken(ctx context.Context, orgID int64, conn *Connection) (string, error) {
	sec, err := b.proxy.secret(conn)
	if err != nil {
		return "", err
	}
	if sec.OAuth == nil || sec.OAuth.AccessToken == "" {
		return sec.Token, nil // plain bearer
	}
	st := sec.OAuth
	if st.ExpiresAt == 0 || time.Until(time.Unix(st.ExpiresAt, 0)) > 2*time.Minute {
		return st.AccessToken, nil
	}
	if st.RefreshToken == "" {
		return "", errors.New("access token expired and no refresh token; sign in again from the console")
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {st.RefreshToken}, "client_id": {st.ClientID}}
	if st.Resource != "" {
		form.Set("resource", st.Resource)
	}
	if err := tokenRequest(ctx, st, form); err != nil {
		return "", err
	}
	sec.Token = st.AccessToken
	if enc, err := b.sealSecret(sec); err == nil {
		b.store.UpdateConnection(ctx, orgID, conn, enc)
	}
	return st.AccessToken, nil
}

func (b *Bot) oauthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/connections/{id}/oauth/start", b.requirePerm(PermConnManage, func(w http.ResponseWriter, r *http.Request) {
		conn, err := b.store.Connection(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || conn == nil || conn.CredType != "mcp" {
			writeJSON(w, 404, map[string]any{"error": "no such MCP connection"})
			return
		}
		var in struct{ ClientID, ClientSecret, Scopes string }
		decode(r, &in)
		if in.ClientID != "" { // pre-registered client from the Advanced tab
			sec, _ := b.proxy.secret(conn)
			if sec.OAuth == nil {
				sec.OAuth = &OAuthState{}
			}
			sec.OAuth.ClientID, sec.OAuth.ClientSecret, sec.OAuth.Scopes = in.ClientID, in.ClientSecret, in.Scopes
			if enc, err := b.sealSecret(sec); err == nil {
				b.store.UpdateConnection(r.Context(), orgOf(r), conn, enc)
				conn, _ = b.store.Connection(r.Context(), orgOf(r), conn.ID)
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		u, err := b.oauthStart(ctx, orgOf(r), conn, b.baseURL(r)+"/api/oauth/callback")
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "url": u})
	}))
	mux.HandleFunc("GET /api/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, "authorization failed: "+e+" "+r.URL.Query().Get("error_description"), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		conn, err := b.oauthCallback(ctx, r.URL.Query().Get("state"), r.URL.Query().Get("code"), b.baseURL(r)+"/api/oauth/callback")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/admin/bundles/?connected=%d", conn.ID), http.StatusFound)
	})
	mux.HandleFunc("GET /api/connections/{id}/oauth/status", b.requirePerm(PermConnView, func(w http.ResponseWriter, r *http.Request) {
		conn, err := b.store.Connection(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil || conn == nil {
			writeJSON(w, 404, map[string]any{"error": "no such connection"})
			return
		}
		sec, _ := b.proxy.secret(conn)
		out := map[string]any{"signed_in": false}
		if sec != nil && sec.OAuth != nil && sec.OAuth.AccessToken != "" {
			out["signed_in"] = true
			out["expires_at"] = sec.OAuth.ExpiresAt
			out["has_refresh"] = sec.OAuth.RefreshToken != ""
			out["client_id"] = sec.OAuth.ClientID
		}
		writeJSON(w, 200, out)
	}))
}
