package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Remote MCP servers are connections of cred_type "mcp". Their tools are listed once per
// connection (cached 10 min) and offered to the model with a prefix derived from the
// connection name, but only once a turn loads the connection (use_connection, a message that
// names it, or a thread that already used it): a server can list dozens of tools, and sending
// them on every call was most of a turn's input tokens. The bearer token stays in this
// process: the transport adds it.

type mcpConn struct {
	session *mcp.ClientSession
	tools   []*mcp.Tool
	fetched time.Time
}

type mcpHub struct {
	proxy *Proxy
	mu    sync.Mutex
	conns map[int64]*mcpConn
	token func(ctx context.Context, orgID int64, c *Connection) (string, error) // OAuth-aware token source
}

func newMCPHub(p *Proxy) *mcpHub { return &mcpHub{proxy: p, conns: map[int64]*mcpConn{}} }

// forget drops a cached session (after re-auth) so the next call reconnects.
func (h *mcpHub) forget(id int64) {
	h.mu.Lock()
	if mc, ok := h.conns[id]; ok && mc.session != nil {
		mc.session.Close()
	}
	delete(h.conns, id)
	h.mu.Unlock()
}

// mcpURLCheck is the SSRF guard applied to every MCP request; tests swap it to reach a loopback server.
var mcpURLCheck = checkURL

// mcpBaseTransport is what carries the request once mcpURLCheck has passed it: the guarded
// transport, which resolves a host once and refuses to dial anything that is not a public
// address. It is a variable for the same reason mcpURLCheck is, and the two only work as a
// pair — a test that relaxes the check alone still ends at a dialler that will not connect to
// the loopback address its server is listening on.
var mcpBaseTransport = publicTransport

