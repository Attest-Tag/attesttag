package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// directModelAccess names the functions allowed to touch an .llm field directly, and why. Every
// other model call goes through llmFor, llmOf, embedderFor or consoleLLM, which send an
// organisation that brought its own key to its own endpoint. A new call site that reached for
// a.llm instead would quietly send that organisation's conversation to the deployment's
// provider, on the deployment's key — the one thing the feature exists to prevent — so it fails
// here, the way a query without its org_id fails TestEveryPerOrgQueryIsScoped.
var directModelAccess = map[string]string{
	"NewAgent":    "stores the deployment's endpoint on the Agent",
	"NewIndexer":  "stores the deployment's endpoint on the Indexer",
	"llmFor":      "the resolver's fallback when an Agent was assembled without one",
	"llmOf":       "caches the resolved endpoint on the Call",
	"embedderFor": "the indexer's resolver",
	"consoleLLM":  "caches the resolved endpoint on the console call",
}

func TestNothingReachesTheDeploymentModelDirectly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if x.Sel.Name == "llm" {
						if _, allowed := directModelAccess[fn.Name.Name]; !allowed {
							bad = append(bad, fset.Position(x.Pos()).String()+" in "+fn.Name.Name+": reads an .llm field")
						}
					}
				case *ast.CallExpr:
					// The SDK itself is llm.go's alone: a client built anywhere else is a model
					// endpoint nothing here decided on.
					if name == "llm.go" || name == "models.go" {
						return true
					}
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "openai" && sel.Sel.Name == "NewClient" {
							bad = append(bad, fset.Position(x.Pos()).String()+": builds its own model client")
						}
						if inner, ok := sel.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "New" &&
							(inner.Sel.Name == "Completions" || inner.Sel.Name == "Embeddings") {
							bad = append(bad, fset.Position(x.Pos()).String()+": calls the model outside LLM")
						}
					}
				}
				return true
			})
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b + " — go through llmFor/llmOf (model_endpoints.go), or add the function to directModelAccess with the reason")
	}
}

// modelServer is an OpenAI-compatible endpoint that answers chat, embeddings and the model list,
// and counts what reached it and with which key.
type modelServer struct {
	*httptest.Server
	mu      sync.Mutex
	hits    map[string]int
	keys    map[string]bool
	models  map[string]bool
	status  int    // non-zero: every completion fails with this status
	errBody string // and this body
}

func newModelServer(t *testing.T) *modelServer {
	t.Helper()
	m := &modelServer{hits: map[string]int{}, keys: map[string]bool{}, models: map[string]bool{}}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.hits[r.Method+" "+r.URL.Path]++
		m.keys[r.Header.Get("Authorization")] = true
		if body.Model != "" {
			m.models[body.Model] = true
		}
		status, errBody := m.status, m.errBody
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			w.Write([]byte(`{"data":[{"id":"org-model"},{"id":"org-embed"}]}`))
		case status != 0:
			w.WriteHeader(status)
			w.Write([]byte(errBody))
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"e","usage":{"prompt_tokens":3,"total_tokens":3}}`))
		default:
			w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"NOTHING"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`))
		}
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *modelServer) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, v := range m.hits {
		n += v
	}
	return n
}

// sent counts the requests that carried something to a model — completions and embeddings. The
// deployment's model list is read to price an organisation's calls (LLM.pricer), which sends it
// nothing of the organisation's, so it is not counted.
func (m *modelServer) sent() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for path, v := range m.hits {
		if strings.HasPrefix(path, "POST ") {
			n += v
		}
	}
	return n
}

func (m *modelServer) sawKey(k string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keys["Bearer "+k]
}

// ownKeyRig is an agent on a deployment endpoint, with organisation 1 holding a key of its own
// on a second one.
type ownKeyRig struct {
	st            *Store
	platform, own *modelServer
	a             *Agent
	ix            *Indexer
	b             *Bot
	endpoints     *modelEndpoints
	alerts        []string
	cfg           Config
	settings      *settingsCache
	sl            *Chat
}

