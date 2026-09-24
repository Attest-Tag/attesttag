package app

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ---- resolver: what a channel may reach ----

// Rule is one connection's allow rule, ranked by scope narrowness (channel beats workspace).
type Rule struct {
	Conn   *Connection
	Rank   int
	Notes  string
	Direct bool // attached to the scope on its own, not through its bundle
}

// Access is the resolved view for one channel: rules, credential-less domains, concatenated
// instructions, enabled tool packs. Cached for a minute; ConfigVersion invalidates it.
type Access struct {
	Rules        []Rule
	Domains      []Domain
	Instructions string
	ToolPacks    map[string]bool
	Version      int64
	BundleNames  []string
	AllowRules   []string // scope-level auto mode allow rules, workspace scope first
	DefaultRepo  string   // owner/name the model assumes when none is named: channel setting, else workspace
	// Whether AllowRules are consulted at all on a turn a forwarded email started. Resolved
	// here rather than walked again at the gate so that it is cached and invalidated with the
	// rules it governs — a rule and the permission to use it going stale apart is the one way
	// this could be wrong in the direction that matters.
	EmailAutoWrites bool
	fetched      time.Time
}

// Repos lists the repository connections reachable here, narrowest grant first, one per repo.
func (a *Access) Repos() []*Connection {
	seen := map[string]bool{}
	var out []*Connection
	for _, r := range a.Rules {
		if r.Conn.Repo == "" || seen[r.Conn.Repo] {
			continue
		}
		seen[r.Conn.Repo] = true
		out = append(out, r.Conn)
	}
	return out
}

type Resolver struct {
	store *Store
	mu    sync.Mutex
	cache map[string]*Access
}

func NewResolver(st *Store) *Resolver {
	return &Resolver{store: st, cache: map[string]*Access{}}
}

// Invalidate drops one organisation's resolved access. The cache key starts with the
// organisation id, so this is a prefix sweep rather than a rebuild of everybody's.
func (r *Resolver) Invalidate(orgID int64) {
	prefix := strconv.FormatInt(orgID, 10) + ":"
	r.mu.Lock()
	for k := range r.cache {
		if strings.HasPrefix(k, prefix) {
			delete(r.cache, k)
		}
	}
	r.mu.Unlock()
}

// Resolve walks the three links of the hierarchy — the account, the Slack workspace the
// message came from, and the channel — widest first, so a narrower link's instructions come
// last and its rules win. Ranks are 0/1/2 with the channel still at 2, because "attached here
// rather than inherited" is tested as Rank == 2 in three other files.
func (r *Resolver) Resolve(ctx context.Context, orgID int64, teamID, channel string, version int64) (*Access, error) {
	key := fmt.Sprintf("%d:%s:%s", orgID, teamID, channel)
	r.mu.Lock()
	if a, ok := r.cache[key]; ok && a.Version == version && time.Since(a.fetched) < time.Minute {
		r.mu.Unlock()
		return a, nil
	}
	r.mu.Unlock()
	acc := &Access{ToolPacks: map[string]bool{}, Version: version, fetched: time.Now()}
	// Each link carries its own key: the account row has no team and no slack id of its own,
	// the team row is keyed by T… in both columns, the channel row by its team plus C….
	chain := []struct {
		kind, team, slackID string
		rank                int
	}{{"workspace", "", "", 0}, {"team", teamID, teamID, 1}, {"channel", teamID, channel, 2}}
	if channel == teamID || channel == "" { // the workspace row itself: no channel link to add
		chain = chain[:2]
	}
	var instr []string
	bundles := map[int64]*Bundle{} // each bundle loaded once per resolve
	bundle := func(id int64) *Bundle {
		if b, ok := bundles[id]; ok {
			return b
		}
		b, err := r.store.Bundle(ctx, orgID, id)
		if err != nil {
			b = nil
		}
		bundles[id] = b
		return b
	}
	for _, link := range chain {
		sc, err := r.store.ScopeFor(ctx, orgID, link.kind, link.team, link.slackID)
		if err != nil {
			return nil, err
		}
		if sc == nil {
			continue
		}
		if strings.TrimSpace(sc.Instructions) != "" {
			instr = append(instr, strings.TrimSpace(sc.Instructions))
		}
		acc.AllowRules = append(acc.AllowRules, sc.AllowRules...)
		// Narrowest wins, so the channel has the final word: the chain runs workspace, team,
		// channel, and only a link that says something overrides the one above it. Same shape as
		// emailIntakeOn, which walks the chain the other way round for the same answer.
		switch sc.EmailAutoWrites {
		case "on":
			acc.EmailAutoWrites = true
		case "off":
			acc.EmailAutoWrites = false
		}
		if repo := strings.TrimSpace(sc.DefaultRepo); repo != "" {
			acc.DefaultRepo = repo // the channel link comes last, so it overrides the workspace
		}
		granted := map[int64]bool{} // connections already granted at this link
		for _, bid := range sc.BundleIDs {
			b := bundle(bid)
			if b == nil {
				continue
			}
			acc.BundleNames = append(acc.BundleNames, b.Name)
			if strings.TrimSpace(b.Instructions) != "" {
				instr = append(instr, strings.TrimSpace(b.Instructions))
			}
			for _, p := range b.ToolPacks {
				acc.ToolPacks[p] = true
			}
			for _, c := range b.Connections {
				if c.Status != "active" {
					continue
				}
				granted[c.ID] = true
				acc.Rules = append(acc.Rules, Rule{Conn: c, Rank: link.rank, Notes: c.Notes})
			}
			acc.Domains = append(acc.Domains, b.Domains...)
		}
		// One-off grants: just the connection, plus the tool pack for its preset when its
		// bundle enables that pack. The bundle's instructions, skills and domains stay behind.
		for _, cid := range sc.ConnectionIDs {
			if granted[cid] {
				continue
			}
			c, err := r.store.Connection(ctx, orgID, cid)
			if err != nil || c == nil || c.Status != "active" {
				continue
			}
			granted[cid] = true
			acc.Rules = append(acc.Rules, Rule{Conn: c, Rank: link.rank, Notes: c.Notes, Direct: true})
			if b := bundle(c.BundleID); b != nil {
				for _, p := range b.ToolPacks {
					if p == c.Preset {
						acc.ToolPacks[p] = true
					}
				}
			}
		}
	}
	var bundleIDs []int64
	seenB := map[int64]bool{}
	for _, link := range chain {
		if sc, _ := r.store.ScopeFor(ctx, orgID, link.kind, link.team, link.slackID); sc != nil {
			for _, id := range sc.BundleIDs {
				if !seenB[id] {
					seenB[id] = true
					bundleIDs = append(bundleIDs, id)
				}
			}
		}
	}
	if sk := r.store.skillsText(ctx, orgID, bundleIDs); sk != "" {
		instr = append(instr, "Skills (how to work with the connected tools):"+sk)
	}
	acc.Instructions = strings.Join(instr, "\n\n")
	sort.SliceStable(acc.Rules, func(i, j int) bool { return acc.Rules[i].Rank > acc.Rules[j].Rank }) // narrowest first
	r.mu.Lock()
	r.cache[key] = acc
	r.mu.Unlock()
	return acc, nil
}

