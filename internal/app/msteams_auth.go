package app

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Both halves of talking to the Bot Framework as a Teams bot: proving that a delivery came from
// Microsoft, and proving to Microsoft that a reply comes from us. Microsoft ships SDKs for this in
// C#, JavaScript and Python and none in Go, so the protocol is spoken here directly — it is two
// small, well documented exchanges, and an unofficial library in the path of every message is a
// worse thing to depend on than the code below.
//
// Reference: https://learn.microsoft.com/azure/bot-service/rest-api/bot-framework-rest-connector-authentication

const (
	msteamsSingleTenant = "SingleTenant"
	msteamsMultiTenant  = "MultiTenant"

	// The scope a bot's token is for, whichever tenant issues it.
	botFrameworkScope = "https://api.botframework.com/.default"
	// Who signs the tokens the Bot Framework sends a bot. The same for every bot type.
	botFrameworkIssuer = "https://api.botframework.com"
	// The channel a delivery from Teams names, and the one a signing key has to be endorsed for.
	msteamsChannelID = "msteams"
)

// These are variables so that a test can stand in for Microsoft; nothing else assigns them.
var (
	botFrameworkOpenID = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	msLoginBase        = "https://login.microsoftonline.com"
	// msServiceURLOK is the last word on where a reply may be sent. The service URL comes from a
	// delivery whose token has to vouch for it, so this is not the check that makes that safe; it
	// is the one that keeps a mistake in that check from turning the bot into a way to make
	// signed requests to anywhere.
	msServiceURLOK = msteamsServiceHost
)

// msteamsServiceHost accepts the hosts the Bot Framework serves Teams conversations from.
func msteamsServiceHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	// Only the Bot Framework's own service hosts. The earlier `*.trafficmanager.net` was far too
	// wide — any Azure customer can create a Traffic Manager profile under that domain — so a
	// token that slipped the serviceurl check could have pointed the bot's bearer token at an
	// attacker's endpoint. Bot Framework serves Teams from smba.trafficmanager.net (the region is
	// in the path, not the host) and its regional variants.
	return h == "smba.trafficmanager.net" || strings.HasSuffix(h, ".smba.trafficmanager.net") ||
		strings.HasSuffix(h, ".botframework.com") || strings.HasSuffix(h, ".botframework.azure.us")
}

// msteamsClient is everything a Teams tenant's transport shares with every other: the deployment's
// app credentials, the token cache, the key cache, and the HTTP client. One per process.
type msteamsClient struct {
	appID, secret, homeTenant, appType string
	// siteURL is the deployment's public site (SITE_URL), used for the Teams package's developer,
	// privacy and terms links; empty on a self-host, which then uses its own console origin
	// instead of naming attesttag.com in every deployment's package.
	siteURL  string
	http     *http.Client
	store    *Store
	verifier *bfVerifier
	tokens   *msTokens
}

func newMSTeamsClient(cfg Config, store *Store) *msteamsClient {
	if !cfg.msteamsConfigured() {
		return nil
	}
	hc := &http.Client{Timeout: 20 * time.Second, Transport: publicTransport()}
	authority := cfg.MSTeamsTenantID
	if cfg.MSTeamsAppType == msteamsMultiTenant {
		// A multi-tenant bot's tokens come from the Bot Framework's own directory. None can be
		// created any more, but one registered before July 2025 still works this way.
		authority = "botframework.com"
	}
	return &msteamsClient{
		appID: cfg.MSTeamsAppID, secret: cfg.MSTeamsAppPassword, homeTenant: authority, appType: cfg.MSTeamsAppType,
		siteURL: strings.TrimRight(cfg.SiteURL, "/"),
		http:    hc, store: store,
		verifier: &bfVerifier{appID: cfg.MSTeamsAppID, http: hc},
		tokens:   &msTokens{appID: cfg.MSTeamsAppID, secret: cfg.MSTeamsAppPassword, http: hc},
	}
}

// botToken is the token a reply to the Bot Framework carries.
func (c *msteamsClient) botToken(ctx context.Context) (string, error) {
	return c.tokens.get(ctx, c.homeTenant, botFrameworkScope)
}

// botID is how Teams addresses the bot itself in a conversation.
func (c *msteamsClient) botID() string { return "28:" + c.appID }

// ---- inbound: is this delivery from Microsoft? ----