func newOwnKeyRig(t *testing.T, mode string) *ownKeyRig {
	t.Helper()
	r := &ownKeyRig{st: testStore(t), platform: newModelServer(t), own: newModelServer(t)}
	sealer := testSealer(t)
	r.cfg = Config{Model: "platform-model", EmbedModel: "platform-embed", MaxToolRounds: 3, TurnMaxMinutes: 12,
		HistoryLimit: 40, OrgModelKeys: mode}
	r.settings = newSettingsCache(r.st, r.cfg)
	platformLLM := NewLLM(Config{LLMBaseURL: r.platform.URL, LLMKey: "sk-platform-key-000", Model: "platform-model", EmbedModel: "platform-embed"})
	r.endpoints = newModelEndpoints(r.cfg, platformLLM, r.st, sealer, r.settings)
	r.endpoints.transport = http.DefaultTransport // the real one refuses loopback, by design
	r.endpoints.alert = func(ctx context.Context, orgID int64, key, text string) { r.alerts = append(r.alerts, text) }
	if err := r.st.PutModelKey(context.Background(), 1, ModelKeyRef{Preset: "compatible", BaseURL: r.own.URL,
		DefaultModel: "org-model", EmbedModel: "org-embed", FixJobs: true}, "sk-org-key-1234567890", "a@x", sealer); err != nil {
		t.Fatal(err)
	}
	r.ix = NewIndexer(platformLLM, r.st, t.TempDir())
	r.ix.endpoints = r.endpoints
	slackSrv := httptest.NewServer(&recordingSlack{})
	t.Cleanup(slackSrv.Close)
	r.sl = &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/api/"))},
		BotUserID: "UBOT", TeamID: "T1", OrgID: 1}
	r.a = &Agent{cfg: r.cfg, store: r.st, tools: map[string]Tool{}, runs: map[int64]*runHandle{}, loc: time.UTC,
		slacks: testRegistry(r.sl), settings: r.settings, llm: platformLLM, endpoints: r.endpoints, indexer: r.ix}
	r.b = &Bot{cfg: r.cfg, store: r.st, settings: r.settings, agent: r.a, sealer: sealer}
	return r
}

func (r *ownKeyRig) previewTurn(t *testing.T, text string) (*Call, error) {
	t.Helper()
	ctx := context.Background()
	thread := newPlaygroundThread()
	sess, err := r.st.EnsureSession(ctx, "T1", "C1", thread, "playground", "")
	if err != nil {
		t.Fatal(err)
	}
	r.st.AddTurn(ctx, "T1", "C1", thread, "user", "U1", text, "", 0, 0)
	c := &Call{TeamID: "T1", OrgID: 1, SL: r.sl, Channel: "C1", ThreadTS: thread, UserID: "U1",
		Text: text, Kind: "channel", Session: sess, Preview: true, NoTools: true,
		Streamer: r.sl.NewSilentStreamer("C1", thread, "U1")}
	return c, r.a.Run(ctx, c)
}

