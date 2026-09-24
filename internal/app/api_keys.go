package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Developer API keys: the credential a script uses instead of a browser session.
//
// The rule that shapes everything here is that a key is a person, not a role. It carries the
// authority of the account that minted it and never a scrap more — the same membership, the same
// console role, the same permissions, re-resolved on every request. So an integration cannot
// reach further than whoever set it up, and when they leave the organisation their scripts stop
// with them rather than outliving their access.
//
// Two things a session has that a key deliberately does not: the organisation's sign-in policy
// and its two-factor requirement. Both are properties of somebody sitting at a keyboard. A
// nightly job has no keyboard, cannot be shown an enrolment screen, and would simply break on
// the Monday after the policy changed. What bounds a key instead is its own expiry, checked
// below on every request, and the revoke button in the console.

const (
	// apiKeyPrefix opens every key. Versioned rather than bare, so a future format can be told
	// apart from this one instead of guessed at — and so a leaked key is greppable, both in our
	// own logs (redact catches it) and in a customer's.
	apiKeyPrefix = "atk1."
	// apiKeyRandomBytes is the whole of the secret: 32 bytes, far past guessing.
	apiKeyRandomBytes = 32
	// apiKeyPrefixLen is how many characters after the prefix the console keeps to show in the
	// table — enough to tell two keys apart, useless to anyone who has only that.
	apiKeyPrefixLen = 6
)

// mintAPIKey builds atk1.<32 random bytes, base64url>. Nothing identifying is encoded in it: the
// row is found by the hash of the whole token, so a key says nothing about the organisation it
// belongs to until it is checked.
func mintAPIKey() string {
	buf := make([]byte, apiKeyRandomBytes)
	rand.Read(buf)
	return apiKeyPrefix + base64.RawURLEncoding.EncodeToString(buf)
}

func hashAPIKey(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// looksLikeAPIKey reports whether a bearer token is one of ours at all. A console session token
// fails here, which is the point: the two credentials open different doors, and neither is
// accepted at the other's.
func looksLikeAPIKey(tok string) bool {
	return strings.HasPrefix(tok, apiKeyPrefix) && len(tok) >= len(apiKeyPrefix)+40
}

// apiKeyDisplayPrefix is what the console shows in the table: the marker and the first few
// characters of the secret.
func apiKeyDisplayPrefix(raw string) string {
	if len(raw) < len(apiKeyPrefix)+apiKeyPrefixLen {
		return raw
	}
	return raw[:len(apiKeyPrefix)+apiKeyPrefixLen]
}

// ---- rate limiting ----
//
// Per key, per minute, in this process. A shared counter in the database would be exact, but it
// would also put a write on the path of every read, and the thing being defended against here is
// a runaway loop in somebody's script rather than a distributed attacker. Cloud Run may hold
// several instances, so the real ceiling is this times the instance count; that is the honest
// trade and it is documented on the endpoint page.

const apiKeyRateLimit = 240 // requests per minute per key

type rateWindow struct {
	start time.Time
	n     int
}

type perMinute = struct {
	sync.Mutex
	m map[int64]*rateWindow
}

var apiRates = perMinute{m: map[int64]*rateWindow{}}

// mcpGrantRates is the same count for connected MCP clients, which are keys by another name and
// get the same ceiling.
var mcpGrantRates = perMinute{m: map[int64]*rateWindow{}}

// allowKeyRequest counts one request against a key's minute and says whether it fits, along with
// how many seconds until the window turns over.
func allowKeyRequest(id int64) (bool, int) { return allowPerMinute(&apiRates, id) }

// allowGrantRequest is allowKeyRequest for a connected MCP client.
func allowGrantRequest(id int64) (bool, int) { return allowPerMinute(&mcpGrantRates, id) }

func allowPerMinute(rates *perMinute, id int64) (bool, int) {
	rates.Lock()
	defer rates.Unlock()
	nowT := time.Now()
	w := rates.m[id]
	if w == nil || nowT.Sub(w.start) >= time.Minute {
		// Sweep while we hold the lock. Keys are few and windows are short, so this stays a
		// handful of entries rather than one per key that ever called.
		if len(rates.m) > 1000 {
			for k, v := range rates.m {
				if nowT.Sub(v.start) >= time.Minute {
					delete(rates.m, k)
				}
			}
		}
		w = &rateWindow{start: nowT}
		rates.m[id] = w
	}
	w.n++
	if w.n > apiKeyRateLimit {
		return false, int(time.Minute.Seconds() - nowT.Sub(w.start).Seconds() + 1)
	}
	return true, 0
}

// ---- authentication ----

// keyError is a refusal with the status it travels as.
type keyError struct {
	msg    string
	status int
}

// authenticateKey resolves an Authorization header to the person a key acts as. Every failure
// path returns the same shape and says as little as it can: whether a key is unknown, revoked or
// simply not ours is not information a caller needs to tell apart.
func (b *Bot) authenticateKey(r *http.Request) (*AdminUser, *keyError) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return nil, &keyError{"Missing Authorization: Bearer <key> header.", http.StatusUnauthorized}
	}
	tok := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if !looksLikeAPIKey(tok) {
		return nil, &keyError{"Invalid API key.", http.StatusUnauthorized}
	}
	ctx := r.Context()
	// Found by the hash of the whole token: what is stored is not enough to authenticate with,
	// so a copy of the database is not a set of working keys.
	row, err := b.store.APIKeyByHash(ctx, hashAPIKey(tok))
	if err != nil {
		return nil, &keyError{"Could not check that key.", http.StatusInternalServerError}
	}
	if row == nil {
		return nil, &keyError{"Invalid API key.", http.StatusUnauthorized}
	}
	if row.RevokedAt != "" {
		return nil, &keyError{"This API key has been revoked.", http.StatusUnauthorized}
	}
	// Expiry is a comparison here rather than a background sweep, so a key stops working the
	// minute it lapses instead of the next time something remembers to look.
	if row.ExpiresAt != "" && row.ExpiresAt <= now() {
		return nil, &keyError{"This API key has expired.", http.StatusUnauthorized}
	}
	if fits, retry := allowKeyRequest(row.ID); !fits {
		return nil, &keyError{fmt.Sprintf("Rate limit exceeded — %d requests a minute. Retry in %d seconds.", apiKeyRateLimit, retry), http.StatusTooManyRequests}
	}
	u, refusal := b.actAs(ctx, row.UserID, row.OrgID, "API key")
	if refusal != nil {
		return nil, refusal
	}
	u.Via, u.APIKeyID = "api_key", row.ID
	b.store.TouchAPIKey(ctx, row.ID, row.LastUsed)
	return u, nil
}

