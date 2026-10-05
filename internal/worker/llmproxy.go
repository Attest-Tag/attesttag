package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"attesttag/internal/app"
)

// llmProxy stands between the coding agent and the model provider, on the worker's loopback.
// Every engine is an OpenAI-compatible client pointed at it, so whatever the engine — Qwen Code,
// pi — the job gets the same four things without the engine's cooperation:
//
//   - The real key never enters the sandbox. The engine runs with the repository's own code and
//     its shell commands inherit its environment; what that environment holds now is a token good
//     for this loopback port for the life of the job, not a provider key.
//   - What each call cost, as the provider reported it. On OpenRouter the request asks for usage
//     accounting, and the answer carries the money, the prompt tokens served from cache and the
//     provider that served it. That is the bill, on any key — the shared one included, where the
//     key's running total is everybody's spend.
//   - The cache kept warm. Every request of the job carries one session_id, so OpenRouter keeps the
//     job on the provider endpoint that holds its conversation; with providers named, they are
//     asked for in that order.
//   - A budget that stops the engine. Once the job's spend reaches it, calls are refused and stop()
//     ends the engine, which leaves what it changed to be committed as a draft.
type llmProxy struct {
	upstream   string // the provider's base URL, e.g. https://openrouter.ai/api/v1
	key        string // the real key, only ever in this process
	token      string // what the engine presents instead
	openRouter bool
	session    string
	providers  []string // OpenRouter provider slugs to ask for, in order
	budget     float64  // dollars; 0 means none
	report     func(app.JobUsage)
	stop       func() // called once, when the budget is reached

	client *http.Client
	srv    *http.Server
	addr   string

	mu      sync.Mutex
	total   app.JobUsage
	calls   int
	priced  int // calls whose answer carried a cost
	served  map[string]int
	stopped bool
}

// errBudget is why the engine was stopped when the proxy's budget ran out.
var errBudget = errors.New("the job's model budget is spent")

func newLLMProxy(llm app.JobLLMSecret, jobID int64, providers []string, budget float64, report func(app.JobUsage), stop func()) *llmProxy {
	tok := make([]byte, 24)
	rand.Read(tok)
	base := strings.TrimRight(llm.BaseURL, "/")
	return &llmProxy{upstream: base, key: llm.APIKey, token: "atp-" + hex.EncodeToString(tok),
		openRouter: strings.Contains(base, "openrouter.ai"), session: fmt.Sprintf("attesttag-job-%d", jobID),
		providers: providers, budget: budget, report: report, stop: stop,
		client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 10 * time.Minute,
			MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second}},
		served: map[string]int{}}
}

// start listens on a loopback port. The engine's base URL is BaseURL().
func (p *llmProxy) start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	p.addr = ln.Addr().String()
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go p.srv.Serve(ln)
	return nil
}

func (p *llmProxy) close() {
	if p.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		p.srv.Shutdown(ctx)
	}
}

// BaseURL is what the engine is configured with: the loopback address and the upstream's own
// path, so an engine that appends /chat/completions lands on the same route.
func (p *llmProxy) BaseURL() string {
	path := "/v1"
	if i := strings.Index(p.upstream, "://"); i >= 0 {
		if j := strings.Index(p.upstream[i+3:], "/"); j >= 0 {
			path = p.upstream[i+3+j:]
		}
	}
	return "http://" + p.addr + path
}

// Secret is the credential the engine gets: the loopback URL, the job's token and the model.
func (p *llmProxy) Secret(model string) app.JobLLMSecret {
	return app.JobLLMSecret{BaseURL: p.BaseURL(), APIKey: p.token, Model: model}
}

// Total is what the job's calls have cost so far, as the provider reported it.
func (p *llmProxy) Total() app.JobUsage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}

// Summary is one line for the job's log: calls, cache share, the money and who served them.
func (p *llmProxy) Summary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls == 0 {
		return ""
	}
	s := fmt.Sprintf("model calls: %d · %s in", p.calls, groupDigits(p.total.In))
	if p.total.In > 0 {
		s += fmt.Sprintf(" (%d%% from cache)", p.total.Cached*100/p.total.In)
	}
	s += fmt.Sprintf(" / %s out", groupDigits(p.total.Out))
	if p.priced > 0 {
		s += fmt.Sprintf(" · $%.4f as billed", p.total.CostUSD)
	}
	if len(p.served) > 0 {
		names := make([]string, 0, len(p.served))
		for n := range p.served {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool { return p.served[names[i]] > p.served[names[j]] })
		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, fmt.Sprintf("%s %d", n, p.served[n]))
		}
		s += " · served by " + strings.Join(parts, ", ")
	}
	return s
}