// Everything an organisation causes goes to the endpoint it brought, on its key, with the model it
// chose — and the deployment's endpoint hears nothing at all.
func TestAnOrgWithAKeyNeverReachesThePlatform(t *testing.T) {
	r := newOwnKeyRig(t, OrgModelKeysAll)
	ctx := context.Background()

	// A turn.
	if c, err := r.previewTurn(t, "are the extractors up?"); err != nil {
		t.Fatalf("turn: %v", err)
	} else if c.model != "org-model" {
		t.Errorf("the turn ran on %q, want the organisation's default", c.model)
	}
	// The channel watcher and the allow-rule checker, which used to name the deployment's model.
	r.a.watchVerdict(ctx, 1, "T1", "answer questions about deploys", "is the deploy done?")
	if err := r.st.PutSetting(ctx, 1, "allow_rules", `["creating a ClickUp task is fine"]`); err != nil {
		t.Fatal(err)
	}
	r.settings.Invalidate(1)
	r.a.allowedByRule(ctx, &Call{OrgID: 1, TeamID: "T1", Channel: "C1"}, "HTTP POST https://api.clickup.com/api/v2/list/1/task", "")
	// The thread summariser, reached through a thread longer than the window.
	var thread []ThreadMsg
	for i := 0; i < 8; i++ {
		thread = append(thread, ThreadMsg{TS: fmt.Sprintf("1.%d", i), Name: "alex", Text: fmt.Sprintf("message %d", i)})
	}
	r.a.windowThread(ctx, &Call{OrgID: 1, TeamID: "T1", Channel: "C1", ThreadTS: "1.0"}, thread, 3)
	// The console assistant.
	if _, err := r.b.runConsoleTurn(ctx, &consoleCall{OrgID: 1, Actor: "a", Perms: map[string]bool{}, model: "org-model"}, "hello", nil); err != nil {
		t.Errorf("console turn: %v", err)
	}
	// Documents: indexed and searched with the organisation's embedding model.
	docs := (&localDocs{dir: t.TempDir(), store: r.st}).For(1).(*localDocs)
	if err := os.MkdirAll(docs.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs.dir, "policy.md"), []byte("# Deploys\n\nDeploys happen on Tuesdays after the release review, never on a Friday afternoon."), 0o644); err != nil {
		t.Fatal(err)
	}
	if rep, err := r.ix.Ingest(ctx, 1, docs); err != nil || len(rep.Errors) > 0 || rep.Embedded == 0 {
		t.Errorf("ingest: %+v %v", rep, err)
	}
	if hits, err := r.ix.Search(ctx, 1, "when do deploys happen", 3, "C1"); err != nil || len(hits) == 0 {
		t.Errorf("search: %v %v", hits, err)
	}
	// The model list the console's pickers read.
	l, err := r.a.llmFor(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if models, err := l.ListModels(ctx, true); err != nil || len(models) != 2 {
		t.Errorf("model list: %v %v", models, err)
	}

	if n := r.platform.sent(); n != 0 {
		t.Errorf("the deployment's endpoint was sent %d requests for an organisation with its own key: %v", n, r.platform.hits)
	}
	for _, path := range []string{"POST /chat/completions", "POST /embeddings", "GET /models"} {
		if r.own.hits[path] == 0 {
			t.Errorf("the organisation's endpoint never saw %s: %v", path, r.own.hits)
		}
	}
	if !r.own.sawKey("sk-org-key-1234567890") || r.own.sawKey("sk-platform-key-000") {
		t.Errorf("keys the organisation's endpoint saw: %v", r.own.keys)
	}
	if !r.own.models["org-model"] || !r.own.models["org-embed"] || r.own.models["platform-model"] || r.own.models["platform-embed"] {
		t.Errorf("models the organisation's endpoint was asked for: %v", r.own.models)
	}
}

// A key the plan no longer includes stops the organisation's model calls with the reason, and
// sends nothing anywhere: the deployment's endpoint is not a fallback for a customer who chose
// its own.
func TestLosingTheEntitlementFailsClosed(t *testing.T) {
	r := newOwnKeyRig(t, OrgModelKeysEnterprise) // organisation 1 is on the free plan
	ctx := context.Background()
	if ok, why := r.a.budgetOK(ctx, 1, "T1"); ok || !strings.Contains(why, "not part of its plan") {
		t.Errorf("budgetOK = %v %q; want a refusal that says the key is not in the plan", ok, why)
	}
	if _, err := r.previewTurn(t, "hello"); err == nil || !strings.Contains(err.Error(), "Settings → Models") {
		t.Errorf("turn error = %v; want the reason and where to fix it", err)
	}
	if v, _ := r.a.watchVerdict(ctx, 1, "T1", "answer everything", "hello"); v != "" {
		t.Errorf("the watcher answered %q with nowhere to send a reply", v)
	}
	if _, err := r.ix.embedderFor(ctx, 1); err == nil {
		t.Error("the indexer found an endpoint to embed on with nowhere allowed")
	}
	if n := r.platform.total() + r.own.total(); n != 0 {
		t.Errorf("a blocked organisation reached a model %d times (platform %v, own %v)", n, r.platform.hits, r.own.hits)
	}
}