// hostMatch implements the leftmost-wildcard rule: *.example.com covers subdomains at any depth
// but not example.com itself.
func hostMatch(pattern, host string) bool {
	pattern, host = strings.ToLower(pattern), strings.ToLower(host)
	if strings.HasPrefix(pattern, "*.") {
		return strings.HasSuffix(host, pattern[1:]) && host != pattern[2:]
	}
	return pattern == host
}

// ReachableHosts lists what the model may call in this channel, for the tool description.
// HasPersonal reports whether anything this channel reaches runs on the asker's own account
// rather than on a credential an admin set up. It decides who gets the connect_account tool,
// and what the prompt can say about whose account is missing.
func (a *Access) HasPersonal() bool {
	for _, r := range a.Rules {
		if r.Conn != nil && r.Conn.CredType == "oauth_user" {
			return true
		}
	}
	return false
}

func (a *Access) ReachableHosts() []string {
	seen := map[string]bool{}
	var out []string
	add := func(h, via string) {
		k := h + " (" + via + ")"
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, r := range a.Rules {
		for _, h := range r.Conn.AllowedHosts {
			add(h, r.Conn.Name)
		}
	}
	for _, d := range a.Domains {
		add(d.Host, "no credential")
	}
	return out
}

// SharedHosts names the hosts this channel reaches through more than one connection, each with
// the connections sharing it: "api.clickup.com (ClickUp, ClickUp testing)".
//
// Without being told, nothing can tell there is a choice to be made. A host that is reachable
// gets called, the highest-ranked credential answers, and the reply is about whichever ClickUp
// workspace happened to sort first — right half the time and wrong silently the other half.
// Named here, the choice becomes a question that can be asked or a name that can be passed.
func (a *Access) SharedHosts() []string {
	var order []string
	byHost := map[string][]string{}
	for _, r := range a.Rules {
		for _, h := range r.Conn.AllowedHosts {
			if _, seen := byHost[h]; !seen {
				order = append(order, h)
			}
			byHost[h] = append(byHost[h], r.Conn.Name)
		}
	}
	var out []string
	for _, h := range order {
		if len(byHost[h]) > 1 {
			out = append(out, h+" ("+strings.Join(byHost[h], ", ")+")")
		}
	}
	return out
}

// ---- proxy ----

type ProxyRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// Connection names which credential to send when a host has more than one in the channel —
	// two ClickUp workspaces, a production and a testing GCP key. Optional, and empty is the
	// old behaviour exactly: the highest-ranked connection covering the URL. It is a choice
	// between credentials somebody has already been granted, never a way to reach a new one.
	Connection string `json:"connection,omitempty"`
	// MaxBytes caps how much of the response is read, 0 meaning modelMaxBody. Never set from
	// the model's arguments (json:"-"): a caller that can hold a large body asks for one.
	MaxBytes int `json:"-"`
	// HoldWrites is a console preview's (playground.go): a request that would change something
	// comes back ErrNeedsConfirmation even through a connection whose writes are automatic, once
	// every other check has had its say, so a preview sends no write and still reports a blocked
	// request as blocked. Never from the model's arguments either.
	HoldWrites bool `json:"-"`
	// AttachFiles are Slack file ids to upload to whatever this request creates, once it has
	// been run and has said what it created. The proxy itself ignores them: they are an
	// instruction to the caller that runs the request, not part of it.
	//
	// Ids rather than bytes, because a held write is stored as JSON and json.Marshal rewrites
	// invalid UTF-8 as U+FFFD — a PNG that went into a pending row would come back corrupted.
	// The bytes are fetched from Slack at the moment of the upload instead.
	AttachFiles []string `json:"attach_files,omitempty"`
}

type ProxyResponse struct {
	Status int
	Body   string
	Conn   *Connection
	// Truncated says the body is the first MaxBytes of a longer response, so nothing mistakes
	// half a JSON document for the whole of one.
	Truncated bool
	// RetryAfter is how long the service says to wait before asking again, on the responses that
	// say so. A rate limit is not a refusal and must not be reported as one: "GitHub allows ten
	// code searches a minute, try again in 40 seconds" is a thing a model can act on, where a
	// bare 403 reads as "this credential cannot do that" and the model stops asking for good.
	RetryAfter time.Duration
}

