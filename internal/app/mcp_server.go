package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP server: the developer API, spoken as tools, at /mcp.
//
// Each tool is one /v1 route, and calling it runs that route — in-process, through the same mux,
// with the caller already resolved — rather than a second implementation of what the route does.
// So a tool cannot say or allow anything its route does not: the permission, the organisation's
// boundary, the validation, the audit row and the answer are all the route's own, and a route
// added to /v1 is one entry in mcpTools away from being a tool.
//
// Who a call acts as is settled once, at the door, from either credential: a developer key,
// exactly as /v1 takes one, or an access token from a grant somebody approved on the consent
// screen (mcp_oauth.go). tools/list offers only what that person's role can use.

// mcpPrincipalKey marks a /v1 request the MCP server makes in-process, for a caller it has already
// authenticated. A context value, so nothing that arrives over the network can carry one.
type mcpPrincipalKey struct{}

// mcpCaller is the caller an in-process MCP call carries, and nil on every other request.
func mcpCaller(r *http.Request) *AdminUser {
	u, _ := r.Context().Value(mcpPrincipalKey{}).(*AdminUser)
	return u
}

type mcpParam struct {
	name, typ, desc string
	// in is where the value goes: "path" fills {name} in the route; "query" and "body" are what
	// they say.
	in       string
	required bool
	enum     []string
}

type mcpTool struct {
	name, title, desc string
	// pattern is the /v1 route exactly as api_v1.go registers it. The tool exists where that
	// route does, and nowhere else: a route registered only on some deployments brings its tool
	// with it.
	pattern string
	perm    Permission
	params  []mcpParam
	// What a client decides with. Claude, for one, asks before it calls a tool that is not
	// read-only, and says so more loudly before one that is destructive.
	readOnly, destructive, idempotent bool
}

var (
	mcpLimit  = mcpParam{name: "limit", typ: "integer", in: "query", desc: "How many rows to return."}
	mcpBefore = mcpParam{name: "before", typ: "integer", in: "query", desc: "Page back: only rows with an id below this, newest first."}
	mcpAfter  = mcpParam{name: "after", typ: "integer", in: "query", desc: "Page forward: only rows with an id above this, oldest first. last_id in each answer is the next value."}
	mcpRange  = mcpParam{name: "range", typ: "string", in: "query", desc: "Only this window.", enum: []string{"today", "7d", "month"}}
	mcpDoc    = mcpParam{name: "path", typ: "string", in: "path", required: true, desc: "The document's path, e.g. runbooks/deploys.md."}
	mcpID     = func(what string) mcpParam {
		return mcpParam{name: "id", typ: "integer", in: "path", required: true, desc: "The " + what + "'s id."}
	}
)

// mcpRoutineParams is what a routine is made of, for creating one (where the first three are
// required) and for changing one (where nothing is).
func mcpRoutineParams(creating bool) []mcpParam {
	return []mcpParam{
		{name: "channel", typ: "string", in: "body", required: creating,
			desc: `The channel it posts in: its id, or its name ("#eng"). One the bot is in; list_scopes shows them.`},
		{name: "cron", typ: "string", in: "body", required: creating,
			desc: `When it runs, as a five-field cron schedule: "0 9 * * 1-5" is 9:00 on weekdays. No more often than every 15 minutes.`},
		{name: "prompt", typ: "string", in: "body", required: creating, desc: "What it does each time it runs, in plain words."},
		{name: "timezone", typ: "string", in: "body", desc: `The schedule's timezone, e.g. "Europe/London". The organisation's when left out.`},
		{name: "team_id", typ: "string", in: "body", desc: "Only when the channel is in more than one connected workspace."},
		{name: "notify", typ: "string", in: "body", enum: []string{"always", "when_needed"},
			desc: `"always" posts every run; "when_needed" posts only the runs that meet notify_when.`},
		{name: "notify_when", typ: "string", in: "body", desc: `With notify "when_needed": what makes a run worth posting.`},
		{name: "model", typ: "string", in: "body", desc: `The model its runs answer on. Leave it out for the default; "heavy" is the advanced model.`},
	}
}

