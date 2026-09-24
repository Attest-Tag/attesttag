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
)

// A web provider is the paid front door to the public web: Tavily, Firecrawl, Cloudflare. It
// exists because the built-in path is a scrape of DuckDuckGo's HTML page and a plain GET of
// whatever comes back — the curl-and-regex answer. That works until the search engine decides
// this server is a robot, or the page is rendered by JavaScript, or the site refuses anything
// without a browser. A provider answers all three, and an organisation that has paid for one
// should not have the bot reaching for curl anyway.
//
// Which provider is chosen lives in settings (web_provider, web_account_id); the API key is
// sealed in web_secrets. Nothing here is per channel: web_search and fetch_url are native tools
// every channel already has, so the provider behind them is one organisation-wide choice.

// webProvider describes one service the Web settings tab can offer.
type webProvider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// What it can actually do. They differ: Cloudflare's Browser Rendering API fetches and
	// renders a page but has no general web search, so a Cloudflare organisation searches on
	// the built-in path and fetches through Cloudflare. Saying so here is what keeps that from
	// being a surprise at the tool call.
	Search       bool `json:"search"`
	Fetch        bool `json:"fetch"`
	NeedsAccount bool `json:"needs_account"` // Cloudflare puts an account id in the path
	// How the key is presented. They disagree: Bearer for most, X-API-KEY for Serper,
	// X-Subscription-Token for Brave, x-api-key for Exa. Empty means Authorization: Bearer.
	AuthHeader string `json:"-"`
	AuthPrefix string `json:"-"`
	// KeyLabel reads after the provider's name — "Firecrawl API key", "Brave Search
	// subscription token" — so it is capitalised for that position, not as a heading.
	KeyLabel    string `json:"key_label"`
	KeyHint     string `json:"key_hint"`
	Placeholder string `json:"placeholder"`
	DocsURL     string `json:"docs_url"`
	Blurb       string `json:"blurb"`
}

const webProviderBuiltin = "builtin"

// webProviders is the catalogue, in the order the console draws it: the built-in floor, then
// the ones that do both halves, then the search engines, then the page readers. Adding one is a
// row here plus a case in providerSearch and/or providerFetch. Microsoft retired the Bing
// Search API in August 2025, which is why the obvious name is missing.
var webProviders = []webProvider{
	{
		ID: webProviderBuiltin, Name: "Built-in (no key)", Search: true, Fetch: true,
		Blurb: "DuckDuckGo's HTML results and a plain fetch of the page. Free, and the first thing to be rate-limited or refused by sites that want a browser.",
	},
	{
		ID: "tavily", Name: "Tavily", Search: true, Fetch: true,
		KeyLabel: "API key", KeyHint: "From tavily.com → API keys.", Placeholder: "tvly-…",
		DocsURL: "https://docs.tavily.com/documentation/api-reference/endpoint/search",
		Blurb:   "Search built for models: ranked results with usable snippets, and /extract for reading a page.",
	},
	{
		ID: "firecrawl", Name: "Firecrawl", Search: true, Fetch: true,
		KeyLabel: "API key", KeyHint: "From firecrawl.dev → API keys.", Placeholder: "fc-…",
		DocsURL: "https://docs.firecrawl.dev/api-reference/endpoint/scrape",
		Blurb:   "Renders the page before reading it and returns Markdown, so JavaScript sites and anti-bot pages come back as text.",
	},
	{
		ID: "exa", Name: "Exa", Search: true, Fetch: true,
		AuthHeader: "x-api-key", AuthPrefix: "-",
		KeyLabel: "API key", KeyHint: "From exa.ai → API keys.", Placeholder: "exa key",
		DocsURL: "https://exa.ai/docs/reference/search",
		Blurb:   "Embeddings-based search rather than keywords, so it finds pages that mean the thing asked about. Returns page text with the results.",
	},
	{
		ID: "jina", Name: "Jina Reader", Search: true, Fetch: true,
		KeyLabel: "API key", KeyHint: "From jina.ai → API key. One key covers both r.jina.ai and s.jina.ai.", Placeholder: "jina_…",
		DocsURL: "https://jina.ai/reader/",
		Blurb:   "r.jina.ai turns any page into clean Markdown and s.jina.ai searches. A generous free tier, and the plainest output of the lot.",
	},
	{
		ID: "brave", Name: "Brave Search", Search: true,
		AuthHeader: "X-Subscription-Token", AuthPrefix: "-",
		KeyLabel: "subscription token", KeyHint: "From the Brave Search API dashboard.", Placeholder: "BSA…",
		DocsURL: "https://api-dashboard.search.brave.com/app/documentation/web-search/get-started",
		Blurb:   "An independent index — not a wrapper around somebody else's results — and cheap per query. Search only; pair it with a page reader below.",
	},
	{
		ID: "serper", Name: "Serper", Search: true,
		AuthHeader: "X-API-KEY", AuthPrefix: "-",
		KeyLabel: "API key", KeyHint: "From serper.dev → API key.", Placeholder: "serper key",
		DocsURL: "https://serper.dev",
		Blurb:   "Google's own results as JSON, fast and cheap. Search only; pair it with a page reader below.",
	},
	{
		ID: "cloudflare", Name: "Cloudflare Browser Rendering", Fetch: true, NeedsAccount: true,
		KeyLabel: "API token", KeyHint: "A token with the Browser Rendering permission, plus the account id it belongs to.",
		Placeholder: "cloudflare API token",
		DocsURL:     "https://developers.cloudflare.com/browser-rendering/rest-api/markdown-endpoint/",
		Blurb:       "Fetches through a real browser and returns Markdown. It has no general web search, so searching stays on the built-in path.",
	},
}