// retryAfterHint reads the two ways a service says when to come back: Retry-After, in seconds or as
// an HTTP date, and the X-RateLimit-Reset epoch that GitHub sends with a spent budget. Zero when
// the response says neither, which is every response that is not a rate limit.
func retryAfterHint(h http.Header, now time.Time) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
		if when, err := http.ParseTime(v); err == nil {
			if d := when.Sub(now); d > 0 {
				return d
			}
			return time.Second
		}
	}
	// Only meaningful once the budget is actually spent: the reset time is sent on every
	// response, and treating it as a wait on a successful call would stall everything.
	if h.Get("X-RateLimit-Remaining") == "0" {
		if epoch, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-RateLimit-Reset")), 10, 64); err == nil {
			if d := time.Until(time.Unix(epoch, 0)); d > 0 {
				return d
			}
		}
	}
	return 0
}

// How much of a response the proxy will read. modelMaxBody is what a tool result carries back
// into the prompt — large enough for a real API page, small enough not to swamp the context —
// and proxyMaxRead is the ceiling for callers that parse the body themselves and need all of it.
//
// 256 KiB was neither of those things: a body that size is ~64k tokens, and a tool result is not
// paid once but on every remaining round of the turn, which is how a single wide query turned a
// 12k-token turn into a 74k one. 48 KiB still holds a real page of API JSON, and what it cuts is
// now said out loud (bodyNote) rather than silently dropped, so the model narrows the query or
// moves the job into run_js, where fetch() reads up to proxyMaxRead and only the answer comes back.
const (
	modelMaxBody = 48 << 10
	proxyMaxRead = 10 << 20
)

var ErrNeedsConfirmation = errors.New("needs confirmation")

type Proxy struct {
	sealer *Sealer
	store  *Store
	client *http.Client
	mu     sync.Mutex
	tokens map[tokenCacheKey]cachedToken // globally unique connection ID + exchange kind + credential digest
	// ghApp is the GitHub App installation tokens are minted from (github_token.go). Set after
	// construction by bot.go; nil on a deployment with no app, where github_app connections
	// cannot exist in the first place.
	ghApp *githubApp
}

type tokenCacheKey struct {
	id     int64
	kind   string
	digest [32]byte
}

func exchangeKey(id int64, kind string, secret *Secret) tokenCacheKey {
	raw, _ := json.Marshal(secret)
	return tokenCacheKey{id, kind, sha256.Sum256(raw)}
}

type cachedToken struct {
	token  string
	expiry time.Time
}

func NewProxy(sealer *Sealer, st *Store) *Proxy {
	return &Proxy{sealer: sealer, store: st, tokens: map[tokenCacheKey]cachedToken{},
		client: &http.Client{Transport: publicTransport(), Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errors.New("redirects are not followed through the proxy")
		}}}
}

func isWrite(method string) bool {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "OPTIONS":
		return false
	}
	return true
}

// Endpoints that only look despite answering to POST. Holding one turns "is Priya free at six?"
// into a Confirm card, and somebody who presses Confirm in order to look at something soon
// presses it without reading — the one habit the gate exists to prevent.
//
// Two path conventions catch most of them without knowing the host, which matters because half
// the services here live on a hostname the customer owns (your-org.grafana.net, your-site.
// atlassian.net) and a table keyed by host would never see them:
//
//   - Google's method names, where the last segment is "collection:verb" and the verb after the
//     colon is the method — entries:list, files:batchGet. Google's own API rules reserve those
//     names, so the verb is not something a service author chose for a resource.
//   - the read endpoints everyone else settled on: .../search, Elasticsearch's _search, and the
//     query endpoint that reads a datasource (a Notion database, Grafana, Prometheus).
//
// The two sets are deliberately not the same one. ":list" is a read, and a bare "list" is not,
// because ClickUp creates a list by POSTing to .../folder/{id}/list. A bare "read" is not one
// either: half the APIs in the world mark a notification read that way. Anything that matches
// neither set is a write, which is the direction this has to be wrong in.
//
// The one place the conventions cannot help is a read with a name of its own, so the table
// stays for those. Its bar is that the path has no write form at all: /calendar/v3/freeBusy
// cannot create anything, while /calendar/v3/calendars/primary/events on the same host books a
// meeting — which is why a whole path is matched and never a bare prefix. Where the caller owns
// one segment, as HubSpot does for the object type, both ends are pinned and the segment between
// them may not itself contain a "/". Linear's /graphql is deliberately absent from all of this:
// a mutation there is one word away from a query, and the method is all the proxy would have to
// go on.
type readShape struct{ path, prefix, suffix string }

var readShapedPOSTs = map[string][]readShape{
	"www.googleapis.com": {{path: "/calendar/v3/freeBusy"}},
	"api.hubapi.com":     {{prefix: "/crm/v3/objects/", suffix: "/batch/read"}},
}

// Method names that read, in the "collection:verb" spelling.
var readColonVerbs = map[string]bool{
	"list": true, "get": true, "batchget": true, "batchread": true, "read": true,
	"search": true, "lookup": true, "aggregate": true, "count": true, "query": true,
}

// Whole path segments that read wherever they appear last. "search"/"query" earn a nicer thread
// for the common read APIs spelled that way (HubSpot search, Notion database query); the risk they
// carry — a host where POST /query mutates, like InfluxDB 1.x — is bounded to a host an admin
// deliberately connected, and the direction is toward Confirm elsewhere. The underscore forms are
// Elasticsearch/OpenSearch reads.
var readSegments = map[string]bool{
	"search": true, "_search": true, "_msearch": true, "_count": true, "_mget": true,
	"query": true,
}

