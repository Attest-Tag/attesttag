package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Web search is a plain fetch of DuckDuckGo's HTML endpoint (no API key), parsed with regexps.
// It is the floor, not the only option: an organisation that has named a provider on the Web
// settings tab gets Tavily, Firecrawl or Cloudflare instead (web_providers.go), and the tool
// definitions below do not change either way.

var httpClient = &http.Client{
	Transport: publicTransport(),
	Timeout:   20 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return checkURL(req.URL)
	},
}

const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36 attesttag/0.1"

// checkURL blocks non-http schemes and private/loopback hosts (SSRF guard).
func checkURL(u *url.URL) error {
	if err := publicURL(u); err != nil {
		return err
	}
	host := u.Hostname()
	ips, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return errors.New("private address blocked")
		}
	}
	return nil
}

func fetch(ctx context.Context, rawURL string, maxBytes int64) (string, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}
	if err := checkURL(u); err != nil {
		return "", "", err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en-US,en;q=0.8")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	return string(body), resp.Header.Get("Content-Type"), err
}

type searchHit struct{ Title, URL, Snippet string }

var (
	ddgResultRe  = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippetRe = regexp.MustCompile(`(?s)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	tagRe        = regexp.MustCompile(`(?s)<[^>]+>`)
	scriptRe     = regexp.MustCompile(`(?is)<(script|style|noscript|svg|nav|footer|header)[^>]*>.*?</(script|style|noscript|svg|nav|footer|header)>`)
	wsRe         = regexp.MustCompile(`[ \t\r\f]+`)
	nlRe         = regexp.MustCompile(`\n{3,}`)
)

func searchWeb(ctx context.Context, query string, n int) ([]searchHit, error) {
	body, _, err := fetch(ctx, "https://html.duckduckgo.com/html/?q="+url.QueryEscape(query), 512<<10)
	if err != nil {
		return nil, err
	}
	links := ddgResultRe.FindAllStringSubmatch(body, -1)
	snips := ddgSnippetRe.FindAllStringSubmatch(body, -1)
	var hits []searchHit
	for i, l := range links {
		href := html.UnescapeString(l[1])
		// DDG wraps results as //duckduckgo.com/l/?uddg=<encoded>&rut=…
		if u, err := url.Parse(href); err == nil {
			if real := u.Query().Get("uddg"); real != "" {
				href = real
			}
		}
		if strings.HasPrefix(href, "//") {
			href = "https:" + href
		}
		h := searchHit{Title: cleanHTML(l[2]), URL: href}
		if i < len(snips) {
			h.Snippet = cleanHTML(snips[i][1])
		}
		if strings.Contains(h.URL, "duckduckgo.com/y.js") {
			continue // ad
		}
		hits = append(hits, h)
		if len(hits) >= n {
			break
		}
	}
	if len(hits) == 0 && strings.Contains(body, "anomaly") {
		return nil, errors.New("search engine rate-limited this request; try again later")
	}
	return hits, nil
}

func cleanHTML(s string) string {
	s = scriptRe.ReplaceAllString(s, " ")
	s = regexp.MustCompile(`(?i)<(br|p|div|li|tr|h[1-6])[^>]*>`).ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = wsRe.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	s = strings.Join(lines, "\n")
	return strings.TrimSpace(nlRe.ReplaceAllString(s, "\n\n"))
}

// webSearch is web_search's body: the organisation's engine where it has one, the DuckDuckGo
// scrape where it has not. An engine that fails for a reason the next minute might fix — a rate
// limit, a bad gateway — falls through to the scrape with a line saying so, because one bad
// minute at Firecrawl is not a reason to have no answer. A failure that will keep failing does
// not fall through: a refused key or a request the engine will not accept is somebody's to fix,
// and quietly leaving the engine out of the path would hide both the bill being paid and the
// egress they thought they had bought.
func (a *Agent) webSearch(ctx context.Context, c *Call, query string, n int) (hits []searchHit, note string, err error) {
	cfg := a.webConfigFor(ctx, c.OrgID, roleSearch)
	var failed string // what the provider said, when it was something to work around
	if cfg.searches() {
		hits, err = providerSearch(ctx, cfg, query, n)
		if err == nil {
			return hits, "", nil
		}
		if configBroken(err) {
			return nil, "", err
		}
		failed = err.Error()
		note = fmt.Sprintf("(%s could not answer — %s — so these are built-in search results.)", cfg.Provider, failed)
	}
	hits, err = searchWeb(ctx, query, n)
	if err != nil && failed != "" {
		// Both paths are gone. The provider's reason is the one worth keeping — it is the one
		// somebody can act on — so it leads, rather than being replaced by DuckDuckGo's.
		return nil, "", fmt.Errorf("%s; the built-in search that followed also failed: %w", failed, err)
	}
	return hits, note, err
}

// webFetch is fetch_url's body, on the same terms.
func (a *Agent) webFetch(ctx context.Context, c *Call, target string) (text, note string, err error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", "", err
	}
	// Checked before the provider, not only before a direct fetch. A crawler would happily
	// report back what an internal hostname resolves to, and an intranet URL is not ours to
	// hand to one.
	if err := checkURL(u); err != nil {
		return "", "", err
	}
	cfg := a.webConfigFor(ctx, c.OrgID, roleFetch)
	var failed string
	if cfg.fetches() {
		text, err = providerFetch(ctx, cfg, target)
		if err == nil {
			return text, "", nil
		}
		if configBroken(err) {
			return "", "", err
		}
		failed = err.Error()
		note = fmt.Sprintf("(%s could not read it — %s — so this is a plain fetch of the page.)", cfg.Provider, failed)
	}
	body, ctype, err := fetch(ctx, target, 2<<20)
	if err != nil {
		if failed != "" {
			return "", "", fmt.Errorf("%s; the plain fetch that followed also failed: %w", failed, err)
		}
		return "", "", err
	}
	text = body
	if strings.Contains(ctype, "html") || strings.Contains(body[:min(len(body), 200)], "<") {
		text = cleanHTML(body)
	}
	return text, note, nil
}

func (a *Agent) registerWebTools() {
	a.register(Tool{
		Name:   "web_search",
		Desc:   "Search the public web. Returns titles, urls and snippets. Follow up with fetch_url to read a page.",
		Params: schema(map[string]any{"query": str("Search query")}, "query"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Query string }
			json.Unmarshal(args, &p)
			hits, note, err := a.webSearch(ctx, c, p.Query, 8)
			if err != nil {
				return "", err
			}
			if len(hits) == 0 {
				return strings.TrimSpace(note + " no results"), nil
			}
			var b strings.Builder
			if note != "" {
				b.WriteString(note + "\n")
			}
			for i, h := range hits {
				fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, h.Title, h.URL, truncate(h.Snippet, 300))
			}
			return b.String(), nil
		},
	})
	a.register(Tool{
		Name:   "fetch_url",
		Desc:   "Fetch a public web page and return its readable text (max ~20 KB). Only http(s); private hosts are blocked.",
		Params: schema(map[string]any{"url": str("Absolute URL")}, "url"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ URL string }
			json.Unmarshal(args, &p)
			text, note, err := a.webFetch(ctx, c, strings.Trim(p.URL, "<>"))
			if err != nil {
				return "", err
			}
			if note != "" {
				text = note + "\n" + text
			}
			return truncate(text, 20_000), nil
		},
	})
}
