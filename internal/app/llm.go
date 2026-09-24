package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// LLM wraps one OpenAI-compatible endpoint (OpenRouter by default) for chat and embeddings.
type LLM struct {
	client       openai.Client
	Model        string
	EmbedModel   string
	dialect      dialect
	denyTraining bool
	reasoning    string
	// providerSort and providerOrder decide which of a model's endpoints serves the call. See
	// providerPrefs: left unset, OpenRouter balances the load across all of them.
	providerSort  string
	providerOrder []string
	// loc is the deployment's own zone, and the one the clock is written in for calls that do
	// not belong to a single organisation. A turn that knows its organisation passes a better
	// zone by building the clock itself; see Agent.clockLine.
	loc *time.Location

	// pricer prices a call the provider did not: an organisation's own endpoint on OpenAI reports
	// tokens and no charge, and without a figure its monthly budget could never bind. It is the
	// deployment's catalogue (priceOf) and is nil on the deployment's own endpoint, where
	// OpenRouter reports what it charged.
	pricer func(ctx context.Context, model string) (modelPrice, bool)
	// own marks an organisation's own endpoint (model_endpoints.go): its failures are explained in
	// words a person can act on, and report hears how every call went. host is what it is known by.
	own    bool
	host   string
	report func(problem string)

	// Provider model list, see ListModels. modelsFetch is non-nil while a fetch is in flight and
	// is closed when it ends; modelsErr is how that fetch went, for the callers that waited on it.
	modelsMu    sync.Mutex
	models      []ModelInfo
	modelsAt    time.Time
	modelsFetch chan struct{}
	modelsErr   error
}

// NewLLM is the deployment's own endpoint: LLM_BASE_URL on LLM_API_KEY.
//
// It has always been sent OpenRouter's request shape, whatever LLM_BASE_URL named, and a
// self-host behind a gateway that forwards those fields may depend on that — so it keeps the
// shape unless its host is one known to refuse it. OpenAI's own API answers the first unknown
// field with a 400, so a deployment pointed straight at it could never have worked before.
func NewLLM(cfg Config) *LLM {
	d := dialectFor(cfg.LLMBaseURL)
	if d == dialectCompatible {
		d = dialectOpenRouter
	}
	return newLLM(cfg, endpoint{BaseURL: cfg.LLMBaseURL, Key: cfg.LLMKey, Dialect: d,
		Model: cfg.Model, EmbedModel: cfg.EmbedModel, ProviderOrder: providerList(cfg.ProviderOrder)})
}

// endpoint is where one LLM sends its calls and in what shape.
type endpoint struct {
	BaseURL, Key      string
	Dialect           dialect
	Model, EmbedModel string
	ProviderOrder     []string
	// HTTPClient carries the transport; nil is the SDK's default, which is what the deployment's
	// own endpoint has always used.
	HTTPClient *http.Client
}

func newLLM(cfg Config, ep endpoint) *LLM {
	opts := []option.RequestOption{
		option.WithBaseURL(ep.BaseURL),
		option.WithAPIKey(ep.Key),
		option.WithMiddleware(stripClientIdentity),
		option.WithMiddleware(dumpRequests),
	}
	if ep.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(ep.HTTPClient))
	}
	return &LLM{
		reasoning:     cfg.Reasoning,
		client:        openai.NewClient(opts...),
		Model:         ep.Model,
		EmbedModel:    ep.EmbedModel,
		dialect:       ep.Dialect,
		denyTraining:  cfg.DenyTraining,
		providerSort:  cfg.ProviderSort,
		providerOrder: ep.ProviderOrder,
		loc:           tzOrUTC(cfg.Timezone),
		host:          urlHost(ep.BaseURL),
	}
}

// dialect is how much of OpenRouter's request shape an endpoint will take. The Chat Completions
// API is one shape on paper and several in practice: OpenRouter adds fields of its own (reasoning,
// provider routing, cache_control markers) and forwards or drops whatever a provider cannot use,
// while OpenAI's own API refuses the first field it does not know, and refuses a temperature on
// the models that reason. Everything else a customer can point at — a gateway, a vendor's
// compatible endpoint, a model they host — is given the plain shape, which all of them read.
type dialect int

const (
	// dialectOpenRouter is the zero value: it is what every endpoint was sent before there was a
	// choice, so an LLM assembled without one behaves exactly as it always did.
	dialectOpenRouter dialect = iota
	dialectOpenAI
	dialectCompatible
)

func (d dialect) String() string {
	switch d {
	case dialectOpenAI:
		return "openai"
	case dialectCompatible:
		return "compatible"
	}
	return "openrouter"
}

// dialectFor names an endpoint's dialect from its host. Azure OpenAI's v1 endpoint is OpenAI's
// API served from a customer's own resource, and is as strict about unknown fields.
func dialectFor(baseURL string) dialect {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return dialectCompatible
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	switch {
	case host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai"):
		return dialectOpenRouter
	case host == "api.openai.com", strings.HasSuffix(host, ".openai.azure.com"),
		strings.HasSuffix(host, ".cognitiveservices.azure.com"), strings.HasSuffix(host, ".services.ai.azure.com"):
		return dialectOpenAI
	}
	return dialectCompatible
}