func readShapedPOST(u *url.URL) bool {
	if u == nil {
		return false
	}
	for _, sh := range readShapedPOSTs[strings.ToLower(u.Hostname())] {
		if sh.path != "" {
			if u.Path == sh.path {
				return true
			}
			continue
		}
		mid, ok := strings.CutPrefix(u.Path, sh.prefix)
		if !ok {
			continue
		}
		if mid, ok = strings.CutSuffix(mid, sh.suffix); ok && mid != "" && !strings.Contains(mid, "/") {
			return true
		}
	}
	last := strings.ToLower(u.Path[strings.LastIndex(u.Path, "/")+1:])
	if verb, ok := lastCut(last, ":"); ok {
		// The collection:verb spelling is a Google Cloud API convention, so it is honoured only on
		// Google hosts. On any other API a mutating POST whose path happens to end ":something"
		// would otherwise slip the gate on a coincidence of spelling.
		return strings.HasSuffix(strings.ToLower(u.Hostname()), ".googleapis.com") && readColonVerbs[verb]
	}
	return readSegments[last]
}

// lastCut splits on the final occurrence of sep and reports whether there was one.
func lastCut(s, sep string) (string, bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", false
	}
	return s[i+len(sep):], true
}

// needsConfirm reports whether a request has to wait for a human. Reading through a connection
// never does — a channel that may reach a service may look at it. "auto" lets writes through
// as well; "all" is the admin override that holds every call, reads included.
//
// A read that travels as a POST is still a read. Holding one turns "is Priya free at six?" into a
// Confirm card, and somebody who presses Confirm in order to look at something soon presses it
// without reading — the one habit the gate exists to prevent. "all" still holds them, since that
// setting is about every call rather than about writes.
func needsConfirm(conn *Connection, method string, u *url.URL) bool {
	if conn == nil {
		// A credential-less domain spends nothing of ours, but a write still changes something on
		// the far side, and the guarantee the gate makes is about writes, not about credentials.
		// A POST that reads is exempt just as it is for a connection; every other write is held.
		return changesData(method, u)
	}
	switch conn.Writes {
	case "auto":
		return false
	case "all":
		return true
	}
	return changesData(method, u)
}

// changesData is whether a request is a write, whatever any connection's policy says about
// letting one through. Only a POST may be read-shaped: a PATCH or a DELETE is the verb of a
// change whatever the path says, so no amount of "…/search" in it earns the exemption.
func changesData(method string, u *url.URL) bool {
	return isWrite(method) && !(strings.ToUpper(method) == "POST" && readShapedPOST(u))
}

// The three headers and two field names by which a caller asks a server to treat one verb as
// another. They exist for clients that cannot send a DELETE; this proxy has a method field, so
// nothing here needs them — and everything here is undone by them. needsConfirm reads the
// method, so `GET` plus `X-HTTP-Method-Override: DELETE` is a write that never meets a Confirm
// card, on an upstream that honours the convention. The model chooses the headers
// (tools_http.go offers them), so a document or a web page that steers a turn chooses them too.
//
// Refused rather than quietly stripped. A model that wrote one meant something by it, and
// turning its DELETE into a GET without saying so is a worse answer than a sentence telling it
// which field to use. Refused rather than honoured, too: honouring them would mean modelling
// which upstream obeys which spelling, which is not knowable from here.
var (
	methodOverrideHeaders = []string{"X-HTTP-Method-Override", "X-Method-Override", "X-HTTP-Method"}
	methodOverrideFields  = []string{"_method", "_httpmethod"}
	// A form body (`_method=delete`) or a JSON one (`"_method": "delete"`), in one expression.
	methodOverrideBody = regexp.MustCompile(`(?i)"?_(?:http)?method"?\s*[:=]\s*"?([a-z]+)`)
)

// writeVerb normalises a claimed verb and returns it only if it is one that changes something.
// Empty is not a verb: isWrite("") is true, which is right for a missing method (it defaults to
// GET elsewhere) and wrong here, where a missing override is simply no override.
func writeVerb(v string) string {
	v = strings.ToUpper(strings.TrimSpace(v))
	if v == "" || !isWrite(v) {
		return ""
	}
	return v
}

// methodOverride finds a write verb the request is carrying somewhere other than its method,
// and names where it found it so the refusal can say.
func methodOverride(req ProxyRequest, u *url.URL) (verb, where string) {
	for k, v := range req.Headers {
		for _, name := range methodOverrideHeaders {
			if strings.EqualFold(k, name) {
				if verb := writeVerb(v); verb != "" {
					return verb, "the " + name + " header"
				}
			}
		}
	}
	if u != nil {
		q := u.Query()
		for _, f := range methodOverrideFields {
			if verb := writeVerb(q.Get(f)); verb != "" {
				return verb, f + " in the query string"
			}
		}
	}
	if m := methodOverrideBody.FindStringSubmatch(req.Body); m != nil {
		if verb := writeVerb(m[1]); verb != "" {
			return verb, "_method in the body"
		}
	}
	return "", ""
}

// ErrNeedsApproval is the stronger sibling of ErrNeedsConfirmation: this write is not the
// requester's to confirm, and no allow rule may pre-approve it.
var ErrNeedsApproval = errors.New("needs approval")

// ErrNeedsUserAuth says the connection spends a credential the asker has not granted yet. The
// third of the three ways a call stops short of the wire, and the only one that no amount of
// waiting fixes: the person has to sign in themselves. Nothing about it is a failure of the
// connection, so it is not reported as one.
var ErrNeedsUserAuth = errors.New("needs the requester to connect their account")

// needsApproval reports whether a request has to go to a named approver instead of to whoever
// happens to be in the thread. A connection an admin marked allow_grants is one that hands out
// access, so a write through it is somebody else's decision by definition.
//
// This is checked before needsConfirm, and it is the reason the gate lives here rather than in the
// tool that asks for approval: nothing stops the model from composing the same POST through plain
// http_request, and that path would otherwise post a five-minute card in the requester's own
// thread for the requester to press.
//
// readShapedPOSTs is not consulted here on purpose. It buys a nicer thread on a calendar lookup,
// which is worth nothing on a connection that hands out access, and the cost of getting the table
// wrong is an unapproved grant rather than an unapproved read.
func needsApproval(conn *Connection, method string) bool {
	return conn != nil && conn.AllowGrants && isWrite(method)
}

