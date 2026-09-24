package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPHubTest runs the console's MCP "Test connection" against an in-process MCP server: it
// must use the stored server url and bearer token (or the hub's OAuth token source), summarise
// the tools it finds, leave the hub cache alone, and fail clearly for a url that is not MCP.
func TestMCPHubTest(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
	for _, name := range []string{"list_document_types", "list_documents", "upload_document"} {
		srv.AddTool(&mcp.Tool{Name: name, Description: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
	}
	var gotAuth atomic.Value
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer ts.Close()

	// httptest listens on loopback, which the SSRF guard rejects; bypass it for this test only.
	relaxMCPOutbound(t)

	key := make([]byte, 32)
	rand.Read(key)
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := newMCPHub(NewProxy(sealer, st))
	seal := func(s Secret) []byte {
		raw, _ := json.Marshal(s)
		enc, err := sealer.Seal(raw)
		if err != nil {
			t.Fatal(err)
		}
		return enc
	}
	ctx := context.Background()

	// Unsaved connection (no id yet), plain bearer token.
	c := &Connection{Name: "acme", CredType: "mcp", secretEnc: seal(Secret{MCPURL: ts.URL + "/mcp", Token: "tok-123"})}
	body, err := h.test(ctx, orgID, c)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	for _, want := range []string{"3 tools", "list_document_types", "upload_document"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary %q lacks %q", body, want)
		}
	}
	if a, _ := gotAuth.Load().(string); a != "Bearer tok-123" {
		t.Errorf("server saw Authorization %q, want the stored bearer token", a)
	}
	if len(h.conns) != 0 {
		t.Errorf("test must not populate the hub cache, got %d entries", len(h.conns))
	}

	// OAuth-backed connection: the hub's token source wins over the stored token.
	h.token = func(context.Context, int64, *Connection) (string, error) { return "oauth-456", nil }
	if _, err := h.test(ctx, orgID, c); err != nil {
		t.Fatal(err)
	}
	if a, _ := gotAuth.Load().(string); a != "Bearer oauth-456" {
		t.Errorf("server saw Authorization %q, want the OAuth token", a)
	}

	// A url that is not an MCP endpoint fails with an error, not a fake HTTP status.
	bad := &Connection{Name: "broken", CredType: "mcp", secretEnc: seal(Secret{MCPURL: ts.URL + "/nope", Token: "x"})}
	if _, err := h.test(ctx, orgID, bad); err == nil {
		t.Error("expected an error for a url that is not an MCP endpoint")
	}
	// No url at all.
	_, err = h.test(ctx, orgID, &Connection{Name: "empty", CredType: "mcp", secretEnc: seal(Secret{Token: "x"})})
	if err == nil || !strings.Contains(err.Error(), "no MCP server url") {
		t.Errorf("expected a 'no MCP server url' error, got %v", err)
	}
}

// fakeMCPServer serves an in-process MCP server listing the given tools (each answers "ok").
// The SSRF guard is relaxed for loopback for the test's lifetime.
func fakeMCPServer(t *testing.T, tools ...string) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
	for _, name := range tools {
		srv.AddTool(&mcp.Tool{Name: name, Description: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	// The hub keeps a live session for ten minutes on purpose, so its streaming connection is
	// still open when the test ends, and Close on its own would sit there until the client's
	// own sixty-second timeout kills it. The server is going away regardless: drop the
	// connections under it first.
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	relaxMCPOutbound(t)
	return ts
}

// relaxMCPOutbound lets a test reach an httptest server on loopback for the test's lifetime.
// Both guards have to give: mcpURLCheck refuses the URL, and underneath it the guarded
// transport resolves the host and refuses to dial a non-public address. Relaxing either one
// alone leaves the connection refused by the other.
func relaxMCPOutbound(t *testing.T) {
	t.Helper()
	check, transport := mcpURLCheck, mcpBaseTransport
	mcpURLCheck = func(*url.URL) error { return nil }
	mcpBaseTransport = func() http.RoundTripper { return http.DefaultTransport }
	t.Cleanup(func() { mcpURLCheck, mcpBaseTransport = check, transport })
}

// storedMCPConn writes a connection to the store for one organisation and reads it back, the way a
// turn is handed one. The hub reads a connection's credential from its row on every request, so
// one that exists only in memory has none to send.
func storedMCPConn(t *testing.T, st *Store, org int64, c *Connection) *Connection {
	t.Helper()
	ctx := context.Background()
	id, err := st.InsertConnection(ctx, org, c, c.secretEnc)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.Connection(ctx, org, id)
	if err != nil || stored == nil {
		t.Fatalf("reading back connection %d: %v", id, err)
	}
	return stored
}

// mcpTestHub returns a hub over a fresh store and sealer, plus a function sealing secrets for it.
func mcpTestHub(t *testing.T) (*mcpHub, *Store, func(Secret) []byte) {
	t.Helper()
	key := make([]byte, 32)
	rand.Read(key)
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString(key))
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	seal := func(s Secret) []byte {
		raw, _ := json.Marshal(s)
		enc, err := sealer.Seal(raw)
		if err != nil {
			t.Fatal(err)
		}
		return enc
	}
	h := newMCPHub(NewProxy(sealer, st))
	// The hub keeps its sessions, and one that outlives its test goes on reconnecting its event
	// stream through mcpURLCheck while the next test swaps it. Registered after the server's
	// cleanup, so this runs first, while the server is still there to be told.
	t.Cleanup(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for id, mc := range h.conns {
			mc.session.Close()
			delete(h.conns, id)
		}
	})
	return h, st, seal
}

// TestMCPToolsLoadOnDemand checks that a turn does not carry MCP tool definitions until the
// connection is loaded: the first tool set has use_connection and no <conn>_* tools and opens
// no MCP session. use_connection, a message naming the connection, a thread that already
// called its tools, or the model naming a prefixed tool directly all load them.
func TestMCPToolsLoadOnDemand(t *testing.T) {
	ts := fakeMCPServer(t, "list_documents", "upload_document")
	hub, st, seal := mcpTestHub(t)
	a := &Agent{store: st, tools: map[string]Tool{}, mcp: hub, settings: newSettingsCache(st, Config{}), slacks: testRegistry(&Chat{}), loc: time.UTC}
	conn := storedMCPConn(t, st, 0, &Connection{Name: "Acme MCP", CredType: "mcp", Writes: "auto", AllowedHosts: []string{"example.com"},
		secretEnc: seal(Secret{MCPURL: ts.URL + "/mcp", Token: "tok"})})
	newCall := func(thread, text string) *Call {
		return &Call{Channel: "C1", ThreadTS: thread, Kind: "dm", Text: text, Session: &Session{}, Streamer: &Streamer{failed: true},
			Access: &Access{Rules: []Rule{{Conn: conn}}, ToolPacks: map[string]bool{}}}
	}
	ctx := context.Background()

	// Nothing names the connection: http_request + use_connection only, and no session opened.
	c := newCall("1.0", "hello there")
	if n := len(a.defsFor(ctx, c)); n != 2 {
		t.Fatalf("first-call tools = %d, want 2 (http_request, use_connection)", n)
	}
	if _, ok := c.tools["acme_mcp_list_documents"]; ok {
		t.Fatal("MCP tools were sent before the connection was loaded")
	}
	if len(hub.conns) != 0 {
		t.Fatalf("hub opened %d MCP sessions before any were needed", len(hub.conns))
	}
	prompt := a.systemPrompt(ctx, c)
	if !strings.Contains(prompt, "use_connection") || !strings.Contains(prompt, "Acme MCP") {
		t.Errorf("system prompt should tell the model how to load Acme MCP; got:\n%s", prompt)
	}

	// use_connection (any case, or the slug) loads the tools; the next round's defs carry them.
	out := a.runTool(ctx, c, "use_connection", `{"connection":"acme mcp"}`)
	if !strings.Contains(out, "Loaded 2 tools") || !strings.Contains(out, "acme_mcp_list_documents") {
		t.Fatalf("use_connection result: %s", out)
	}
	if n := len(a.defsFor(ctx, c)); n != 4 {
		t.Fatalf("tools after loading = %d, want 4", n)
	}
	if out := a.runTool(ctx, c, "use_connection", `{"connection":"Acme MCP"}`); !strings.Contains(out, "Already loaded") {
		t.Errorf("second use_connection should say already loaded: %s", out)
	}
	if out := a.runTool(ctx, c, "acme_mcp_list_documents", `{}`); !strings.Contains(out, "ok") {
		t.Errorf("loaded tool did not run: %s", out)
	}
	if out := a.runTool(ctx, c, "use_connection", `{"connection":"nope"}`); !strings.Contains(out, "error") {
		t.Errorf("unknown connection should error: %s", out)
	}
	if strings.Contains(a.systemPrompt(ctx, c), "not listed yet") {
		t.Error("system prompt still says the tools are unloaded after loading")
	}

	// A message naming the connection (a distinctive word of its name is enough) preloads. In a
	// channel with no history, so that what is being read here is the message and not the room:
	// C1 has now called the connection, and ChannelToolNames preloads on that alone.
	c = newCall("1.1", "upload this file to acme please")
	c.Channel = "C_MENTION"
	a.ensureTools(ctx, c)
	if _, ok := c.tools["acme_mcp_upload_document"]; !ok {
		t.Error("a message naming the connection should preload its tools")
	}
	c = newCall("1.2", "what is the mcp server status")
	c.Channel = "C_MENTION"
	a.ensureTools(ctx, c)
	if _, ok := c.tools["acme_mcp_upload_document"]; ok {
		t.Error("generic words like 'mcp' or 'server' must not count as a mention")
	}

	// A thread that already called the connection's tools preloads on its next turn.
	st.LogToolCall(ctx, 1, "", "C1", "2.0", "acme_mcp_list_documents", "{}", "ok", true, 1)
	c = newCall("2.0", "and the next page?")
	a.ensureTools(ctx, c)
	if _, ok := c.tools["acme_mcp_list_documents"]; !ok {
		t.Error("a thread that used the connection should preload its tools")
	}

	// The model names a prefixed tool without loading first: runTool loads and runs it. In a
	// channel of its own, because C1 has now called the connection and ChannelToolNames would
	// preload it — which would leave this asserting nothing.
	c = newCall("3.0", "anything")
	c.Channel = "C_LATE"
	if out := a.runTool(ctx, c, "acme_mcp_list_documents", `{}`); !strings.Contains(out, "ok") {
		t.Errorf("direct call to an unloaded MCP tool should load and run it: %s", out)
	}
	if out := a.runTool(ctx, c, "acme_mcp_nope", `{}`); !strings.Contains(out, "unknown tool") {
		t.Errorf("a tool the server does not list stays unknown: %s", out)
	}

	// Summary-only turns carry no MCP door at all.
	c = newCall("4.0", "acme")
	c.NoTools = true
	if defs := a.defsFor(ctx, c); defs != nil {
		t.Errorf("NoTools turn returned %d tools", len(defs))
	}
	if _, ok := c.tools["use_connection"]; ok {
		t.Error("NoTools turn should not build use_connection")
	}
}

// The first turn in a thread has no history of its own, and every thread has exactly one of
// those. Without the channel's history behind it, a room that calls the same server every day
// pays for the habit twice on each new thread: a round spent on use_connection, and then a round
// at full price, because loading the tools changes the tool array and the array sits in front of
// everything else in the prefix the provider has cached.
func TestNewThreadPreloadsWhatTheChannelKeepsCalling(t *testing.T) {
	ts := fakeMCPServer(t, "list_documents", "upload_document")
	hub, st, seal := mcpTestHub(t)
	a := &Agent{store: st, tools: map[string]Tool{}, mcp: hub, settings: newSettingsCache(st, Config{}), slacks: testRegistry(&Chat{}), loc: time.UTC}
	conn := storedMCPConn(t, st, 0, &Connection{Name: "Acme MCP", CredType: "mcp", Writes: "auto", AllowedHosts: []string{"example.com"},
		secretEnc: seal(Secret{MCPURL: ts.URL + "/mcp", Token: "tok"})})
	newCall := func(channel, thread string) *Call {
		return &Call{Channel: channel, ThreadTS: thread, Kind: "channel", Text: "and then?", Session: &Session{}, Streamer: &Streamer{failed: true},
			Access: &Access{Rules: []Rule{{Conn: conn}}, ToolPacks: map[string]bool{}}}
	}
	loaded := func(c *Call) bool {
		_, ok := c.tools["acme_mcp_list_documents"]
		return ok
	}
	ctx := context.Background()

	// A channel with no history of the connection does not carry its definitions.
	c := newCall("C1", "9.0")
	a.ensureTools(ctx, c)
	if loaded(c) {
		t.Error("a channel that has never called the connection was made to pay for its tools")
	}

	// Some other thread in that channel calls it. A new thread now starts with it in hand.
	st.LogToolCall(ctx, 1, "", "C1", "8.0", "acme_mcp_list_documents", "{}", "ok", true, 1)
	c = newCall("C1", "9.1")
	a.ensureTools(ctx, c)
	if !loaded(c) {
		t.Error("a new thread in a channel that keeps calling the connection did not preload it")
	}

	// A different room is a different habit.
	c = newCall("C2", "9.2")
	a.ensureTools(ctx, c)
	if loaded(c) {
		t.Error("one channel's history preloaded another channel's turn")
	}

	// A thread that has been calling tools answers for itself; the room's average does not
	// override what this conversation has actually been doing.
	st.LogToolCall(ctx, 1, "", "C3", "other", "acme_mcp_list_documents", "{}", "ok", true, 1)
	st.LogToolCall(ctx, 1, "", "C3", "9.3", "http_request", "{}", "ok", true, 1)
	c = newCall("C3", "9.3")
	a.ensureTools(ctx, c)
	if loaded(c) {
		t.Error("a thread with a history of its own was overruled by the channel's")
	}

	// And a habit has to be recent. One call, long enough ago, is not evidence of the next turn.
	if _, err := st.db.ExecContext(ctx, `update tool_calls set created_at=? where channel='C1'`,
		time.Now().Add(-mcpPreloadWindow-24*time.Hour).UTC().Format(time.DateTime)); err != nil {
		t.Fatal(err)
	}
	c = newCall("C1", "9.4")
	a.ensureTools(ctx, c)
	if loaded(c) {
		t.Error("a connection last touched before the window is still being paid for on every turn")
	}
}

// replyMCPServer serves an in-process MCP server whose tools answer with the text the map gives
// them, and keeps the Authorization header of the latest POST, which is where calls travel.
// jsonReplies picks how the server answers one: a single application/json body, or an event
// stream.
func replyMCPServer(t *testing.T, jsonReplies bool, tools map[string]string) (*httptest.Server, *atomic.Value) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
	for name, text := range tools {
		srv.AddTool(&mcp.Tool{Name: name, Description: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
			})
	}
	var auth atomic.Value
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{JSONResponse: jsonReplies})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			auth.Store(r.Header.Get("Authorization"))
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	relaxMCPOutbound(t)
	return ts, &auth
}