// openAIFamily sorts an OpenAI model id by what its requests may carry. The reasoning models
// refuse any temperature but the default and take an effort instead; the older chat models take a
// temperature and refuse an effort. Anything not recognised here — a model newer than this list,
// an Azure deployment its owner named — gets neither field, which every model accepts: a wrong
// guess costs a refused request, and on an organisation's own key a refused request is an outage.
func openAIFamily(model string) string {
	m := strings.TrimPrefix(strings.ToLower(model), "openai/")
	switch {
	case strings.HasPrefix(m, "gpt-5") && strings.Contains(m, "chat"):
		return "" // the chat snapshots of gpt-5 reason with no effort to set
	case strings.HasPrefix(m, "o1-mini"), strings.HasPrefix(m, "o1-preview"):
		return "" // retired before reasoning_effort existed, and refuse it
	case strings.HasPrefix(m, "gpt-5"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "reasoning"
	case strings.HasPrefix(m, "gpt-4"), strings.HasPrefix(m, "gpt-3.5"), strings.HasPrefix(m, "chatgpt-"):
		return "classic"
	}
	return ""
}

// openAIEffort is REASONING in the words OpenAI's reasoning models accept. Only low, medium and
// high are common to all of them — "minimal" is gpt-5's alone and "none" is a later model's — so
// everything below medium is sent as low, which is also what the default of this setting means.
func openAIEffort(reasoning string) openai.ReasoningEffort {
	switch reasoning {
	case "medium":
		return openai.ReasoningEffortMedium
	case "high":
		return openai.ReasoningEffortHigh
	}
	return openai.ReasoningEffortLow
}

// providerList reads a comma-separated PROVIDER_ORDER into the slice OpenRouter wants.
func providerList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// providerPrefs is OpenRouter's provider routing block, and it is about far more than price.
// A popular open model is served by a dozen endpoints at once -- glm-5.3-flash by eleven, at
// three different quantizations -- and left to itself OpenRouter balances across them. Two
// consequences, both paid on every turn: the rounds of one turn can land on different endpoints,
// so the prompt prefix the last round just wrote is not in the cache the next one reads (turns
// with identical work measured 42% cached on one run and 0% on the next), and the same question
// can be answered by an fp4 endpoint one minute and an fp8 one the next. Naming a sort disables
// the balancing, which pins both. Fallbacks stay on, so an endpoint going down still routes
// elsewhere rather than failing the turn.
func (l *LLM) providerPrefs() map[string]any {
	p := map[string]any{}
	if l.denyTraining {
		p["data_collection"] = "deny"
	}
	if len(l.providerOrder) > 0 {
		p["order"] = l.providerOrder
	}
	if s := l.providerSort; s != "" && s != "none" {
		p["sort"] = s
	}
	return p
}

// chatOpts are the body fields only OpenRouter reads. Every other dialect is sent none of them:
// OpenAI refuses an unknown field outright, and a compatible endpoint that ignored one today may
// not tomorrow.
func (l *LLM) chatOpts() []option.RequestOption {
	if l.dialect != dialectOpenRouter {
		return nil
	}
	opts := []option.RequestOption{option.WithJSONSet("reasoning", reasoningBody(l.reasoning))}
	if p := l.providerPrefs(); len(p) > 0 {
		opts = append(opts, option.WithJSONSet("provider", p))
	}
	return opts
}

// chatParams is one completion's request in this endpoint's dialect. The temperature is the
// deployment's long-standing 0.2 wherever it is safe to send, and nowhere it is not.
// maxAnswerTokens caps one response. It is generous enough that no real answer or tool-call round
// is truncated, and it turns an unbounded generation — a runaway model, or a member pointing an
// expensive model at the shared key — into a bounded per-round cost. Providers clamp a value above
// a model's own limit rather than erroring, so one number is safe across the catalogue.
const maxAnswerTokens = 32 << 10

func (l *LLM) chatParams(model string, msgs []openai.ChatCompletionMessageParamUnion) openai.ChatCompletionNewParams {
	p := openai.ChatCompletionNewParams{Model: openai.ChatModel(model), Messages: l.prompt(model, msgs), MaxCompletionTokens: openai.Int(maxAnswerTokens)}
	switch l.dialect {
	case dialectOpenRouter:
		p.Temperature = openai.Float(0.2)
	case dialectOpenAI:
		switch openAIFamily(model) {
		case "classic":
			p.Temperature = openai.Float(0.2)
		case "reasoning":
			p.ReasoningEffort = openAIEffort(l.reasoning)
		}
	}
	return p
}

// reasoningBody is OpenRouter's unified reasoning field. Thinking is kept out of the visible
// answer with exclude rather than by switching reasoning off: some endpoints refuse to run
// without it ("Reasoning is mandatory for this endpoint and cannot be disabled"), and a model
// that reasons quietly still answers. REASONING=none restores the old off switch for a model
// that allows it; stripThinking is the backstop when a provider ignores exclude either way.
func reasoningBody(effort string) map[string]any {
	switch effort {
	case "none", "off", "false":
		return map[string]any{"enabled": false}
	case "":
		effort = "low"
	}
	return map[string]any{"effort": effort, "exclude": true}
}

// stripClientIdentity takes off everything the request says about us that is not needed to
// answer it. Two things were being volunteered:
//
// OpenRouter's HTTP-Referer and X-Title are how an app puts its name and a link to itself on
// openrouter.ai's public rankings. They are not sent at all any more — the endpoint needs a key,
// a model and the messages, and who is asking is nobody else's list to be on.
//
// The SDK adds X-Stainless-* on every call (Go version, OS, architecture, package version,
// retry count) and a "OpenAI/Go x.y.z" user agent. None of it changes an answer; all of it
// describes this deployment to someone else's log, so it is removed here, on the way out, where
// it catches the headers the SDK sets per attempt as well as the ones set once at construction.
//
// A User-Agent that is present but empty is dropped by net/http on both HTTP/1.1 and HTTP/2 —
// deleting the key outright would only make it substitute a default of its own.
func stripClientIdentity(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	for k := range req.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Stainless-") {
			delete(req.Header, k)
		}
	}
	req.Header.Set("User-Agent", "")
	return next(req)
}

