package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- the fixture ----

type assistFix struct {
	t    *testing.T
	b    *Bot
	st   *Store
	mux  *http.ServeMux
	sess string
	org  int64
}

// newAssist wires a Bot with the console routes and one channel to talk about. The LLM is a
// handler the caller supplies, so a test can make the model do the one thing it is about.
func newAssist(t *testing.T, role string, llm http.Handler) *assistFix {
	t.Helper()
	fixedMasterKey(t)
	ctx := context.Background()
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	llmSrv := httptest.NewServer(llm)
	t.Cleanup(llmSrv.Close)

	cfg := Config{Timezone: "UTC"}
	b := &Bot{store: st, sealer: sealer, mail: logMailer{}, settings: newSettingsCache(st, cfg), resolver: NewResolver(st)}
	b.slacks = NewChatRegistry(st, sealer)
	b.agent = &Agent{cfg: cfg, store: st, settings: b.settings, slacks: b.slacks, loc: time.UTC,
		tools: map[string]Tool{}, runs: map[int64]*runHandle{},
		llm: NewLLM(Config{LLMBaseURL: llmSrv.URL, LLMKey: "k", Model: "test"})}

	org, _, tok := seedOrg(t, st, role)
	if _, err := st.UpsertChannelScope(ctx, org, "T1", "C1", "ops", false); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	b.routes(mux, nil)
	return &assistFix{t: t, b: b, st: st, mux: mux, sess: tok, org: org}
}