// actAs resolves the person a credential acts as — a key here, a connected MCP client in
// mcp_server.go. The membership is the authority. Resolved per request, so a role changed in the
// console this morning binds this afternoon's cron run, and a member who was removed takes their
// credentials with them. noun is what a refusal calls the credential.
func (b *Bot) actAs(ctx context.Context, userID, orgID int64, noun string) (*AdminUser, *keyError) {
	m, err := b.store.Membership(ctx, userID, orgID)
	if err != nil {
		return nil, &keyError{"Could not check that " + noun + ".", http.StatusInternalServerError}
	}
	if m == nil {
		return nil, &keyError{"This " + noun + " is no longer valid: the account that created it is not a member of that organisation any more.", http.StatusUnauthorized}
	}
	u := &AdminUser{ID: userID, OrgID: orgID, OrgName: m.OrgName, OrgSlug: m.OrgSlug,
		OrgPublic: m.OrgPublic, PublicID: m.UserPublic}
	if acct, _ := b.store.User(ctx, userID); acct != nil {
		if acct.Status != "active" {
			return nil, &keyError{"This " + noun + " is no longer valid: the account that created it is disabled.", http.StatusUnauthorized}
		}
		u.Name, u.Email = acct.Name, acct.Email
	}
	u.Role = m.Role
	u.Permissions = permissionsForRole(m.Role, b.store.CustomRoleMap(ctx, u.OrgID))
	return u, nil
}