// hasDotSegment reports a path carrying a "." or ".." segment, in either its plain or its
// percent-encoded spelling. Nothing here resolves them, because resolving is the bug: the path
// that is matched has to be the path that goes on the wire, and the server at the other end is
// the one that collapses "..".
func hasDotSegment(u *url.URL) bool {
	for _, raw := range []string{u.Path, u.EscapedPath()} {
		for _, seg := range strings.Split(raw, "/") {
			switch strings.ReplaceAll(strings.ToLower(seg), "%2e", ".") {
			case ".", "..":
				return true
			}
		}
	}
	return false
}

// Match finds the rule (or credential-less domain) covering a URL. Returns the connection
// (nil for a domain entry) and a human reason when blocked.
func (p *Proxy) Match(acc *Access, method string, u *url.URL) (*Connection, string) {
	return p.MatchNamed(acc, method, u, "")
}

// connByName picks the connection a caller asked for out of the ones that already cover a URL.
//
// The match is forgiving about case, spacing and punctuation, because a model told the channel
// holds "ClickUp (testing)" writes clickup_testing about as often as it writes the name in
// full, and making it spell the name exactly costs a whole round to learn one. An unambiguous
// fragment is therefore enough — but a fragment matching two connections is not a choice that
// may be made on the caller's behalf, so it matches nothing and the caller is told the names.
func connByName(covering []*Connection, want string) *Connection {
	norm := func(s string) string {
		return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}), " ")
	}
	w := norm(want)
	if w == "" {
		return nil
	}
	for _, c := range covering {
		if norm(c.Name) == w {
			return c
		}
	}
	var hit *Connection
	for _, c := range covering {
		if strings.Contains(norm(c.Name), w) {
			if hit != nil {
				return nil // ambiguous
			}
			hit = c
		}
	}
	return hit
}

// MatchNamed is Match with a caller's choice between connections that all cover the URL: a
// channel with two ClickUp workspaces, or a production and a testing GCP key, has no way to
// say which one it means otherwise, and the highest-ranked one silently wins every time. An
// empty want is the ordinary case and behaves exactly as it always did.
//
// It only ever chooses among credentials this channel already holds. A name cannot reach a
// connection the channel was not granted, and cannot widen what the chosen one permits: the
// host, prefix and method checks below have already run by the time a name is consulted.
func (p *Proxy) MatchNamed(acc *Access, method string, u *url.URL, want string) (*Connection, string) {
	host := u.Hostname()
	// A path is matched by prefix and then sent exactly as written, so a "." or ".." in it means
	// the prefix that was checked and the path the service actually serves are two different
	// strings: /calendar/v3/../gmail/v1/… passes a Calendar connection's /calendar/v3/ prefix
	// and arrives at Google as Gmail. Path prefixes are the whole of what separates Calendar,
	// Drive and Gmail on www.googleapis.com, so this is refused rather than normalised — an API
	// URL never legitimately carries one, and rewriting it would only move the disagreement.
	if hasDotSegment(u) {
		return nil, "blocked by the proxy: a URL path may not contain \".\" or \"..\" segments. Ask for the path you mean."
	}
	// A host on its own does not always pick the connection out. Google Drive and Google Calendar
	// are both served from www.googleapis.com, and it is the path prefix that tells them apart. So
	// a rule whose host matches but whose prefixes or methods do not is not an answer: keep
	// looking, and report the near miss only if nothing else covers the URL. Returning on the
	// first host match sent Calendar calls out under Drive's service account, and let a read-only
	// Drive connection block a calendar booking the Google connection was allowed to make.
	// Nothing is widened by this: each connection still permits exactly what it permitted.
	outside := ""
	// Every connection in this channel that covers the URL, in the order Match always preferred
	// them. Collected rather than returned on sight so a name can choose between them; with no
	// name the first is taken, which is the connection that would have been returned before.
	var covering []*Connection
	for _, r := range acc.Rules {
		for _, h := range r.Conn.AllowedHosts {
			if !hostMatch(h, host) {
				continue
			}
			if len(r.Conn.PathPrefixes) > 0 {
				ok := false
				for _, pfx := range r.Conn.PathPrefixes {
					if strings.HasPrefix(u.Path, pfx) {
						ok = true
					}
				}
				if !ok {
					if outside == "" {
						outside = fmt.Sprintf("path %s is outside the allowed prefixes for %s", u.Path, r.Conn.Name)
					}
					continue
				}
			}
			if len(r.Conn.Methods) > 0 {
				ok := false
				for _, m := range r.Conn.Methods {
					if strings.EqualFold(m, method) {
						ok = true
					}
				}
				if !ok {
					if outside == "" {
						outside = fmt.Sprintf("method %s is not allowed for %s", method, r.Conn.Name)
					}
					continue
				}
			}
			covering = append(covering, r.Conn)
			break // this connection covers the URL; its other hosts cannot say otherwise
		}
	}
	if len(covering) > 0 {
		if want == "" {
			return covering[0], ""
		}
		if conn := connByName(covering, want); conn != nil {
			return conn, ""
		}
		names := make([]string, len(covering))
		for i, c := range covering {
			names[i] = strconv.Quote(c.Name)
		}
		return nil, fmt.Sprintf("blocked by the proxy: this channel has no connection called %q for %s. The ones that cover it are %s. "+
			"Use one of those names exactly, or leave connection out to use %q.", want, host, strings.Join(names, ", "), covering[0].Name)
	}
	for _, d := range acc.Domains {
		if hostMatch(d.Host, host) {
			return nil, ""
		}
	}
	if outside != "" {
		return nil, outside
	}
	return nil, fmt.Sprintf("blocked by the proxy: host %s is not allowed in this channel. An admin can add it as a connection or a domain in the console.", host)
}