// send runs a request through the real mux, authenticated the way the console is: the session
// cookie, the CSRF cookie and the header that must match it.
func (f *assistFix) send(r *http.Request) *httptest.ResponseRecorder {
	f.t.Helper()
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.sess})
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: "tok"})
	r.Header.Set(csrfHeader, "tok")
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func (f *assistFix) call(method, path, body string) (int, map[string]any) {
	f.t.Helper()
	w := f.send(httptest.NewRequest(method, path, strings.NewReader(body)))
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// ask posts one question to the assistant and decodes the reply.
func (f *assistFix) ask(question string) (int, assistantReply) {
	f.t.Helper()
	body, _ := json.Marshal(assistantRequest{Question: question})
	w := f.send(httptest.NewRequest("POST", "/api/assistant", strings.NewReader(string(body))))
	var out assistantReply
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// askWithFile posts a question carrying one attachment, the way the panel does.
func (f *assistFix) askWithFile(question, name string, content []byte) (int, assistantReply) {
	f.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("question", question)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		f.t.Fatal(err)
	}
	part.Write(content)
	mw.Close()
	r := httptest.NewRequest("POST", "/api/assistant", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := f.send(r)
	var out assistantReply
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// consoleCallFor is a call as the handler would build it, for the tests that exercise a tool
// directly rather than through a model.
func (f *assistFix) consoleCallFor(perms ...string) *consoleCall {
	p := map[string]bool{}
	for _, s := range perms {
		p[s] = true
	}
	return &consoleCall{OrgID: f.org, Actor: "actor", Perms: p, model: "test"}
}

func (f *assistFix) tool(c *consoleCall, name string) consoleTool {
	f.t.Helper()
	for _, t := range f.b.consoleTools(c) {
		if t.Name == name {
			return t
		}
	}
	f.t.Fatalf("no tool %q was offered", name)
	return consoleTool{}
}

func (f *assistFix) toolNames(c *consoleCall) map[string]bool {
	out := map[string]bool{}
	for _, t := range f.b.consoleTools(c) {
		out[t.Name] = true
	}
	return out
}

// resources is what read_console would name for this caller, read out of the tool's own schema
// rather than the table, so what is asserted is what the model is actually shown.
func (f *assistFix) resources(c *consoleCall) map[string]bool {
	f.t.Helper()
	raw, _ := json.Marshal(f.tool(c, "read_console").Params)
	var probe struct {
		Properties struct {
			Resource struct {
				Enum []string `json:"enum"`
			} `json:"resource"`
		} `json:"properties"`
	}
	json.Unmarshal(raw, &probe)
	out := map[string]bool{}
	for _, n := range probe.Properties.Resource.Enum {
		out[n] = true
	}
	return out
}

func args(m map[string]any) json.RawMessage {
	b, _ := json.Marshal(m)
	return b
}

func proposeChannel(channel string, changes map[string]string) json.RawMessage {
	return args(map[string]any{"channel": channel, "changes": changes})
}

func (f *assistFix) seedTier(name string, rank int) *ApprovalRole {
	f.t.Helper()
	role := &ApprovalRole{Name: name, Rank: rank}
	if err := f.st.AddApprovalRole(context.Background(), f.org, role); err != nil {
		f.t.Fatal(err)
	}
	return role
}

// ---- the two claims this design rests on ----

// The assistant changes nothing. Not "changes things carefully" — there is no configuration
// write in either of its files, and the browser applies a confirmed proposal by making the
// requests the console's own pages make.
//
// That is a property of the code rather than of the prompt, so it is checked as one. The list
// below is the whole set of store methods those two files may call: a reader who wants to know
// what the assistant can do to the database reads this list instead of a thousand lines, and
// anyone adding to it has to say why here, in front of the two entries that are writes and the
// reasons they are allowed.
func TestAssistantHasNoWriteCapability(t *testing.T) {
	allowed := map[string]string{
		// Reads. Each is the same query the console's own route for that page makes, and the
		// tool offering it is withheld unless the caller holds that route's permission.
		"Scopes": "channels", "ScopeByID": "one channel by id", "Bundles": "bundle names",
		"Bundle": "one bundle", "Connection": "one connection",
		"ApprovalRoles": "approval tiers", "ApprovalRole": "one tier",
		"AccessRequests": "access requests", "RecentTurns": "activity", "AuditEvents": "the audit log",
		"Documents": "the document corpus", "Routines": "routines", "Jobs": "fix jobs",
		"Artifacts": "artifacts", "OverviewStats": "the overview tiles", "Teams": "whether a workspace is connected",
		"UsageByChannel": "spend per channel", "MonthSpend": "spend against the account's budget",
		"RecentToolCalls":   "the tool-call log, for running an analysis over",
		"ConsoleTurnsSince": "this account's own rate limit",
		// The two writes, and neither touches configuration. Both are the record of what
		// happened, which is the thing an assistant that can propose changes most needs to
		// leave behind.
		"LogToolCall":      "append-only: what the assistant read, shown in Activity",
		"LogUsageBy":       "append-only: what the turn spent, so it lands inside the budget",
		"AddAssistantTurn": "append-only: the transcript Activity shows, in its own org-scoped table",
	}
	for _, file := range []string{"assistant.go", "assistant_api.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// b.changed invalidates the resolver and settings caches. Only a write needs it, so
			// reaching for it here is the tell that one arrived.
			if inner, ok := sel.X.(*ast.Ident); ok && inner.Name == "b" && sel.Sel.Name == "changed" {
				t.Errorf("%s calls b.changed, which only a write needs", file)
				return true
			}
			store, ok := sel.X.(*ast.SelectorExpr)
			if !ok || store.Sel.Name != "store" {
				return true
			}
			if _, ok := allowed[sel.Sel.Name]; !ok {
				t.Errorf("%s calls b.store.%s, which is not in the assistant's allow-list.\n"+
					"If it cannot change anything, add it with the reason. If it can, it does not belong here.",
					file, sel.Sel.Name)
			}
			return true
		})
	}
}

// The second claim: a staged step is always one of a fixed set of paths the console already
// offers. The model supplies ids and names; it never writes a method or a path. Without this
// the panel could draw "Add Alice to Approvers" over a button that did something else entirely,
// in the console's own voice — the permission on the endpoint would stop the worst of it, but
// an admin pressing Confirm holds most of those permissions already.
func TestStagedStepsAreOnTheAllowList(t *testing.T) {
	if !stepAllowed("PUT", "/api/scopes/7") || !stepAllowed("POST", "/api/approval-roles/3/members") {
		t.Fatal("the allow-list does not admit the steps the tools build")
	}
	for _, bad := range [][2]string{
		{"DELETE", "/api/approval-roles/3"},     // deliberately not offered
		{"DELETE", "/api/org"},                  // nothing may reach this
		{"POST", "/api/scopes/7/connections/2"}, // reach is granted on the channel's own page
		{"PUT", "/api/settings"},
		{"PUT", "/api/scopes/7/../../org"},
		{"GET", "/api/scopes/7"},
	} {
		if stepAllowed(bad[0], bad[1]) {
			t.Errorf("the allow-list admits %s %s", bad[0], bad[1])
		}
	}
	// And staging refuses rather than silently dropping a step, so a card never does less than
	// it says.
	c := &consoleCall{}
	err := c.stage(proposal{Steps: []proposalStep{{Method: "DELETE", Path: "/api/org"}}})
	if err == nil {
		t.Fatal("an off-list step was staged")
	}
	if len(c.proposals) != 0 {
		t.Error("a refused proposal was kept anyway")
	}
}

// No console tool shares a name with a Slack one. The two registries cannot be mixed — the Run
// signatures differ, so a tool in the wrong one will not compile — but they write to the same
// tool_calls table, which is what Activity renders. Two different tools under one name make
// that page lie.
func TestConsoleToolsAreNotAgentTools(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	a := NewAgent(Config{Timezone: "UTC"}, f.b.agent.llm, f.b.slacks, f.st, nil, f.b.resolver, nil, f.b.settings)
	for _, ct := range f.b.consoleTools(f.consoleCallFor(PermScopesManage, PermConnView, PermApproversManage)) {
		if _, clash := a.tools[ct.Name]; clash {
			t.Errorf("%q is registered on the Slack agent as well as the console", ct.Name)
		}
	}
}

// ---- the registry follows the caller, not the endpoint ----

// The endpoint is open to any signed-in member, so every permission question is answered by
// what the caller is offered. A resource whose page they cannot open is not in the enum, and a
// propose_ tool whose endpoint would refuse them is not in the list — absent rather than
// present and refusing, because a tool the model can see but cannot call costs a whole round to
// discover.
func TestConsoleToolsFollowTheCallersPermissions(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})

	viewer := f.consoleCallFor(PermActivityView)
	res := f.resources(viewer)
	for _, open := range []string{"channels", "channel", "models", "overview", "activity"} {
		if !res[open] {
			t.Errorf("a member holding activity.view was not offered %q", open)
		}
	}
	for _, shut := range []string{"audit", "bundles", "settings", "jobs", "artifacts", "routines", "access_requests"} {
		if res[shut] {
			t.Errorf("a member holding only activity.view was offered %q", shut)
		}
	}
	if names := f.toolNames(viewer); names["propose_channel_settings"] || names["propose_approval_change"] {
		t.Error("a member who may change nothing was offered a propose tool")
	}

	// Each permission opens exactly its own resource, and nothing else.
	for perm, want := range map[string]string{
		PermAuditView: "audit", PermConnView: "bundles", PermSettingsManage: "settings",
		PermJobsView: "jobs", PermArtifactsView: "artifacts", PermRoutinesManage: "routines",
		PermAccessView: "access_requests",
	} {
		if !f.resources(f.consoleCallFor(perm))[want] {
			t.Errorf("holding %s did not open %q", perm, want)
		}
	}
	if !f.toolNames(f.consoleCallFor(PermScopesManage))["propose_channel_settings"] {
		t.Error("scopes.manage did not offer the channel proposal tool")
	}
	if !f.toolNames(f.consoleCallFor(PermApproversManage))["propose_approval_change"] {
		t.Error("approvers.manage did not offer the approval proposal tool")
	}

	// And the same rule inside a resource both may read: the settings are open, the names of
	// the credentials behind them are not.
	c := f.consoleCallFor(PermScopesManage)
	out, err := f.tool(c, "read_console").Run(context.Background(), c, args(map[string]any{"resource": "channel", "id": "C1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hidden") {
		t.Errorf("a caller without connections.view was shown what the channel can reach:\n%s", out)
	}

	// A resource this caller cannot read is refused by name, and the refusal says what they can
	// read instead — the model asked for a page it cannot see, and the useful answer is which
	// ones it can.
	if _, err := f.tool(viewer, "read_console").Run(context.Background(), viewer, args(map[string]any{"resource": "audit"})); err == nil {
		t.Error("a member without audit.view read the audit log")
	}
}

// ---- staging: channels ----

// A proposal is checked with the save's own rule, so a Confirm button is never put in front of
// somebody for a change that would fail under their hand.
func TestProposalIsRefusedWhenItWouldFailOnConfirm(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	c := f.consoleCallFor(PermScopesManage)
	tool := f.tool(c, "propose_channel_settings")

	for _, tc := range []struct {
		name    string
		changes map[string]string
		want    string
	}{
		{"a budget that is not a number", map[string]string{"monthly_budget_usd": "lots"}, "number of dollars"},
		{"a negative budget", map[string]string{"monthly_budget_usd": "-5"}, "negative"},
		{"member edits that is not one of the three", map[string]string{"member_edits": "yes"}, "inherit, allow or block"},
		{"read_all that is not one of the three", map[string]string{"read_all": "maybe"}, "inherit, on or off"},
		{"email_intake that is not one of the three", map[string]string{"email_intake": "sure"}, "inherit, on or off"},
		{"rounds that are not a whole number", map[string]string{"max_tool_rounds": "abc"}, "whole number"},
		{"allow rules that are not a JSON array", map[string]string{"allow_rules": "be nice"}, "JSON array"},
		{"an allow rule longer than the cap", map[string]string{"allow_rules": `["` + strings.Repeat("x", 1100) + `"]`}, "at most 1024"},
		{"a repository nothing here can open", map[string]string{"default_repo": "acme/secrets"}, "not connected here"},
		{"a field that is not a channel setting", map[string]string{"nickname": "ops"}, "not a channel setting"},
		{"a field that is attached elsewhere", map[string]string{"bundle_ids": "3"}, "not a channel setting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.Run(context.Background(), c, proposeChannel("C1", tc.changes)); err == nil {
				t.Fatalf("accepted %v", tc.changes)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal should say %q, said %q", tc.want, err)
			}
		})
	}
	if len(c.proposals) != 0 {
		t.Errorf("a refused proposal was staged anyway: %+v", c.proposals)
	}
}