// dumpRequests writes each outgoing request body to LLM_DUMP_DIR when set (debugging only).
func dumpRequests(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	// Never on a managed deployment: a request body is every tenant's documents, memories and
	// transcripts, and a file on a shared instance is not where those go.
	if dir := os.Getenv("LLM_DUMP_DIR"); dir != "" && req.Body != nil && !managedDeployment() {
		b, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(b))
		f, err := os.CreateTemp(dir, "req-*.json")
		if err == nil {
			f.Write(b)
			f.Close()
			slog.Debug("dumped request", "file", f.Name(), "bytes", len(b))
		}
	}
	return next(req)
}

type Usage struct {
	In, Out int
	// CachedIn is how much of In the provider served from its prompt cache. It is not a
	// separate charge — it is the part of In that was cheap — and it is the only way to tell
	// whether the breakpoints below are doing anything, so it is logged with every turn.
	CachedIn int
	// Reasoning is the part of Out the model spent thinking rather than answering. It is not a
	// separate charge either: reasoningBody asks for the trace to be excluded from the reply,
	// which hides it but does not stop it being generated or billed, and a turn pays it again
	// on every tool round. Without this number the only honest answer to "is REASONING=low
	// worth what it costs" is that nobody knows, so it is counted and stored with the rest.
	Reasoning int
	CostUSD   float64
	// CostEstimated says CostUSD is this deployment's estimate from a list price rather than a
	// charge the provider reported (see LLM.pricer).
	CostEstimated bool
	// KeyOwner is keyOwnerOrg when the call was made on the organisation's own key, which its
	// provider bills it for: LogUsageBy records it and charges none of it to credit. Empty is the
	// deployment's key.
	KeyOwner string
}

// add folds one call's usage into a turn's running total. Four fields were being summed by hand
// at each of the loops that do this, which is three chances to add a fifth field everywhere but
// one and not notice, because a token counter that is quietly too low looks exactly like thrift.
func (u *Usage) add(o Usage) {
	u.In += o.In
	u.Out += o.Out
	u.CachedIn += o.CachedIn
	u.Reasoning += o.Reasoning
	u.CostUSD += o.CostUSD
	u.CostEstimated = u.CostEstimated || o.CostEstimated
	if u.KeyOwner == "" {
		u.KeyOwner = o.KeyOwner
	}
}

func usageOf(u openai.CompletionUsage) Usage {
	us := Usage{
		In:        int(u.PromptTokens),
		Out:       int(u.CompletionTokens),
		CachedIn:  int(u.PromptTokensDetails.CachedTokens),
		Reasoning: int(u.CompletionTokensDetails.ReasoningTokens),
	}
	if f, ok := u.JSON.ExtraFields["cost"]; ok { // OpenRouter reports the real charge
		us.CostUSD = validCost(f.Raw())
	}
	return us
}