// A key that cannot be opened — MASTER_KEY replaced without MASTER_KEY_PREVIOUS — is the same
// refusal, not a quiet move to the deployment's endpoint.
func TestAnUnreadableKeyFailsClosed(t *testing.T) {
	r := newOwnKeyRig(t, OrgModelKeysAll)
	other := make([]byte, 32)
	for i := range other {
		other[i] = byte(i * 3)
	}
	t.Setenv("MASTER_KEY", base64.StdEncoding.EncodeToString(other))
	t.Setenv("MASTER_KEY_PREVIOUS", "")
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	r.endpoints.sealer = sealer
	_, err = r.previewTurn(t, "hello")
	var me *ModelKeyError
	if !errors.As(err, &me) || me.Kind != keyUnreadable {
		t.Errorf("turn error = %v; want the unreadable-key refusal", err)
	}
	if n := r.platform.sent(); n != 0 {
		t.Errorf("the deployment's endpoint was sent %d requests", n)
	}
}

// What the provider says is turned into a sentence that names the problem and where it is
// fixed, recorded for the console, and said once in the alert channel.
func TestAFailingKeyIsExplainedRecordedAndAlerted(t *testing.T) {
	r := newOwnKeyRig(t, OrgModelKeysAll)
	ctx := context.Background()
	r.own.status, r.own.errBody = 401, `{"error":{"message":"Incorrect API key provided: sk-org-k************7890.","type":"invalid_request_error","code":"invalid_api_key"}}`
	_, err := r.previewTurn(t, "hello")
	var me *ModelKeyError
	if !errors.As(err, &me) || me.Kind != keyRefused || !strings.Contains(err.Error(), "refused this organisation's key") ||
		!strings.Contains(err.Error(), "Settings → Models") {
		t.Fatalf("turn error = %v; want the refused-key sentence", err)
	}
	if strings.Contains(err.Error(), "sk-org-key-1234567890") {
		t.Error("the refusal carries the key")
	}
	ref, _ := r.st.ModelKeyRef(ctx, 1)
	if ref == nil || !ref.Failing() || !strings.Contains(ref.LastError, "refused") {
		t.Errorf("status after a refusal: %+v", ref)
	}
	if len(r.alerts) != 1 {
		t.Errorf("alerts = %v; want one", r.alerts)
	}
	// A second failure straight after is neither written again nor alerted again.
	r.previewTurn(t, "hello again")
	if len(r.alerts) != 1 {
		t.Errorf("a repeated failure alerted again: %v", r.alerts)
	}
	// Out of quota is its own sentence: the key is fine and the account is not.
	r.own.status, r.own.errBody = 429, `{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","code":"insufficient_quota"}}`
	if _, err := r.previewTurn(t, "and now"); !errors.As(err, &me) || me.Kind != keyQuota {
		t.Errorf("quota error = %v", err)
	}
	if n := r.platform.sent(); n != 0 {
		t.Errorf("a failing key fell back to the deployment's endpoint %d times", n)
	}
}

func TestClassifyModelError(t *testing.T) {
	if classifyModelError("h", "m", context.Canceled) != nil || classifyModelError("h", "m", context.DeadlineExceeded) != nil {
		t.Error("a cancelled call was blamed on the endpoint")
	}
	if me := classifyModelError("llm.example", "m", &url.Error{Op: "Post", URL: "https://llm.example/v1", Err: errors.New("non-public address blocked")}); me == nil ||
		me.Kind != keyUnreachable || !strings.Contains(me.Error(), "could not be reached") {
		t.Errorf("transport error = %+v", me)
	}
}

