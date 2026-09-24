package app

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const pricedCatalogue = `{"data":[
 {"id":"openai/gpt-5-mini","pricing":{"prompt":"0.00000025","completion":"0.000002","input_cache_read":"0.000000025"}},
 {"id":"openai/gpt-4o","pricing":{"prompt":"0.0000025","completion":"0.00001"}},
 {"id":"z-ai/glm-5.3-flash","pricing":{"prompt":"0.0000004","completion":"0.0000016"}},
 {"id":"openrouter/auto","pricing":{"prompt":"-1","completion":"-1"}}
]}`

func catalogueLLM(t *testing.T, body string) *LLM {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return newLLM(Config{}, endpoint{BaseURL: srv.URL, Key: "k", Dialect: dialectCompatible})
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestPriceOfFindsTheModelAsOpenRouterNamesIt(t *testing.T) {
	l := catalogueLLM(t, pricedCatalogue)
	ctx := context.Background()
	for _, c := range []struct {
		model string
		ok    bool
		in    float64
	}{
		{"gpt-5-mini", true, 0.25e-6},            // OpenAI's own id, found as openai/gpt-5-mini
		{"gpt-5-mini-2025-08-07", true, 0.25e-6}, // a dated snapshot, found without its date
		{"openai/gpt-4o", true, 2.5e-6},          // already OpenRouter's spelling
		{"z-ai/glm-5.3-flash", true, 0.4e-6},     // a compatible gateway using OpenRouter's ids
		{"prod-gpt4o-deployment", false, 0},      // an Azure deployment name: nothing to go on
		{"openrouter/auto", false, 0},            // listed without a price
		{"", false, 0},
	} {
		p, ok := l.priceOf(ctx, c.model)
		if ok != c.ok || !near(p.In, c.in) {
			t.Errorf("priceOf(%q) = %+v, %v; want in=%g, %v", c.model, p, ok, c.in, c.ok)
		}
	}
	// A listed cache-read price is used for cached tokens; an unlisted one bills them as prompt.
	if p, _ := l.priceOf(ctx, "gpt-5-mini"); !near(p.CachedIn, 0.025e-6) {
		t.Errorf("gpt-5-mini cached price = %g, want the listed cache read", p.CachedIn)
	}
	if p, _ := l.priceOf(ctx, "gpt-4o"); !near(p.CachedIn, p.In) {
		t.Errorf("gpt-4o cached price = %g, want the prompt price %g when none is listed", p.CachedIn, p.In)
	}
}

func TestModelPriceCost(t *testing.T) {
	p := modelPrice{In: 1e-6, CachedIn: 0.1e-6, Out: 4e-6}
	got := p.cost(Usage{In: 10_000, CachedIn: 8_000, Out: 500, Reasoning: 300})
	// 2,000 fresh at 1e-6 + 8,000 cached at 0.1e-6 + 500 out at 4e-6. Reasoning is inside Out.
	if want := 0.002 + 0.0008 + 0.002; !near(got, want) {
		t.Errorf("cost = %g, want %g", got, want)
	}
	if got := p.cost(Usage{In: 10, CachedIn: 50}); got < 0 {
		t.Errorf("more cached than sent priced negative: %g", got)
	}
}

// A call the provider priced keeps the provider's figure; one it did not is given an estimate,
// and says it is one.
func TestChatPricesWhatTheProviderDidNot(t *testing.T) {
	for _, c := range []struct {
		name      string
		usage     string
		wantCost  float64
		estimated bool
	}{
		{"openai reports tokens only", `"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100}`, 1000*0.25e-6 + 100*2e-6, true},
		{"openrouter reports its charge", `"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100,"cost":0.5}`, 0.5, false},
		{"no usage at all", `"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, 0, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"1","object":"chat.completion","model":"gpt-5-mini","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` + c.usage + `}`))
		}))
		catalogue := catalogueLLM(t, pricedCatalogue)
		l := newLLM(Config{}, endpoint{BaseURL: srv.URL, Key: "k", Dialect: dialectOpenAI, Model: "gpt-5-mini"})
		l.pricer = catalogue.priceOf
		_, us, err := l.Chat(context.Background(), "", turnPrompt(), nil, "")
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !near(us.CostUSD, c.wantCost) || us.CostEstimated != c.estimated {
			t.Errorf("%s: cost %g estimated %v, want %g %v", c.name, us.CostUSD, us.CostEstimated, c.wantCost, c.estimated)
		}
	}
}

func TestAnEstimateStaysAnEstimateWhenAdded(t *testing.T) {
	var total Usage
	total.add(Usage{CostUSD: 0.1})
	total.add(Usage{CostUSD: 0.2, CostEstimated: true})
	total.add(Usage{CostUSD: 0.3})
	if !total.CostEstimated {
		t.Error("a total with an estimated part lost the mark")
	}
}