// validCost is the boundary between what a provider said a turn cost and what this process is
// willing to call money. Anything it cannot vouch for is zero: a turn we failed to price is a turn
// nobody is charged for, which is the wrong answer in our favour and not in the customer's.
//
// It exists because strconv.ParseFloat("NaN", 64) SUCCEEDS. A provider answering `"cost": NaN`
// — or Infinity, or a negative — used to be stored verbatim, and on Postgres sum() propagates NaN,
// so a single such row made MonthSpend return NaN for the rest of the month. Every comparison
// against NaN is false, so `spent >= budget` stopped being true, the monthly budget silently
// stopped binding, and nothing anywhere logged a word about it. With prepaid credit the same row
// would switch the credit floor off as well: `balance <= 0` is false against NaN too.
func validCost(raw string) float64 {
	v, err := strconv.ParseFloat(raw, 64)
	switch {
	case err != nil:
		slog.Warn("usage cost was not a number; the turn is recorded as free", "raw", raw)
		return 0
	case math.IsNaN(v) || math.IsInf(v, 0):
		slog.Warn("usage cost was not finite; the turn is recorded as free", "raw", raw)
		return 0
	case v < 0:
		// A negative charge is a credit nobody asked for. Refuse it rather than let a provider
		// hand an account spending money back.
		slog.Warn("usage cost was negative; the turn is recorded as free", "raw", raw)
		return 0
	}
	return v
}

// Prompt caching. A turn re-sends its whole prompt on every tool round, and every turn in a
// channel opens with the same system block and the same tool list, so the same thousands of
// tokens are bought again and again at full price. Providers will serve that prefix from cache
// instead — but only if it reaches them byte for byte identical, and some of them only if we
// say where the reusable part ends.
//
// cache_control is OpenRouter's spelling of that marker. It is forwarded to the providers that
// need one (Anthropic, Gemini) and ignored by the ones that cache on their own (OpenAI,
// DeepSeek, z.ai), for which the only thing that matters is a prefix that does not move.
// chatOpts already sends OpenRouter-only fields, so this makes the same assumption about who
// is on the other end of LLM_BASE_URL.
func cacheableText(s string) openai.ChatCompletionContentPartTextParam {
	p := openai.ChatCompletionContentPartTextParam{Text: s}
	p.SetExtraFields(map[string]any{"cache_control": map[string]any{"type": "ephemeral"}})
	return p
}

// CachedSystemMessage sends the system prompt as two pieces: the half that is the same on every
// call in this channel, with the breakpoint on it, and then whatever changes each time — the
// clock. Ordering is the whole point. A timestamp near the top of the prompt means no two calls
// ever share a prefix and nothing after it can be reused, however it is marked.
func CachedSystemMessage(stable, volatile string) openai.ChatCompletionMessageParamUnion {
	parts := []openai.ChatCompletionContentPartTextParam{cacheableText(stable)}
	if volatile != "" {
		parts = append(parts, openai.ChatCompletionContentPartTextParam{Text: volatile})
	}
	return openai.ChatCompletionMessageParamUnion{OfSystem: &openai.ChatCompletionSystemMessageParam{
		Content: openai.ChatCompletionSystemMessageParamContentUnion{OfArrayOfContentParts: parts},
	}}
}

// minCacheChars is the point below which a breakpoint is not worth setting: providers have a
// minimum cacheable prefix (1024 tokens on Anthropic) and charge a premium to write the entry,
// so the small classify and summarise calls are left as they are.
const minCacheChars = 4096

// withCache marks the leading system message of a prompt that was not built with
// CachedSystemMessage, so every call made through this package carries a breakpoint rather than
// only the ones that assemble their own conversation. It is a no-op on an already-marked
// prompt, and it copies rather than edits the caller's slice.
func withCache(msgs []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	if len(msgs) == 0 {
		return msgs
	}
	sys := msgs[0].OfSystem
	if sys == nil || !sys.Content.OfString.Valid() || len(sys.Content.OfString.Value) < minCacheChars {
		return msgs
	}
	out := make([]openai.ChatCompletionMessageParamUnion, len(msgs))
	copy(out, msgs)
	out[0] = CachedSystemMessage(sys.Content.OfString.Value, "")
	return out
}