// mcpTools is the whole server. The order is the order a client lists them in.
var mcpTools = []mcpTool{
	{name: "whoami", title: "Who this connection is", pattern: "GET /v1/whoami", readOnly: true,
		desc: "The organisation, account, role and permissions this connection acts with."},
	{name: "list_workspaces", title: "List workspaces", pattern: "GET /v1/workspaces", readOnly: true,
		desc: "The Slack workspaces and Microsoft Teams tenants connected to the bot."},
	{name: "list_scopes", title: "List channel settings", pattern: "GET /v1/scopes", readOnly: true,
		desc: "The organisation, workspace and channel settings the bot has: which channels it is in and how each is set up."},

	{name: "list_documents", title: "List documents", pattern: "GET /v1/documents", readOnly: true,
		desc: "Every document the bot searches when it answers, with its path, the channel it is limited to and whether it is indexed."},
	{name: "read_document", title: "Read a document", pattern: "GET /v1/documents/{path...}", readOnly: true,
		params: []mcpParam{mcpDoc},
		desc:   "The text of one document. Text types only: a PDF has no text to hand back here."},
	{name: "write_document", title: "Write a document", pattern: "PUT /v1/documents/{path...}", perm: PermDocsManage, idempotent: true,
		params: []mcpParam{mcpDoc,
			{name: "content", typ: "string", in: "body", desc: "The whole new text. Leave it out to change only the scope."},
			{name: "scope", typ: "string", in: "body", desc: "The channel the document is limited to. Leave it out to keep the one it has."}},
		desc: "Create or replace a text document (.md, .txt, .csv, .json, .html, .rst) and re-index it in the background, so the bot answers from the new text."},
	{name: "delete_document", title: "Delete a document", pattern: "DELETE /v1/documents/{path...}", perm: PermDocsManage, destructive: true, idempotent: true,
		params: []mcpParam{mcpDoc},
		desc:   "Remove a document, and what it contributed to the bot's answers."},

	{name: "list_memories", title: "List memories", pattern: "GET /v1/memories", readOnly: true,
		desc: "The facts the bot keeps per workspace and per channel."},
	{name: "add_memory", title: "Remember something", pattern: "POST /v1/memories", perm: PermMemoryManage,
		params: []mcpParam{
			{name: "scope", typ: "string", in: "body", required: true, desc: `Where it applies: "team:T0123" for a whole workspace, or "channel:T0123/C0456" for one channel.`},
			{name: "text", typ: "string", in: "body", required: true, desc: "The fact, in a sentence."}},
		desc: "Add a fact the bot will keep in mind when it answers in that workspace or channel."},
	{name: "update_memory", title: "Change a memory", pattern: "PUT /v1/memories/{id}", perm: PermMemoryManage, idempotent: true,
		params: []mcpParam{mcpID("memory"), {name: "text", typ: "string", in: "body", required: true, desc: "What it should say now."}},
		desc:   "Change what a memory says."},
	{name: "delete_memory", title: "Forget a memory", pattern: "DELETE /v1/memories/{id}", perm: PermMemoryManage, destructive: true, idempotent: true,
		params: []mcpParam{mcpID("memory")},
		desc:   "Forget a memory."},

	{name: "list_artifacts", title: "List artifacts", pattern: "GET /v1/artifacts", perm: PermArtifactsView, readOnly: true,
		params: []mcpParam{mcpLimit},
		desc:   "Files the bot made in its answers — reports, tables, drafts — newest first, without their contents."},
	{name: "get_artifact", title: "Read an artifact", pattern: "GET /v1/artifacts/{id}", perm: PermArtifactsView, readOnly: true,
		params: []mcpParam{mcpID("artifact")},
		desc:   "One file the bot made, contents included."},

	{name: "list_routines", title: "List routines", pattern: "GET /v1/routines", readOnly: true,
		desc: "Scheduled prompts: what each asks, when it runs, where it posts, and how its last run went."},
	{name: "list_routine_runs", title: "List a routine's runs", pattern: "GET /v1/routines/{id}/runs", readOnly: true,
		params: []mcpParam{mcpID("routine"), mcpLimit},
		desc:   "One routine's run history with each run's output, cost and error — quiet runs that posted nothing included."},
	{name: "run_routine", title: "Run a routine now", pattern: "POST /v1/routines/{id}/run", perm: PermRoutinesManage,
		params: []mcpParam{mcpID("routine")},
		desc:   "Run a routine now, out of schedule. It runs in its own time, and its reply lands in the channel it posts to; list_routine_runs shows the result."},
	{name: "create_routine", title: "Create a routine", pattern: "POST /v1/routines", perm: PermRoutinesManage,
		params: mcpRoutineParams(true),
		desc: "Create a routine: a prompt the bot runs on a schedule, posting in a channel it is in. It runs as the bot, " +
			"with that channel's connections and budget, and asks before any write until someone switches that off in the console."},
	{name: "update_routine", title: "Change a routine", pattern: "PUT /v1/routines/{id}", perm: PermRoutinesManage, idempotent: true,
		params: append([]mcpParam{mcpID("routine"),
			{name: "enabled", typ: "boolean", in: "body", desc: "false pauses it; true resumes it."}}, mcpRoutineParams(false)...),
		desc: "Change a routine; only what is sent changes. Rewriting what it does takes it off the person it ran as, so it runs as the bot from then on."},
	{name: "delete_routine", title: "Delete a routine", pattern: "DELETE /v1/routines/{id}", perm: PermRoutinesManage, destructive: true, idempotent: true,
		params: []mcpParam{mcpID("routine")},
		desc:   "Delete a routine, and its run history with it."},

	{name: "list_jobs", title: "List fix jobs", pattern: "GET /v1/jobs", perm: PermJobsView, readOnly: true,
		params: []mcpParam{{name: "status", typ: "string", in: "query", desc: `Only jobs in this state, e.g. "active".`}, mcpLimit},
		desc:   "Fixes the bot handed to a worker: what each is changing, its status, the pull request it opened and what it cost."},
	{name: "get_job", title: "Read a fix job", pattern: "GET /v1/jobs/{id}", perm: PermJobsView, readOnly: true,
		params: []mcpParam{mcpID("job")},
		desc:   "One fix job: its status, branch, pull request, cost and error."},

	{name: "list_access_requests", title: "List access requests", pattern: "GET /v1/access-requests", perm: PermAccessView, readOnly: true,
		params: []mcpParam{{name: "status", typ: "string", in: "query", desc: `Only requests in this state, e.g. "open".`}, mcpLimit},
		desc:   "Who asked the bot for access to what, and how each request was closed."},
	{name: "list_activity", title: "List turns", pattern: "GET /v1/activity", perm: PermActivityView, readOnly: true,
		params: []mcpParam{{name: "channel", typ: "string", in: "query", desc: "Only this channel's id."}, mcpRange, mcpLimit, mcpBefore, mcpAfter},
		desc:   "Every turn the bot took, with the model, tokens and cost, newest first. Each has an id to page by."},
	{name: "list_audit", title: "Read the audit log", pattern: "GET /v1/audit", perm: PermAuditView, readOnly: true,
		params: []mcpParam{
			{name: "action", typ: "string", in: "query", desc: `One action, or a family with a trailing dot, e.g. "auth.".`},
			{name: "actor", typ: "string", in: "query", desc: "An account id, email or Slack id."},
			{name: "outcome", typ: "string", in: "query", desc: "Only rows with this outcome.", enum: []string{"ok", "denied", "failed"}},
			{name: "q", typ: "string", in: "query", desc: "A word in the actor, target, details or address."},
			mcpRange, mcpLimit, mcpBefore, mcpAfter},
		desc: "Who signed in and from where, what they changed and approved, and what they exported."},
	{name: "get_usage", title: "Usage and spend", pattern: "GET /v1/usage", readOnly: true,
		desc: "This month's spend against the budget, what the bot has to work with, the busiest channels, and paused_by: empty while the bot can answer, else the limit that stopped it."},
	{name: "get_billing", title: "Plan and credit", pattern: "GET /v1/billing", readOnly: true,
		desc: "The plan, what it costs, when it renews, and the credit left."},
}