// callAudit is the identity half of an audit row: which workspace, channel, thread and person a
// tool call belongs to. Every path that runs a call on somebody's behalf builds it from the Call
// the same way, because filling it by hand is how the http_request and MCP paths ended up
// recording rows with no team_id — untraceable once an account has more than one workspace.
func callAudit(c *Call) ProxyAudit {
	return ProxyAudit{TeamID: c.TeamID, Channel: c.Channel, ThreadTS: c.ThreadTS, Requester: c.UserID}
}

// wireSafeQuery percent-encodes the bytes of a query string that cannot travel in an HTTP
// request line: spaces, control characters and anything non-ASCII.
//
// A model writing q=fullText contains 'SOC 2' by hand has written the query every Drive doc
// shows, but Go hands RawQuery to the transport untouched, the space lands in the request
// line, and the server answers 400 before it has read a word of the query. The model recovers
// by sending the same URL encoded, so nothing is lost but a round — and rounds are the budget
// a turn actually runs out of. One Drive search spent four of its twelve re-typing its own URL
// and gave up reporting the document was not there.
//
// Only bytes that are illegal on the wire are touched. A query that arrived encoded, or that
// leans on characters an API treats as literal, comes back byte for byte as it went in, so
// this cannot re-order parameters or change what a service is asked for the way a full
// re-encode through url.Values would.
func wireSafeQuery(raw string) string {
	illegal := func(c byte) bool { return c <= 0x20 || c >= 0x7f }
	clean := true
	for i := 0; i < len(raw) && clean; i++ {
		clean = !illegal(raw[i])
	}
	if clean {
		return raw
	}
	var b strings.Builder
	b.Grow(len(raw) + 16)
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; illegal(c) {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// safeTransportErr rebuilds a transport failure without the request URL. Go wraps these in
// *url.Error{Op, URL, Err}, and for a query-parameter credential the URL carries the secret — a
// redirect refusal, a timeout or a dial error would otherwise hand that URL back to the model, the
// thread and the activity log. The operation and the underlying cause are kept; the URL is not.
func safeTransportErr(method, host string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("request to %s %s failed: %v", method, host, ue.Err)
	}
	return err
}

// Do performs the request with the credential injected. Requests that need a human (see
// needsConfirm) return ErrNeedsConfirmation without sending anything.
func (p *Proxy) Do(ctx context.Context, orgID int64, acc *Access, req ProxyRequest, audit ProxyAudit, confirmed bool) (*ProxyResponse, error) {
	u, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil {
		return nil, err
	}
	u.RawQuery = wireSafeQuery(u.RawQuery)
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = "GET"
	}
	audit.Method, audit.Host, audit.Path = method, u.Hostname(), u.Path
	// Before the address is even resolved: this is a refusal about what the request says of
	// itself, and it costs nothing to make it here.
	if verb, where := methodOverride(req, u); verb != "" {
		why := fmt.Sprintf("blocked by the proxy: %s asks the server to treat this as %s, but it was sent as %s. "+
			"The confirmation step reads the method, so an override would put a write past it. Say the verb in the "+
			"method field instead.", where, verb, method)
		audit.Blocked = why
		p.store.LogProxy(ctx, orgID, audit)
		return nil, errors.New(why)
	}
	if err := checkURL(u); err != nil {
		return nil, err
	}
	conn, why := p.MatchNamed(acc, method, u, req.Connection)
	if why != "" {
		audit.Blocked = why
		p.store.LogProxy(ctx, orgID, audit)
		return nil, errors.New(why)
	}
	// A matched connection means a credential is about to be injected. It must not travel in
	// cleartext: an http upstream that redirects to https has already had the request — headers
	// and all — on the wire. The model endpoint refuses non-https for the same reason.
	if conn != nil && u.Scheme != "https" {
		why := fmt.Sprintf("blocked by the proxy: %s carries a credential, so it may only be sent over https, not %s.", conn.Name, u.Scheme)
		audit.Blocked = why
		p.store.LogProxy(ctx, orgID, audit)
		return nil, errors.New(why)
	}
	if !confirmed {
		if needsApproval(conn, method) {
			return nil, ErrNeedsApproval
		}
		if needsConfirm(conn, method, u) || req.HoldWrites && changesData(method, u) {
			return nil, ErrNeedsConfirmation
		}
	}
	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	hr, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	hr.Header.Set("User-Agent", "attesttag/0.2 (+proxy)")
	hr.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.5")
	if req.Body != "" && req.Headers["Content-Type"] == "" {
		hr.Header.Set("Content-Type", "application/json")
	}
	for k, v := range req.Headers {
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "Cookie") || strings.EqualFold(k, "Host") {
			continue // the model never sets credentials
		}
		hr.Header.Set(k, v)
	}
	if conn != nil {
		if err := p.inject(ctx, orgID, conn, hr, audit); err != nil {
			// Nobody has signed in yet: that is not a credential fault, it is a person who has
			// not been asked. Let it up untouched so the caller can send them a link.
			if errors.Is(err, ErrNeedsUserAuth) {
				audit.Blocked = "not connected: " + audit.Requester
				p.store.LogProxy(ctx, orgID, audit)
				return nil, err
			}
			audit.Blocked = "credential error: " + err.Error()
			p.store.LogProxy(ctx, orgID, audit)
			return nil, fmt.Errorf("credential error for %s: %w", conn.Name, err)
		}
		audit.ConnectionID = conn.ID
	}
	start := time.Now()
	resp, err := p.client.Do(hr)
	if err != nil {
		err = safeTransportErr(method, u.Hostname(), err)
		audit.Blocked = "request failed: " + err.Error()
		p.store.LogProxy(ctx, orgID, audit)
		return nil, err
	}
	defer resp.Body.Close()
	limit := req.MaxBytes
	if limit <= 0 {
		limit = modelMaxBody
	}
	if limit > proxyMaxRead {
		limit = proxyMaxRead
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	truncated := len(raw) > limit
	if truncated {
		raw = raw[:limit]
	}
	audit.Status, audit.MS = resp.StatusCode, time.Since(start).Milliseconds()
	p.store.LogProxy(ctx, orgID, audit)
	if conn != nil {
		p.store.TouchConnection(ctx, orgID, conn.ID)
	}
	text := string(raw)
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "html") {
		text = cleanHTML(text)
	}
	if conn != nil {
		text = p.scrubSecrets(conn, text)
	}
	if truncated {
		text += fmt.Sprintf("\n\n[truncated: the response is larger than %d bytes; narrow it with the API's own paging or filters]", limit)
	}
	return &ProxyResponse{Status: resp.StatusCode, Body: redact(text), Conn: conn, Truncated: truncated,
		RetryAfter: retryAfterHint(resp.Header, time.Now())}, nil
}