// Providers split two ways on how they find a cached prefix, and which way decides whether a
// second breakpoint is worth sending at all.
//
// OpenAI, DeepSeek and z.ai look the request up against the longest prefix they have seen before,
// so the tool results a turn piles up are already read back without being asked for and a marker
// is a wire change that buys nothing. Anthropic and Gemini cache only as far as a marker: with
// one breakpoint on the system block, everything a tool loop has accumulated behind it is bought
// again at full price on every round, and a turn's prompt grows with its rounds.
//
// So the tail breakpoint goes only to the models that need one. Matching on the model id is crude
// and it is also the right thing to match on: the model is what decides which behaviour is on the
// other end of LLM_BASE_URL. Both spellings, because a self-host may talk to a provider directly
// rather than through OpenRouter's prefixed ids.
func needsTailBreakpoint(model string) bool {
	m := strings.ToLower(model)
	for _, p := range []string{"anthropic/", "google/", "claude-", "gemini-"} {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// withTailCache puts a second breakpoint on the last tool result, so the round after this one
// reads back the whole conversation rather than only the system block in front of it.
//
// Rolling rather than fixed: every round marks its own last result, writing an entry that extends
// the one the round before it wrote and reading that one back in the same call. Only the new part
// is written. A tool result is then paid for once, at the write premium, instead of once per
// remaining round at full price — the difference between a turn's input growing with its rounds
// and growing with the square of them, which is where this deployment's largest turns have always
// gone (one of them spent 9.1M tokens re-sending 198 paged results).
//
// Only a tool result, and deliberately. It is the message that means "I am going back for another
// round", which is the only shape this pays for: the opening question has nothing behind it worth
// an entry, and the note that lands a turn is the last thing that will ever be sent. The result
// before that note was marked on the previous round, so the landing call — the biggest of the
// turn — reads everything anyway.
func withTailCache(msgs []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	if len(msgs) < 2 {
		return msgs // the only message is the system block, which withCache has already marked
	}
	last := msgs[len(msgs)-1].OfTool
	if last == nil || !last.Content.OfString.Valid() {
		return msgs
	}
	marked := *last
	marked.Content = openai.ChatCompletionToolMessageParamContentUnion{
		OfArrayOfContentParts: []openai.ChatCompletionContentPartTextParam{cacheableText(last.Content.OfString.Value)},
	}
	out := make([]openai.ChatCompletionMessageParamUnion, len(msgs))
	copy(out, msgs)
	out[len(out)-1] = openai.ChatCompletionMessageParamUnion{OfTool: &marked}
	return out
}

// prompt is every message rewrite this package makes, in the one order they work in: the
// breakpoint goes on the stable half of the system block, the clock is appended after it, and
// only then is the tail marked — the tail rewrite copies the slice, and doing it first would
// leave withClock editing a copy nobody sends.
//
// Only OpenRouter is sent breakpoints. cache_control is its field, and the endpoints that take
// the other dialects cache the longest prefix they have seen on their own, so on those the
// marker buys nothing and costs a refused request. The system block the agent built as marked
// parts (CachedSystemMessage) is flattened back into one string there, which is also the one
// shape every compatible server reads a system message in.
func (l *LLM) prompt(model string, msgs []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	if l.dialect != dialectOpenRouter {
		return plainSystem(l.withClock(msgs))
	}
	msgs = l.withClock(withCache(msgs))
	if needsTailBreakpoint(model) {
		msgs = withTailCache(msgs)
	}
	return msgs
}

// plainSystem joins a system message sent as text parts into one string, dropping whatever
// extra fields the parts carried. It copies rather than edits the caller's slice.
func plainSystem(msgs []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	if len(msgs) == 0 || msgs[0].OfSystem == nil || msgs[0].OfSystem.Content.OfString.Valid() {
		return msgs
	}
	var texts []string
	for _, part := range msgs[0].OfSystem.Content.OfArrayOfContentParts {
		texts = append(texts, part.Text)
	}
	out := make([]openai.ChatCompletionMessageParamUnion, len(msgs))
	copy(out, msgs)
	out[0] = openai.SystemMessage(strings.Join(texts, "\n\n"))
	return out
}

// tzOrUTC reads a zone name, falling back to UTC rather than to whatever zone the container
// happens to run in: an unset or misspelt TZ_NAME should produce a wrong-but-stated zone, not a
// silently different one on every host.
func tzOrUTC(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// clockPrefix opens the clock sentence, and is how withClock recognises a prompt that already
// carries one — the agent writes its own, in the organisation's zone.
const clockPrefix = "Current time:"

// clockText is today's date and the time, in the reader's zone and in UTC. The last sentence
// earns its place: a model that is told the date still reaches for the year it saw most often in
// training when it writes a filter, and a search for "the last 3 days" that quietly starts a year
// early comes back with a year of records and no error to show for it.
func clockText(now time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return fmt.Sprintf("%s %s (%s); the same moment in UTC is %s. Timestamps you send to an API are UTC unless it says otherwise — take them from the UTC value rather than converting the local clock yourself. Every date you write — into a filter, a query or a file name — is derived from this line and never from memory; check the year you wrote against it before you send the call.",
		clockPrefix, now.In(loc).Format("Mon 2 Jan 2006 15:04 -07:00"), loc, now.UTC().Format(time.RFC3339))
}

// systemHasClock reports whether the caller already put the clock in the system prompt.
func systemHasClock(sys *openai.ChatCompletionSystemMessageParam) bool {
	if sys.Content.OfString.Valid() {
		return strings.Contains(sys.Content.OfString.Value, clockPrefix)
	}
	for _, part := range sys.Content.OfArrayOfContentParts {
		if strings.Contains(part.Text, clockPrefix) {
			return true
		}
	}
	return false
}

// withClock puts the current date and time in the system prompt of every call this package makes,
// not only the ones the agent assembles: the permission checker, the channel watcher and the
// thread summariser each build their own prompt, and any of them can be asked something a date
// answers. It runs after withCache and appends, so the clock lands after the breakpoint and the
// prefix in front of it stays reusable. A prompt that already states the time is left alone, and
// one with no system message at all is given one.
func (l *LLM) withClock(msgs []openai.ChatCompletionMessageParamUnion) []openai.ChatCompletionMessageParamUnion {
	line := clockText(time.Now(), l.loc)
	if len(msgs) == 0 || msgs[0].OfSystem == nil {
		return append([]openai.ChatCompletionMessageParamUnion{openai.SystemMessage(line)}, msgs...)
	}
	sys := msgs[0].OfSystem
	if systemHasClock(sys) {
		return msgs
	}
	var parts []openai.ChatCompletionContentPartTextParam
	if sys.Content.OfString.Valid() {
		parts = []openai.ChatCompletionContentPartTextParam{{Text: sys.Content.OfString.Value}}
	} else {
		parts = make([]openai.ChatCompletionContentPartTextParam, len(sys.Content.OfArrayOfContentParts))
		copy(parts, sys.Content.OfArrayOfContentParts)
	}
	parts = append(parts, openai.ChatCompletionContentPartTextParam{Text: line})
	out := make([]openai.ChatCompletionMessageParamUnion, len(msgs))
	copy(out, msgs)
	out[0] = openai.ChatCompletionMessageParamUnion{OfSystem: &openai.ChatCompletionSystemMessageParam{
		Content: openai.ChatCompletionSystemMessageParamContentUnion{OfArrayOfContentParts: parts},
	}}
	return out
}

// toolChoiceNone is forceTool's other meaning: send the tools but ask for none of them. It is
// the string the API itself uses, and no tool can be called it — every tool here is registered
// under a name this package chose — so there is nothing for it to collide with.
//
// It exists so that a turn which has finished searching can stop calling tools without changing
// the request in the one way that is expensive: see landingChoice in agent.go.
const toolChoiceNone = "none"

// Chat is one non-streaming completion, optionally with tools.
func (l *LLM) Chat(ctx context.Context, model string, msgs []openai.ChatCompletionMessageParamUnion, tools []openai.ChatCompletionToolUnionParam, forceTool string) (*openai.ChatCompletion, Usage, error) {
	if model == "" {
		model = l.Model
	}
	p := l.chatParams(model, msgs)
	if len(tools) > 0 {
		p.Tools = tools
		switch forceTool {
		case "":
		case toolChoiceNone:
			p.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String(toolChoiceNone)}
		default:
			p.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
				OfFunctionToolChoice: &openai.ChatCompletionNamedToolChoiceParam{
					Function: openai.ChatCompletionNamedToolChoiceFunctionParam{Name: forceTool},
				},
			}
		}
	}
	resp, err := l.client.Chat.Completions.New(ctx, p, l.chatOpts()...)
	if err != nil {
		return nil, Usage{}, l.failed(model, err)
	}
	l.succeeded()
	us := usageOf(resp.Usage)
	if l.own {
		us.KeyOwner = keyOwnerOrg
	}
	return resp, l.priced(ctx, model, us), nil
}

// catalogueComplete says this endpoint's model list names every model it serves, so a model it
// does not list is one it does not serve. True of OpenAI's own API and of OpenRouter; not of
// Azure, which lists base models while requests name deployments, nor of a gateway, whose list is
// whatever its operator configured.
func (l *LLM) catalogueComplete() bool {
	return l.host == "api.openai.com" || l.host == "openrouter.ai"
}

// failed turns an error from an organisation's own endpoint into a sentence a person can act on
// (ModelKeyError), and records it. The deployment's own endpoint's errors go back as they came:
// its operator reads the log, not a Slack thread.
func (l *LLM) failed(model string, err error) error {
	if !l.own {
		return err
	}
	me := classifyModelError(l.host, model, err)
	if me == nil {
		return err
	}
	if l.report != nil {
		l.report(me.Error())
	}
	return me
}

func (l *LLM) succeeded() {
	if l.own && l.report != nil {
		l.report("")
	}
}

// Servable is the model a call on this endpoint should name, given the one the settings chose.
// Model ids live in many places — the organisation's default, a channel's, a thread's !model, a
// routine's, the fix worker's — and all of them may have been chosen for a different endpoint than
// the one in use now: before the organisation brought a key, or before it removed one. A model the
// endpoint does not serve is replaced by the endpoint's default rather than sent to fail:
//
//   - on an organisation's own OpenAI or OpenRouter endpoint, whose model lists are complete, by
//     looking it up there (an unreachable list lets the id through);
//   - on OpenRouter, where every id names its vendor ("openai/gpt-5-mini"), an id with no vendor is
//     one left behind by another endpoint, and needs no lookup to know it; on Azure, whose
//     deployment names cannot hold a "/", an id with one is the same.
//
// Anywhere else the id is trusted: an Azure deployment is named by its owner and listed nowhere.
func (l *LLM) Servable(ctx context.Context, model string) string {
	model = strings.TrimSpace(model)
	if model == "" || model == l.Model {
		return l.Model
	}
	// OpenRouter itself, not any host given its shape: a self-host's gateway in front of it may well
	// serve ids without a vendor.
	if l.host == "openrouter.ai" && !strings.Contains(model, "/") {
		slog.Info("model named for another endpoint; using this one's default", "model", model, "default", l.Model)
		return l.Model
	}
	// Azure names a model by its deployment, and a deployment's name cannot hold a "/": an id with
	// one was chosen for another endpoint.
	if l.own && l.dialect == dialectOpenAI && l.host != "api.openai.com" && strings.Contains(model, "/") {
		slog.Info("model named for another endpoint; using this one's default", "model", model, "default", l.Model, "host", l.host)
		return l.Model
	}
	if !l.own || !l.catalogueComplete() {
		return model
	}
	models, err := l.ListModels(ctx, false)
	if err != nil || len(models) == 0 {
		return model
	}
	base := model
	if i := strings.Index(base, ":"); i > 0 {
		base = base[:i]
	}
	for _, m := range models {
		if m.ID == model || m.ID == base {
			return model
		}
	}
	slog.Info("model not served by the organisation's endpoint; using its default", "model", model, "default", l.Model, "host", l.host)
	return l.Model
}

// priced fills in an estimated cost for a call the provider reported tokens for and no charge.
// A charge the provider did report is never replaced: it is what the account was billed.
func (l *LLM) priced(ctx context.Context, model string, us Usage) Usage {
	if us.CostUSD > 0 || l.pricer == nil || (us.In == 0 && us.Out == 0) {
		return us
	}
	if p, ok := l.pricer(ctx, model); ok {
		us.CostUSD, us.CostEstimated = p.cost(us), true
	}
	return us
}

// Embed returns one vector per input, in order.
func (l *LLM) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	resp, err := l.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: openai.EmbeddingModel(l.EmbedModel),
		Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
	})
	if err != nil {
		return nil, l.failed(l.EmbedModel, err)
	}
	l.succeeded()
	out := make([][]float32, len(resp.Data))
	for _, d := range resp.Data {
		v := make([]float32, len(d.Embedding))
		for i, f := range d.Embedding {
			v[i] = float32(f)
		}
		out[d.Index] = v
	}
	return out, nil
}