// bfVerifier checks the JWT on a Bot Framework delivery against Microsoft's published keys.
type bfVerifier struct {
	appID string
	http  *http.Client

	mu       sync.Mutex
	issuer   string
	jwksURI  string
	keys     map[string]bfKey
	fetched  time.Time // when keys were last loaded
	lastMiss time.Time // when a token last named a key we did not have
}

type bfKey struct {
	pub          *rsa.PublicKey
	endorsements []string
}

// Microsoft asks for the keys to be refreshed at least daily. A token naming a key we have never
// seen reloads them sooner, but not more than once every few minutes: otherwise anybody could make
// the bot fetch Microsoft's keys once per request they sent.
const (
	bfKeysMaxAge    = 24 * time.Hour
	bfKeysMissDelay = 5 * time.Minute
	bfClockSkew     = 5 * time.Minute
)

var errBFToken = errors.New("the delivery's token did not verify")

// verify checks a delivery's Authorization header and returns the token's claims. channelID and
// serviceURL are what the activity itself says; the token has to agree with both.
func (v *bfVerifier) verify(ctx context.Context, authz, channelID, serviceURL string) (map[string]any, error) {
	raw, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok || raw == "" {
		return nil, fmt.Errorf("%w: no bearer token", errBFToken)
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not a JWT", errBFToken)
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeJWTPart(parts[0], &head); err != nil {
		return nil, fmt.Errorf("%w: header: %v", errBFToken, err)
	}
	// Only RS256. Accepting whatever alg the header asks for is the classic way to let a token
	// sign itself.
	if head.Alg != "RS256" || head.Kid == "" {
		return nil, fmt.Errorf("%w: alg %q, kid %q", errBFToken, head.Alg, head.Kid)
	}
	key, issuer, err := v.key(ctx, head.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature encoding", errBFToken)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key.pub, crypto.SHA256, sum[:], sig); err != nil {
		return nil, fmt.Errorf("%w: bad signature", errBFToken)
	}
	var claims map[string]any
	if err := decodeJWTPart(parts[1], &claims); err != nil {
		return nil, fmt.Errorf("%w: claims: %v", errBFToken, err)
	}
	if iss, _ := claims["iss"].(string); iss != issuer {
		return nil, fmt.Errorf("%w: issuer %q", errBFToken, iss)
	}
	if !audienceIs(claims["aud"], v.appID) {
		return nil, fmt.Errorf("%w: audience %v is not this bot", errBFToken, claims["aud"])
	}
	now := time.Now()
	exp, ok := claims["exp"].(float64)
	if !ok || now.After(time.Unix(int64(exp), 0).Add(bfClockSkew)) {
		return nil, fmt.Errorf("%w: expired", errBFToken)
	}
	if nbf, ok := claims["nbf"].(float64); ok && now.Add(bfClockSkew).Before(time.Unix(int64(nbf), 0)) {
		return nil, fmt.Errorf("%w: not valid yet", errBFToken)
	}
	// The token names the service URL it was issued for, and a reply goes to the one in the
	// activity: without this check a genuine token could carry an activity that points replies at
	// a host of the sender's choosing. The claim is required — a token that omits it is refused
	// rather than waved through, so the reply target is always pinned to something Microsoft signed.
	if su, _ := claims["serviceurl"].(string); su == "" || !sameServiceURL(su, serviceURL) {
		return nil, fmt.Errorf("%w: service url %q does not match the activity's", errBFToken, su)
	}
	if !slices.Contains(key.endorsements, channelID) {
		return nil, fmt.Errorf("%w: key is not endorsed for channel %q", errBFToken, channelID)
	}
	return claims, nil
}

func sameServiceURL(a, b string) bool {
	return strings.TrimRight(strings.ToLower(a), "/") == strings.TrimRight(strings.ToLower(b), "/")
}

func audienceIs(aud any, appID string) bool {
	switch a := aud.(type) {
	case string:
		return a == appID
	case []any:
		for _, x := range a {
			if s, _ := x.(string); s == appID {
				return true
			}
		}
	}
	return false
}

func decodeJWTPart(part string, into any) error {
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// key finds a signing key by id, loading Microsoft's keys when they are stale or the id is new.
func (v *bfVerifier) key(ctx context.Context, kid string) (bfKey, string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	k, known := v.keys[kid]
	stale := time.Since(v.fetched) > bfKeysMaxAge
	if known && !stale {
		return k, v.issuer, nil
	}
	// Only a real miss — keys that are fresh and still do not name this one — counts against the
	// reload limit. Counting the first load of all as a miss would stop a key Microsoft rotated in
	// the process's first minutes from being picked up until the limit ran out.
	if !known && !stale {
		if time.Since(v.lastMiss) < bfKeysMissDelay {
			return bfKey{}, "", fmt.Errorf("%w: unknown signing key %q", errBFToken, kid)
		}
		v.lastMiss = time.Now()
	}
	if err := v.loadLocked(ctx); err != nil {
		if known { // Microsoft unreachable: a key we already trusted is still the key it was
			return k, v.issuer, nil
		}
		return bfKey{}, "", fmt.Errorf("could not load the Bot Framework's signing keys: %w", err)
	}
	if k, ok := v.keys[kid]; ok {
		return k, v.issuer, nil
	}
	return bfKey{}, "", fmt.Errorf("%w: unknown signing key %q", errBFToken, kid)
}

func (v *bfVerifier) loadLocked(ctx context.Context) error {
	var meta struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, botFrameworkOpenID, &meta); err != nil {
		return err
	}
	if meta.Issuer == "" || meta.JWKSURI == "" {
		return errors.New("the OpenID configuration names no issuer or no key set")
	}
	var set struct {
		Keys []struct {
			Kty          string   `json:"kty"`
			Kid          string   `json:"kid"`
			N            string   `json:"n"`
			E            string   `json:"e"`
			Endorsements []string `json:"endorsements"`
		} `json:"keys"`
	}
	if err := v.getJSON(ctx, meta.JWKSURI, &set); err != nil {
		return err
	}
	keys := map[string]bfKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		keys[k.Kid] = bfKey{pub: &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}, endorsements: k.Endorsements}
	}
	if len(keys) == 0 {
		return errors.New("the key set held no usable RSA keys")
	}
	// The issuer is the one Microsoft documents for deliveries to a bot, not merely whatever the
	// metadata says: a metadata endpoint that ever named another would otherwise quietly widen
	// who can sign a delivery.
	if meta.Issuer != botFrameworkIssuer {
		return fmt.Errorf("the OpenID configuration names issuer %q, not %q", meta.Issuer, botFrameworkIssuer)
	}
	v.issuer, v.jwksURI, v.keys, v.fetched = meta.Issuer, meta.JWKSURI, keys, time.Now()
	return nil
}