// The hub keeps a session for ten minutes, and the token it was dialled on can be replaced in
// less. Every request asks for the token the connection's row holds now, so a kept session
// carries the new one from the next call, still from the copy of the connection a turn was
// handed before it changed.
func TestAnMCPSessionCarriesTheTokenTheConnectionHasNow(t *testing.T) {
	ts, auth := replyMCPServer(t, false, map[string]string{"list_documents": "ok"})
	h, st, seal := mcpTestHub(t)
	ctx := context.Background()
	id, err := st.InsertConnection(ctx, orgID, &Connection{Name: "acme", CredType: "mcp", Status: "active"},
		seal(Secret{MCPURL: ts.URL + "/mcp", Token: "tok-A"}))
	if err != nil {
		t.Fatal(err)
	}
	c := mustConn(t, st, id)
	if _, err := h.call(ctx, orgID, c, "list_documents", nil, ProxyAudit{}); err != nil {
		t.Fatal(err)
	}
	if got := auth.Load(); got != "Bearer tok-A" {
		t.Fatalf("the server saw %q, want the stored token", got)
	}
	kept := h.conns[id]

	// Written to the row the way freshMCPToken writes a renewed token, with nothing told to
	// forget the session.
	if err := st.UpdateConnection(ctx, orgID, mustConn(t, st, id), seal(Secret{MCPURL: ts.URL + "/mcp", Token: "tok-B"})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.call(ctx, orgID, c, "list_documents", nil, ProxyAudit{}); err != nil {
		t.Fatal(err)
	}
	if got := auth.Load(); got != "Bearer tok-B" {
		t.Errorf("the kept session sent %q, want the token the row holds now", got)
	}
	if h.conns[id] != kept {
		t.Error("the session was dialled again, where the kept one should have carried the new token")
	}
}

// The case the ten minutes went wrong in: a session dialled on an OAuth token with minutes left,
// kept past the point freshMCPToken renews it. The request after that point renews it, once, and
// every request after carries the renewed token. Renewing from the copy the session was dialled
// from would have spent the old refresh token again on every request.
func TestAnMCPSessionRenewsItsTokenWhenItComesDue(t *testing.T) {
	ts, auth := replyMCPServer(t, false, map[string]string{"list_documents": "ok"})
	h, st, _ := mcpTestHub(t)
	b := &Bot{store: st, sealer: h.proxy.sealer, proxy: h.proxy}
	h.token = b.freshMCPToken

	refreshed := make(chan string, 8)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		refreshed <- r.Form.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"at-2","refresh_token":"rt-2","expires_in":3600}`))
	}))
	defer idp.Close()
	saved := oauthHTTPClient
	oauthHTTPClient = idp.Client()
	defer func() { oauthHTTPClient = saved }()

	ctx := context.Background()
	sealed := func(expires time.Time) []byte {
		t.Helper()
		enc, err := b.sealSecret(&Secret{MCPURL: ts.URL + "/mcp", OAuth: &OAuthState{AccessToken: "at-1",
			RefreshToken: "rt-1", ExpiresAt: expires.Unix(), ClientID: "cid", TokenURL: idp.URL + "/token"}})
		if err != nil {
			t.Fatal(err)
		}
		return enc
	}
	id, err := st.InsertConnection(ctx, orgID, &Connection{Name: "acme", CredType: "mcp", Status: "active"},
		sealed(time.Now().Add(5*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	c := mustConn(t, st, id)
	call := func() {
		t.Helper()
		if _, err := h.call(ctx, orgID, c, "list_documents", nil, ProxyAudit{}); err != nil {
			t.Fatal(err)
		}
	}
	call()
	if got := auth.Load(); got != "Bearer at-1" {
		t.Fatalf("the server saw %q, want the stored access token", got)
	}
	// Four minutes on, in the row's terms: the same token, now inside its last two.
	if err := st.UpdateConnection(ctx, orgID, mustConn(t, st, id), sealed(time.Now().Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	call()
	call()
	if got := auth.Load(); got != "Bearer at-2" {
		t.Errorf("the server saw %q, want the renewed token", got)
	}
	close(refreshed)
	var spent []string
	for rt := range refreshed {
		spent = append(spent, rt)
	}
	if len(spent) != 1 || spent[0] != "rt-1" {
		t.Errorf("refreshed with %v, want once, with rt-1", spent)
	}
}

// A reply over the cap fails the call that asked for it and nothing more: it is not read into
// memory whole first, and the call after it gets a fresh session rather than the one the SDK
// gave up on. Both ways a server can answer are capped, one JSON body and an event stream.
func TestAnOversizedMCPReplyFailsOnlyItsOwnCall(t *testing.T) {
	huge := strings.Repeat("x", proxyMaxRead+1<<20)
	for _, jsonReplies := range []bool{true, false} {
		t.Run(map[bool]string{true: "json", false: "event stream"}[jsonReplies], func(t *testing.T) {
			ts, _ := replyMCPServer(t, jsonReplies, map[string]string{"dump_everything": huge, "list_documents": "ok"})
			h, st, seal := mcpTestHub(t)
			ctx := context.Background()
			id, err := st.InsertConnection(ctx, orgID, &Connection{Name: "acme", CredType: "mcp", Status: "active"},
				seal(Secret{MCPURL: ts.URL + "/mcp", Token: "t"}))
			if err != nil {
				t.Fatal(err)
			}
			c := mustConn(t, st, id)
			_, err = h.call(ctx, orgID, c, "dump_everything", nil, ProxyAudit{})
			if want := map[bool]string{true: "over 10 MiB", false: "exceeded"}[jsonReplies]; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("the oversized reply got %v, want an error saying it %s", err, want)
			}
			if _, kept := h.conns[id]; kept {
				t.Error("the session the SDK gave up on is still the one cached")
			}
			out, err := h.call(ctx, orgID, c, "list_documents", nil, ProxyAudit{})
			if err != nil || strings.TrimSpace(out) != "ok" {
				t.Fatalf("the call after it got %q, %v", out, err)
			}
			// An error the server answers with is no sign of a broken session, and costs no new one.
			live := h.conns[id]
			if _, err := h.call(ctx, orgID, c, "no_such_tool", nil, ProxyAudit{}); err == nil {
				t.Fatal("a tool the server does not have was answered")
			}
			if h.conns[id] != live {
				t.Error("an error from the server dropped a session that was working")
			}
		})
	}
}