func webProviderByID(id string) (webProvider, bool) {
	for _, p := range webProviders {
		if p.ID == id {
			return p, true
		}
	}
	return webProvider{}, false
}

// webConfig is one organisation's answer, resolved: the provider, its account id and the
// unsealed key. It is built per call and never stored.
type webConfig struct {
	Provider  string
	AccountID string
	Key       string
}

// searches and fetches report whether the configured provider handles that half. A provider
// that does not (or one with no key yet) falls back to the built-in path rather than failing:
// the tools worked before anybody opened the Web tab and must keep working.
func (c webConfig) searches() bool {
	p, ok := webProviderByID(c.Provider)
	return ok && p.Search && c.Key != ""
}

func (c webConfig) fetches() bool {
	p, ok := webProviderByID(c.Provider)
	return ok && p.Fetch && c.Key != "" && (!p.NeedsAccount || c.AccountID != "")
}

// The two halves a provider can be chosen for. They are separate because the services are:
// Brave and Serper search and cannot read a page, Cloudflare reads a page and cannot search.
type webRole int

const (
	roleSearch webRole = iota
	roleFetch
)

// providerFor is the organisation's answer for one half, before the key is looked up. An empty
// reader follows the search engine, which is what somebody who picked Tavily and nothing else
// plainly meant; if that engine cannot read pages, nothing is chosen and the built-in path runs.
func providerFor(set Settings, role webRole) string {
	if role == roleSearch {
		return set.WebProvider
	}
	if set.WebFetchProvider != "" {
		return set.WebFetchProvider
	}
	if p, ok := webProviderByID(set.WebProvider); ok && p.Fetch {
		return set.WebProvider
	}
	return webProviderBuiltin
}

// webConfigFor reads the organisation's choice for one half and unseals that provider's key. An
// unreadable key is not fatal: the caller carries on down the built-in path.
func (a *Agent) webConfigFor(ctx context.Context, orgID int64, role webRole) webConfig {
	set := a.settings.Get(ctx, orgID)
	cfg := webConfig{Provider: providerFor(set, role), AccountID: set.WebAccountID}
	if cfg.Provider == "" || cfg.Provider == webProviderBuiltin {
		return webConfig{Provider: webProviderBuiltin}
	}
	key, err := a.store.WebKey(ctx, orgID, cfg.Provider, a.proxy.sealer)
	if err != nil || key == "" {
		return cfg // no usable key: searches()/fetches() say false and the built-in path runs
	}
	cfg.Key = key
	return cfg
}

// ---- calling a provider ----