// The other half of that promise, and the only thing that catches the two validators drifting
// apart: every step the assistant is willing to stage is one its endpoint actually takes.
func TestProposalStepsAreAcceptedByTheirEndpoints(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	ctx := context.Background()
	tier := f.seedTier("Seniors", 2)
	c := f.consoleCallFor(PermScopesManage, PermApproversManage, PermConnView)

	stage := func(tool string, a json.RawMessage) proposal {
		t.Helper()
		before := len(c.proposals)
		if _, err := f.tool(c, tool).Run(ctx, c, a); err != nil {
			t.Fatalf("%s refused a valid proposal: %v", tool, err)
		}
		if len(c.proposals) != before+1 {
			t.Fatalf("%s staged nothing", tool)
		}
		return c.proposals[len(c.proposals)-1]
	}

	props := []proposal{
		stage("propose_channel_settings", proposeChannel("C1", map[string]string{
			"monthly_budget_usd": "12.5", "max_tool_rounds": "9", "read_all": "on",
			"member_edits": "block", "email_intake": "off",
			"allow_rules": `["Filing tickets in ClickUp is expected."]`, "instructions": "Answer briefly.",
		})),
		stage("propose_approval_change", args(map[string]any{"action": "create_tier", "name": "Juniors", "rank": 1})),
		stage("propose_approval_change", args(map[string]any{"action": "rename_tier", "tier_id": tier.ID, "name": "Senior approvers"})),
	}

	for _, p := range props {
		for _, s := range p.Steps {
			body := "{}"
			if s.Body != nil {
				raw, _ := json.Marshal(s.Body)
				body = string(raw)
			}
			code, out := f.call(s.Method, s.Path, body)
			if code != 200 {
				t.Errorf("%s %s was staged but the endpoint refused it: %d %v", s.Method, s.Path, code, out)
			}
		}
	}

	sc, err := f.st.ChannelScope(ctx, f.org, "T1", "C1")
	if err != nil || sc == nil {
		t.Fatal(err)
	}
	if sc.MonthlyBudgetUSD != 12.5 || sc.MaxToolRounds != 9 || sc.ReadAll != "on" ||
		sc.MemberEdits != "block" || sc.EmailIntake != "off" || len(sc.AllowRules) != 1 {
		t.Errorf("the confirmed channel proposal did not land: %+v", sc)
	}
	after, err := f.st.ApprovalRole(ctx, f.org, tier.ID)
	if err != nil || after == nil {
		t.Fatal(err)
	}
	if after.Name != "Senior approvers" {
		t.Errorf("the confirmed rename did not land: %q", after.Name)
	}
}