func (v *bfVerifier) getJSON(ctx context.Context, u string, into any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s answered %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
}

// ---- outbound: a token that says a reply is ours ----

// msTokens holds client-credential tokens, one per (tenant, scope), until shortly before they
// expire. The Bot Framework token is one for the whole deployment; Graph tokens are one per
// customer tenant, because Graph answers for the tenant that issued the token.
type msTokens struct {
	appID, secret string
	http          *http.Client

	mu    sync.Mutex
	cache map[string]msToken
}

type msToken struct {
	value   string
	expires time.Time
}

// msTokenEarly is how long before expiry a token is replaced, so no request goes out with one
// that dies on the way.
const msTokenEarly = 5 * time.Minute

func (t *msTokens) get(ctx context.Context, tenant, scope string) (string, error) {
	key := tenant + " " + scope
	t.mu.Lock()
	if tok, ok := t.cache[key]; ok && time.Now().Before(tok.expires.Add(-msTokenEarly)) {
		t.mu.Unlock()
		return tok.value, nil
	}
	t.mu.Unlock()
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {t.appID},
		"client_secret": {t.secret},
		"scope":         {scope},
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		msLoginBase+"/"+url.PathEscape(tenant)+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("token endpoint answered %s", resp.Status)
	}
	if body.AccessToken == "" {
		// The description names what is wrong — an expired secret, a tenant that has not consented
		// — and never carries the secret, so it is worth passing on whole.
		return "", fmt.Errorf("Microsoft refused a token for %s: %s %s", tenant, body.Error, firstLine(body.Description))
	}
	t.mu.Lock()
	if t.cache == nil {
		t.cache = map[string]msToken{}
	}
	t.cache[key] = msToken{value: body.AccessToken, expires: time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)}
	t.mu.Unlock()
	return body.AccessToken, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(s)
}
