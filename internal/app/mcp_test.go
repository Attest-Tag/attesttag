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
	return newMCPHub(NewProxy(sealer, st)), st, seal
}

// TestMCPToolsLoadOnDemand checks that a turn does not carry MCP tool definitions until the
// connection is loaded: the first tool set has use_connection and no <conn>_* tools and opens
// no MCP session. use_connection, a message naming the connection, a thread that already
// called its tools, or the model naming a prefixed tool directly all load them.
func TestMCPToolsLoadOnDemand(t *testing.T) {
	ts := fakeMCPServer(t, "list_documents", "upload_document")
	hub, st, seal := mcpTestHub(t)
	a := &Agent{store: st, tools: map[string]Tool{}, mcp: hub, settings: newSettingsCache(st, Config{}), slacks: testRegistry(&Chat{}), loc: time.UTC}
	conn := &Connection{ID: 7, Name: "Acme MCP", CredType: "mcp", Writes: "auto", AllowedHosts: []string{"example.com"},
		secretEnc: seal(Secret{MCPURL: ts.URL + "/mcp", Token: "tok"})}
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
	conn := &Connection{ID: 7, Name: "Acme MCP", CredType: "mcp", Writes: "auto", AllowedHosts: []string{"example.com"},
		secretEnc: seal(Secret{MCPURL: ts.URL + "/mcp", Token: "tok"})}
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
