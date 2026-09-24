package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withProvider points one provider's calls at a test server and lets the plain client through:
// webAPIClient normally refuses a loopback address, which is exactly what httptest hands out.
func withProvider(t *testing.T, id string, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	oldHost, oldClient := providerHosts[id], webAPIClient
	providerHosts[id] = srv.URL
	webAPIClient = srv.Client()
	t.Cleanup(func() {
		providerHosts[id] = oldHost
		webAPIClient = oldClient
		srv.Close()
	})
}

func TestTavilySearchAndExtract(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	withProvider(t, "tavily", func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&gotBody)
		if r.URL.Path == "/search" {
			w.Write([]byte(`{"results":[{"title":"Go","url":"https://go.dev","content":"The <b>Go</b> site"}]}`))
			return
		}
		w.Write([]byte(`{"results":[{"url":"https://go.dev","raw_content":"# Go\n\ntext"}]}`))
	})
	cfg := webConfig{Provider: "tavily", Key: "tvly-x"}

	hits, err := providerSearch(context.Background(), cfg, "go", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotPath != "/search" || gotAuth != "Bearer tvly-x" {
		t.Errorf("called %s with %q", gotPath, gotAuth)
	}
	if gotBody["query"] != "go" || gotBody["max_results"] != float64(5) {
		t.Errorf("body = %v", gotBody)
	}
	if len(hits) != 1 || hits[0].URL != "https://go.dev" || hits[0].Snippet != "The Go site" {
		t.Errorf("hits = %+v", hits) // snippet markup is stripped, as the built-in path does
	}

	text, err := providerFetch(context.Background(), cfg, "https://go.dev")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotPath != "/extract" || !strings.Contains(text, "# Go") {
		t.Errorf("extract %s -> %q", gotPath, text)
	}
}

func TestFirecrawlSearchReadsBothShapes(t *testing.T) {
	// v2 groups results by kind; v1 returned a flat array. A key on either plan must work,
	// because the settings page offers "Firecrawl", not "Firecrawl v2".
	for _, body := range []string{
		`{"success":true,"data":{"web":[{"title":"Go","url":"https://go.dev","description":"docs"}]}}`,
		`{"success":true,"data":[{"title":"Go","url":"https://go.dev","description":"docs"}]}`,
	} {
		withProvider(t, "firecrawl", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v2/search" {
				t.Errorf("search path = %s", r.URL.Path)
			}
			w.Write([]byte(body))
		})
		hits, err := providerSearch(context.Background(), webConfig{Provider: "firecrawl", Key: "fc-x"}, "go", 3)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(hits) != 1 || hits[0].Title != "Go" || hits[0].Snippet != "docs" {
			t.Errorf("%s -> %+v", body, hits)
		}
	}
}

func TestFirecrawlScrapeFallsBackToHTML(t *testing.T) {
	withProvider(t, "firecrawl", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":{"html":"<p>Hello</p><script>x()</script>"}}`))
	})
	text, err := providerFetch(context.Background(), webConfig{Provider: "firecrawl", Key: "fc-x"}, "https://go.dev")
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if text != "Hello" {
		t.Errorf("html fallback = %q", text)
	}
}