type bearerTransport struct {
	token  string
	origin *url.URL
	base   http.RoundTripper
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := mcpURLCheck(r.URL); err != nil {
		return nil, err
	}
	if t.token != "" {
		if !sameOrigin(t.origin, r.URL) {
			return nil, fmt.Errorf("MCP credential origin mismatch")
		}
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(r)
}

// endpoint resolves a connection's MCP server url and the token to send: the stored bearer
// token, or a fresh one from the hub's OAuth-aware token source when it has one.
func (h *mcpHub) endpoint(ctx context.Context, orgID int64, c *Connection) (endpoint, token string, err error) {
	s, err := h.proxy.secret(c)
	if err != nil {
		return "", "", err
	}
	if s.MCPURL == "" {
		return "", "", fmt.Errorf("connection %s has no MCP server url", c.Name)
	}
	token = s.Token
	if h.token != nil {
		if t, err := h.token(ctx, orgID, c); err != nil {
			return "", "", fmt.Errorf("mcp auth %s: %w", c.Name, err)
		} else if t != "" {
			token = t
		}
	}
	return s.MCPURL, token, nil
}

// mcpDial opens a streamable-HTTP session (initialize) and lists the server's tools.
// The caller owns the returned session.
func mcpDial(ctx context.Context, endpoint, token string) (*mcp.ClientSession, []*mcp.Tool, error) {
	origin, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "attesttag", Version: "0.2"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: endpoint,
		HTTPClient: &http.Client{Timeout: 60 * time.Second, CheckRedirect: rejectRedirect, Transport: &bearerTransport{token: token, origin: origin, base: mcpBaseTransport()}}}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sess, err := client.Connect(cctx, transport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	res, err := sess.ListTools(cctx, &mcp.ListToolsParams{})
	if err != nil {
		sess.Close()
		return nil, nil, fmt.Errorf("list tools: %w", err)
	}
	return sess, res.Tools, nil
}

// connect returns a live MCP session for one connection, dialling if the cache is cold.
//
// The lock is held to read the cache and to install the result, never across the dial. The dial
// has a 30-second timeout and reaches a server this tenant chose, so holding one process-wide
// mutex across it meant a single slow or hostile MCP endpoint stalled every tenant's turns — one
// customer's misconfigured server was a deployment-wide outage.
func (h *mcpHub) connect(ctx context.Context, orgID int64, c *Connection) (*mcpConn, error) {
	h.mu.Lock()
	if mc, ok := h.conns[c.ID]; ok && time.Since(mc.fetched) < 10*time.Minute {
		h.mu.Unlock()
		return mc, nil
	}
	h.mu.Unlock()

	endpoint, token, err := h.endpoint(ctx, orgID, c)
	if err != nil {
		return nil, err
	}
	sess, tools, err := mcpDial(ctx, endpoint, token)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", c.Name, err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	// Another goroutine may have dialled the same connection while this one was waiting. Keep
	// the fresh entry that is already installed and close the duplicate, rather than leaking a
	// session by overwriting it.
	if mc, ok := h.conns[c.ID]; ok && time.Since(mc.fetched) < 10*time.Minute {
		sess.Close()
		return mc, nil
	}
	if old, ok := h.conns[c.ID]; ok && old.session != nil {
		old.session.Close()
	}
	mc := &mcpConn{session: sess, tools: tools, fetched: time.Now()}
	h.conns[c.ID] = mc
	slog.Info("mcp connected", "connection", c.Name, "tools", len(tools))
	return mc, nil
}

// test backs the console's "Test connection" for MCP connections: one fresh session against
// the stored server url (initialize + tools/list), closed straight after. It bypasses the hub
// cache on purpose: the pre-save test runs on a connection that has no id yet. Returns a
// one-line summary naming the server and the first few tools.
func (h *mcpHub) test(ctx context.Context, orgID int64, c *Connection) (string, error) {
	endpoint, token, err := h.endpoint(ctx, orgID, c)
	if err != nil {
		return "", err
	}
	host := endpoint
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		host = u.Host
	}
	start := time.Now()
	sess, tools, err := mcpDial(ctx, endpoint, token)
	audit := ProxyAudit{Requester: "console-test", ConnectionID: c.ID, Method: "MCP", Host: c.Name, Path: "tools/list",
		Status: 200, MS: time.Since(start).Milliseconds()}
	if err != nil {
		audit.Status = 500
		h.proxy.store.LogProxy(ctx, orgID, audit)
		return "", fmt.Errorf("MCP server %s: %w", host, err)
	}
	defer sess.Close()
	h.proxy.store.LogProxy(ctx, orgID, audit)
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	summary := fmt.Sprintf("Connected to %s: %d tools", host, len(tools))
	const show = 8
	switch {
	case len(names) == 0:
		return summary + " (the server lists none)", nil
	case len(names) > show:
		return fmt.Sprintf("%s: %s, … and %d more", summary, strings.Join(names[:show], ", "), len(names)-show), nil
	}
	return summary + ": " + strings.Join(names, ", "), nil
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// tools returns Tool wrappers for an MCP connection, prefixed "<conn>_".
func (h *mcpHub) tools(ctx context.Context, orgID int64, a *Agent, c *Connection) ([]Tool, error) {
	mc, err := h.connect(ctx, orgID, c)
	if err != nil {
		return nil, err
	}
	prefix := slug(c.Name) + "_"
	var out []Tool
	for _, t := range mc.tools {
		t := t
		params := map[string]any{"type": "object", "properties": map[string]any{}}
		if raw, err := json.Marshal(t.InputSchema); err == nil {
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil && m["type"] != nil {
				params = m
			}
		}
		desc := truncate(t.Description, 300)
		gate := mcpNeedsConfirm(c, t) // fixed for this tool: it depends on the server's hint, not the arguments
		if gate {
			desc += " (May change data; someone in the thread approves it before it runs.)"
		}
		out = append(out, Tool{Name: prefix + slug(t.Name), Desc: desc, Params: params,
			Run: func(ctx context.Context, call *Call, args json.RawMessage) (string, error) {
				var m map[string]any
				json.Unmarshal(args, &m)
				// A preview holds anything that may change data before the checks below can let
				// it run — a routine's pre-approval, an allow rule — and outside the gate
				// altogether, because on a connection whose writes are automatic the gate is shut
				// for every tool and the write would simply have run.
				if call.Preview && (gate || !mcpReadOnly(t)) {
					return call.previewHold(mcpConfirmSummary(c, t.Name, m), previewInChannel(c)), nil
				}
				// MCP tools can mutate; reads go straight through, writes wait for a human. A write
				// that did run says where what it made can be opened, the same as an http_request
				// write: an id in a tool result is not something anyone in the thread can click.
				if gate {
					// A routine set up to act was told this once, when it was written; asking
					// again on every run asks nobody. Same gate as an http_request write.
					if call.autoConfirm {
						out, err := h.call(ctx, orgID, c, t.Name, m, callAudit(call))
						if err != nil {
							return "", err
						}
						call.writesRun++
						return out + viewLine(out) + "\n(ran without asking: this routine is set to confirm its own writes)", nil
					}
					// The second description is what a turn a forwarded email started is judged
					// on: the tool and the connection without the arguments, which on that lane
					// are composed from a stranger's mail.
					if rule, ok := a.allowedByRule(ctx, call, describeMCPWrite(c, t.Name, m), describeMCPWriteDestination(c, t.Name)); ok {
						out, err := h.call(ctx, orgID, c, t.Name, m, callAudit(call))
						if err != nil {
							return "", err
						}
						call.writesRun++
						return out + viewLine(out) + fmt.Sprintf("\n(ran without asking: pre-approved by the allow rule %q)", rule), nil
					}
					if call.Silent {
						return call.silentRefusal("Tool " + t.Name + " on " + c.Name), nil
					}
					raw, _ := json.Marshal(map[string]any{"mcp": c.ID, "tool": t.Name, "args": m})
					id, err := a.store.AddPendingWrite(ctx, call.OrgID, call.TeamID, call.Channel, call.ThreadTS, call.UserID, string(raw))
					if err != nil {
						return "", err
					}
					call.holdForConfirm(id, mcpConfirmSummary(c, t.Name, m))
					return fmt.Sprintf("Tool %s on %s may change data and needs a human OK. Tell the user plainly what it would do; Confirm and Cancel buttons are posted under your reply and the request expires in 5 minutes.", t.Name, c.Name), nil
				}
				return h.call(ctx, orgID, c, t.Name, m, callAudit(call))
			}})
	}
	return out, nil
}

// ---- on-demand loading: the agent side ----

// mcpConns lists the MCP connections a call may use, in rule order, once each.
func (c *Call) mcpConns() []*Connection {
	if c.Access == nil {
		return nil
	}
	seen := map[int64]bool{}
	var out []*Connection
	for _, r := range c.Access.Rules {
		if r.Conn.CredType == "mcp" && !seen[r.Conn.ID] {
			seen[r.Conn.ID] = true
			out = append(out, r.Conn)
		}
	}
	return out
}

// mcpConnNamed resolves what the model passed to use_connection: the name, any case, or its slug.
func (c *Call) mcpConnNamed(name string) *Connection {
	want := slug(name)
	for _, conn := range c.mcpConns() {
		if strings.EqualFold(conn.Name, name) || slug(conn.Name) == want {
			return conn
		}
	}
	return nil
}

// mcpConnForTool maps a prefixed tool name back to its connection.
func (c *Call) mcpConnForTool(name string) *Connection {
	for _, conn := range c.mcpConns() {
		if strings.HasPrefix(name, slug(conn.Name)+"_") {
			return conn
		}
	}
	return nil
}

// mentionsConn reports whether a message names a connection (full name, slug, or a distinctive
// word of the name), which lets the turn load its tools before the first model call.
func mentionsConn(text string, c *Connection) bool {
	t := strings.ToLower(text)
	if t == "" {
		return false
	}
	needles := []string{strings.ToLower(c.Name), slug(c.Name)}
	for _, w := range strings.Fields(strings.ReplaceAll(slug(c.Name), "_", " ")) {
		if len(w) >= 4 && !genericWord[w] {
			needles = append(needles, w)
		}
	}
	for _, n := range needles {
		if len(n) >= 3 && strings.Contains(t, n) {
			return true
		}
	}
	return false
}

// genericWord lists name parts too common to count as a mention on their own.
var genericWord = map[string]bool{"server": true, "custom": true, "remote": true, "prod": true, "production": true,
	"staging": true, "test": true, "tools": true, "internal": true, "https": true, "http": true, "api": true}

// usedConn reports whether any of the tool names carries the connection's prefix.
func usedConn(names []string, c *Connection) bool {
	prefix := slug(c.Name) + "_"
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// loadMCP adds one connection's tools to the call (once per turn) and returns their names.
func (a *Agent) loadMCP(ctx context.Context, c *Call, conn *Connection) ([]string, error) {
	a.ensureTools(ctx, c)
	if names, ok := c.mcpLoaded[conn.ID]; ok {
		return names, nil
	}
	tools, err := a.mcp.tools(ctx, c.OrgID, a, conn)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if _, dup := c.tools[t.Name]; dup {
			continue
		}
		c.tools[t.Name] = t
		names = append(names, t.Name)
	}
	sortStrings(names)
	if c.mcpLoaded == nil {
		c.mcpLoaded = map[int64][]string{}
	}
	c.mcpLoaded[conn.ID] = names
	return names, nil
}

// useConnectionTool is the model's door to MCP servers: it loads one connection's tools into
// the turn so they appear in the next request. Listing names here rather than tools is what
// keeps the first call small.
func (a *Agent) useConnectionTool(conns []*Connection) Tool {
	names := make([]string, 0, len(conns))
	hints := make([]string, 0, len(conns))
	for _, c := range conns {
		names = append(names, c.Name)
		hints = append(hints, fmt.Sprintf("%s (its tools will be named %s_*)", c.Name, slug(c.Name)))
	}
	return Tool{
		Name: "use_connection",
		Desc: "Load the tools of a connected service so you can call them in your next step. Call this first, on its own, whenever the request involves one of these services; their tools are not listed until you do. Services: " + strings.Join(hints, "; ") + ".",
		Params: schema(map[string]any{
			"connection": map[string]any{"type": "string", "enum": names, "description": "Service to load"},
		}, "connection"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct {
				Connection string `json:"connection"`
			}
			json.Unmarshal(args, &p)
			conn := c.mcpConnNamed(p.Connection)
			if conn == nil {
				return "", fmt.Errorf("no connected service named %q here; choose one of: %s", p.Connection, strings.Join(names, ", "))
			}
			_, already := c.mcpLoaded[conn.ID]
			loaded, err := a.loadMCP(ctx, c, conn)
			if err != nil {
				return "", err
			}
			verb := "Loaded"
			if already {
				verb = "Already loaded"
			}
			return fmt.Sprintf("%s %d tools for %s. They are available now; call them directly rather than calling use_connection again: %s",
				verb, len(loaded), conn.Name, strings.Join(loaded, ", ")), nil
		},
	}
}

// mcpNeedsConfirm reports whether a tool on this connection waits for a human. Reading never
// does: the point of a connection is that the channel may look at what it reaches. A connection
// set to "auto" runs writes too; one set to "all" is the admin override that holds everything,
// reads included.
func mcpNeedsConfirm(c *Connection, t *mcp.Tool) bool {
	switch c.Writes {
	case "auto":
		return false
	case "all":
		return true
	}
	return !mcpReadOnly(t)
}

// mcpReadOnly decides whether a tool only reads, and so may run without a human.
//
// The tool's name decides it. The server's own readOnlyHint annotation is deliberately not
// trusted to make a tool read-only: it is a remote server's claim about itself, and a hostile
// or compromised one that wanted its writes to skip the Confirm card would only have to set
// the flag. The MCP spec says the same — annotations from an untrusted server are hints, not
// a security boundary. The hint is honoured in the other direction only, because that adds
// caution: a server saying a tool is destructive is believed.
//
// An admin who does trust a server can still let its writes through, by setting that
// connection's writes to "auto" in the console. That is a decision someone makes, which is
// the point.
func mcpReadOnly(t *mcp.Tool) bool {
	if t.Annotations != nil && t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint {
		return false
	}
	return mcpLooksReadOnly(t.Name)
}

// Verbs that change something. Checked first, so a name carrying both ("search_and_delete",
// "get_or_create_list") counts as a write.
var mcpWriteVerbs = []string{"create", "add", "insert", "update", "edit", "modify", "patch", "set", "put",
	"delete", "remove", "destroy", "drop", "trash", "clear", "purge", "send", "post", "reply", "comment",
	"write", "upload", "attach", "import", "move", "copy", "merge", "rename", "archive", "restore",
	"assign", "unassign", "invite", "share", "publish", "approve", "close", "reopen", "resolve_thread",
	"schedule", "start", "stop", "cancel", "run", "execute", "trigger", "apply", "mark", "unmark",
	"label", "unlabel", "subscribe", "unsubscribe", "enable", "disable", "grant", "revoke"}

// Verbs that only look. Anything matching neither list is treated as a write.
var mcpReadVerbs = []string{"get", "list", "search", "find", "read", "query", "describe", "show", "count",
	"aggregate", "explain", "ask", "fetch", "retrieve", "lookup", "look_up", "check", "browse", "resolve",
	"view", "inspect", "status", "summarize", "summarise", "filter", "download", "export", "discover",
	"diff", "history", "info", "detail", "details", "schema", "preview", "sample", "stats", "usage",
	"metrics", "report", "whoami", "me", "ping", "help", "docs"}

func mcpLooksReadOnly(name string) bool {
	n := strings.ToLower(name)
	has := func(verbs []string) bool {
		for _, v := range verbs {
			if strings.HasPrefix(n, v) || strings.Contains(n, "_"+v) || strings.Contains(n, "-"+v) {
				return true
			}
		}
		return false
	}
	if has(mcpWriteVerbs) {
		return false
	}
	return has(mcpReadVerbs)
}

// audit carries who this call is for — workspace, channel, thread, requester. It used to be
// built here from nothing, so every MCP call an answer made was recorded with no team, no
// channel and no requester: an audit row nobody could trace back to a person or a workspace.
func (h *mcpHub) call(ctx context.Context, orgID int64, c *Connection, tool string, args map[string]any, audit ProxyAudit) (string, error) {
	mc, err := h.connect(ctx, orgID, c)
	if err != nil {
		return "", err
	}
	start := time.Now()
	res, err := mc.session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	audit.ConnectionID, audit.Method, audit.Host, audit.Path = c.ID, "MCP", c.Name, tool
	audit.Status = map[bool]int{true: 500, false: 200}[err != nil || (res != nil && res.IsError)]
	audit.MS = time.Since(start).Milliseconds()
	h.proxy.store.LogProxy(ctx, orgID, audit)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ct := range res.Content {
		switch v := ct.(type) {
		case *mcp.TextContent:
			b.WriteString(v.Text + "\n")
		default:
			raw, _ := json.Marshal(v)
			b.Write(raw)
			b.WriteString("\n")
		}
	}
	if res.StructuredContent != nil && b.Len() == 0 {
		raw, _ := json.Marshal(res.StructuredContent)
		b.Write(raw)
	}
	h.proxy.store.TouchConnection(ctx, orgID, c.ID)
	if res.IsError {
		return "", fmt.Errorf("%s", truncate(b.String(), 500))
	}
	return b.String(), nil
}