func TestValidModelBaseURL(t *testing.T) {
	for _, ok := range []string{"https://api.openai.com/v1", "https://openrouter.ai/api/v1", "https://acme.openai.azure.com/openai/v1/"} {
		if err := validModelBaseURL(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://api.openai.com/v1", "https://localhost/v1", "https://10.0.0.5/v1", "https://169.254.169.254/latest",
		"https://api.openai.com:8443/v1", "https://user:pass@api.openai.com/v1", "https://api.openai.com/v1?x=1", "api.openai.com/v1", ""} {
		if err := validModelBaseURL(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// The transport an organisation's calls leave on refuses a plain-http address and a private one
// on every call, whatever the stored URL says.
func TestTheOrgTransportRefusesWhatSaveWouldHave(t *testing.T) {
	client := &http.Client{Transport: orgTransport(), CheckRedirect: rejectRedirect}
	for _, u := range []string{"http://api.openai.com/v1/models", "https://127.0.0.1/v1/models", "https://metadata.google.internal/v1"} {
		if resp, err := client.Get(u); err == nil {
			resp.Body.Close()
			t.Errorf("%s was fetched", u)
		}
	}
}

// A key saved again on another instance while this one still holds the old settings: the client is
// built from the row — the new key and the address it was saved for, from one read — so the new key
// never reaches the address it replaced.
func TestARotatedKeyNeverGoesToTheOldAddress(t *testing.T) {
	r := newOwnKeyRig(t, OrgModelKeysAll)
	ctx := context.Background()
	r.settings.Get(ctx, 1) // this instance has read the old endpoint
	moved := newModelServer(t)
	if err := r.st.PutModelKey(ctx, 1, ModelKeyRef{Preset: "compatible", BaseURL: moved.URL, DefaultModel: "org-model",
		EmbedModel: "org-embed", FixJobs: true}, "sk-org-rotated-key-99999", "b@x", r.endpoints.sealer); err != nil {
		t.Fatal(err)
	}
	l, err := r.endpoints.For(ctx, 1) // nothing here was told
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Chat(ctx, "", turnPrompt(), nil, ""); err != nil {
		t.Fatal(err)
	}
	if r.own.sawKey("sk-org-rotated-key-99999") {
		t.Error("the new key was sent to the old address")
	}
	if !moved.sawKey("sk-org-rotated-key-99999") {
		t.Error("the new key did not reach the address it was saved for")
	}
}

// A key removed on another instance: this one's settings still say there is a key, and let the
// call past the deployment's limits. It is refused once rather than sent to the deployment's key
// on the strength of them; the next call reads afresh.
func TestAKeyRemovedElsewhereIsRefusedOnceNotSentToThePlatform(t *testing.T) {
	r := newOwnKeyRig(t, OrgModelKeysAll)
	ctx := context.Background()
	r.settings.Get(ctx, 1)
	if _, err := r.st.DeleteModelKey(ctx, 1); err != nil {
		t.Fatal(err)
	}
	_, err := r.endpoints.For(ctx, 1)
	var me *ModelKeyError
	if !errors.As(err, &me) || me.Kind != keyUnavailable {
		t.Errorf("first call after a removal elsewhere = %v; want a refusal", err)
	}
	if l, err := r.endpoints.For(ctx, 1); err != nil || l != r.endpoints.platform {
		t.Errorf("the next call, on fresh settings: %v", err)
	}
	if n := r.platform.sent(); n != 0 {
		t.Errorf("the deployment's endpoint was sent %d requests", n)
	}
}

// Servable's shortcuts are for the hosts they are true of: OpenRouter's ids always name a vendor,
// Azure's deployment names never hold a "/". A self-host's own gateway is neither, and its ids are
// left alone.
func TestServableSwapsOnlyWhereTheRuleHolds(t *testing.T) {
	ctx := context.Background()
	gateway := NewLLM(Config{LLMBaseURL: "https://litellm.internal.example/v1", Model: "anthropic/claude-sonnet-4"})
	if got := gateway.Servable(ctx, "gpt-4o"); got != "gpt-4o" {
		t.Errorf("a gateway's own id was swapped for %q", got)
	}
	openrouter := NewLLM(Config{LLMBaseURL: "https://openrouter.ai/api/v1", Model: "z-ai/glm-5.3-flash"})
	if got := openrouter.Servable(ctx, "gpt-4o"); got != "z-ai/glm-5.3-flash" {
		t.Errorf("a vendorless id on OpenRouter = %q", got)
	}
	azure := newLLM(Config{}, endpoint{BaseURL: "https://acme.openai.azure.com/openai/v1", Dialect: dialectOpenAI, Model: "prod-gpt4o"})
	azure.own = true
	if got := azure.Servable(ctx, "z-ai/glm-5.3"); got != "prod-gpt4o" {
		t.Errorf("an OpenRouter id on Azure = %q; no deployment is named with a slash", got)
	}
	if got := azure.Servable(ctx, "reports-deployment"); got != "reports-deployment" {
		t.Errorf("an Azure deployment name was swapped for %q", got)
	}
}
