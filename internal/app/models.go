package app

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3/option"
)

// ModelInfo is one entry from the provider's OpenAI-compatible model list. The plain
// OpenAI shape only carries an id; OpenRouter adds a display name, context length,
// modalities and pricing, which the console shows when present.
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Kind is "chat" or "embedding", so the console can offer the right list per field.
	Kind          string   `json:"kind"`
	ContextLength int      `json:"context_length,omitempty"`
	Inputs        []string `json:"input_modalities,omitempty"`
	OwnedBy       string   `json:"owned_by,omitempty"`
	// USD per million tokens, when the provider reports pricing. Nil when unknown.
	PromptPerM     *float64 `json:"prompt_per_m,omitempty"`
	CompletionPerM *float64 `json:"completion_per_m,omitempty"`
	// CacheReadPerM is what a prompt token served from the provider's cache costs, when that is
	// listed separately; nil means it is billed like any other prompt token.
	CacheReadPerM *float64 `json:"cache_read_per_m,omitempty"`
}

// modelsPage is the lenient wire shape of GET /models and GET /embeddings/models.
type modelsPage struct {
	Data []struct {
		ID            string  `json:"id"`
		Name          string  `json:"name"`
		OwnedBy       string  `json:"owned_by"`
		ContextLength float64 `json:"context_length"`
		Architecture  struct {
			InputModalities  []string `json:"input_modalities"`
			OutputModalities []string `json:"output_modalities"`
		} `json:"architecture"`
		Pricing struct {
			Prompt         string `json:"prompt"`
			Completion     string `json:"completion"`
			InputCacheRead string `json:"input_cache_read"`
		} `json:"pricing"`
	} `json:"data"`
}

const modelsCacheTTL = 10 * time.Minute

// modelRefreshes bounds how often one organisation may make the console ask the provider for its
// whole catalogue again. The picker's refresh button is the one honest reason to; past a handful the
// cached list is served, which is what every other caller is given anyway.
var modelRefreshes = newRateLimiter()

const modelRefreshesPerOrg = 6 // per ten minutes

