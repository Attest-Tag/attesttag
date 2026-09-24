package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The playground promises that nothing it runs changes data. Three things in a channel exist to let
// a write through with nobody pressing anything — a connection whose writes are automatic, an
// allow rule, and a routine set to confirm its own — and each of them used to be consulted before
// the preview was. Every test below runs the same write twice: as a preview, where nothing may
// reach the service, and as the channel, where it does, so the preview is shown to be what stops it.

// approvingChecker stands in for the model that reads allow rules, approving rule 1 every time and
// counting how often it was asked.
func approvingChecker(t *testing.T) (Config, *atomic.Int32) {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "x", "object": "chat.completion", "created": 1, "model": "m",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": `{"approved": true, "rule": 1, "why": "covered"}`}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	t.Cleanup(srv.Close)
	return Config{LLMBaseURL: srv.URL, LLMKey: "k", Model: "m"}, &asked
}

// clickupConn is a ClickUp connection in the test bot's store, with its writes set as given.
func clickupConn(t *testing.T, b *Bot, writes string) *Connection {
	t.Helper()
	c, sec, err := b.buildConnection(&connectionInput{Name: "ClickUp", Preset: "clickup", CredType: "bearer",
		Secret: &Secret{Token: "tok"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.secretEnc, err = b.sealSecret(sec); err != nil {
		t.Fatal(err)
	}
	c.Status, c.Writes = "active", writes
	return c
}

func TestAPreviewSendsNoHTTPWrite(t *testing.T) {
	ctx := context.Background()
	var sent []string
	b := repoTestBot(t, func(r *http.Request) (int, string) {
		sent = append(sent, r.Method+" "+r.URL.Path)
		return 200, `{"id":"86d4","url":"https://app.clickup.com/t/86d4"}`
	})
	cfg, asked := approvingChecker(t)
	a := &Agent{proxy: b.proxy, store: b.store, llm: NewLLM(cfg), settings: newSettingsCache(b.store, cfg)}
	create := ProxyRequest{Method: "POST", URL: "https://api.clickup.com/api/v2/list/1/task", Body: `{"name":"x"}`}

	for _, tc := range []struct {
		name, writes string
		rules        []string
		inChannel    string
	}{
		{"writes automatic", "auto", nil, "run without asking"},
		{"an allow rule covering it", "", []string{"Creating ClickUp tasks is expected."}, "unless an allow rule covers it"},
	} {
		conn := clickupConn(t, b, tc.writes)
		call := func(preview bool) *Call {
			return &Call{OrgID: orgID, TeamID: "T1", Channel: "C1", ThreadTS: "1.1", UserID: "U1", Preview: preview,
				Access: &Access{Rules: []Rule{{Conn: conn, Rank: 2}}, AllowRules: tc.rules}}
		}
		sent = nil
		before := asked.Load()
		c := call(true)
		out, err := a.proxied(ctx, c, create)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(sent) != 0 {
			t.Errorf("%s: a preview sent %v", tc.name, sent)
		}
		if !strings.Contains(out, "NOT sent") || !strings.Contains(out, tc.inChannel) || len(c.previewHeld) != 1 {
			t.Errorf("%s: the held write should be reported with what the channel would do: %q (held %v)", tc.name, out, c.previewHeld)
		}
		if n := asked.Load() - before; n != 0 {
			t.Errorf("%s: a preview asked the allow-rule checker %d times; it holds before any rule is read", tc.name, n)
		}
		// A read still goes through: trying a channel's reads is what a preview is for.
		if _, err := a.proxied(ctx, c, ProxyRequest{Method: "GET", URL: "https://api.clickup.com/api/v2/team"}); err != nil || len(sent) != 1 {
			t.Errorf("%s: a preview's read did not go through: %v %v", tc.name, sent, err)
		}

		// The channel itself sends the same write without asking anybody, which is the point of
		// the setting — and what the preview must not.
		sent = nil
		if out, err := a.proxied(ctx, call(false), create); err != nil || len(sent) != 1 || !strings.HasPrefix(out, "HTTP 200") {
			t.Errorf("%s: the channel should run it without asking: %q %v %v", tc.name, out, sent, err)
		}
	}
}

// countingMCPServer is fakeMCPServer with a count of the tools actually called on it.
func countingMCPServer(t *testing.T, calls *atomic.Int32, tools ...string) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
	for _, name := range tools {
		srv.AddTool(&mcp.Tool{Name: name, Description: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(func() {
		ts.CloseClientConnections()
		ts.Close()
	})
	relaxMCPOutbound(t)
	return ts
}

func TestAPreviewRunsNoMCPWrite(t *testing.T) {
	ctx := context.Background()
	var called atomic.Int32
	ts := countingMCPServer(t, &called, "create_ticket")
	hub, st, seal := mcpTestHub(t)
	cfg, asked := approvingChecker(t)
	a := &Agent{store: st, tools: map[string]Tool{}, mcp: hub, settings: newSettingsCache(st, cfg), llm: NewLLM(cfg),
		slacks: testRegistry(&Chat{}), loc: time.UTC}

	for i, tc := range []struct {
		name, writes string
		rules        []string
	}{
		{"writes automatic", "auto", nil},
		{"an allow rule covering it", "", []string{"Filing tickets is expected."}},
	} {
		conn := &Connection{ID: int64(10 + i), Name: "Desk", CredType: "mcp", Writes: tc.writes, AllowedHosts: []string{"example.com"},
			secretEnc: seal(Secret{MCPURL: ts.URL, Token: "tok"})}
		call := func(preview bool) *Call {
			return &Call{OrgID: orgID, Channel: "C1", ThreadTS: "1.1", UserID: "U1", Kind: "channel", Preview: preview,
				Session: &Session{}, Streamer: &Streamer{failed: true},
				Access: &Access{Rules: []Rule{{Conn: conn}}, ToolPacks: map[string]bool{}, AllowRules: tc.rules}}
		}
		before, checks := called.Load(), asked.Load()
		c := call(true)
		out := a.runTool(ctx, c, "desk_create_ticket", `{"title":"x"}`)
		if n := called.Load() - before; n != 0 {
			t.Errorf("%s: a preview ran the tool %d times", tc.name, n)
		}
		if !strings.Contains(out, "NOT sent") || len(c.previewHeld) != 1 {
			t.Errorf("%s: the held call should be reported: %q", tc.name, out)
		}
		if n := asked.Load() - checks; n != 0 {
			t.Errorf("%s: a preview asked the allow-rule checker %d times", tc.name, n)
		}
		before = called.Load()
		if out := a.runTool(ctx, call(false), "desk_create_ticket", `{"title":"x"}`); called.Load()-before != 1 {
			t.Errorf("%s: the channel should run it without asking: %q", tc.name, out)
		}
	}
}

// start_fix_job is not offered to a preview at all, and holds on its own account too: an allow
// rule that may dispatch jobs would otherwise start one from the playground.
func TestAPreviewStartsNoFixJob(t *testing.T) {
	h := newJobHarness(t, nil)
	ctx := context.Background()
	b := h.b
	b.store.PutSetting(ctx, orgID, "worker_allow_rules", "1")
	b.settings.Invalidate(orgID)
	cfg, asked := approvingChecker(t)
	a := &Agent{cfg: b.cfg, slacks: b.slacks, store: b.store, proxy: b.proxy, settings: b.settings, jobs: b.jobs,
		tools: map[string]Tool{}, llm: NewLLM(cfg)}
	conn, _ := b.store.Connection(ctx, orgID, h.connID)
	c := &Call{OrgID: orgID, TeamID: "T1", SL: a.slacks.Any(ctx), Channel: "C1", ThreadTS: "playground:1", UserID: "U1",
		Session: &Session{}, Preview: true,
		Access: &Access{Rules: []Rule{{Conn: conn, Rank: 2}}, DefaultRepo: "acme/app", ToolPacks: map[string]bool{},
			AllowRules: []string{"Fixing bugs in acme/app is expected."}}}
	if _, ok := a.toolsFor(ctx, c)["start_fix_job"]; ok {
		t.Error("a preview was offered start_fix_job")
	}

	out, err := a.fixJobTool(c).Run(ctx, c, json.RawMessage(`{"title":"Null ticket id","requirement":"Guard the retry path."}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "NOT sent") || len(c.previewHeld) != 1 {
		t.Errorf("the job should be reported as held: %q", out)
	}
	if jobs, _ := b.store.Jobs(ctx, orgID, JobFilter{}); len(jobs) != 0 || len(h.fd.launches) != 0 {
		t.Fatalf("a preview dispatched a fix job: %+v", jobs)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("a preview asked the allow-rule checker %d times", n)
	}
	if id, _, _ := b.store.LatestPendingWrite(ctx, orgID, "T1", "C1", "playground:1"); id != 0 {
		t.Error("a preview left a held job behind that no card will ever answer")
	}
}