var (
	thinkRe      = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)
	fakeResultRe = regexp.MustCompile(`(?s)\s*<tool_result[^>]*>.*?</tool_result>\s*`)
	// An unterminated block counts: a call written as text is often the last thing in the
	// message, and the closing tag is the part that goes missing.
	textCallRe = regexp.MustCompile(`(?s)\s*<tool_call>\s*(.*?)\s*(?:</tool_call>|$)`)
)

// stripThinking removes <think>…</think> blocks a reasoning model might still emit, and any
// <tool_result> or <tool_call> blocks the model imitates from our tool-output framing. The
// <tool_call> case is a backstop for markup textToolCall could not rescue: whatever else is
// wrong with that answer, the raw markup is not something to post into a channel.
func stripThinking(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	s = fakeResultRe.ReplaceAllString(s, "\n")
	s = textCallRe.ReplaceAllString(s, "\n")
	return strings.TrimSpace(s)
}

// streamTags are the blocks stripThinking takes out, as a filter reading a stream has to see
// them: an opening tag, and the closing tag that ends the block.
var streamTags = [][2]string{
	{"<think>", "</think>"},
	{"<tool_call>", "</tool_call>"},
	{"<tool_result>", "</tool_result>"},
	{"<tool_result ", "</tool_result>"},
}