// scrubSecrets removes the connection's own credential values from a response body, in case
// the service echoes them back (some debug endpoints do).
func (p *Proxy) scrubSecrets(conn *Connection, text string) string {
	s, err := p.secret(conn)
	if err != nil {
		return text
	}
	vals := []string{s.Token, s.Password, s.ClientSecret, s.AWSSecret, s.AWSSessionToken}
	for _, v := range s.Headers {
		vals = append(vals, v)
	}
	for _, v := range vals {
		if len(v) >= 6 {
			text = strings.ReplaceAll(text, v, "[redacted-secret]")
		}
	}
	return text
}

func (p *Proxy) secret(conn *Connection) (*Secret, error) {
	if len(conn.secretEnc) == 0 {
		return nil, errors.New("no secret stored")
	}
	plain, err := p.sealer.Open(conn.secretEnc)
	if err != nil {
		return nil, err
	}
	var s Secret
	return &s, json.Unmarshal(plain, &s)
}

func (p *Proxy) inject(ctx context.Context, orgID int64, conn *Connection, hr *http.Request, audit ProxyAudit) error {
	s, err := p.secret(conn)
	if err != nil {
		return err
	}
	if pr := presetByID(conn.Preset); pr != nil {
		for k, v := range pr.StaticHeaders {
			if hr.Header.Get(k) == "" {
				hr.Header.Set(k, v)
			}
		}
	}
	for name, val := range s.Headers { // extra secret headers (e.g. DD-APPLICATION-KEY)
		hr.Header.Set(name, val)
	}
	switch conn.CredType {
	case "bearer":
		hr.Header.Set("Authorization", "Bearer "+s.Token)
	case "basic":
		hr.SetBasicAuth(s.User, s.Password)
	case "header":
		name := s.HeaderName
		if name == "" {
			name = "Authorization"
		}
		prefix := ""
		if pr := presetByID(conn.Preset); pr != nil {
			prefix = pr.HeaderPrefix
		}
		for _, h := range conn.Headers {
			if h.Name == name {
				prefix = h.Prefix
			}
		}
		hr.Header.Set(name, prefix+s.Token)
	case "query":
		name := s.HeaderName
		if name == "" {
			return errors.New("query parameter name missing")
		}
		q := hr.URL.Query()
		q.Set(name, s.Token)
		hr.URL.RawQuery = q.Encode()
	case "gcp_sa":
		tok, err := p.gcpToken(ctx, conn.ID, s)
		if err != nil {
			return err
		}
		hr.Header.Set("Authorization", "Bearer "+tok)
	case "oauth2_cc":
		tok, err := p.clientCredentialsToken(ctx, conn.ID, s)
		if err != nil {
			return err
		}
		hr.Header.Set("Authorization", "Bearer "+tok)
	case "aws_sigv4":
		// The signature covers the body, so read back what the request was built
		// with rather than the string the caller happened to have.
		var payload []byte
		if hr.GetBody != nil {
			rc, err := hr.GetBody()
			if err != nil {
				return err
			}
			payload, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return err
			}
		}
		return signAWSv4(hr, payload, s, time.Now())
	case "github_app":
		// No stored token: one is minted from the app key, scoped to this repository, and
		// cached against the installation rather than this connection.
		tok, err := p.installationToken(ctx, orgID, conn, s)
		if err != nil {
			return err
		}
		hr.Header.Set("Authorization", "Bearer "+tok)
	case "oauth_user":
		// The asker's own token, never the channel's. The connection holds the organisation's
		// OAuth client and nothing that can call anything; the credential belongs to the person
		// in audit.Requester, which every path that runs a call on somebody's behalf fills in
		// through callAudit. An empty requester therefore means nobody, not everybody.
		tok, err := p.userToken(ctx, orgID, conn, audit.TeamID, audit.Requester)
		if err != nil {
			return err
		}
		hr.Header.Set("Authorization", "Bearer "+tok)
	case "mcp":
		if s.Token != "" {
			hr.Header.Set("Authorization", "Bearer "+s.Token)
		}
	default:
		return fmt.Errorf("unsupported credential type %q", conn.CredType)
	}
	return nil
}

// ---- token exchanges ----

func (p *Proxy) cached(id tokenCacheKey) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id.id == 0 {
		return "", false
	}
	t, ok := p.tokens[id]
	if ok && time.Until(t.expiry) > 2*time.Minute {
		return t.token, true
	}
	return "", false
}

func (p *Proxy) remember(id tokenCacheKey, tok string, ttl time.Duration) {
	if id.id == 0 {
		return
	}
	p.mu.Lock()
	p.tokens[id] = cachedToken{token: tok, expiry: time.Now().Add(ttl)}
	p.mu.Unlock()
}

