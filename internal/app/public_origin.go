package app

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// The bot's public origin (https://app.attesttag.com) goes into reply footers, setup links,
// the worker's callback and the whoami line, none of which have a request to read it from.
// Rather than configuring it per environment, the bot learns it: every authenticated console
// request carries the origin the admins actually use, and that origin is remembered in the
// settings table (key public_origin) so it survives restarts and redeploys. ADMIN_BASE_URL,
// when set, overrides the learned value; with neither, links fall back to the process's own
// listen address, which only works on the machine the bot runs on.
//
// Only authenticated requests teach it. That used to be the whole defence, and it stopped being
// enough the moment anyone could sign up: "authenticated" now means "created an account a minute
// ago", not "runs this deployment". A stranger who signs up and sends one request with a forged
// Host header would otherwise repoint the origin for the entire deployment — and the origin is
// what password-reset, verification and invitation links are built from, for every tenant.
//
// So the host is pinned rather than merely authenticated. PUBLIC_ORIGIN_HOSTS, when set, is the
// allowlist and nothing outside it is ever learned. With it unset the first origin learned wins,
// and afterwards only the same host may teach again — which still lets http become https on a
// redeploy, but never lets one host replace another. Loopback origins are ignored throughout;
// the local fallback already covers them.

const publicOriginKey = "public_origin"

// originHostAllowed decides whether host may teach the deployment its public origin. cur is the
// origin already learned, "" on a fresh deployment.
func originHostAllowed(host, cur string) bool {
	if list := strings.TrimSpace(os.Getenv("PUBLIC_ORIGIN_HOSTS")); list != "" {
		for _, h := range strings.Split(list, ",") {
			if strings.EqualFold(strings.TrimSpace(h), host) {
				return true
			}
		}
		return false
	}
	if cur == "" {
		return true // bootstrap: the first origin to arrive is the one this deployment is served on
	}
	return strings.EqualFold(originHost(cur), host)
}

// originHost is the bare hostname of an origin string, without scheme or port.
func originHost(origin string) string {
	h := origin
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if bare, _, err := net.SplitHostPort(h); err == nil {
		h = bare
	}
	return h
}

// requestOrigin is the scheme and host the client used for r, honouring the proxy's
// X-Forwarded-Proto (Cloud Run terminates TLS in front of the container).
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// publicOriginTTL bounds how long an instance may go on believing an origin it read once.
// Short, because the cost of re-reading is one indexed row and the cost of being wrong is a
// password-reset link pointing at the wrong host.
const publicOriginTTL = time.Minute

var publicOriginMu sync.Mutex // guards the Store's cached origin fields

// PublicOrigin returns the remembered origin, "" if none was learned yet.
func (s *Store) PublicOrigin(ctx context.Context) string {
	if s == nil {
		return ""
	}
	publicOriginMu.Lock()
	defer publicOriginMu.Unlock()
	// Re-read on a TTL. It was loaded once and kept forever, which was right when one process
	// both learned and served it. With two, the instance that never saw the request that taught
	// the origin keeps answering "" — and the origin is what password-reset, verification and
	// invitation links are built from, so half of them would be built wrong.
	if !s.originLoaded || time.Since(s.originAt) > publicOriginTTL {
		// org_id 0 marks the settings rows that belong to the deployment rather than to any
		// organisation. The console's public origin is one of exactly two such facts: there is
		// one console, on one hostname, whoever is signed into it.
		s.db.QueryRowContext(ctx, `select value from settings where org_id=0 and key = ?`, publicOriginKey).Scan(&s.origin)
		s.originLoaded, s.originAt = true, time.Now()
	}
	return s.origin
}

// putGlobalSetting writes a setting that belongs to the deployment rather than to an
// organisation, under the reserved org_id 0.
func (s *Store) putGlobalSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `insert into settings (org_id, key, value, updated_at) values (0, ?, ?, ?)
		on conflict(org_id, key) do update set value=excluded.value, updated_at=excluded.updated_at`, key, value, now())
	return err
}

// LearnOrigin remembers origin as the public one, subject to the rules above. Cheap when
// nothing changes, which is every request but the first.
func (s *Store) LearnOrigin(ctx context.Context, origin string) {
	if s == nil {
		return
	}
	origin = strings.TrimRight(origin, "/")
	host := origin[strings.Index(origin, "://")+3:]
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return
	}
	cur := s.PublicOrigin(ctx)
	if cur == origin || (strings.HasPrefix(cur, "https://") && !strings.HasPrefix(origin, "https://")) {
		return
	}
	if !originHostAllowed(host, cur) {
		slog.Warn("refusing to learn a public origin from an unexpected Host header",
			"host", host, "current", cur,
			"hint", "set PUBLIC_ORIGIN_HOSTS or ADMIN_BASE_URL if this host is legitimate")
		return
	}
	if err := s.putGlobalSetting(ctx, publicOriginKey, origin); err != nil {
		return
	}
	slog.Info("public origin learned", "origin", origin)
	publicOriginMu.Lock()
	s.origin = origin
	publicOriginMu.Unlock()
}

// publicBaseURL is the origin to put in links that leave the process: ADMIN_BASE_URL, else
// the learned origin, else the local listen address, else "".
func publicBaseURL(ctx context.Context, st *Store, cfg Config) string {
	if v := strings.TrimRight(os.Getenv("ADMIN_BASE_URL"), "/"); v != "" {
		return v
	}
	if v := st.PublicOrigin(ctx); v != "" {
		return v
	}
	return localBaseURL(cfg.HealthAddr)
}