// markupFilter takes that markup out of a stream of deltas, before any of it can reach a
// message. stripThinking cleans a finished string, which is too late for an answer that is being
// streamed: a stream is append-only, so a <tool_call> block already sent stays in the thread no
// matter how clean the finished string is -- and the copy kept in the database was clean, which
// is why nothing noticed. The filter holds back anything that might still grow into a tag and
// writes out only what is safe to show. Deltas split tags anywhere, so "<tool" arriving alone is
// held rather than shown.
type markupFilter struct {
	held    string // not safe to write yet: a partial tag, or a block still waiting to be closed
	closing string // the closing tag being waited for; empty when not inside a block
}

// push takes the next delta and returns the part of it that is safe to write out.
func (f *markupFilter) push(delta string) string {
	f.held += delta
	var out strings.Builder
	for {
		if f.closing != "" {
			i := strings.Index(f.held, f.closing)
			if i < 0 {
				return out.String() // still inside the block: hold everything
			}
			f.held, f.closing = f.held[i+len(f.closing):], ""
			continue
		}
		i := strings.IndexByte(f.held, '<')
		if i < 0 {
			out.WriteString(f.held)
			f.held = ""
			return out.String()
		}
		out.WriteString(f.held[:i])
		f.held = f.held[i:]
		if open, closing, ok := tagAt(f.held); ok {
			f.held, f.closing = f.held[len(open):], closing
			continue
		}
		if partialTag(f.held) {
			return out.String() // might still become a tag: wait for the next delta
		}
		out.WriteString(f.held[:1]) // an ordinary '<'
		f.held = f.held[1:]
	}
}