// requireKey gates a public API route on a valid key. The person it resolves goes into the
// request context exactly as a console session would, so orgOf(r) and adminFromCtx(r) mean the
// same thing on both sides of the app and no handler has to know which door it was reached
// through.
func (b *Bot) requireKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// A call the MCP server is making in-process arrives already resolved: the server checked
		// the credential at its own door, and checking it again here would count it twice
		// against the rate limit and refuse an access token this door does not take.
		var refusal *keyError
		u := mcpCaller(r)
		if u == nil {
			u, refusal = b.authenticateKey(r)
		}
		if refusal != nil {
			if refusal.status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="attest_tag"`)
			}
			writeJSON(w, refusal.status, map[string]any{"error": refusal.msg})
			return
		}
		// Under the same audit floor as the console: a script's writes are recorded like a
		// person's, with the key they were made with (audit.go).
		b.auditedWrites(next)(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

// requireKeyPerm adds the permission the route needs. Same vocabulary as the console's
// requirePerm, deliberately: one definition of what "may manage documents" means, checked
// wherever the request came from.
func (b *Bot) requireKeyPerm(perm Permission, next http.HandlerFunc) http.HandlerFunc {
	return b.requireKey(func(w http.ResponseWriter, r *http.Request) {
		u := adminFromCtx(r.Context())
		if u == nil || !u.Permissions[perm] {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": denialCopy(perm)})
			return
		}
		next(w, r)
	})
}

// ---- console: managing keys ----

// apiKeyRoutes registers key management. It lives behind the console session, never behind a key:
// a credential that can mint further credentials is how a narrow leak becomes a wide one.
func (b *Bot) apiKeyRoutes(mux *http.ServeMux) {
	// The list is open to any member, like the users list: names, prefixes and dates say who has
	// automation pointed at this organisation, and that is worth being able to see.
	mux.HandleFunc("GET /api/api-keys", b.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		keys, err := b.store.APIKeys(r.Context(), orgOf(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"keys": keys, "rate_limit_per_minute": apiKeyRateLimit})
	}))

	mux.HandleFunc("POST /api/api-keys", b.requirePerm(PermAPIKeysManage, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
			// ExpiresOn is a plain "YYYY-MM-DD" from the picker. Empty means a key that never
			// expires, which is the default the console offers for a reason: an expiry nobody
			// diarised is an outage on a date nobody remembers.
			ExpiresOn string `json:"expires_on"`
		}
		if err := decode(r, &in); err != nil {
			bad(w, err)
			return
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			bad(w, fmt.Errorf("give the key a name"))
			return
		}
		if len(name) > 80 {
			name = name[:80]
		}
		expires := ""
		if in.ExpiresOn != "" {
			day, err := time.Parse("2006-01-02", strings.TrimSpace(in.ExpiresOn))
			if err != nil {
				bad(w, fmt.Errorf("that expiry date isn't valid"))
				return
			}
			// End of the chosen day, not its first second: a key set to expire today should last
			// until midnight rather than having died before it was made.
			end := day.Add(24*time.Hour - time.Second)
			if !end.After(time.Now().UTC()) {
				bad(w, fmt.Errorf("pick an expiry date in the future"))
				return
			}
			expires = end.Format(time.DateTime)
		}
		me := adminFromCtx(r.Context())
		raw := mintAPIKey()
		id, err := b.store.CreateAPIKey(r.Context(), me.OrgID, me.ID, name, apiKeyDisplayPrefix(raw), hashAPIKey(raw), expires, me.Email)
		if err != nil {
			fail(w, err)
			return
		}
		// Read the row back so the console gets exactly what the list would give it. If that read
		// fails the key still exists and must still be shown, so fall back to what we already
		// know rather than answering with a null row and losing the only copy of the key.
		made := APIKey{ID: id, OrgID: me.OrgID, UserID: me.ID, Name: name,
			Prefix: apiKeyDisplayPrefix(raw), Owner: me.Email, CreatedBy: me.Email,
			CreatedAt: now(), ExpiresAt: expires}
		if keys, err := b.store.APIKeys(r.Context(), me.OrgID); err == nil {
			for _, k := range keys {
				if k.ID == id {
					made = k
				}
			}
		}
		// The prefix and the expiry, never the key: the audit row outlives the one response the
		// raw key is allowed to appear in.
		b.audit(r, "api_key.created", AuditEvent{TargetKind: "api_key", TargetID: strconv.FormatInt(id, 10), TargetName: name,
			Details: auditDetails(map[string]any{"prefix": made.Prefix, "expires_at": expires})})
		// The one and only time the raw key exists in a response. Nothing stores it; if it is
		// lost the answer is a new key, not a recovery.
		writeJSON(w, 201, map[string]any{"key": made, "raw": raw})
	}))

	mux.HandleFunc("DELETE /api/api-keys/{id}", b.requirePerm(PermAPIKeysManage, func(w http.ResponseWriter, r *http.Request) {
		// Read before revoking, so the row can carry the key's name and prefix — after the
		// revoke it is gone from the list the reader would look it up in.
		name, prefix := "", ""
		if keys, err := b.store.APIKeys(r.Context(), orgOf(r)); err == nil {
			for _, k := range keys {
				if k.ID == pathID(r, "id") {
					name, prefix = k.Name, k.Prefix
				}
			}
		}
		ok, err := b.store.RevokeAPIKey(r.Context(), orgOf(r), pathID(r, "id"))
		if err != nil {
			fail(w, err)
			return
		}
		if !ok {
			writeJSON(w, 404, map[string]any{"error": "no such key, or it is already revoked"})
			return
		}
		b.audit(r, "api_key.revoked", AuditEvent{TargetKind: "api_key", TargetID: r.PathValue("id"), TargetName: name,
			Details: auditDetails(map[string]any{"prefix": prefix})})
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
}
