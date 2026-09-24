package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// The MCP server and its OAuth. The claims these tests hold up: /mcp takes a developer key or a
// connected client's token and nothing else; a tool is its /v1 route, so it can do no more than
// the route and the caller's role allow; a token is only ever minted after a signed-in person
// with the key permission pressed Allow; and every way a token can be copied, replayed or
// outlived ends the connection rather than extending it.

const mcpTestOrigin = "https://console.example.com" // identityBot's ADMIN_BASE_URL

// mcpRPC posts one JSON-RPC request to /mcp the way a streamable-HTTP client does.
func mcpRPC(t *testing.T, mux *http.ServeMux, token, method string, params any) (int, map[string]any, http.Header) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	r := httptest.NewRequest("POST", "/mcp", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Header()
}

func mcpToolNames(t *testing.T, mux *http.ServeMux, token string) []string {
	t.Helper()
	code, out, _ := mcpRPC(t, mux, token, "tools/list", map[string]any{})
	if code != 200 || out["result"] == nil {
		t.Fatalf("tools/list = %d %v", code, out)
	}
	var names []string
	for _, tool := range out["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

// mcpTool calls one tool and returns its text and whether it was an error. A tool the caller was
// not given comes back as a protocol error, which is reported as an error too.
func mcpToolCall(t *testing.T, mux *http.ServeMux, token, name string, args map[string]any) (string, bool) {
	t.Helper()
	code, out, _ := mcpRPC(t, mux, token, "tools/call", map[string]any{"name": name, "arguments": args})
	if code != 200 {
		t.Fatalf("tools/call %s = %d %v", name, code, out)
	}
	if e, ok := out["error"].(map[string]any); ok {
		return e["message"].(string), true
	}
	res := out["result"].(map[string]any)
	var sb strings.Builder
	for _, c := range res["content"].([]any) {
		sb.WriteString(c.(map[string]any)["text"].(string))
	}
	isErr, _ := res["isError"].(bool)
	return sb.String(), isErr
}

func metaJSON(t *testing.T, mux *http.ServeMux, path string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func postForm(t *testing.T, mux *http.ServeMux, path string, form url.Values) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// narrowMember adds somebody to the organisation on a role holding only the given permissions,
// and returns a console session for them.
func narrowMember(t *testing.T, st *Store, orgID int64, email string, perms ...string) string {
	t.Helper()
	ctx := context.Background()
	key := strings.ReplaceAll(strings.Split(email, "@")[0], ".", "")
	if err := st.UpsertConsoleRole(ctx, orgID, &ConsoleRole{Key: key, Label: key, Permissions: perms}); err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(ctx, email, key, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMembership(ctx, u.ID, orgID, key, 0); err != nil {
		t.Fatal(err)
	}
	tok, err := st.CreateAdminSession(ctx, AdminUser{ID: u.ID, Email: u.Email, OrgID: orgID, Via: "password"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// ---- discovery ----

func TestMCPWithoutACredentialSaysWhereToSignIn(t *testing.T) {
	_, mux, _ := docsBot(t)

	code, _, hdr := mcpRPC(t, mux, "", "tools/list", map[string]any{})
	if code != 401 {
		t.Fatalf("no credential = %d, want 401", code)
	}
	if got := hdr.Get("WWW-Authenticate"); !strings.Contains(got, `resource_metadata="`+mcpTestOrigin+`/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("WWW-Authenticate = %q, want it to name the resource metadata", got)
	}
	// A console session is not a way in, and neither is anything that merely looks like a token.
	for _, tok := range []string{"not-a-token", mcpAccessPrefix + "made-up", mcpRefreshPrefix + "made-up"} {
		if code, _, hdr := mcpRPC(t, mux, tok, "tools/list", map[string]any{}); code != 401 || !strings.Contains(hdr.Get("WWW-Authenticate"), `error="invalid_token"`) {
			t.Errorf("token %q = %d %q, want 401 invalid_token", tok, code, hdr.Get("WWW-Authenticate"))
		}
	}

	_, prm := metaJSON(t, mux, "/.well-known/oauth-protected-resource/mcp")
	if prm["resource"] != mcpTestOrigin+"/mcp" || prm["authorization_servers"].([]any)[0] != mcpTestOrigin {
		t.Errorf("resource metadata at the endpoint's path = %v", prm)
	}
	_, root := metaJSON(t, mux, "/.well-known/oauth-protected-resource")
	if root["resource"] != mcpTestOrigin {
		t.Errorf("resource metadata at the root names %v, want the origin", root["resource"])
	}
	_, asm := metaJSON(t, mux, "/.well-known/oauth-authorization-server")
	if asm["issuer"] != mcpTestOrigin || asm["token_endpoint"] != mcpTestOrigin+"/oauth/token" ||
		asm["registration_endpoint"] != mcpTestOrigin+"/oauth/register" ||
		!slices.Equal(anyStrings(asm["code_challenge_methods_supported"]), []string{"S256"}) ||
		asm["authorization_response_iss_parameter_supported"] != true {
		t.Errorf("authorization server metadata = %v", asm)
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

// ---- tools, through a developer key ----

func TestMCPToolsAreTheCallersRoutes(t *testing.T) {
	b, mux, st := docsBot(t)
	_, orgID, session := signedUp(t, b, mux, st, "founder@example.com")
	key, _ := mintKey(t, mux, session, "claude code")

	names := mcpToolNames(t, mux, key)
	for _, want := range []string{"whoami", "list_documents", "write_document", "delete_memory", "list_activity", "list_audit", "get_usage"} {
		if !slices.Contains(names, want) {
			t.Errorf("the admin's tools lack %s: %v", want, names)
		}
	}
	// A tool is its route, and a route this deployment does not register brings no tool.
	for _, tool := range mcpTools {
		if slices.Contains(names, tool.name) != mcpRouteExists(mux, tool) {
			t.Errorf("%s is offered=%v but its route %q exists=%v", tool.name, slices.Contains(names, tool.name), tool.pattern, mcpRouteExists(mux, tool))
		}
	}

	text, isErr := mcpToolCall(t, mux, key, "whoami", nil)
	if isErr || !strings.Contains(text, "founder@example.com") {
		t.Fatalf("whoami = %q (error %v)", text, isErr)
	}

	// A write through a tool is the route's write: stored, readable back, and in the audit log as
	// the person, through MCP, with the key it came in on.
	if text, isErr := mcpToolCall(t, mux, key, "write_document", map[string]any{"path": "runbooks/deploys.md", "content": "# Deploys\n\nFrom main."}); isErr {
		t.Fatalf("write_document = %q", text)
	}
	if text, isErr := mcpToolCall(t, mux, key, "read_document", map[string]any{"path": "runbooks/deploys.md"}); isErr || !strings.Contains(text, "From main.") {
		t.Fatalf("read_document = %q (error %v)", text, isErr)
	}
	events, _ := st.AuditEvents(context.Background(), orgID, AuditFilter{Limit: 20})
	var found bool
	for _, e := range events {
		if e.Via == viaMCP && strings.Contains(e.TargetID, "/v1/documents/runbooks/deploys.md") && e.ActorEmail == "founder@example.com" {
			found = strings.Contains(string(e.Details), "api_key_id")
		}
	}
	if !found {
		t.Errorf("no audit row for the write via mcp with its key: %+v", events)
	}

	// A path that tries to leave the documents is refused before it is a request at all.
	if text, isErr := mcpToolCall(t, mux, key, "read_document", map[string]any{"path": "../../api/api-keys"}); !isErr {
		t.Errorf("a climbing path was read: %q", text)
	}
	// Arguments of the wrong type are the caller's mistake, said plainly.
	if text, isErr := mcpToolCall(t, mux, key, "get_artifact", map[string]any{"id": "seven"}); !isErr || !strings.Contains(text, "whole number") {
		t.Errorf("a non-numeric id = %q (error %v)", text, isErr)
	}
}

func TestMCPOffersARoleOnlyWhatItHolds(t *testing.T) {
	b, mux, st := docsBot(t)
	_, orgID, _ := signedUp(t, b, mux, st, "founder@example.com")
	narrow := narrowMember(t, st, orgID, "integrator@example.com", PermAPIKeysManage)
	key, _ := mintKey(t, mux, narrow, "narrow")

	names := mcpToolNames(t, mux, key)
	for _, never := range []string{"write_document", "delete_document", "add_memory", "run_routine", "list_activity", "list_audit", "list_jobs"} {
		if slices.Contains(names, never) {
			t.Errorf("a role without %s's permission was offered it", never)
		}
	}
	if !slices.Contains(names, "list_documents") || !slices.Contains(names, "whoami") {
		t.Errorf("the reads any member may make are missing: %v", names)
	}
	// Asking for a tool it was not offered does not reach the route.
	if _, isErr := mcpToolCall(t, mux, key, "write_document", map[string]any{"path": "planted.md", "content": "x"}); !isErr {
		t.Error("a tool that was not offered ran anyway")
	}
	if _, ok := storedDoc(t, b, orgID, "planted.md"); ok {
		t.Error("the refused write was stored")
	}
}

// ---- OAuth, by hand ----

type oauthRig struct {
	t        *testing.T
	mux      *http.ServeMux
	clientID string
	redirect string
}

func registerMCPClient(t *testing.T, mux *http.ServeMux, redirects ...string) string {
	t.Helper()
	code, out := authReq(t, mux, "POST", "/oauth/register", map[string]any{"client_name": "Test client", "redirect_uris": redirects}, "")
	if code != 201 {
		t.Fatalf("register = %d %v", code, out)
	}
	return out["client_id"].(string)
}

// authorize walks one authorization request through the consent page as the person holding
// session, and returns the redirect it ends in.
func (o *oauthRig) authorize(session, challenge string, allow bool) (string, int, map[string]any) {
	o.t.Helper()
	q := url.Values{"response_type": {"code"}, "client_id": {o.clientID}, "redirect_uri": {o.redirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"st8"},
		"resource": {mcpTestOrigin + "/mcp"}, "scope": {"mcp"}}
	w := httptest.NewRecorder()
	o.mux.ServeHTTP(w, httptest.NewRequest("GET", "/oauth/authorize?"+q.Encode(), nil))
	loc := w.Header().Get("Location")
	if w.Code != 302 || !strings.HasPrefix(loc, mcpConsentPage+"?") {
		o.t.Fatalf("authorize = %d to %q, want the consent page", w.Code, loc)
	}
	query := strings.TrimPrefix(loc, mcpConsentPage+"?")
	code, got := authReq(o.t, o.mux, "GET", "/api/oauth/consent?"+query, nil, session)
	if code != 200 {
		return "", code, got
	}
	code, out := authReq(o.t, o.mux, "POST", "/api/oauth/consent", map[string]any{"query": query, "allow": allow}, session)
	redirect, _ := out["redirect"].(string)
	return redirect, code, got
}

func (o *oauthRig) exchange(code, verifier string) (int, map[string]any) {
	return postForm(o.t, o.mux, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {o.redirect}, "client_id": {o.clientID}, "code_verifier": {verifier}, "resource": {mcpTestOrigin + "/mcp"}})
}

func (o *oauthRig) refresh(tok string) (int, map[string]any) {
	return postForm(o.t, o.mux, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok}, "client_id": {o.clientID}})
}

func codeFrom(t *testing.T, redirect string) string {
	t.Helper()
	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("state") != "st8" || u.Query().Get("iss") != mcpTestOrigin {
		t.Fatalf("redirect %q lost the state or the issuer", redirect)
	}
	return u.Query().Get("code")
}

func TestMCPOAuthConnectsRefreshesAndEnds(t *testing.T) {
	b, mux, st := docsBot(t)
	_, orgID, session := signedUp(t, b, mux, st, "founder@example.com")
	o := &oauthRig{t: t, mux: mux, redirect: "http://127.0.0.1:43117/callback"}
	// Registered on one port, sent back on another: a command-line client listens wherever it can.
	o.clientID = registerMCPClient(t, mux, "http://127.0.0.1:33418/callback")

	verifier, challenge := pkcePair()
	redirect, code, consent := o.authorize(session, challenge, true)
	if code != 200 || consent["client_name"] != "Test client" || consent["returns_to"] != "127.0.0.1:43117" || consent["refusal"] != "" {
		t.Fatalf("consent = %d %v", code, consent)
	}
	authCode := codeFrom(t, redirect)

	// The code alone is not enough: the wrong verifier, or another client, gets nothing.
	if code, out := o.exchange(authCode, verifier+"x"); code != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("a wrong verifier = %d %v", code, out)
	}
	// ...and spending it wrongly spent it: the right verifier afterwards is a second use.
	verifier, challenge = pkcePair()
	redirect, _, _ = o.authorize(session, challenge, true)
	authCode = codeFrom(t, redirect)
	code, tokens := o.exchange(authCode, verifier)
	if code != 200 || tokens["token_type"] != "Bearer" || tokens["expires_in"] != 3600.0 {
		t.Fatalf("exchange = %d %v", code, tokens)
	}
	access, refresh := tokens["access_token"].(string), tokens["refresh_token"].(string)
	if !strings.HasPrefix(access, mcpAccessPrefix) || !strings.HasPrefix(refresh, mcpRefreshPrefix) {
		t.Fatalf("tokens = %v", tokens)
	}

	// The token is the person, through /mcp and nowhere else.
	if text, isErr := mcpToolCall(t, mux, access, "whoami", nil); isErr || !strings.Contains(text, "founder@example.com") {
		t.Fatalf("whoami with the token = %q (error %v)", text, isErr)
	}
	if code, _ := authReq(t, mux, "GET", "/v1/whoami", nil, access); code != 401 {
		t.Errorf("the MCP token opened /v1: %d", code)
	}
	if code, _ := authReq(t, mux, "GET", "/api/api-keys", nil, access); code != 401 {
		t.Errorf("the MCP token opened the console: %d", code)
	}

	// Refreshing replaces both tokens; the old access token stops at once.
	code, again := o.refresh(refresh)
	if code != 200 {
		t.Fatalf("refresh = %d %v", code, again)
	}
	if code, _, _ := mcpRPC(t, mux, access, "tools/list", map[string]any{}); code != 401 {
		t.Errorf("the replaced access token still works: %d", code)
	}
	newAccess := again["access_token"].(string)
	if _, isErr := mcpToolCall(t, mux, newAccess, "whoami", nil); isErr {
		t.Fatal("the refreshed token does not work")
	}

	// The console lists the connection as the person's, returning to where it said it would.
	code, page := authReq(t, mux, "GET", "/api/mcp", nil, session)
	grants, _ := page["grants"].([]any)
	if code != 200 || len(grants) != 1 || grants[0].(map[string]any)["returns_to"] != "127.0.0.1:43117" ||
		grants[0].(map[string]any)["owner"] != "founder@example.com" || page["url"] != mcpTestOrigin+"/mcp" {
		t.Fatalf("GET /api/mcp = %d %v", code, page)
	}

	// Presenting the spent refresh token again means somebody else holds it: the connection ends.
	if code, out := o.refresh(refresh); code != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("a replayed refresh token = %d %v", code, out)
	}
	if code, _, _ := mcpRPC(t, mux, newAccess, "tools/list", map[string]any{}); code != 401 {
		t.Errorf("the connection survived a replayed refresh token: %d", code)
	}
	events, _ := st.AuditEvents(context.Background(), orgID, AuditFilter{Action: "mcp.", Limit: 20})
	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action)
	}
	if !slices.Contains(actions, "mcp.approved") || !slices.Contains(actions, "mcp.replay_ended") {
		t.Errorf("audit actions = %v, want the approval and the replay", actions)
	}
}

func TestMCPAuthorizationCodeIsSingleUse(t *testing.T) {
	b, mux, st := docsBot(t)
	_, _, session := signedUp(t, b, mux, st, "founder@example.com")
	o := &oauthRig{t: t, mux: mux, redirect: "https://claude.ai/api/mcp/auth_callback"}
	o.clientID = registerMCPClient(t, mux, o.redirect)

	verifier, challenge := pkcePair()
	redirect, _, _ := o.authorize(session, challenge, true)
	authCode := codeFrom(t, redirect)
	code, tokens := o.exchange(authCode, verifier)
	if code != 200 {
		t.Fatalf("exchange = %d %v", code, tokens)
	}
	// A second exchange of the same code is a copy of it being used: refused, and the tokens the
	// first exchange made are ended with it (RFC 6749 §4.1.2).
	if code, out := o.exchange(authCode, verifier); code != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("a second exchange = %d %v", code, out)
	}
	if code, _, _ := mcpRPC(t, mux, tokens["access_token"].(string), "tools/list", map[string]any{}); code != 401 {
		t.Errorf("tokens from a code used twice still work: %d", code)
	}
	// Another client cannot spend somebody else's code either.
	other := registerMCPClient(t, mux, o.redirect)
	verifier, challenge = pkcePair()
	redirect, _, _ = o.authorize(session, challenge, true)
	if code, out := postForm(t, mux, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {codeFrom(t, redirect)},
		"redirect_uri": {o.redirect}, "client_id": {other}, "code_verifier": {verifier}}); code != 400 || out["error"] != "invalid_grant" {
		t.Errorf("another client's exchange = %d %v", code, out)
	}
}

func TestMCPConsentTakesWhatAKeyTakes(t *testing.T) {
	b, mux, st := docsBot(t)
	_, orgID, _ := signedUp(t, b, mux, st, "founder@example.com")
	viewer := narrowMember(t, st, orgID, "viewer@example.com", PermActivityView)
	o := &oauthRig{t: t, mux: mux, redirect: "https://claude.ai/api/mcp/auth_callback"}
	o.clientID = registerMCPClient(t, mux, o.redirect)

	_, challenge := pkcePair()
	redirect, code, consent := o.authorize(viewer, challenge, true)
	if consent["refusal"] == "" || code != 403 || redirect != "" {
		t.Fatalf("a role without the key permission: consent %v, allow = %d %q", consent, code, redirect)
	}
	// Cancelling needs no permission, and tells the client so.
	redirect, code, _ = o.authorize(viewer, challenge, false)
	if u, _ := url.Parse(redirect); code != 200 || u.Query().Get("error") != "access_denied" || u.Query().Get("state") != "st8" {
		t.Fatalf("cancel = %d %q", code, redirect)
	}
	// Without a console session there is no consent at all.
	if code, _ := authReq(t, mux, "POST", "/api/oauth/consent", map[string]any{"query": "client_id=" + o.clientID, "allow": true}, ""); code != 401 {
		t.Errorf("consent without a session = %d", code)
	}
}

func TestMCPAuthorizeSendsErrorsOnlyWhereItCanTrust(t *testing.T) {
	_, mux, _ := docsBot(t)
	good := "https://claude.ai/api/mcp/auth_callback"
	clientID := registerMCPClient(t, mux, good)
	_, challenge := pkcePair()
	base := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {good},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"s"}}
	try := func(change func(url.Values)) string {
		q := url.Values{}
		for k, v := range base {
			q[k] = slices.Clone(v)
		}
		change(q)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/oauth/authorize?"+q.Encode(), nil))
		return w.Header().Get("Location")
	}
	cases := []struct {
		name   string
		change func(url.Values)
		// toClient is whether the error goes back to the client's redirect address, which it
		// may only when that address is one the client registered.
		toClient bool
		code     string
	}{
		{"an unknown client", func(q url.Values) { q.Set("client_id", "mcpc_nobody") }, false, "invalid_client"},
		{"an unregistered redirect", func(q url.Values) { q.Set("redirect_uri", "https://evil.example/cb") }, false, "invalid_request"},
		{"no PKCE", func(q url.Values) { q.Del("code_challenge") }, true, "invalid_request"},
		{"plain PKCE", func(q url.Values) { q.Set("code_challenge_method", "plain") }, true, "invalid_request"},
		{"a token response", func(q url.Values) { q.Set("response_type", "token") }, true, "unsupported_response_type"},
		{"another server's resource", func(q url.Values) { q.Set("resource", "https://other.example/mcp") }, true, "invalid_target"},
	}
	for _, c := range cases {
		loc := try(c.change)
		u, _ := url.Parse(loc)
		toClient := strings.HasPrefix(loc, good)
		if toClient != c.toClient || u.Query().Get("error") != c.code {
			t.Errorf("%s: redirected to %q, want error %s sent to the client=%v", c.name, loc, c.code, c.toClient)
		}
		if toClient && (u.Query().Get("state") != "s" || u.Query().Get("iss") != mcpTestOrigin) {
			t.Errorf("%s: %q lost the state or the issuer", c.name, loc)
		}
	}
}

func TestMCPRegistrationRefusesAddressesThatCouldLeakTheCode(t *testing.T) {
	_, mux, _ := docsBot(t)
	for _, bad := range []string{"javascript:alert(1)", "data:text/html,x", "http://evil.example/cb",
		"file:///etc/passwd", "https://claude.ai/cb#frag", "https://user:pass@claude.ai/cb", "not a url"} {
		if code, out := authReq(t, mux, "POST", "/oauth/register", map[string]any{"redirect_uris": []string{bad}}, ""); code != 400 || out["error"] != "invalid_redirect_uri" {
			t.Errorf("registering %q = %d %v", bad, code, out)
		}
	}
	for _, ok := range []string{"https://claude.ai/api/mcp/auth_callback", "http://localhost:6274/oauth/callback",
		"http://127.0.0.1:33418/callback", "cursor://anysphere.cursor-retrieval/oauth/callback"} {
		if code, out := authReq(t, mux, "POST", "/oauth/register", map[string]any{"redirect_uris": []string{ok}}, ""); code != 201 {
			t.Errorf("registering %q = %d %v", ok, code, out)
		}
	}
	// A client that asks to be confidential gets a secret, and then has to use it.
	code, out := authReq(t, mux, "POST", "/oauth/register", map[string]any{"redirect_uris": []string{"https://app.example/cb"},
		"token_endpoint_auth_method": "client_secret_post"}, "")
	if code != 201 || !strings.HasPrefix(out["client_secret"].(string), mcpSecretPrefix) {
		t.Fatalf("confidential registration = %d %v", code, out)
	}
	if code, res := postForm(t, mux, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"},
		"client_id": {out["client_id"].(string)}}); code != 401 || res["error"] != "invalid_client" {
		t.Errorf("a confidential client without its secret = %d %v", code, res)
	}
}

func TestMCPConnectionEndsWhenRevokedOrWhenThePersonLeaves(t *testing.T) {
	b, mux, st := docsBot(t)
	ctx := context.Background()
	u, orgID, session := signedUp(t, b, mux, st, "founder@example.com")
	o := &oauthRig{t: t, mux: mux, redirect: "https://claude.ai/api/mcp/auth_callback"}
	o.clientID = registerMCPClient(t, mux, o.redirect)
	connect := func() string {
		verifier, challenge := pkcePair()
		redirect, _, _ := o.authorize(session, challenge, true)
		code, tokens := o.exchange(codeFrom(t, redirect), verifier)
		if code != 200 {
			t.Fatalf("exchange = %d %v", code, tokens)
		}
		return tokens["access_token"].(string)
	}

	access := connect()
	_, page := authReq(t, mux, "GET", "/api/mcp", nil, session)
	id := int64(page["grants"].([]any)[0].(map[string]any)["id"].(float64))
	if code, _ := authReq(t, mux, "DELETE", "/api/mcp/grants/"+itoa(id), nil, session); code != 200 {
		t.Fatalf("revoke = %d", code)
	}
	if code, _, _ := mcpRPC(t, mux, access, "tools/list", map[string]any{}); code != 401 {
		t.Errorf("a revoked connection still works: %d", code)
	}

	// Connecting again replaces rather than accumulates, and the account's lock-out ends it too:
	// a password reset takes connected clients with it as it takes keys.
	access = connect()
	if err := st.RevokeAPIKeysForUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := mcpRPC(t, mux, access, "tools/list", map[string]any{}); code != 401 {
		t.Errorf("a connection outlived the account's lock-out: %d", code)
	}
	grants, _ := st.MCPGrants(ctx, orgID)
	for _, g := range grants {
		if g.RevokedAt == "" {
			t.Errorf("grant %d is still live", g.ID)
		}
	}
}

// ---- the official client, end to end ----

// The Go SDK's own client connects the way Claude, Cursor and VS Code do: it reads the 401,
// discovers the metadata, registers itself, sends the person to authorize, exchanges the code with
// PKCE and the resource, checks the issuer, and retries with the token. If any piece here drifted
// from the spec, this is where it would fail.
func TestMCPTheSDKClientConnectsWithOAuth(t *testing.T) {
	b, mux, st := docsBot(t)
	_, _, session := signedUp(t, b, mux, st, "founder@example.com")
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("ADMIN_BASE_URL", srv.URL)

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	fetch := func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		// What a browser would do: open the authorization URL, land on the consent page, press Allow.
		resp, err := noFollow.Get(args.URL)
		if err != nil {
			return nil, err
		}
		resp.Body.Close()
		query := strings.TrimPrefix(resp.Header.Get("Location"), mcpConsentPage+"?")
		code, out := authReq(t, mux, "POST", "/api/oauth/consent", map[string]any{"query": query, "allow": true}, session)
		if code != 200 {
			t.Fatalf("consent = %d %v", code, out)
		}
		u, err := url.Parse(out["redirect"].(string))
		if err != nil {
			return nil, err
		}
		return &auth.AuthorizationResult{Code: u.Query().Get("code"), State: u.Query().Get("state"), Iss: u.Query().Get("iss")}, nil
	}
	redirect := "http://127.0.0.1:5173/callback"
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{RedirectURIs: []string{redirect}, ClientName: "SDK test"}},
		RedirectURL:              redirect,
		AuthorizationCodeFetcher: fetch,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "sdk-test", Version: "1"}, nil)
	ctx := context.Background()
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", OAuthHandler: handler}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools.Tools) < 10 {
		t.Fatalf("only %d tools", len(tools.Tools))
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "whoami"})
	if err != nil || res.IsError {
		t.Fatalf("whoami: %v %+v", err, res)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "founder@example.com") {
		t.Fatalf("whoami = %s", text)
	}
}