// ---- staging: approvals ----

func TestApprovalProposalsAreCheckedBeforeTheyAreOffered(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	ctx := context.Background()
	tier := f.seedTier("Seniors", 2)
	c := f.consoleCallFor(PermApproversManage)
	tool := f.tool(c, "propose_approval_change")

	for _, tc := range []struct {
		name string
		a    map[string]any
		want string
	}{
		{"a tier in another organisation", map[string]any{"action": "rename_tier", "tier_id": tier.ID + 9999, "name": "x"}, "no approval tier"},
		{"a member who is neither an id nor an address", map[string]any{"action": "add_member", "tier_id": tier.ID, "member": "alice"}, "not a Slack user id"},
		{"removing somebody who is not in the tier", map[string]any{"action": "remove_member", "tier_id": tier.ID, "member": "U0123456"}, "not a member"},
		{"a bundle that does not exist here", map[string]any{"action": "attach_bundle", "tier_id": tier.ID, "bundle_id": 4242}, "no bundle"},
		{"a new tier with no name", map[string]any{"action": "create_tier"}, "needs a name"},
		{"deleting a tier, which is not offered", map[string]any{"action": "delete_tier", "tier_id": tier.ID}, "not something that can be proposed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.Run(ctx, c, args(tc.a)); err == nil {
				t.Fatalf("accepted %v", tc.a)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal should say %q, said %q", tc.want, err)
			}
		})
	}
	// Adding an approver needs a workspace to resolve them in — the endpoint says so, so the
	// tool has to say so too rather than drawing a card that cannot be pressed.
	if _, err := tool.Run(ctx, c, args(map[string]any{"action": "add_member", "tier_id": tier.ID, "member": "U0123456"})); err == nil {
		t.Error("an approver was proposed with no workspace connected")
	} else if !strings.Contains(err.Error(), "connect a Slack workspace") {
		t.Errorf("the refusal should name the missing workspace, said %q", err)
	}
	if len(c.proposals) != 0 {
		t.Errorf("a refused proposal was staged anyway: %+v", c.proposals)
	}
}