// mcpInstructions is what a client reads before it reads the tools.
const mcpInstructions = "attest_tag is a bot that answers in Slack and Microsoft Teams channels from the documents, " +
	"memories and connections an organisation gives it. These tools read and change what it knows (documents, " +
	"memories), show what it did (turns, artifacts, fix jobs, routine runs) and what that cost. Every call acts as " +
	"the person who connected this client, with exactly the permissions their role holds."

// mcpResultCap keeps one tool result from filling a model's context. A document longer than this
// is better read in the console, or through the API.
const mcpResultCap = 100_000

func (t mcpTool) method() string { m, _, _ := strings.Cut(t.pattern, " "); return m }
func (t mcpTool) path() string   { _, p, _ := strings.Cut(t.pattern, " "); return p }

// definition is the tool as a client sees it.
func (t mcpTool) definition() *mcp.Tool {
	props := map[string]any{}
	required := []string{}
	for _, p := range t.params {
		s := map[string]any{"type": p.typ, "description": p.desc}
		if len(p.enum) > 0 {
			s["enum"] = p.enum
		}
		props[p.name] = s
		if p.required {
			required = append(required, p.name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	closed := false
	ann := &mcp.ToolAnnotations{Title: t.title, ReadOnlyHint: t.readOnly, IdempotentHint: t.idempotent || t.readOnly,
		OpenWorldHint: &closed}
	if !t.readOnly {
		d := t.destructive
		ann.DestructiveHint = &d
	}
	return &mcp.Tool{Name: t.name, Title: t.title, Description: t.desc, InputSchema: schema, Annotations: ann}
}

// mcpRouteExists is whether this deployment registers the tool's route. Asked of the mux rather
// than assumed, so a route that is only there on some deployments takes its tool with it.
func mcpRouteExists(mux *http.ServeMux, t mcpTool) bool {
	p := strings.NewReplacer("{path...}", "probe.md", "{id}", "1").Replace(t.path())
	r, err := http.NewRequest(t.method(), p, nil)
	if err != nil {
		return false
	}
	_, pattern := mux.Handler(r)
	return pattern == t.pattern
}

// mcpToolsFor is the tools one person may call here: their role holds the permission, and the
// deployment serves the route.
func mcpToolsFor(mux *http.ServeMux, u *AdminUser) []mcpTool {
	var out []mcpTool
	for _, t := range mcpTools {
		if t.perm != "" && (u == nil || !u.Permissions[t.perm]) {
			continue
		}
		if mcpRouteExists(mux, t) {
			out = append(out, t)
		}
	}
	return out
}

func mcpToolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// mcpArg reads one argument: as the text it travels as in a path or a query, and as the typed
// value a JSON body carries.
func mcpArg(v any, typ string) (string, any, error) {
	switch typ {
	case "integer":
		s := ""
		switch n := v.(type) {
		case json.Number:
			s = n.String()
		case string:
			s = n
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return "", nil, errors.New("must be a whole number")
		}
		return s, i, nil
	case "boolean":
		switch t := v.(type) {
		case bool:
			return strconv.FormatBool(t), t, nil
		case string:
			if parsed, err := strconv.ParseBool(t); err == nil {
				return t, parsed, nil
			}
		}
		return "", nil, errors.New("must be true or false")
	default:
		s, ok := v.(string)
		if !ok {
			return "", nil, errors.New("must be a string")
		}
		return s, s, nil
	}
}

// mcpDocPath escapes a document path one segment at a time, so it names a document and nothing
// else: no query, no fragment, and no climbing out of /v1/documents on the way to the mux.
func mcpDocPath(p string) (string, error) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", errors.New("is not a document path")
		}
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/"), nil
}

// mcpRecorder is the response of a route run in-process.
type mcpRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *mcpRecorder) Header() http.Header { return r.header }
func (r *mcpRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}
func (r *mcpRecorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(p)
}