func (p *llmProxy) overBudget() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.budget > 0 && p.total.CostUSD >= p.budget
}

func (p *llmProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+p.token {
		writeProxyError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if p.overBudget() {
		writeProxyError(w, http.StatusPaymentRequired, errBudget.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, "unreadable request")
		return
	}
	path := r.URL.Path
	if base := strings.TrimPrefix(p.BaseURL(), "http://"+p.addr); base != "" && strings.HasPrefix(path, base) {
		path = strings.TrimPrefix(path, base)
	}
	completion := r.Method == http.MethodPost && strings.HasSuffix(path, "/chat/completions")
	if completion {
		body = p.amend(body)
	}
	target := p.upstream + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, "bad request")
		return
	}
	for k, vs := range r.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Authorization", "Host", "Content-Length", "Accept-Encoding", "Connection":
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	resp, err := p.client.Do(req)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "the model provider could not be reached")
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Connection", "Transfer-Encoding":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if !completion || resp.StatusCode >= 300 {
		io.Copy(w, resp.Body)
		return
	}
	var u callUsage
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		u = p.relayStream(w, resp.Body)
	} else {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		w.Write(raw)
		u.take(raw)
	}
	p.record(u)
}

// amend adds what OpenRouter needs to keep the cache and report the bill. A field the engine
// set itself is left alone.
func (p *llmProxy) amend(body []byte) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if stream, _ := m["stream"].(bool); stream {
		so, _ := m["stream_options"].(map[string]any)
		if so == nil {
			so = map[string]any{}
		}
		so["include_usage"] = true
		m["stream_options"] = so
	}
	if p.openRouter {
		if _, ok := m["session_id"]; !ok {
			m["session_id"] = p.session
		}
		m["usage"] = map[string]any{"include": true}
		if _, ok := m["provider"]; !ok && len(p.providers) > 0 {
			m["provider"] = map[string]any{"order": p.providers, "allow_fallbacks": true}
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// relayStream passes server-sent events through as they arrive and keeps the usage the last
// chunk carries.
func (p *llmProxy) relayStream(w http.ResponseWriter, body io.Reader) callUsage {
	var u callUsage
	fl, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(body, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			w.Write(line)
			if bytes.HasPrefix(line, []byte("data:")) {
				data := bytes.TrimSpace(line[5:])
				if len(data) > 0 && data[0] == '{' {
					u.take(data)
				}
			}
			if fl != nil && (len(bytes.TrimSpace(line)) == 0) {
				fl.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	if fl != nil {
		fl.Flush()
	}
	return u
}

// callUsage is one call's usage, from the answer or the last chunk of a stream that carries it.
type callUsage struct {
	in, cached, out int
	cost            float64
	priced          bool
	provider        string
	seen            bool
}

func (u *callUsage) take(raw []byte) {
	var v struct {
		Provider string `json:"provider"`
		Usage    *struct {
			Prompt  int      `json:"prompt_tokens"`
			Out     int      `json:"completion_tokens"`
			Cost    *float64 `json:"cost"`
			Details *struct {
				Cached int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	if v.Provider != "" {
		u.provider = v.Provider
	}
	if v.Usage == nil || (v.Usage.Prompt == 0 && v.Usage.Out == 0) {
		return
	}
	u.seen, u.in, u.out = true, v.Usage.Prompt, v.Usage.Out
	if v.Usage.Details != nil {
		u.cached = min(v.Usage.Details.Cached, u.in)
	}
	if v.Usage.Cost != nil {
		u.cost, u.priced = *v.Usage.Cost, true
	}
}

func (p *llmProxy) record(u callUsage) {
	if !u.seen {
		return
	}
	d := app.JobUsage{In: u.in, Cached: u.cached, Out: u.out, CostUSD: u.cost}
	p.mu.Lock()
	p.calls++
	if u.priced {
		p.priced++
	}
	if u.provider != "" {
		p.served[u.provider]++
	}
	p.total.In += d.In
	p.total.Cached += d.Cached
	p.total.Out += d.Out
	p.total.CostUSD += d.CostUSD
	over := p.budget > 0 && p.total.CostUSD >= p.budget && !p.stopped
	if over {
		p.stopped = true
	}
	p.mu.Unlock()
	if p.report != nil {
		p.report(d)
	}
	if over {
		slog.Warn("job budget reached; stopping the engine", "budget_usd", p.budget)
		if p.stop != nil {
			p.stop()
		}
	}
}

func writeProxyError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "code": code}})
}