// A channel in another organisation is not a channel this request can name. The assistant is a
// second read surface over the same data as the console, with its own entry points that no
// compiler links to the page queries, so the tenancy check has to be in the tool.
func TestAssistantRefusesAnotherOrgsScope(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	ctx := context.Background()
	other, err := f.st.UpsertChannelScope(ctx, f.org+1, "T9", "C9", "theirs", false)
	if err != nil {
		t.Fatal(err)
	}
	c := f.consoleCallFor(PermScopesManage)

	if _, err := f.tool(c, "read_console").Run(ctx, c, args(map[string]any{"resource": "channel", "id": "C9"})); err == nil {
		t.Error("read_console reached another organisation's channel")
	}
	if _, err := f.tool(c, "propose_channel_settings").Run(ctx, c, proposeChannel("C9", map[string]string{"read_all": "on"})); err == nil {
		t.Error("a proposal reached another organisation's channel")
	}
	// And the page context cannot smuggle one in either: a scope id from another tenant simply
	// does not resolve, so the turn runs as though no channel were selected.
	body, _ := json.Marshal(assistantRequest{Question: "what is this?", ScopeID: other.ID})
	if w := f.send(httptest.NewRequest("POST", "/api/assistant", strings.NewReader(string(body)))); w.Code != 200 {
		t.Fatalf("expected the turn to run without the foreign channel, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- attachments ----

// A file reaches the model as part of the question and is stored nowhere: it is context for one
// turn, not a document the organisation now owns.
func TestAttachedFilesReachTheModel(t *testing.T) {
	llm := &fakeLLM{answer: "It is a runbook."}
	f := newAssist(t, RoleAdmin, llm)
	code, reply := f.askWithFile("what is this?", "runbook.md", []byte("# Restarting the extractor\nRun make deploy."))
	if code != 200 {
		t.Fatalf("turn failed: %d", code)
	}
	if len(reply.Files) != 1 || reply.Files[0].Kind != "text" {
		t.Fatalf("the reply did not report the attachment: %+v", reply.Files)
	}
	if !strings.Contains(llm.prompt(), "Restarting the extractor") {
		t.Error("the file's contents never reached the model")
	}
	if !strings.Contains(llm.prompt(), "runbook.md") {
		t.Error("the model was not told the file's name, so it cannot cite it")
	}
	if n := countDocuments(t, f); n != 0 {
		t.Errorf("an attachment was stored as a document: %d", n)
	}
}

// A file with nothing a model can read is named and skipped, not silently dropped — and the
// turn still runs, because "I could not read that" is an answer and a 400 is not.
func TestAnUnreadableAttachmentIsNamedNotDropped(t *testing.T) {
	llm := &fakeLLM{answer: "I could not read that."}
	f := newAssist(t, RoleAdmin, llm)
	code, reply := f.askWithFile("what is this?", "scan.bin", []byte{0x00, 0x01, 0x02, 0xff, 0xfe})
	if code != 200 {
		t.Fatalf("an unreadable file failed the request: %d", code)
	}
	if len(reply.Files) != 1 || reply.Files[0].Kind != "skipped" || reply.Files[0].Note == "" {
		t.Fatalf("the skip was not reported with a reason: %+v", reply.Files)
	}
	if !strings.Contains(llm.prompt(), "scan.bin") {
		t.Error("the model was not told a file it could not read had been attached")
	}
}

func countDocuments(t *testing.T, f *assistFix) int {
	t.Helper()
	docs, err := f.st.Documents(context.Background(), f.org)
	if err != nil {
		t.Fatal(err)
	}
	return len(docs)
}

// ---- what a turn costs ----

// failingLLM answers a tool call, takes the tokens for it, and then falls over. It is the shape
// that matters for spend: the round that fails is the one that has already been paid for.
type failingLLM struct {
	mu    sync.Mutex
	calls int
}

func (f *failingLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	first := f.calls == 1
	f.mu.Unlock()
	if !first {
		http.Error(w, `{"error":{"message":"upstream exploded"}}`, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "object": "chat.completion", "model": "test",
		"choices": []map[string]any{{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
			"role": "assistant", "content": "", "tool_calls": []map[string]any{{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "read_console", "arguments": `{"resource":"channels"}`},
			}},
		}}},
		"usage": map[string]any{"prompt_tokens": 900, "completion_tokens": 40},
	})
}

func TestAssistantLogsSpendOnAFailedTurn(t *testing.T) {
	f := newAssist(t, RoleAdmin, &failingLLM{})
	code, reply := f.ask("what channels are there?")
	if code != 200 {
		t.Fatalf("a failed turn should still answer with what it did, got %d", code)
	}
	if reply.Error == "" {
		t.Error("a failed turn reported no error")
	}
	if reply.TokensIn == 0 {
		t.Error("the reply did not report the tokens the failed turn spent")
	}
	if n := usageRows(t, f); n == 0 {
		t.Error("a turn that failed after spending tokens wrote no usage row, so it spent outside every budget")
	}
}

// The assistant's spend counts against the account and against no workspace: it happened in a
// browser tab, not in anybody's channel, and filing it under one would make that channel's
// budget answer for it.
func TestAssistantCountsItsSpend(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "Three channels."})
	if code, _ := f.ask("how many channels?"); code != 200 {
		t.Fatalf("turn failed: %d", code)
	}
	ctx := context.Background()
	team, err := f.st.MonthSpend(ctx, f.org, "T1", "")
	if err != nil {
		t.Fatal(err)
	}
	if n := usageRows(t, f); n != 1 {
		t.Fatalf("expected one usage row filed under the assistant, got %d", n)
	}
	if team != 0 {
		t.Errorf("the assistant's spend was charged to a workspace: %v", team)
	}
}

func usageRows(t *testing.T, f *assistFix) int {
	t.Helper()
	var n int
	f.st.db.QueryRowContext(context.Background(),
		`select count(*) from usage where org_id=? and channel=?`, f.org, assistantChannel).Scan(&n)
	return n
}