// mcpCall runs one tool: its route, as the caller, with the caller's arguments put where the
// route reads them.
func (b *Bot) mcpCall(ctx context.Context, mux *http.ServeMux, outer *http.Request, u *AdminUser, t mcpTool, raw json.RawMessage) *mcp.CallToolResult {
	args := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			return mcpToolError("The arguments are not a JSON object.")
		}
	}
	path, query, body := t.path(), url.Values{}, map[string]any{}
	for _, p := range t.params {
		v, ok := args[p.name]
		if !ok || v == nil {
			if p.required {
				return mcpToolError(p.name + " is required.")
			}
			continue
		}
		s, typed, err := mcpArg(v, p.typ)
		if err != nil {
			return mcpToolError(p.name + " " + err.Error() + ".")
		}
		switch p.in {
		case "path":
			if p.typ == "string" {
				if s, err = mcpDocPath(s); err != nil {
					return mcpToolError(p.name + " " + err.Error() + ".")
				}
			}
			path = strings.Replace(strings.Replace(path, "{"+p.name+"...}", s, 1), "{"+p.name+"}", s, 1)
		case "query":
			query.Set(p.name, s)
		case "body":
			body[p.name] = typed
		}
	}
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var rd io.Reader
	if len(body) > 0 {
		enc, _ := json.Marshal(body)
		rd = bytes.NewReader(enc)
	}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, mcpPrincipalKey{}, u), t.method(), target, rd)
	if err != nil {
		return mcpToolError("Could not make that call: " + err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	// Where the call came from, so a write's audit row names the caller's address and client
	// rather than nobody's.
	req.RemoteAddr = outer.RemoteAddr
	for _, h := range []string{"X-Forwarded-For", "User-Agent"} {
		if v := outer.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	rec := &mcpRecorder{header: http.Header{}}
	mux.ServeHTTP(rec, req)

	if rec.code >= 400 {
		msg := strings.TrimSpace(rec.body.String())
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(rec.body.Bytes(), &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return mcpToolError(fmt.Sprintf("%s (HTTP %d)", msg, rec.code))
	}
	text, cut := cutRunes(rec.body.String(), mcpResultCap)
	if cut {
		text += fmt.Sprintf("\n\n[Cut at %d characters. The rest is in the console, or through the API.]", mcpResultCap)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// mcpServerFor is the server one request talks to: the tools this caller may use, each bound to
// the caller. Stateless, so it is made per request, and so a role changed a minute ago changes the
// next tools/list.
func (b *Bot) mcpServerFor(mux *http.ServeMux, outer *http.Request, u *AdminUser) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "attest_tag", Title: "attest_tag", Version: "1"},
		&mcp.ServerOptions{Instructions: mcpInstructions})
	for _, t := range mcpToolsFor(mux, u) {
		t := t
		s.AddTool(t.definition(), func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return b.mcpCall(ctx, mux, outer, u, t, req.Params.Arguments), nil
		})
	}
	return s
}

// ---- who is calling ----

// mcpPrincipal resolves the credential on an MCP request: a developer key, or an access token from
// a grant.
func (b *Bot) mcpPrincipal(r *http.Request) (*AdminUser, *keyError) {
	tok, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok = strings.TrimSpace(tok)
	switch {
	case !bearer || tok == "":
		return nil, &keyError{"Connect this client with OAuth, or send a developer key as Authorization: Bearer <key>.", http.StatusUnauthorized}
	case looksLikeAPIKey(tok):
		u, refusal := b.authenticateKey(r)
		if refusal != nil {
			return nil, refusal
		}
		u.Via = viaMCP
		return u, nil
	case strings.HasPrefix(tok, mcpAccessPrefix):
		return b.authenticateMCPToken(r.Context(), tok)
	}
	return nil, &keyError{"Invalid access token.", http.StatusUnauthorized}
}

// authenticateMCPToken is authenticateKey for a grant's access token: found by its hash, refused
// once revoked or expired, rate limited like a key, and resolved to its person afresh.
func (b *Bot) authenticateMCPToken(ctx context.Context, tok string) (*AdminUser, *keyError) {
	g, err := b.store.MCPGrantByAccess(ctx, hashAPIKey(tok))
	if err != nil {
		return nil, &keyError{"Could not check that token.", http.StatusInternalServerError}
	}
	switch {
	case g == nil:
		return nil, &keyError{"Invalid access token.", http.StatusUnauthorized}
	case g.RevokedAt != "":
		return nil, &keyError{"This connection has been revoked. Connect the app again.", http.StatusUnauthorized}
	case g.AccessExpiresAt <= now():
		return nil, &keyError{"The access token has expired.", http.StatusUnauthorized}
	}
	if fits, retry := allowGrantRequest(g.ID); !fits {
		return nil, &keyError{fmt.Sprintf("Rate limit exceeded — %d requests a minute. Retry in %d seconds.", apiKeyRateLimit, retry), http.StatusTooManyRequests}
	}
	u, refusal := b.actAs(ctx, g.UserID, g.OrgID, "connection")
	if refusal != nil {
		return nil, refusal
	}
	u.Via, u.MCPGrantID = viaMCP, g.ID
	b.store.TouchMCPGrant(ctx, g.OrgID, g.ID, g.LastUsed)
	return u, nil
}

// mcpRefuse answers a request that is not allowed in. A 401 names the metadata that says how to
// get in (RFC 9728 §5.1), which is how a client that has never heard of this server finds its
// way to the consent screen.
func (b *Bot) mcpRefuse(w http.ResponseWriter, r *http.Request, k *keyError) {
	if k.status == http.StatusUnauthorized {
		challenge := fmt.Sprintf(`Bearer resource_metadata=%q, scope=%q`,
			b.mcpOrigin(r.Context())+"/.well-known/oauth-protected-resource/mcp", mcpScope)
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			challenge += fmt.Sprintf(`, error="invalid_token", error_description=%q`, strings.ReplaceAll(k.msg, `"`, "'"))
		}
		w.Header().Set("WWW-Authenticate", challenge)
	}
	writeJSON(w, k.status, map[string]any{"error": k.msg})
}

