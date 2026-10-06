package worker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"attesttag/internal/app"
)

// fakeProvider is an OpenRouter-shaped chat completions endpoint that records what it was sent
// and answers with usage, streamed or not.
type fakeProvider struct {
	mu     sync.Mutex
	bodies []map[string]any
	auths  []string
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.mu.Unlock()
	if !strings.HasSuffix(r.URL.Path, "/api/v1/chat/completions") {
		http.NotFound(w, r)
		return
	}
	usage := `{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800},"cost":0.4}`
	if stream, _ := body["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"g1","provider":"DeepInfra","choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"g1","provider":"DeepInfra","choices":[],"usage":`+usage+`}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"id":"g2","provider":"Novita","choices":[{"message":{"content":"hi"}}],"usage":`+usage+`}`)
}

func startTestProxy(t *testing.T, budget float64, stop func()) (*llmProxy, *fakeProvider, *[]app.JobUsage) {
	t.Helper()
	fp := &fakeProvider{}
	up := httptest.NewServer(fp)
	t.Cleanup(up.Close)
	var mu sync.Mutex
	var reported []app.JobUsage
	p := newLLMProxy(app.JobLLMSecret{BaseURL: up.URL + "/api/v1", APIKey: "sk-or-v1-realkey"}, 7, []string{"deepinfra", "novita"}, budget,
		func(u app.JobUsage) { mu.Lock(); reported = append(reported, u); mu.Unlock() }, stop)
	p.openRouter = true // the fake upstream is not on openrouter.ai
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.close)
	return p, fp, &reported
}

func proxyCall(t *testing.T, p *llmProxy, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", p.BaseURL()+"/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// The engine holds a loopback token, never the provider key; every completion is sent with the
// job's session and provider order and asks for the bill; what comes back is metered per call.
func TestLLMProxyMetersAndKeepsTheKey(t *testing.T) {
	p, fp, reported := startTestProxy(t, 0, nil)
	if sec := p.Secret("z-ai/glm-5.3"); strings.Contains(sec.APIKey, "realkey") || !strings.HasPrefix(sec.BaseURL, "http://127.0.0.1:") || !strings.HasSuffix(sec.BaseURL, "/api/v1") {
		t.Fatalf("engine secret = %+v", sec)
	}
	if code, _ := proxyCall(t, p, "sk-or-v1-realkey", `{"model":"m"}`); code != http.StatusUnauthorized {
		t.Errorf("a caller without the job's token got %d", code)
	}
	code, out := proxyCall(t, p, p.token, `{"model":"m","stream":true,"messages":[]}`)
	if code != 200 || !strings.Contains(out, `"content":"hi"`) || !strings.Contains(out, "[DONE]") {
		t.Fatalf("stream relay: %d %q", code, out)
	}
	if code, out := proxyCall(t, p, p.token, `{"model":"m","messages":[],"provider":{"only":["z-ai"]}}`); code != 200 || !strings.Contains(out, "Novita") {
		t.Fatalf("plain relay: %d %q", code, out)
	}
	fp.mu.Lock()
	first, second, auths := fp.bodies[0], fp.bodies[1], fp.auths
	fp.mu.Unlock()
	for _, a := range auths {
		if a != "Bearer sk-or-v1-realkey" {
			t.Errorf("upstream saw %q, want the real key", a)
		}
	}
	if first["session_id"] != "attesttag-job-7" || first["usage"] == nil {
		t.Errorf("session and usage accounting not asked for: %v", first)
	}
	if so, _ := first["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("a stream must ask for its usage: %v", first)
	}
	if pr, _ := first["provider"].(map[string]any); pr == nil || len(pr["order"].([]any)) != 2 || pr["allow_fallbacks"] != true {
		t.Errorf("provider order: %v", first["provider"])
	}
	if pr, _ := second["provider"].(map[string]any); pr["only"] == nil || pr["order"] != nil {
		t.Errorf("the engine's own provider choice was overridden: %v", second["provider"])
	}
	tot := p.Total()
	if tot.In != 2000 || tot.Cached != 1600 || tot.Out != 100 || tot.CostUSD < 0.79 || tot.CostUSD > 0.81 {
		t.Errorf("total = %+v", tot)
	}
	if len(*reported) != 2 || (*reported)[0].Cached != 800 || (*reported)[0].CostUSD != 0.4 {
		t.Errorf("per-call reports: %+v", *reported)
	}
	if s := p.Summary(); !strings.Contains(s, "model calls: 2") || !strings.Contains(s, "80% from cache") || !strings.Contains(s, "DeepInfra 1") || !strings.Contains(s, "$0.8000 as billed") {
		t.Errorf("summary: %q", s)
	}
}

// At the budget the engine is stopped once and every later call is refused.
func TestLLMProxyStopsAtTheBudget(t *testing.T) {
	stops := 0
	p, _, _ := startTestProxy(t, 0.7, func() { stops++ })
	proxyCall(t, p, p.token, `{"model":"m"}`)
	if stops != 0 {
		t.Fatal("stopped under budget")
	}
	proxyCall(t, p, p.token, `{"model":"m"}`)
	if stops != 1 {
		t.Fatalf("stops = %d at $%.2f of $0.70", stops, p.Total().CostUSD)
	}
	if code, out := proxyCall(t, p, p.token, `{"model":"m"}`); code != http.StatusPaymentRequired || !strings.Contains(out, "budget") {
		t.Errorf("a call past the budget got %d %q", code, out)
	}
	if stops != 1 {
		t.Errorf("stopped %d times", stops)
	}
}

// pi's JSON lines: turns are counted and capped, tools reported, and usage taken from assistant
// messages, with cache reads counted inside the input as the job's usage does.
func TestPiStream(t *testing.T) {
	capped := 0
	st := &piStream{maxRounds: 2, capped: func() { capped++ }, rep: newReporter(newClient(Options{JobID: 1, BotURL: "http://127.0.0.1:1", Token: testToken}), newScrubber(), func(error) {})}
	for _, l := range []string{
		`{"type":"session","version":3,"id":"x","cwd":"/r"}`,
		`{"type":"turn_start"}`,
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"read","args":{"path":"app.py"}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","name":"read"}],"stopReason":"toolUse","usage":{"input":200,"output":20,"cacheRead":800,"cacheWrite":0,"cost":{"total":0.01}}}}`,
		`{"type":"turn_start"}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"SUMMARY: Fixed app.py."}],"stopReason":"stop","usage":{"input":100,"output":30,"cacheRead":1000,"cacheWrite":0,"cost":{"total":0.02}}}}`,
		`{"type":"turn_start"}`,
	} {
		st.line(l)
	}
	if st.turns != 3 || st.tools != 1 || capped != 1 {
		t.Errorf("turns=%d tools=%d capped=%d", st.turns, st.tools, capped)
	}
	if st.usage.In != 2100 || st.usage.Cached != 1800 || st.usage.Out != 50 || st.usage.CostUSD < 0.0299 {
		t.Errorf("usage = %+v", st.usage)
	}
	if got := st.summaryText(); got != "Fixed app.py." {
		t.Errorf("summary = %q", got)
	}
	st.line(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"401 unauthorized"}}`)
	if st.failed != "401 unauthorized" {
		t.Errorf("failed = %q", st.failed)
	}
}

// The files a brief names are found in the clone: written-out paths, the tail of one, bare file
// names and component names — and nothing from dependencies or common words.
func TestLocateFiles(t *testing.T) {
	repo := t.TempDir()
	for _, f := range []string{"web/src/Workflow/TemplatesPage/PromptBox/PromptBox.tsx", "web/src/Workflow/TemplatesPage/PromptBox/PromptBox.scss",
		"web/src/Workflow/WorkflowChat/ChatBox/ChatBox.tsx", "web/src/Workflow/WorkflowChat/WorkflowChat.tsx", "api/handlers/order_lookup.py",
		"node_modules/lib/ChatBox.tsx", "README.md", "web/src/index.ts"} {
		os.MkdirAll(filepath.Join(repo, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(repo, f), []byte("x"), 0o644)
	}
	got := locateFiles(repo, app.JobSpec{Title: "Align the templates prompt box with the chat",
		Requirement: "Make PromptBox look like the WorkflowChat input (see Workflow/WorkflowChat/ChatBox/ChatBox.tsx). The README is fine.",
		Evidence:    "Traceback: File \"/srv/app/api/handlers/order_lookup.py\", line 12"}, 15)
	want := map[string]bool{"web/src/Workflow/TemplatesPage/PromptBox/PromptBox.tsx": true, "web/src/Workflow/TemplatesPage/PromptBox/PromptBox.scss": true,
		"web/src/Workflow/WorkflowChat/WorkflowChat.tsx": true, "web/src/Workflow/WorkflowChat/ChatBox/ChatBox.tsx": true, "api/handlers/order_lookup.py": true}
	for _, f := range got {
		if !want[f] {
			t.Errorf("unexpected %s", f)
		}
		delete(want, f)
	}
	for f := range want {
		t.Errorf("missed %s (got %v)", f, got)
	}
	if got := locateFiles(repo, app.JobSpec{}, 15); got != nil {
		t.Errorf("an empty brief located %v", got)
	}
}

func TestNodeHeapMB(t *testing.T) {
	for _, c := range []struct {
		limit int64
		want  int
	}{{8 << 30, 4915}, {2 << 30, 2048}, {64 << 30, 16384}, {0, 0}} {
		if got := nodeHeapMB(c.limit); got != c.want {
			t.Errorf("nodeHeapMB(%d GiB) = %d, want %d", c.limit>>30, got, c.want)
		}
	}
}