// The rate limit counts this person, in this organisation, asking the assistant — and nothing
// else. TurnsByUserSince could not do this: it is keyed on team_id and user_id with no
// organisation in it.
func TestAssistantRateLimitIsPerOrgAndPerPerson(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	ctx := context.Background()

	// Noise that must not count: another organisation's rows, and this person's Slack turns.
	for i := 0; i < assistantPerHour; i++ {
		f.st.LogUsageBy(ctx, f.org+1, "", assistantChannel, "", "actor", "test", Usage{In: 10, Out: 10, CostUSD: 0})
		f.st.LogUsageBy(ctx, f.org, "T1", "C1", "thread", "actor", "test", Usage{In: 10, Out: 10, CostUSD: 0})
	}
	if n := f.st.ConsoleTurnsSince(ctx, f.org, "actor", time.Hour); n != 0 {
		t.Fatalf("another organisation's rows, or this person's Slack turns, were counted: %d", n)
	}
	for i := 0; i < assistantPerHour; i++ {
		f.st.LogUsageBy(ctx, f.org, "", assistantChannel, "", "actor", "test", Usage{In: 10, Out: 10, CostUSD: 0})
	}
	if n := f.st.ConsoleTurnsSince(ctx, f.org, "actor", time.Hour); n != assistantPerHour {
		t.Fatalf("this person's own assistant turns were not counted: %d", n)
	}

	r := httptest.NewRequest("POST", "/api/assistant", strings.NewReader(`{"question":"hello"}`))
	r = r.WithContext(context.WithValue(r.Context(), userKey, &AdminUser{ID: 1, OrgID: f.org, PublicID: "actor"}))
	w := httptest.NewRecorder()
	f.b.handleAssistant(w, r)
	if w.Code != 429 {
		t.Fatalf("a rate-limited person should be refused, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- audit ----

// Two rows for one change, and they say different things: a model suggested this, and then a
// named person applied it. A single row would leave the console unable to tell a settings edit
// somebody typed from one they accepted on a card.
func TestAssistantAuditsTheProposalAndTheConfirmSeparately(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{
		toolCall: "propose_channel_settings",
		toolArgs: `{"channel":"C1","changes":{"read_all":"on"}}`,
		answer:   "I've proposed turning that on. Nothing has changed yet.",
	})
	ctx := context.Background()
	code, reply := f.ask("make it read every message")
	if code != 200 {
		t.Fatalf("turn failed: %d", code)
	}
	if len(reply.Proposals) != 1 {
		t.Fatalf("expected one staged proposal, got %d (%s)", len(reply.Proposals), reply.Error)
	}
	p := reply.Proposals[0]

	// Nothing was written by the proposal itself.
	sc, _ := f.st.ChannelScope(ctx, f.org, "T1", "C1")
	if sc.ReadAll == "on" {
		t.Fatal("the proposal changed the setting without anybody confirming it")
	}

	step := p.Steps[0]
	body, _ := json.Marshal(step.Body)
	if code, out := f.call(step.Method, step.Path, string(body)); code != 200 {
		t.Fatalf("confirm failed: %d %v", code, out)
	}

	events, err := f.st.AuditEvents(ctx, f.org, AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var proposed, updated *AuditEvent
	for i := range events {
		switch events[i].Action {
		case "assistant.proposed":
			proposed = &events[i]
		case "scope.updated":
			updated = &events[i]
		}
	}
	if proposed == nil {
		t.Fatal("the proposal was never audited")
	}
	if updated == nil {
		t.Fatal("the confirm was never audited")
	}
	if !strings.Contains(string(proposed.Details), p.ID) {
		t.Errorf("the proposal row does not carry its id: %s", proposed.Details)
	}
	if !strings.Contains(string(updated.Details), p.ID) {
		t.Errorf("the confirm row does not carry the proposal id, so the two cannot be tied together: %s", updated.Details)
	}
}

// ---- the endpoint this all leans on ----

// PUT /api/scopes/{id} used to throw away the error from ParseFloat and store 0, so "lots" read
// as success and quietly switched the channel's budget off. The assistant makes that worse by
// composing the value, but the bug was always the endpoint's.
func TestScopePutRejectsANonNumericBudget(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	ctx := context.Background()
	sc, _ := f.st.ChannelScope(ctx, f.org, "T1", "C1")
	f.st.SetScopeBudget(ctx, f.org, sc.ID, 20)

	code, out := f.call("PUT", fmt.Sprintf("/api/scopes/%d", sc.ID), `{"monthly_budget_usd":"lots"}`)
	if code != 400 {
		t.Fatalf("expected 400, got %d: %v", code, out)
	}
	after, _ := f.st.ChannelScope(ctx, f.org, "T1", "C1")
	if after.MonthlyBudgetUSD != 20 {
		t.Errorf("a refused budget still changed the stored one: %v", after.MonthlyBudgetUSD)
	}
}

// A form carrying one good field and one bad one used to store the good one and then 400,
// leaving half a change applied and the page showing the other half.
func TestScopePutIsAllOrNothing(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	ctx := context.Background()
	sc, _ := f.st.ChannelScope(ctx, f.org, "T1", "C1")

	if code, _ := f.call("PUT", fmt.Sprintf("/api/scopes/%d", sc.ID), `{"read_all":"on","max_tool_rounds":"abc"}`); code != 400 {
		t.Fatalf("expected 400, got %d", code)
	}
	after, _ := f.st.ChannelScope(ctx, f.org, "T1", "C1")
	if after.ReadAll == "on" {
		t.Error("the good half of a refused request was written anyway")
	}
}

// ---- the product's own documentation ----

// The assistant is asked two kinds of question and only one is about the organisation's data.
// "How do I set this up so somebody can approve a repository invite" is in the guide, and a
// model asked it without the guide answers out of how such products usually work — confidently,
// and wrongly about the details somebody then goes and tries.
func TestGuideSearchFindsTheRightSection(t *testing.T) {
	for _, tc := range []struct{ query, wantFile, wantTitle string }{
		{"plan pricing budget", "plans.md", ""},
		{"path prefixes and methods on a connection", "connections.md", "Connections"},
		{"approve a github repository collaborator", "connections.md", "Repositories"},
		{"monorepo more than one package", "fix-jobs.md", "Monorepos"},
	} {
		out := searchGuide(tc.query)
		if !strings.Contains(out, tc.wantFile) {
			t.Errorf("%q did not reach %s:\n%s", tc.query, tc.wantFile, firstHeadings(out))
		}
		if tc.wantTitle != "" && !strings.Contains(out, tc.wantTitle) {
			t.Errorf("%q did not reach a section titled like %q:\n%s", tc.query, tc.wantTitle, firstHeadings(out))
		}
	}
}

// A "#" inside a fenced block is a shell comment, and the guide is mostly runbook. Treating them
// as headings cut plans.md into fragments titled with whole sentences about one command, which
// ranked for everything and answered nothing.
func TestGuideDoesNotSplitOnShellComments(t *testing.T) {
	for _, s := range loadGuide() {
		if strings.HasPrefix(s.title, "Move one to pro") || strings.Contains(s.title, "omit ?email=") {
			t.Errorf("a shell comment became a section heading: %q in %s", s.title, s.file)
		}
		if len(s.title) > 90 {
			t.Errorf("%s has a heading that reads like a sentence, so the split is wrong: %q", s.file, s.title)
		}
	}
}

// A question that matches nothing gets the contents rather than silence: "here is what the
// documentation covers" is an answer, and silence is what sends a model back to inventing.
func TestGuideSearchNeverAnswersWithNothing(t *testing.T) {
	out := searchGuide("zzzz quibbling marmoset")
	if !strings.Contains(out, ".md") {
		t.Errorf("an unmatched query returned no way forward:\n%s", out)
	}
}

func firstHeadings(out string) string {
	var titles []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "## ") {
			titles = append(titles, line)
		}
	}
	return strings.Join(titles, "\n")
}

// ---- everything it can do, actually doing it ----

// Every resource read_console offers, read for real. Sixteen readers each touch a different
// table and a different struct, and a renamed field or a changed signature in any one of them
// is a tool that returns an error to the model instead of an answer — which the model then
// apologises for in the panel rather than failing loudly anywhere a test would see.
//
// An empty organisation is the point: these have to work before anybody has done anything, and
// "no routines yet" is an answer while a nil dereference is not.
func TestEveryResourceReadsWithoutError(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	ctx := context.Background()
	f.seedTier("Seniors", 2)

	c := f.consoleCallFor(
		PermConnView, PermAccessView, PermActivityView, PermAuditView,
		PermRoutinesManage, PermJobsView, PermArtifactsView, PermSettingsManage,
		PermScopesManage, PermApproversManage,
	)
	read := f.tool(c, "read_console")

	names := f.resources(c)
	if len(names) < 14 {
		t.Fatalf("an admin was offered only %d resources: %v", len(names), names)
	}
	for name := range names {
		t.Run(name, func(t *testing.T) {
			// "channel" is the one that needs an id; everything else answers bare.
			a := map[string]any{"resource": name}
			if name == "channel" {
				a["id"] = "C1"
			}
			out, err := read.Run(ctx, c, args(a))
			if err != nil {
				t.Fatalf("%s failed: %v", name, err)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("%s answered with nothing at all", name)
			}
		})
	}
}

// The whole set, in one place, so adding a tool is a decision somebody makes rather than
// something that happens. An admin holding everything gets exactly these.
func TestTheToolsAnAdminIsOffered(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	want := []string{"read_console", "search_guide", "propose_channel_settings", "propose_approval_change"}
	got := f.toolNames(f.consoleCallFor(PermScopesManage, PermApproversManage, PermConnView))
	for _, w := range want {
		if !got[w] {
			t.Errorf("an admin was not offered %q", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("the tool set has changed: %v", got)
	}
}

// A turn end to end through the real route, with the model calling a tool and answering from
// what it got back — the shape every question in the panel takes.
func TestATurnRunsThroughTheRealRoute(t *testing.T) {
	llm := &fakeLLM{
		toolCall: "read_console",
		toolArgs: `{"resource":"channels"}`,
		answer:   "There is one channel, #ops.",
	}
	f := newAssist(t, RoleAdmin, llm)
	code, reply := f.ask("what channels are there?")
	if code != 200 {
		t.Fatalf("turn failed: %d", code)
	}
	if reply.Error != "" {
		t.Fatalf("turn reported an error: %s", reply.Error)
	}
	if reply.Reply == "" {
		t.Error("the turn answered with nothing")
	}
	if len(reply.Tools) != 1 || reply.Tools[0].Name != "read_console" || !reply.Tools[0].OK {
		t.Fatalf("the tool call was not reported back for the panel to show: %+v", reply.Tools)
	}
	if !strings.Contains(reply.Tools[0].Result, "ops") {
		t.Errorf("the tool result the panel will show does not contain the answer: %q", reply.Tools[0].Result)
	}
	if reply.Model == "" || reply.Rounds == 0 {
		t.Errorf("the panel's meta line would be blank: model=%q rounds=%d", reply.Model, reply.Rounds)
	}
}

// The page the question was asked from reaches the model, and so does the channel the page had
// selected — the two together are what let somebody ask "what can this reach" without naming
// anything. Without the prose the model has a default it does not know about and asks which
// channel; without the resolved row the tools have nothing to act on.
func TestPageContextReachesTheModel(t *testing.T) {
	llm := &fakeLLM{answer: "ok"}
	f := newAssist(t, RoleAdmin, llm)
	sc, err := f.st.ChannelScope(context.Background(), f.org, "T1", "C1")
	if err != nil || sc == nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(assistantRequest{
		Question: "what can this reach?", Path: "/workspaces/", Page: "Workspaces", ScopeID: sc.ID,
	})
	if w := f.send(httptest.NewRequest("POST", "/api/assistant", strings.NewReader(string(body)))); w.Code != 200 {
		t.Fatalf("turn failed: %d %s", w.Code, w.Body.String())
	}
	prompt := llm.prompt()
	for _, want := range []string{"Workspaces", "/workspaces/", "#ops", "C1", "do not ask which one"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the model was not told %q:\n%s", want, truncate(prompt, 600))
		}
	}
}

// A model the person picked in the panel is the one the turn runs on, and one the provider does
// not offer is refused before anything is spent rather than after.
func TestTheModelPickerChoosesTheModel(t *testing.T) {
	llm := &fakeLLM{answer: "ok"}
	f := newAssist(t, RoleAdmin, llm)
	// On the deployment's own key the assistant may pick from the models the org offers its
	// channels, the same list a routine picks from — so a member cannot point the shared key at the
	// dearest model in the catalogue. "test" is put on that list so the pick is a real one.
	if err := f.st.PutSettings(context.Background(), f.org, map[string]string{"channel_models": "test"}); err != nil {
		t.Fatal(err)
	}
	f.b.settings.Invalidate(f.org)
	body, _ := json.Marshal(assistantRequest{Question: "hello", Model: "test"})
	if w := f.send(httptest.NewRequest("POST", "/api/assistant", strings.NewReader(string(body)))); w.Code != 200 {
		t.Fatalf("a picked model was refused: %d %s", w.Code, w.Body.String())
	}
	if llm.model() != "test" {
		t.Errorf("the turn ran on %q rather than the picked model", llm.model())
	}
	// A model the org does not offer is refused rather than run on the shared key.
	body, _ = json.Marshal(assistantRequest{Question: "hello", Model: "openai/o1-pro"})
	if w := f.send(httptest.NewRequest("POST", "/api/assistant", strings.NewReader(string(body)))); w.Code == 200 {
		t.Errorf("a model the org does not offer was accepted on the shared key: %s", w.Body.String())
	}
}

// ---- the transcript ----

// What was asked and what came back, on Activity. The usage row and the tool calls already said
// a console turn happened, on which model and for how much — everything except the two things
// somebody reviewing it actually wants to read.
func TestTheTranscriptIsRecorded(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{
		toolCall: "read_console",
		toolArgs: `{"resource":"channels"}`,
		answer:   "There is one channel, #ops.",
	})
	ctx := context.Background()
	body, _ := json.Marshal(assistantRequest{
		Question: "what channels are there?", Page: "Workspaces", Conversation: "conv-1",
	})
	if w := f.send(httptest.NewRequest("POST", "/api/assistant", strings.NewReader(string(body)))); w.Code != 200 {
		t.Fatalf("turn failed: %d %s", w.Code, w.Body.String())
	}

	ts, err := f.st.AssistantTurns(ctx, f.org, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 {
		t.Fatalf("expected one recorded turn, got %d", len(ts))
	}
	got := ts[0]
	if got.Question != "what channels are there?" || !strings.Contains(got.Reply, "#ops") {
		t.Errorf("the transcript does not carry the conversation: %+v", got)
	}
	if got.Page != "Workspaces" || got.Conversation != "conv-1" {
		t.Errorf("the transcript lost where it was asked: page=%q conversation=%q", got.Page, got.Conversation)
	}
	if got.ToolCalls != 1 || got.TokensIn == 0 || got.ActorName == "" {
		t.Errorf("the transcript lost what the turn did: %+v", got)
	}
	// Searchable, which is the point of putting it on a page rather than in a log file.
	hits, err := f.st.AssistantTurns(ctx, f.org, "channels", 0)
	if err != nil || len(hits) != 1 {
		t.Errorf("searching the transcript found %d rows (%v)", len(hits), err)
	}
	if miss, _ := f.st.AssistantTurns(ctx, f.org, "nothing like this", 0); len(miss) != 0 {
		t.Errorf("a search that should match nothing returned %d rows", len(miss))
	}
	// And it is another organisation's business and nobody else's.
	if other, _ := f.st.AssistantTurns(ctx, f.org+1, "", 0); len(other) != 0 {
		t.Errorf("another organisation could read this transcript: %d rows", len(other))
	}
}

// A turn that failed is recorded too — those are the ones worth reading afterwards.
func TestAFailedTurnIsStillRecorded(t *testing.T) {
	f := newAssist(t, RoleAdmin, &failingLLM{})
	if code, _ := f.ask("what channels are there?"); code != 200 {
		t.Fatalf("turn failed the request: %d", code)
	}
	ts, err := f.st.AssistantTurns(context.Background(), f.org, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 {
		t.Fatalf("a failed turn left no record: %d rows", len(ts))
	}
	if ts[0].Error == "" {
		t.Error("the record does not say the turn failed")
	}
}

// A channel reads with one # however its scope was named, and each turn in the activity says
// where it happened — the channel, the console assistant, the playground — instead of "in —",
// which sent the assistant to read every channel in turn and still left its own turns unplaced.
func TestTheAssistantNamesWhereEachTurnHappened(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	ctx := context.Background()
	if _, err := f.st.UpsertChannelScope(ctx, f.org, "T1", "C2", "#deploys", false); err != nil {
		t.Fatal(err)
	}
	c := f.consoleCallFor(PermActivityView)
	read := func(resource string) string {
		t.Helper()
		out, err := f.tool(c, "read_console").Run(ctx, c, args(map[string]any{"resource": resource}))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if chans := read("channels"); strings.Contains(chans, "##") || !strings.Contains(chans, "#ops (") || !strings.Contains(chans, "#deploys (") {
		t.Errorf("the channel list names them with one # each:\n%s", chans)
	}

	for _, ch := range []string{"C2", assistantChannel, playgroundPrefix + "abc"} {
		f.st.LogUsageBy(ctx, f.org, "T1", ch, "", "U1", "test", Usage{In: 10, Out: 5})
	}
	act := read("activity")
	for _, want := range []string{"in the console assistant (", "in the playground (", "in C2 ("} {
		if !strings.Contains(act, want) {
			t.Errorf("the activity does not say %q:\n%s", want, act)
		}
	}
	if strings.Contains(act, "in — (") {
		t.Errorf("a turn is still placed nowhere:\n%s", act)
	}
}