// forgetToken drops a connection's exchanged access token. Rotating or deleting a credential has
// to reach this: the token minted from the old credential keeps working for as long as the
// provider says, so without it a rotation looked instant in the console while the bot carried on
// using the revoked one for up to an hour.
func (p *Proxy) forgetToken(id int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	for key := range p.tokens {
		if key.id == id {
			delete(p.tokens, key)
		}
	}
	p.mu.Unlock()
}

// gcpToken signs a JWT with the service-account key and exchanges it for an access token.
func (p *Proxy) gcpToken(ctx context.Context, id int64, s *Secret) (string, error) {
	keyID := exchangeKey(id, "gcp_sa", s)
	if t, ok := p.cached(keyID); ok {
		return t, nil
	}
	var sa struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal([]byte(s.SAJSON), &sa); err != nil {
		return "", fmt.Errorf("service-account json: %w", err)
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return "", errors.New("private_key is not PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		key, _ = k.(*rsa.PrivateKey)
	} else if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	}
	if key == nil {
		return "", errors.New("private_key is not an RSA key")
	}
	now := time.Now()
	b64 := func(v any) string { j, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(j) }
	header := b64(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims := b64(map[string]any{"iss": sa.ClientEmail, "scope": s.Scopes, "aud": sa.TokenURI, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	sum := sha256.Sum256([]byte(header + "." + claims))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	jwt := header + "." + claims + "." + base64.RawURLEncoding.EncodeToString(sig)
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {jwt}}
	req, err := http.NewRequestWithContext(ctx, "POST", sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error_description"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	json.Unmarshal(raw, &tr)
	if tr.AccessToken == "" {
		return "", fmt.Errorf("token exchange failed: %s", truncate(tr.Error+" "+string(raw), 200))
	}
	p.remember(keyID, tr.AccessToken, time.Duration(tr.ExpiresIn)*time.Second)
	return tr.AccessToken, nil
}

func (p *Proxy) clientCredentialsToken(ctx context.Context, id int64, s *Secret) (string, error) {
	keyID := exchangeKey(id, "oauth2_cc", s)
	if t, ok := p.cached(keyID); ok {
		return t, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if s.Scopes != "" {
		form.Set("scope", s.Scopes)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(s.ClientID, s.ClientSecret)
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	json.Unmarshal(raw, &tr)
	if tr.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl == 0 {
		ttl = 30 * time.Minute
	}
	p.remember(keyID, tr.AccessToken, ttl)
	return tr.AccessToken, nil
}

// ---- test call (console "Test connection") ----

// GCPProject is the project id inside a service account key, so the model can name the project a
// connection covers instead of asking which one to use.
func (p *Proxy) GCPProject(conn *Connection) string {
	if conn == nil || conn.CredType != "gcp_sa" {
		return ""
	}
	s, err := p.secret(conn)
	if err != nil {
		return ""
	}
	var sa struct {
		ProjectID string `json:"project_id"`
	}
	json.Unmarshal([]byte(s.SAJSON), &sa)
	return sa.ProjectID
}

func (p *Proxy) Test(ctx context.Context, orgID int64, conn *Connection) (int, string, error) {
	pr := presetByID(conn.Preset)
	if pr == nil {
		pr = presetByID("custom")
	}
	if len(conn.AllowedHosts) == 0 {
		return 0, "", errors.New("no allowed host to test against")
	}
	host := conn.AllowedHosts[0]
	if strings.HasPrefix(host, "*.") {
		return 0, "", errors.New("first allowed host is a wildcard; add the concrete host first for testing")
	}
	path, body := testPath(pr, conn)
	if proj := p.GCPProject(conn); proj != "" {
		path = strings.ReplaceAll(path, "{project_id}", proj)
		body = strings.ReplaceAll(body, "{project_id}", proj)
	}
	acc := &Access{Rules: []Rule{{Conn: conn, Rank: 9}}}
	resp, err := p.Do(ctx, orgID, acc, ProxyRequest{Method: pr.Test.Method, URL: "https://" + host + path, Body: body},
		ProxyAudit{Requester: "console-test"}, true)
	if err != nil {
		return 0, "", err
	}
	return resp.Status, truncate(resp.Body, 600), nil
}

// testPath is the path the console's "Test connection" button calls, and the body it sends with it.
//
// A preset's check call is chosen for the whole service, and a connection narrowed to its own
// paths is not the whole service: calling the preset's path there is either refused against this
// connection's own prefixes or answered by a part of the service it was never pointed at, and
// neither outcome says anything about whether the credential works. So unless the preset's path
// already lives under the first prefix, the first prefix is what gets called — it is a path this
// connection exists to reach, and on a custom API it is the only path anyone here has named.
//
// Only a GET is moved. A handful of presets check themselves with a POST carrying a body written
// for one endpoint — Linear's and New Relic's GraphQL queries, Logging's entries:list — and that
// body means nothing anywhere else, while the verb still means "change something". Pointing a
// write at a path chosen for being *allowed* rather than for being a check call is not a test
// worth making, so those keep their own endpoint and are refused by the prefixes if the admin has
// narrowed them away from it, which is the honest answer and the one they got before.
func testPath(pr *Preset, conn *Connection) (string, string) {
	path, body := pr.Test.Path, pr.Test.Body
	get := pr.Test.Method == "" || strings.EqualFold(pr.Test.Method, "GET")
	if get && len(conn.PathPrefixes) > 0 && !strings.HasPrefix(path, conn.PathPrefixes[0]) {
		path = conn.PathPrefixes[0]
	}
	// A prefix typed without its leading slash never matches a path the proxy sees, so it is
	// already doing no work as an allow rule. It still says which path was meant, and the slash
	// is what makes this a URL rather than something glued to the hostname.
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path, body
}

// bufferString is a tiny helper for tests.
func bufferString(b []byte) string { return bytes.NewBuffer(b).String() }