func TestCloudflarePutsTheAccountInThePath(t *testing.T) {
	var gotPath string
	withProvider(t, "cloudflare", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"success":true,"result":"# Example"}`))
	})
	cfg := webConfig{Provider: "cloudflare", AccountID: "acct1", Key: "cf-x"}
	text, err := providerFetch(context.Background(), cfg, "https://example.com")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotPath != "/client/v4/accounts/acct1/browser-rendering/markdown" || text != "# Example" {
		t.Errorf("path %q text %q", gotPath, text)
	}
	// Cloudflare has no general web search, so it never claims that half.
	if cfg.searches() {
		t.Error("cloudflare claimed to search")
	}
	if _, err := providerSearch(context.Background(), cfg, "go", 3); err == nil {
		t.Error("providerSearch should refuse a provider that cannot search")
	}
}

func TestProviderErrorSeparatesABadKeyFromABadMinute(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		broken  bool
		wantSub string
	}{
		{401, `{"detail":"invalid api key"}`, true, "invalid api key"},
		{402, `{}`, true, "the key was refused"},
		// Brave answers a bad subscription token with 422, and the message is nested one level
		// down inside "error". Neither may be mistaken for a bad minute.
		{422, `{"error":{"code":"SUBSCRIPTION_TOKEN_INVALID","detail":"The provided subscription token is invalid."}}`, true, "subscription token is invalid"},
		{400, `{"message":"bad request"}`, true, "bad request"},
		{404, `{}`, true, "HTTP 404"},
		{408, `{}`, false, "HTTP 408"},
		{429, `{}`, false, "rate limited"},
		{502, `{"error":"upstream"}`, false, "upstream"},
	} {
		withProvider(t, "tavily", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		})
		_, err := providerSearch(context.Background(), webConfig{Provider: "tavily", Key: "k"}, "go", 3)
		if err == nil {
			t.Fatalf("status %d returned no error", tc.status)
		}
		if !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("status %d error = %q, want it to mention %q", tc.status, err, tc.wantSub)
		}
		if configBroken(err) != tc.broken {
			t.Errorf("status %d: configBroken = %v, want %v", tc.status, configBroken(err), tc.broken)
		}
	}
}

func TestWebConfigFallsBackWithoutAUsableKey(t *testing.T) {
	for _, tc := range []struct {
		name              string
		cfg               webConfig
		searches, fetches bool
	}{
		{"no key", webConfig{Provider: "tavily"}, false, false},
		{"keyed", webConfig{Provider: "tavily", Key: "k"}, true, true},
		{"cloudflare without an account", webConfig{Provider: "cloudflare", Key: "k"}, false, false},
		{"cloudflare", webConfig{Provider: "cloudflare", Key: "k", AccountID: "a"}, false, true},
		{"built-in", webConfig{Provider: webProviderBuiltin}, false, false},
		{"unknown", webConfig{Provider: "nope", Key: "k"}, false, false},
	} {
		if got := tc.cfg.searches(); got != tc.searches {
			t.Errorf("%s: searches = %v, want %v", tc.name, got, tc.searches)
		}
		if got := tc.cfg.fetches(); got != tc.fetches {
			t.Errorf("%s: fetches = %v, want %v", tc.name, got, tc.fetches)
		}
	}
}

func TestValidateWebSetting(t *testing.T) {
	ok := []struct{ k, v string }{{"web_provider", ""}, {"web_provider", "firecrawl"}, {"web_account_id", "abc123"}}
	for _, c := range ok {
		if err := validateWebSetting(c.k, c.v); err != nil {
			t.Errorf("%s=%q: %v", c.k, c.v, err)
		}
	}
	bad := []struct{ k, v string }{{"web_provider", "bing"}, {"web_account_id", "https://api.cloudflare.com/x"}}
	for _, c := range bad {
		if err := validateWebSetting(c.k, c.v); err == nil {
			t.Errorf("%s=%q was accepted", c.k, c.v)
		}
	}
}

func TestWebKeyRoundTrip(t *testing.T) {
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got := st.WebKeyProviders(ctx, 1); len(got) != 0 {
		t.Fatalf("a fresh org already holds keys: %v", got)
	}
	if key, err := st.WebKey(ctx, 1, "firecrawl", sealer); key != "" || err != nil {
		t.Fatalf("missing key = %q, %v; want empty and no error", key, err)
	}
	if err := st.PutWebKey(ctx, 1, "firecrawl", "fc-secret", "admin@example.com", sealer); err != nil {
		t.Fatal(err)
	}
	// One row per provider: saving the same one again replaces it, a different one sits beside
	// it, and neither is visible to another organisation.
	if err := st.PutWebKey(ctx, 1, "firecrawl", "fc-second", "admin@example.com", sealer); err != nil {
		t.Fatal(err)
	}
	if err := st.PutWebKey(ctx, 1, "tavily", "tvly-1", "admin@example.com", sealer); err != nil {
		t.Fatal(err)
	}
	if key, err := st.WebKey(ctx, 1, "firecrawl", sealer); err != nil || key != "fc-second" {
		t.Errorf("firecrawl key = %q, %v", key, err)
	}
	if key, _ := st.WebKey(ctx, 1, "tavily", sealer); key != "tvly-1" {
		t.Errorf("tavily key = %q — one provider's key must never answer for another", key)
	}
	if key, _ := st.WebKey(ctx, 1, "cloudflare", sealer); key != "" {
		t.Errorf("cloudflare, which has no key, answered %q", key)
	}
	if got := strings.Join(st.WebKeyProviders(ctx, 1), ","); got != "firecrawl,tavily" {
		t.Errorf("providers = %q", got)
	}
	if got := st.WebKeyProviders(ctx, 2); len(got) != 0 {
		t.Errorf("another organisation sees %v", got)
	}
	if err := st.DeleteWebKey(ctx, 1, "firecrawl"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(st.WebKeyProviders(ctx, 1), ","); got != "tavily" {
		t.Errorf("after removing firecrawl: %q", got)
	}
}

// The routing decision, end to end through the tool body: which path a turn actually takes when
// the provider answers, stumbles, or refuses the key.
func TestWebToolsFallBackOnWeatherButNotOnABadKey(t *testing.T) {
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, NewProxy(sealer, st), newSettingsCache(st, Config{}))
	call := &Call{OrgID: 1}

	choose := func(provider string) {
		if err := st.PutSetting(ctx, 1, "web_provider", provider); err != nil {
			t.Fatal(err)
		}
		a.settings.Invalidate(1)
	}
	if err := st.PutWebKey(ctx, 1, "firecrawl", "fc-x", "admin", sealer); err != nil {
		t.Fatal(err)
	}

	// No provider chosen: the built-in path, and nothing is said about a provider.
	fake := func(status int, body string) {
		withProvider(t, "firecrawl", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(body))
		})
	}

	// The built-in path, stubbed. The target has to be an address the SSRF guard accepts —
	// a public IP on port 80 — because that guard runs before either path and an httptest
	// server is neither. The transport answers without a packet leaving the machine.
	const target = "http://93.184.216.34/"
	oldFetch := httpClient.Transport
	// roundTripFunc (oauth_user_test.go) stands in for the open web, so this never touches it.
	httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}},
			Body: io.NopCloser(strings.NewReader("<h1>Plain</h1>")), Request: r}, nil
	})
	defer func() { httpClient.Transport = oldFetch }()

	choose("firecrawl")
	fake(200, `{"success":true,"data":{"markdown":"# From Firecrawl"}}`)
	text, note, err := a.webFetch(ctx, call, target)
	if err != nil || note != "" || text != "# From Firecrawl" {
		t.Errorf("working provider: %q, note %q, err %v", text, note, err)
	}

	// Rate-limited: the page still comes back, with the reason attached.
	fake(429, `{"error":"slow down"}`)
	text, note, err = a.webFetch(ctx, call, target)
	if err != nil {
		t.Errorf("a rate limit should not have failed the fetch: %v", err)
	}
	if !strings.Contains(text, "Plain") {
		t.Errorf("fell back but returned %q", text)
	}
	if !strings.Contains(note, "slow down") || !strings.Contains(note, "plain fetch") {
		t.Errorf("note = %q; it has to say what happened", note)
	}

	// A refused key is a broken setting: it is reported, and nothing goes out direct.
	fake(401, `{"error":"invalid token"}`)
	if _, _, err = a.webFetch(ctx, call, target); err == nil || !strings.Contains(err.Error(), "invalid token") {
		t.Errorf("a refused key should surface, got %v", err)
	}

	// And a private address is refused before either path can see it.
	if _, _, err := a.webFetch(ctx, call, "http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Error("a link-local address was not blocked")
	}
}

// Each engine hides the same three fields somewhere different, and two of them are not even a
// POST with a Bearer token. This is the table that says we read each one the way it answers.
func TestEveryEngineNormalisesToTitleURLSnippet(t *testing.T) {
	for _, tc := range []struct {
		provider, host                  string
		body                            string
		wantPath                        string
		wantAuthHeader                  string
		wantAuthValue                   string
		wantMethod                      string
		wantTitle, wantURL, wantSnippet string
	}{
		{
			provider: "exa", host: "exa",
			body:     `{"results":[{"title":"Go","url":"https://go.dev","text":"full page","highlights":["the bit that matters"]}]}`,
			wantPath: "/search", wantAuthHeader: "X-Api-Key", wantAuthValue: "k", wantMethod: "POST",
			wantTitle: "Go", wantURL: "https://go.dev", wantSnippet: "the bit that matters",
		},
		{
			provider: "brave", host: "brave",
			body:     `{"web":{"results":[{"title":"Go","url":"https://go.dev","description":"docs &amp; more"}]}}`,
			wantPath: "/res/v1/web/search", wantAuthHeader: "X-Subscription-Token", wantAuthValue: "k", wantMethod: "GET",
			wantTitle: "Go", wantURL: "https://go.dev", wantSnippet: "docs & more",
		},
		{
			provider: "serper", host: "serper",
			body:     `{"organic":[{"title":"Go","link":"https://go.dev","snippet":"docs"}]}`,
			wantPath: "/search", wantAuthHeader: "X-Api-Key", wantAuthValue: "k", wantMethod: "POST",
			wantTitle: "Go", wantURL: "https://go.dev", wantSnippet: "docs",
		},
		{
			provider: "jina", host: "jina_search",
			body:     `{"code":200,"data":[{"title":"Go","url":"https://go.dev","description":"docs","content":"full"}]}`,
			wantPath: "/", wantAuthHeader: "Authorization", wantAuthValue: "Bearer k", wantMethod: "GET",
			wantTitle: "Go", wantURL: "https://go.dev", wantSnippet: "docs",
		},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			var gotPath, gotAuth, gotMethod, gotQuery string
			withProvider(t, tc.host, func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotMethod = r.URL.Path, r.Method
				gotQuery = r.URL.Query().Get("q")
				gotAuth = r.Header.Get(tc.wantAuthHeader)
				w.Write([]byte(tc.body))
			})
			hits, err := providerSearch(context.Background(), webConfig{Provider: tc.provider, Key: "k"}, "go", 5)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if gotMethod != tc.wantMethod || gotPath != tc.wantPath {
				t.Errorf("%s %s, want %s %s", gotMethod, gotPath, tc.wantMethod, tc.wantPath)
			}
			if gotAuth != tc.wantAuthValue {
				t.Errorf("%s = %q, want %q", tc.wantAuthHeader, gotAuth, tc.wantAuthValue)
			}
			if tc.wantMethod == "GET" && gotQuery != "go" {
				t.Errorf("a GET engine must carry the query in the URL, got %q", gotQuery)
			}
			if len(hits) != 1 {
				t.Fatalf("hits = %+v", hits)
			}
			if hits[0].Title != tc.wantTitle || hits[0].URL != tc.wantURL || hits[0].Snippet != tc.wantSnippet {
				t.Errorf("hit = %+v", hits[0])
			}
		})
	}
}

func TestExaAndJinaReadPages(t *testing.T) {
	withProvider(t, "exa", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/contents" {
			t.Errorf("exa fetch path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"results":[{"url":"https://go.dev","text":"The page"}]}`))
	})
	if text, err := providerFetch(context.Background(), webConfig{Provider: "exa", Key: "k"}, "https://go.dev"); err != nil || text != "The page" {
		t.Errorf("exa fetch = %q, %v", text, err)
	}

	var gotPath string
	withProvider(t, "jina", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"code":200,"data":{"title":"Go","url":"https://go.dev","content":"# Go"}}`))
	})
	text, err := providerFetch(context.Background(), webConfig{Provider: "jina", Key: "k"}, "https://go.dev/x?y=1")
	if err != nil || text != "# Go" {
		t.Errorf("jina fetch = %q, %v", text, err)
	}
	// The target rides as a path suffix, unescaped — that is the documented r.jina.ai form.
	if gotPath != "/https://go.dev/x" {
		t.Errorf("jina path = %q", gotPath)
	}

	// Search-only engines refuse the reading half rather than guessing an endpoint.
	for _, id := range []string{"brave", "serper"} {
		if _, err := providerFetch(context.Background(), webConfig{Provider: id, Key: "k"}, "https://go.dev"); err == nil {
			t.Errorf("%s claimed it could read a page", id)
		}
	}
}

// The reader follows the search engine only where that engine can read; a search-only engine
// leaves reading on the built-in path unless somebody names a reader.
func TestPageReaderFollowsSearchOnlyWhenItCanRead(t *testing.T) {
	for _, tc := range []struct{ search, fetch, wantReader string }{
		{"tavily", "", "tavily"},            // one choice, both halves
		{"brave", "", "builtin"},            // Brave cannot read: nothing is pretended
		{"brave", "firecrawl", "firecrawl"}, // the pairing this whole split exists for
		{"serper", "cloudflare", "cloudflare"},
		{"builtin", "", "builtin"},
		{"tavily", "builtin", "builtin"}, // deliberately sent back to the plain fetch
	} {
		set := Settings{WebProvider: tc.search, WebFetchProvider: tc.fetch}
		if got := providerFor(set, roleSearch); got != tc.search {
			t.Errorf("search %q + reader %q: searching with %q", tc.search, tc.fetch, got)
		}
		if got := providerFor(set, roleFetch); got != tc.wantReader {
			t.Errorf("search %q + reader %q: reading with %q, want %q", tc.search, tc.fetch, got, tc.wantReader)
		}
	}
}