// providerHosts is where each provider is called. A variable so a test can point the call at
// an httptest server and check what actually goes over the wire; nothing else rewrites it.
var providerHosts = map[string]string{
	"tavily":     "https://api.tavily.com",
	"firecrawl":  "https://api.firecrawl.dev",
	"cloudflare": "https://api.cloudflare.com",
	"exa":        "https://api.exa.ai",
	"brave":      "https://api.search.brave.com",
	"serper":     "https://google.serper.dev",
	// Jina is two hosts, one key: r… reads a page, s… searches.
	"jina":        "https://r.jina.ai",
	"jina_search": "https://s.jina.ai",
}

// webAPIClient is separate from httpClient: a rendered fetch takes longer than the twenty
// seconds a plain GET is given, and a provider host is a fixed endpoint rather than something
// the model chose, so there is no redirect to re-check.
var webAPIClient = &http.Client{Transport: publicTransport(), Timeout: 90 * time.Second}

// providerJSON calls a provider endpoint and decodes the reply into out. A nil body is a GET,
// which is how Brave and Jina are reached; everything else posts JSON. The provider's own error
// text is carried through — "Firecrawl: HTTP 401 — Unauthorized: Invalid token" tells an admin
// what to fix, where "http 401" does not.
func providerJSON(ctx context.Context, p webProvider, endpoint, key string, body, out any) error {
	method, reader := "GET", io.Reader(nil)
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		method, reader = "POST", bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set(nonEmpty(p.AuthHeader, "Authorization"), authPrefix(p)+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := webAPIClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", p.Name, err)
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return &providerErr{Provider: p.Name, Status: resp.StatusCode, Msg: decodeProviderMessage(dec)}
	}
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s: could not read the reply: %w", p.Name, err)
	}
	return nil
}

// authPrefix defaults to Bearer, and a provider that wants the bare key says so with a prefix
// of "-": an empty string cannot mean both "the default" and "nothing".
func authPrefix(p webProvider) string {
	switch p.AuthPrefix {
	case "":
		return "Bearer "
	case "-":
		return ""
	}
	return p.AuthPrefix
}

// decodeProviderMessage digs the human sentence out of an error body. Every provider buries it
// somewhere different — Tavily in detail, Jina in message, Brave inside a nested error object,
// Cloudflare in an errors array — so all of them are tried and the first real one wins.
func decodeProviderMessage(dec *json.Decoder) string {
	var e struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if dec.Decode(&e) != nil {
		return ""
	}
	var nested struct{ Message, Detail, Code string }
	_ = json.Unmarshal(e.Error, &nested)
	return pickMessage(e.Message, e.Detail, nested.Detail, nested.Message, nested.Code,
		firstErrMessage(e.Errors), string(e.Error))
}

func firstErrMessage(errs []struct {
	Message string `json:"message"`
}) string {
	if len(errs) > 0 {
		return errs[0].Message
	}
	return ""
}

// providerErr is a provider that answered with a status. It is a type rather than a string
// because the caller's next move turns on which status it was: a refused key means the
// organisation's choice is broken and has to be said out loud, while a rate limit or a bad
// gateway is a bad minute the built-in path can cover.
type providerErr struct {
	Provider string
	Status   int
	Msg      string
}

func (e *providerErr) Error() string {
	switch {
	case e.Msg != "":
		return fmt.Sprintf("%s: HTTP %d — %s", e.Provider, e.Status, e.Msg)
	case e.badKey():
		return fmt.Sprintf("%s: HTTP %d — the key was refused; check it in Settings → Web", e.Provider, e.Status)
	case e.Status == 429:
		return fmt.Sprintf("%s: HTTP %d — rate limited or out of credit", e.Provider, e.Status)
	}
	return fmt.Sprintf("%s: HTTP %d", e.Provider, e.Status)
}

// badKey is a refused credential — 401/403 the key itself, 402 the account behind it. Brave
// says the same thing with a 422, which is why this is only the wording and permanent() is the
// decision.
func (e *providerErr) badKey() bool {
	return e.Status == 401 || e.Status == 402 || e.Status == 403
}