func tagAt(s string) (open, closing string, ok bool) {
	for _, t := range streamTags {
		if strings.HasPrefix(s, t[0]) {
			return t[0], t[1], true
		}
	}
	return "", "", false
}

func partialTag(s string) bool {
	for _, t := range streamTags {
		if len(s) < len(t[0]) && strings.HasPrefix(t[0], s) {
			return true
		}
	}
	return false
}

// textToolCall pulls out a tool call the model wrote into its message content instead of
// returning it in tool_calls. Providers whose chat template renders calls as <tool_call> markup
// sometimes have a model write that markup out literally, and a turn is where it happens: a
// follow-up replays the thread as prose, so the round that did the work — and with it every
// structured call the model could copy the shape from — is nowhere in front of it. The call
// itself is real and usually right; only the channel it arrived on is wrong.
func textToolCall(content string) (name, args string, ok bool) {
	m := textCallRe.FindStringSubmatch(content)
	if m == nil {
		return "", "", false
	}
	block := strings.TrimSpace(m[1])
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(block), &call); err != nil || call.Name == "" {
		return keyValueCall(block)
	}
	// The markup carries arguments as an object; the wire format uses a JSON string. Tools are
	// handed a string either way.
	args = strings.TrimSpace(string(call.Arguments))
	var asString string
	if err := json.Unmarshal(call.Arguments, &asString); err == nil {
		args = asString
	}
	if args == "" || args == "null" {
		args = "{}"
	}
	return call.Name, args, true
}

var argPairRe = regexp.MustCompile(`(?s)<arg_key>\s*(.*?)\s*</arg_key>\s*<arg_value>\s*(.*?)\s*</arg_value>`)

// keyValueCall reads the other shape a <tool_call> block arrives in: the tool's name on its own,
// then <arg_key>/<arg_value> pairs. That is how GLM renders a call in its own chat template, so
// it is the shape the model writes out when it writes one as text -- and the shape that got past
// the JSON reader above, left the call unrun, and put the raw markup in front of a person.
func keyValueCall(block string) (name, args string, ok bool) {
	head := block
	if i := strings.Index(block, "<arg_key>"); i >= 0 {
		head = block[:i]
	}
	if f := strings.Fields(head); len(f) > 0 {
		name = f[0]
	}
	// A tool name and nothing else -- the same shape a routine's pinned step has to have.
	// Markup that is neither JSON nor this, a model writing `peek(a=1)` say, is not a call
	// anyone can run, and guessing at one would be worse than letting the block be stripped.
	if !toolNameRe.MatchString(name) {
		return "", "", false
	}
	var b strings.Builder
	b.WriteString("{")
	for i, p := range argPairRe.FindAllStringSubmatch(block, -1) {
		if i > 0 {
			b.WriteString(",")
		}
		b.Write(jsonString(p[1]))
		b.WriteString(":")
		b.Write(argJSON(p[2]))
	}
	b.WriteString("}")
	return name, b.String(), true
}

// argJSON renders one <arg_value> as JSON. The markup carries values bare, so a number has to be
// given back as a number: a tool that unmarshals `limit` into an int gets nothing at all from a
// quoted one, and falls back to its default without saying so. A value that merely starts with a
// quote stays a string -- the quotes in a log filter are the filter's own, not JSON's.
func argJSON(v string) []byte {
	if v != "" && json.Valid([]byte(v)) {
		switch v[0] {
		case '{', '[', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			return []byte(v)
		}
		if v == "true" || v == "false" || v == "null" {
			return []byte(v)
		}
	}
	return jsonString(v)
}

// jsonString quotes s as JSON without Go's default HTML escaping. Arguments rescued from markup
// are stored, logged and shown on the activity page, and a log filter full of \u003e is unreadable
// in all three.
func jsonString(s string) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}

var secretRes = []*regexp.Regexp{
	regexp.MustCompile(`xox[abpers]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`xapp-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`),     // fine-grained GitHub tokens
	regexp.MustCompile(`atj1\.[0-9]+\.[A-Za-z0-9_-]{40,}`), // fix-job worker tokens (jobs.go)
	regexp.MustCompile(`atk1\.[A-Za-z0-9_-]{40,}`),         // developer API keys (api_keys.go): 32 random bytes, base64url
	regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}`), // JWT
}

// redact masks obvious credentials before anything leaves for the model provider.
func redact(s string) string {
	for _, re := range secretRes {
		s = re.ReplaceAllString(s, "[redacted-secret]")
	}
	return s
}

// Redact and StripThinking are the same helpers for the worker binary (internal/worker), which
// shares this package's secret patterns so both ends scrub the same things.
func Redact(s string) string        { return redact(s) }
func StripThinking(s string) string { return stripThinking(s) }