// mcpSweeps paces the clean-up of lapsed codes and abandoned registrations to once an hour,
// whichever instance gets there.
var mcpSweeps = newRateLimiter()

// mcpRoutes registers the MCP endpoint, its authorization server, and the console's view of both.
func (b *Bot) mcpRoutes(mux *http.ServeMux) {
	b.mcpOAuthRoutes(mux)
	stream := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return b.mcpServerFor(mux, r, adminFromCtx(r.Context()))
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// The protection this turns off stops a web page from reaching an unauthenticated server
		// on somebody's own machine by rebinding a hostname to it. Nothing reaches this one
		// without a bearer token, which such a page cannot attach, and it answers behind proxies
		// that connect on localhost — Tailscale Funnel, a sidecar — which it would otherwise
		// refuse wholesale.
		DisableLocalhostProtection: true,
		// write_document carries a whole document, and /v1 takes one of up to 16 MB.
		MaxRequestBodyBytes: 20 << 20,
	})
	mux.HandleFunc("/mcp", mcpCORS(func(w http.ResponseWriter, r *http.Request) {
		u, refusal := b.mcpPrincipal(r)
		if refusal != nil {
			b.mcpRefuse(w, r, refusal)
			return
		}
		if ok, _ := mcpSweeps.allow("mcp-sweep", 1, time.Hour); ok {
			go b.store.SweepMCP(context.WithoutCancel(r.Context()))
		}
		stream.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}))

	// The console's page: the address to connect to, the tools and who may use each, and the
	// apps people have connected. Any member may look, as with API keys — who has automation
	// pointed at this organisation is worth being able to see.
	mux.HandleFunc("GET /api/mcp", b.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		u := adminFromCtx(ctx)
		tools := []map[string]any{}
		for _, t := range mcpTools {
			if !mcpRouteExists(mux, t) {
				continue
			}
			tools = append(tools, map[string]any{"name": t.name, "title": t.title, "description": t.desc,
				"permission": string(t.perm), "read_only": t.readOnly, "destructive": t.destructive,
				"allowed": t.perm == "" || u.Permissions[t.perm]})
		}
		grants, err := b.store.MCPGrants(ctx, u.OrgID)
		if err != nil {
			fail(w, err)
			return
		}
		for i := range grants {
			grants[i].ReturnsTo = mcpReturnsTo(grants[i].RedirectURI)
		}
		writeJSON(w, 200, map[string]any{"url": mcpResourceOf(b.mcpOrigin(ctx)), "tools": tools, "grants": grants,
			"refusal": b.mayConnectMCP(ctx, u)})
	}))

	// Disconnecting an app: its own person may always, and so may whoever may manage keys.
	mux.HandleFunc("DELETE /api/mcp/grants/{id}", b.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		u := adminFromCtx(ctx)
		grants, err := b.store.MCPGrants(ctx, u.OrgID)
		if err != nil {
			fail(w, err)
			return
		}
		var g *MCPGrant
		for i := range grants {
			if grants[i].ID == pathID(r, "id") {
				g = &grants[i]
			}
		}
		if g == nil || g.RevokedAt != "" {
			writeJSON(w, 404, map[string]any{"error": "no such connection, or it has ended already"})
			return
		}
		if g.UserID != u.ID && !u.Permissions[PermAPIKeysManage] {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": denialCopy(PermAPIKeysManage)})
			return
		}
		if _, err := b.store.RevokeMCPGrant(ctx, u.OrgID, g.ID); err != nil {
			fail(w, err)
			return
		}
		b.audit(r, "mcp.revoked", AuditEvent{TargetKind: "mcp_grant", TargetID: strconv.FormatInt(g.ID, 10), TargetName: g.ClientName,
			Details: auditDetails(map[string]any{"owner": g.Owner, "returns_to": mcpReturnsTo(g.RedirectURI)})})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
}