// permanent reports whether the same call would fail the same way in a minute. Every 4xx is,
// except the two that are explicitly "not now": a request that was refused, malformed or sent
// to the wrong place will be refused, malformed and misaddressed on the retry too. 5xx and a
// dead connection are weather.
//
// It is a rule rather than a list of auth codes because the codes disagree — Brave answers 422
// to a bad token, Serper 403, Exa and Jina 401 — and a rule that only knew about 401 would have
// quietly sent a Brave organisation's searches down the built-in path for ever.
func (e *providerErr) permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != 408 && e.Status != 429
}

// configBroken reports whether err says the provider will keep failing until somebody changes
// something, as opposed to failing right now.
func configBroken(err error) bool {
	var pe *providerErr
	return errors.As(err, &pe) && pe.permanent()
}

// pickMessage takes the most specific thing the provider said, ignoring the empties and the
// essays. Providers disagree about which field carries it, so every candidate is offered.
func pickMessage(candidates ...string) string {
	for _, c := range candidates {
		c = strings.TrimSpace(strings.Trim(c, `"`))
		if c != "" && c != "null" && c != "{}" && len(c) < 400 {
			return c
		}
	}
	return ""
}

// providerSearch runs the search half. Callers check searches() first.
//
// Every case ends the same way — a list of title, url, snippet — because that is all the tool
// prints. What differs is where each provider hides those three: Tavily calls the snippet
// "content", Serper "snippet", Brave "description", Exa returns page text and highlights and no
// snippet at all. Normalising here keeps that out of the tool and out of the prompt.
func providerSearch(ctx context.Context, cfg webConfig, query string, n int) ([]searchHit, error) {
	p, ok := webProviderByID(cfg.Provider)
	if !ok || !p.Search {
		return nil, fmt.Errorf("%s cannot search the web", cfg.Provider)
	}
	switch cfg.Provider {
	case "tavily":
		var out struct {
			Results []struct {
				Title, URL, Content string
			} `json:"results"`
		}
		body := map[string]any{"query": query, "max_results": n, "search_depth": "basic", "include_answer": false}
		if err := providerJSON(ctx, p, providerHosts["tavily"]+"/search", cfg.Key, body, &out); err != nil {
			return nil, err
		}
		hits := make([]searchHit, 0, len(out.Results))
		for _, r := range out.Results {
			hits = append(hits, searchHit{Title: r.Title, URL: r.URL, Snippet: cleanHTML(r.Content)})
		}
		return hits, nil
	case "firecrawl":
		// v2 answers {"data":{"web":[…]}}; v1 answered {"data":[…]}. Both are read, so a key on
		// either plan works and neither has to be named on the settings page.
		var out struct {
			Data json.RawMessage `json:"data"`
		}
		body := map[string]any{"query": query, "limit": n}
		if err := providerJSON(ctx, p, providerHosts["firecrawl"]+"/v2/search", cfg.Key, body, &out); err != nil {
			return nil, err
		}
		return firecrawlHits(out.Data), nil
	case "exa":
		// Asking for text costs more than asking for links, so this asks for a little: enough
		// to stand in for a snippet, not a page the model never wanted. fetch_url is there for
		// the page, and it goes through Exa too when Exa is also the reader.
		var out struct {
			Results []struct {
				Title, URL, Text string
				Highlights       []string
			} `json:"results"`
		}
		body := map[string]any{"query": query, "numResults": n,
			"contents": map[string]any{"text": map[string]any{"maxCharacters": 600}}}
		if err := providerJSON(ctx, p, providerHosts["exa"]+"/search", cfg.Key, body, &out); err != nil {
			return nil, err
		}
		hits := make([]searchHit, 0, len(out.Results))
		for _, r := range out.Results {
			hits = append(hits, searchHit{Title: r.Title, URL: r.URL,
				Snippet: cleanHTML(nonEmpty(strings.Join(r.Highlights, " … "), r.Text))})
		}
		return hits, nil
	case "brave":
		var out struct {
			Web struct {
				Results []struct {
					Title, URL, Description string
				} `json:"results"`
			} `json:"web"`
		}
		// Brave is a GET, and it caps count at 20.
		q := url.Values{"q": {query}, "count": {strconv.Itoa(min(n, 20))}}
		if err := providerJSON(ctx, p, providerHosts["brave"]+"/res/v1/web/search?"+q.Encode(), cfg.Key, nil, &out); err != nil {
			return nil, err
		}
		hits := make([]searchHit, 0, len(out.Web.Results))
		for _, r := range out.Web.Results {
			hits = append(hits, searchHit{Title: cleanHTML(r.Title), URL: r.URL, Snippet: cleanHTML(r.Description)})
		}
		return hits, nil
	case "serper":
		var out struct {
			Organic []struct {
				Title, Link, Snippet string
			} `json:"organic"`
		}
		body := map[string]any{"q": query, "num": n}
		if err := providerJSON(ctx, p, providerHosts["serper"]+"/search", cfg.Key, body, &out); err != nil {
			return nil, err
		}
		hits := make([]searchHit, 0, len(out.Organic))
		for _, r := range out.Organic {
			hits = append(hits, searchHit{Title: r.Title, URL: r.Link, Snippet: cleanHTML(r.Snippet)})
		}
		return hits, nil
	case "jina":
		var out struct {
			Data []struct {
				Title, URL, Description, Content string
			} `json:"data"`
		}
		q := url.Values{"q": {query}}
		if err := providerJSON(ctx, p, providerHosts["jina_search"]+"/?"+q.Encode(), cfg.Key, nil, &out); err != nil {
			return nil, err
		}
		hits := make([]searchHit, 0, len(out.Data))
		for i, r := range out.Data {
			if i >= n {
				break // Jina decides how many to return; the tool decides how many to print
			}
			hits = append(hits, searchHit{Title: r.Title, URL: r.URL,
				Snippet: truncate(cleanHTML(nonEmpty(r.Description, r.Content)), 400)})
		}
		return hits, nil
	}
	return nil, fmt.Errorf("%s cannot search the web", cfg.Provider)
}

type firecrawlResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Snippet     string `json:"snippet"`
}

func firecrawlHits(data json.RawMessage) []searchHit {
	var flat []firecrawlResult
	if err := json.Unmarshal(data, &flat); err != nil {
		var grouped struct {
			Web  []firecrawlResult `json:"web"`
			News []firecrawlResult `json:"news"`
		}
		if err := json.Unmarshal(data, &grouped); err != nil {
			return nil
		}
		flat = append(grouped.Web, grouped.News...)
	}
	hits := make([]searchHit, 0, len(flat))
	for _, r := range flat {
		hits = append(hits, searchHit{Title: r.Title, URL: r.URL, Snippet: cleanHTML(nonEmpty(r.Description, r.Snippet))})
	}
	return hits
}

// providerFetch reads one page through the provider. Callers check fetches() first.
func providerFetch(ctx context.Context, cfg webConfig, target string) (string, error) {
	p, ok := webProviderByID(cfg.Provider)
	if !ok || !p.Fetch {
		return "", fmt.Errorf("%s cannot fetch pages", cfg.Provider)
	}
	switch cfg.Provider {
	case "tavily":
		var out struct {
			Results []struct {
				RawContent string `json:"raw_content"`
			} `json:"results"`
			Failed []struct {
				Error string `json:"error"`
			} `json:"failed_results"`
		}
		body := map[string]any{"urls": []string{target}, "format": "markdown"}
		if err := providerJSON(ctx, p, providerHosts["tavily"]+"/extract", cfg.Key, body, &out); err != nil {
			return "", err
		}
		if len(out.Results) == 0 {
			if len(out.Failed) > 0 && out.Failed[0].Error != "" {
				return "", fmt.Errorf("Tavily could not read that page: %s", out.Failed[0].Error)
			}
			return "", errors.New("Tavily returned nothing for that page")
		}
		return out.Results[0].RawContent, nil
	case "firecrawl":
		var out struct {
			Data struct {
				Markdown string `json:"markdown"`
				HTML     string `json:"html"`
			} `json:"data"`
		}
		body := map[string]any{"url": target, "formats": []string{"markdown"}, "onlyMainContent": true}
		if err := providerJSON(ctx, p, providerHosts["firecrawl"]+"/v2/scrape", cfg.Key, body, &out); err != nil {
			return "", err
		}
		if out.Data.Markdown == "" && out.Data.HTML != "" {
			return cleanHTML(out.Data.HTML), nil
		}
		if out.Data.Markdown == "" {
			return "", errors.New("Firecrawl returned an empty page")
		}
		return out.Data.Markdown, nil
	case "exa":
		var out struct {
			Results []struct {
				Text, Title string
			} `json:"results"`
		}
		body := map[string]any{"urls": []string{target}, "text": true}
		if err := providerJSON(ctx, p, providerHosts["exa"]+"/contents", cfg.Key, body, &out); err != nil {
			return "", err
		}
		if len(out.Results) == 0 || out.Results[0].Text == "" {
			return "", errors.New("Exa returned nothing for that page")
		}
		return out.Results[0].Text, nil
	case "jina":
		// The reader answers {"code":200,"data":{…}} and takes the target as a path suffix, so
		// the URL is not escaped — r.jina.ai/https://example.com/a?b=c is the documented form.
		var out struct {
			Data struct {
				Title, URL, Content string
			} `json:"data"`
		}
		if err := providerJSON(ctx, p, providerHosts["jina"]+"/"+target, cfg.Key, nil, &out); err != nil {
			return "", err
		}
		if out.Data.Content == "" {
			return "", errors.New("Jina returned an empty page")
		}
		return out.Data.Content, nil
	case "cloudflare":
		var out struct {
			Result  string `json:"result"`
			Success bool   `json:"success"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		endpoint := providerHosts["cloudflare"] + "/client/v4/accounts/" + url.PathEscape(cfg.AccountID) + "/browser-rendering/markdown"
		body := map[string]any{"url": target}
		if err := providerJSON(ctx, p, endpoint, cfg.Key, body, &out); err != nil {
			return "", err
		}
		if !out.Success || out.Result == "" {
			return "", fmt.Errorf("Cloudflare could not read that page: %s", nonEmpty(firstErrMessage(out.Errors), "empty result"))
		}
		return out.Result, nil
	}
	return "", fmt.Errorf("%s cannot fetch pages", cfg.Provider)
}

// checkWebProvider is the Test button: one real call, so an admin finds out the key works here
// rather than in a channel. It uses whichever half the provider offers.
func checkWebProvider(ctx context.Context, cfg webConfig) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if p, ok := webProviderByID(cfg.Provider); ok && p.NeedsAccount && cfg.AccountID == "" {
		return "", fmt.Errorf("%s needs the account id as well as the token", p.Name)
	}
	if cfg.searches() {
		hits, err := providerSearch(ctx, cfg, "attest tag slack agent", 3)
		if err != nil {
			return "", err
		}
		if len(hits) == 0 {
			return "", errors.New("the key worked but the search returned no results")
		}
		return fmt.Sprintf("Search works — %d results, first was %s", len(hits), hits[0].URL), nil
	}
	if cfg.fetches() {
		text, err := providerFetch(ctx, cfg, "https://example.com/")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Fetch works — example.com came back as %d characters", len(text)), nil
	}
	return "", errors.New("nothing to test: choose a provider and save a key first")
}

// ---- store ----

// PutWebKey seals and stores one organisation's key for one provider.
func (s *Store) PutWebKey(ctx context.Context, orgID int64, provider, key, by string, sealer *Sealer) error {
	enc, err := sealer.Seal([]byte(key))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `insert into web_keys (org_id, provider, secret_enc, updated_by, updated_at) values (?, ?, ?, ?, ?)
		on conflict(org_id, provider) do update set secret_enc=excluded.secret_enc, updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		orgID, provider, enc, by, now())
	return err
}

func (s *Store) DeleteWebKey(ctx context.Context, orgID int64, provider string) error {
	_, err := s.db.ExecContext(ctx, `delete from web_keys where org_id=? and provider=?`, orgID, provider)
	return err
}

// WebKey unseals one provider's key. No row is the empty string and no error: an organisation
// that has never set one is the ordinary case, not a failure.
func (s *Store) WebKey(ctx context.Context, orgID int64, provider string, sealer *Sealer) (string, error) {
	var enc []byte
	err := s.db.QueryRowContext(ctx, `select secret_enc from web_keys where org_id=? and provider=?`, orgID, provider).Scan(&enc)
	if err != nil || len(enc) == 0 {
		return "", nil
	}
	plain, err := sealer.Open(enc)
	if err != nil {
		return "", fmt.Errorf("the stored %s key cannot be read with the current MASTER_KEY: %w", provider, err)
	}
	return string(plain), nil
}

// WebKeyProviders lists the providers this organisation holds a key for, so the console can
// say "Saved" about the one being looked at without unsealing anything.
func (s *Store) WebKeyProviders(ctx context.Context, orgID int64) []string {
	rows, err := s.db.QueryContext(ctx, `select provider from web_keys where org_id=? and length(coalesce(secret_enc,''))>0 order by provider`, orgID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// ---- console ----

// handleWebKey stores the provider key. It never comes back out: the console is told only
// whether one exists (Settings.WebKeySet), which is the whole of what a page needs to draw
// "Saved" and a Replace button.
func (b *Bot) handleWebKey(w http.ResponseWriter, r *http.Request) {
	var in struct{ Provider, Key string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	provider, err := namedProvider(strings.TrimSpace(in.Provider))
	if err != nil {
		bad(w, err)
		return
	}
	key := strings.TrimSpace(in.Key)
	if key == "" {
		bad(w, errors.New("paste the key, or use Remove to clear the one that is stored"))
		return
	}
	if len(key) > 1000 {
		bad(w, errors.New("that is too long to be an API key"))
		return
	}
	me := adminFromCtx(r.Context())
	if err := b.store.PutWebKey(r.Context(), orgOf(r), provider.ID, key, me.Email, b.sealer); err != nil {
		fail(w, err)
		return
	}
	b.changed(r.Context(), orgOf(r))
	writeJSON(w, 200, map[string]any{"ok": true, "provider": provider.ID})
}

func (b *Bot) handleWebKeyDelete(w http.ResponseWriter, r *http.Request) {
	provider, err := namedProvider(strings.TrimSpace(r.URL.Query().Get("provider")))
	if err != nil {
		bad(w, err)
		return
	}
	if err := b.store.DeleteWebKey(r.Context(), orgOf(r), provider.ID); err != nil {
		fail(w, err)
		return
	}
	b.changed(r.Context(), orgOf(r))
	writeJSON(w, 200, map[string]any{"ok": true, "provider": provider.ID})
}

// namedProvider resolves a provider the console named, refusing the built-in one: it has no
// key, so storing one against it would be storing a secret nothing will ever spend.
func namedProvider(id string) (webProvider, error) {
	p, ok := webProviderByID(id)
	if !ok {
		return webProvider{}, fmt.Errorf("unknown web provider %q", id)
	}
	if p.ID == webProviderBuiltin {
		return webProvider{}, errors.New("the built-in path takes no key")
	}
	return p, nil
}

// handleWebKeyTest makes one real call. A key may be passed in unsaved, so somebody can find
// out whether it works before committing it to the organisation's configuration; with no key
// in the body it tests what is stored.
func (b *Bot) handleWebKeyTest(w http.ResponseWriter, r *http.Request) {
	var in struct{ Provider, AccountID, Key string }
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	ctx, org := r.Context(), orgOf(r)
	set := b.settings.Get(ctx, org)
	cfg := webConfig{
		Provider:  nonEmpty(strings.TrimSpace(in.Provider), set.WebProvider),
		AccountID: nonEmpty(strings.TrimSpace(in.AccountID), set.WebAccountID),
		Key:       strings.TrimSpace(in.Key),
	}
	if _, ok := webProviderByID(cfg.Provider); !ok {
		bad(w, fmt.Errorf("unknown web provider %q", cfg.Provider))
		return
	}
	if cfg.Provider == webProviderBuiltin {
		writeJSON(w, 200, map[string]any{"ok": true, "detail": "The built-in path needs no key."})
		return
	}
	if cfg.Key == "" {
		stored, err := b.store.WebKey(ctx, org, cfg.Provider, b.sealer)
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "detail": err.Error()})
			return
		}
		cfg.Key = stored
	}
	if cfg.Key == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "detail": "No key is stored for this provider yet."})
		return
	}
	detail, err := checkWebProvider(ctx, cfg)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "detail": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "detail": detail})
}