// ListModels returns the provider's models, cached for modelsCacheTTL unless refresh is
// set. It reads /models and, where the provider has one, /embeddings/models (OpenRouter
// keeps embedding models there); a missing embeddings list is not an error.
//
// One fetch at a time, and never under the lock. The lock used to be held across the network call,
// so every refresh — which any member's console can ask for — made every caller that only wanted
// the cached list wait on the provider, and an organisation's own-key calls, image turns and model
// checks all read it. A caller arriving while a fetch runs waits for that one rather than starting
// another, and the fetch runs detached from any one request so that a closed tab does not fail it
// for the others waiting on it.
func (l *LLM) ListModels(ctx context.Context, refresh bool) ([]ModelInfo, error) {
	l.modelsMu.Lock()
	if !refresh && l.models != nil && time.Since(l.modelsAt) < modelsCacheTTL {
		m := l.models
		l.modelsMu.Unlock()
		return m, nil
	}
	if ch := l.modelsFetch; ch != nil {
		l.modelsMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		l.modelsMu.Lock()
		defer l.modelsMu.Unlock()
		if l.models == nil && l.modelsErr != nil {
			return nil, l.modelsErr
		}
		return l.models, nil
	}
	ch := make(chan struct{})
	l.modelsFetch = ch
	l.modelsMu.Unlock()

	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	out, err := l.fetchModels(fctx)
	cancel()

	l.modelsMu.Lock()
	if err == nil {
		l.models, l.modelsAt = out, time.Now()
	}
	l.modelsErr, l.modelsFetch = err, nil
	l.modelsMu.Unlock()
	close(ch)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// fetchModels reads the provider's lists. It holds no lock; ListModels decides who calls it.
func (l *LLM) fetchModels(ctx context.Context) ([]ModelInfo, error) {
	opts := []option.RequestOption{option.WithRequestTimeout(20 * time.Second), option.WithMaxRetries(1)}

	var chat modelsPage
	if err := l.client.Get(ctx, "models", nil, &chat, opts...); err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	// Only OpenRouter keeps a second list. Everywhere else the route is a 404 at best, and the
	// embedding models are already in /models, where modelKind finds them by name.
	var embed modelsPage
	if l.dialect == dialectOpenRouter {
		_ = l.client.Get(ctx, "embeddings/models", nil, &embed, opts...)
	}

	seen := map[string]bool{}
	var out []ModelInfo
	add := func(page modelsPage, kind string) {
		for _, m := range page.Data {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			info := ModelInfo{ID: m.ID, Name: m.Name, Kind: kind, ContextLength: int(m.ContextLength),
				Inputs: m.Architecture.InputModalities, OwnedBy: m.OwnedBy,
				PromptPerM: perMillion(m.Pricing.Prompt), CompletionPerM: perMillion(m.Pricing.Completion),
				CacheReadPerM: perMillion(m.Pricing.InputCacheRead)}
			if info.Kind == "" {
				info.Kind = modelKind(m.ID, m.Architecture.OutputModalities)
			}
			if info.Name == info.ID {
				info.Name = ""
			}
			out = append(out, info)
		}
	}
	add(chat, "")
	add(embed, "embedding")
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if out == nil {
		out = []ModelInfo{}
	}
	return out, nil
}

// modelKind guesses whether a model embeds or chats: OpenRouter says so in its output
// modalities; plain OpenAI lists everything under /models, so fall back to the id.
func modelKind(id string, outputs []string) string {
	for _, o := range outputs {
		if o == "embeddings" || o == "embedding" {
			return "embedding"
		}
	}
	if strings.Contains(strings.ToLower(id), "embed") {
		return "embedding"
	}
	return "chat"
}

// perMillion turns OpenRouter's per-token price string into USD per million tokens.
// Unknown or negative ("-1", the auto router) prices come back nil.
func perMillion(perToken string) *float64 {
	if perToken == "" {
		return nil
	}
	f, err := strconv.ParseFloat(perToken, 64)
	if err != nil || f < 0 {
		return nil
	}
	v := f * 1e6
	return &v
}

// acceptsImages reports whether a model takes image input, from the provider's catalogue. known
// is false when the catalogue is unreachable or silent about the model; the caller then leaves
// the request alone rather than second-guess a provider it cannot see. A ":variant" suffix
// (":nitro", ":exacto") resolves to its base model.
func (l *LLM) acceptsImages(ctx context.Context, model string) (accepts, known bool) {
	models, err := l.ListModels(ctx, false)
	if err != nil {
		return true, false
	}
	base := model
	if i := strings.Index(base, ":"); i > 0 {
		base = base[:i]
	}
	for _, m := range models {
		if m.ID != model && m.ID != base {
			continue
		}
		if len(m.Inputs) == 0 {
			return true, false
		}
		for _, in := range m.Inputs {
			if in == "image" {
				return true, true
			}
		}
		return false, true
	}
	return true, false
}

// modelPrice is one model's list price in USD per token.
type modelPrice struct{ In, CachedIn, Out float64 }

// cost prices one call's tokens. Prompt tokens served from cache are priced as cache reads when
// the catalogue lists that price, and as ordinary prompt tokens when it does not — the estimate
// errs high rather than low, since it is what a monthly budget is checked against. Reasoning is
// already inside Out on every provider that reports it, so it is not added again.
func (p modelPrice) cost(u Usage) float64 {
	fresh := u.In - u.CachedIn
	if fresh < 0 {
		fresh = 0
	}
	return float64(fresh)*p.In + float64(u.CachedIn)*p.CachedIn + float64(u.Out)*p.Out
}

// dateSuffixRe matches the snapshot date a provider may put on a model id it answers under
// (gpt-4o-2024-08-06, claude-sonnet-4-20250514), which a catalogue lists without.
var dateSuffixRe = regexp.MustCompile(`-(\d{4}-\d{2}-\d{2}|\d{8})$`)

// priceOf finds a model's list price in this endpoint's catalogue. It is how a call on an
// endpoint that reports no charge — OpenAI's own API reports tokens and nothing else — is given a
// figure: the model is looked up as named, as OpenRouter names OpenAI's models ("openai/<id>"),
// and both again without a snapshot date. An endpoint whose catalogue carries no prices, or a
// model it does not list, prices nothing, and the call stays unpriced rather than guessed at.
func (l *LLM) priceOf(ctx context.Context, model string) (modelPrice, bool) {
	models, err := l.ListModels(ctx, false)
	if err != nil || model == "" {
		return modelPrice{}, false
	}
	candidates := []string{model, "openai/" + model}
	if base := dateSuffixRe.ReplaceAllString(model, ""); base != model {
		candidates = append(candidates, base, "openai/"+base)
	}
	for _, id := range candidates {
		for _, m := range models {
			if m.ID != id || m.PromptPerM == nil || m.CompletionPerM == nil {
				continue
			}
			p := modelPrice{In: *m.PromptPerM / 1e6, Out: *m.CompletionPerM / 1e6}
			p.CachedIn = p.In
			if m.CacheReadPerM != nil {
				p.CachedIn = *m.CacheReadPerM / 1e6
			}
			return p, true
		}
	}
	return modelPrice{}, false
}
