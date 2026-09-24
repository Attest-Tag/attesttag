package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Ceilings on what one organisation, one address or one caller may do in a window. Signup is
// open, so every one of these is what stands between a stranger with a script and the mail
// domain, the model key, the scheduler or the disk that every other tenant shares.

// rateLimiter counts events per key inside a fixed window.
//
// Shared across instances when a store has been attached, because these are security controls
// rather than conveniences: signup, invitations, the login lockout, the operator secret. Counted
// per process, N instances give a stranger with a script N times the budget the number claims —
// eight login attempts becomes twenty-four, and nothing says so. The count is a query on an
// indexed table; that is affordable for something that runs on a failed sign-in.
//
// The in-process map stays as the fallback. A limiter used before the store is attached — and
// there is one, the operator route's, which exists before boot finishes — still limits, just
// per instance. Degrading to the old behaviour is better than not limiting at all.
type rateLimiter struct {
	mu    sync.Mutex
	m     map[string]*rateWindow
	store *Store
}

func newRateLimiter() *rateLimiter { return &rateLimiter{m: map[string]*rateWindow{}} }

// shareAcross puts this limiter's counting in the database. Called once at boot, for every
// limiter that guards something a second instance would otherwise double.
func (l *rateLimiter) shareAcross(st *Store) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.store = st
	l.mu.Unlock()
}

// allow counts one event and says whether it fits; when it does not, how long until it would.
func (l *rateLimiter) allow(key string, limit int, window time.Duration) (bool, time.Duration) {
	l.mu.Lock()
	st := l.store
	l.mu.Unlock()
	if st != nil {
		if ok, retry, err := st.countThrottle(context.Background(), key, limit, window); err == nil {
			return ok, retry
		}
		// A database that cannot be counted against is not a reason to stop limiting; fall
		// through to this process's own count, which is what it did before.
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	nowT := time.Now()
	if len(l.m) > 10000 {
		for k, w := range l.m {
			if nowT.Sub(w.start) >= window {
				delete(l.m, k)
			}
		}
	}
	w := l.m[key]
	if w == nil || nowT.Sub(w.start) >= window {
		w = &rateWindow{start: nowT}
		l.m[key] = w
	}
	w.n++
	if w.n > limit {
		return false, window - nowT.Sub(w.start)
	}
	return true, 0
}

var (
	signups = newRateLimiter() // accounts founded per caller
	invites = newRateLimiter() // invitations per organisation and per destination address
)

// The ceilings themselves. Variables rather than constants so a deployment can move them, and
// read after the dotenv file is loaded rather than at package init, which is the whole reason
// applyLimitOverrides exists as a function (config.go calls it).
//
// The defaults are sized for a service with open signup, where these are what stands between a
// stranger with a script and the disk, the mail domain and the model key everyone shares. On a
// twenty-person deployment behind an invitation they are simply in the way: two thousand
// documents is a number somebody hits, and five signups an hour is absurd when there will
// be twenty ever.
//
// Not everything here is exposed. minRoutineInterval protects the scheduler from a routine
// that runs every ten seconds, and the personal-note caps are per person and already generous;
// letting either be raised makes somebody else's deployment worse, not theirs.
var (
	signupsPerIPPerHour   = 5
	invitesPerOrgPerHour  = 20
	invitesPerAddressADay = 3

	maxRoutinesPerOrg    = 50
	maxDocumentsPerOrg   = 2000
	maxConnectionsPerOrg = 100
	maxBundlesPerOrg     = 50
	maxMemoriesPerOrg    = 500 // shared memory only; personal notes have their own budget
	maxDriveSyncsPerOrg  = 25
)

// limitOverrides maps each variable to the name that moves it. One table, so the documentation
// and the code cannot disagree about what a variable is called.
var limitOverrides = []struct {
	env string
	to  *int
}{
	{"LIMIT_SIGNUPS_PER_IP_PER_HOUR", &signupsPerIPPerHour},
	{"LIMIT_INVITES_PER_ORG_PER_HOUR", &invitesPerOrgPerHour},
	{"LIMIT_INVITES_PER_ADDRESS_A_DAY", &invitesPerAddressADay},
	{"LIMIT_EMAIL_TURNS_PER_CHANNEL_PER_HOUR", &emailTurnsPerChannelPerHour},
	{"LIMIT_EMAIL_AUTO_WRITES_PER_TURN", &emailAutoWritesPerTurn},
	{"LIMIT_ROUTINES_PER_ORG", &maxRoutinesPerOrg},
	{"LIMIT_DOCUMENTS_PER_ORG", &maxDocumentsPerOrg},
	{"LIMIT_CONNECTIONS_PER_ORG", &maxConnectionsPerOrg},
	{"LIMIT_BUNDLES_PER_ORG", &maxBundlesPerOrg},
	{"LIMIT_MEMORIES_PER_ORG", &maxMemoriesPerOrg},
	{"LIMIT_DRIVE_SYNCS_PER_ORG", &maxDriveSyncsPerOrg},
}

// applyLimitOverrides reads the environment into the variables above. A value that is not a
// positive number is refused rather than silently ignored: a typo in a limit is a limit that is
// not the one anybody chose, and the log line is the only place it would ever show.
func applyLimitOverrides() {
	for _, o := range limitOverrides {
		raw := strings.TrimSpace(os.Getenv(o.env))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			slog.Warn("ignoring a limit that is not a positive number", "var", o.env, "value", raw, "keeping", *o.to)
			continue
		}
		slog.Info("limit overridden", "var", o.env, "was", *o.to, "now", n)
		*o.to = n
	}
}

const (
	// A person's own notes are capped per person, not out of the organisation's 500. Sharing
	// that budget would let one long to-do list be the reason nobody else can remember
	// anything -- and the admin sent to /memory to prune would find far fewer than 500 there,
	// because personal notes are not on that page. The org cap here is disk, not fairness.
	maxPersonalNotesPerUser = 200
	maxPersonalNotesPerOrg  = 20000
	minRoutineInterval      = 15 * time.Minute
	// throttleGCHorizon bounds the shared throttle table without letting a short-window caller
	// evict another's rows: countThrottle sweeps only its own key within the window, and this is
	// the separate, far-longer horizon at which any key's stale rows are collected. It must
	// exceed the longest window any caller passes to allow(); the longest today is 24h (invites,
	// size requests), so a week is comfortable headroom.
	throttleGCHorizon = 7 * 24 * time.Hour
)

var errOrgCap = errors.New("this organisation has reached its limit for that; remove some first")

// Separate from errOrgCap because the remedy is. The organisation's cap is an admin's problem;
// this one the person can clear in ten seconds, and the model relays this text to them, so it is
// written to them rather than about them.
var errPersonalCap = errors.New("you have reached your limit of personal notes; delete a few first")

// cleanName is the rule for names that reach other people — an organisation's, an account's —
// in mail subjects and Slack cards: trimmed, bounded, and free of control characters, since a
// carriage return in a name is a second header line in an email.
func cleanName(s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("a name is needed")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", errors.New("names cannot contain control characters")
		}
	}
	// A name is not a link. Nothing is really called "https://…", and a name that is gets read
	// back to people in mail signed by attest_tag and shown all over somebody else's console —
	// which hands whoever picked it a phishing page with our name on it. A company called
	// "acme.com" is somebody's company and passes; a scheme in front of it is not a name.
	if nameLooksLikeURL.MatchString(s) {
		return "", errors.New("a name cannot be a web address")
	}
	if len(s) > max {
		return "", errors.New("that name is too long")
	}
	return s, nil
}

var nameLooksLikeURL = regexp.MustCompile(`(?i)://|\b(?:javascript|data|vbscript):`)

// countThrottle records one event and reports whether it fits the window, counting what every
// instance has seen rather than only this one.
//
// One insert and one count. The sweep of anything older than the window happens on the same
// call, so the table stays the size of the traffic in one window and needs no background job —
// the same argument PutOAuthPending makes for cleaning up where the work already is.
func (s *Store) countThrottle(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	cutoff := time.Now().Add(-window).UnixNano()
	// Sweep only THIS key's expired rows. A global `at < cutoff` here let any caller with a short
	// window — POST /api/auth/forgot runs a 15-minute one — delete every other limiter's older
	// rows and so shrink their windows to its own. A separate sweep at a horizon longer than any
	// window keeps the table bounded without that cross-key effect.
	if _, err := s.db.ExecContext(ctx, `delete from throttle_events where key=? and at < ?`, key, cutoff); err != nil {
		return false, 0, err
	}
	if _, err := s.db.ExecContext(ctx, `delete from throttle_events where at < ?`, time.Now().Add(-throttleGCHorizon).UnixNano()); err != nil {
		return false, 0, err
	}
	if _, err := s.db.ExecContext(ctx, `insert into throttle_events (key, at) values (?, ?)`,
		key, time.Now().UnixNano()); err != nil {
		return false, 0, err
	}
	var n int
	var oldest int64
	if err := s.db.QueryRowContext(ctx,
		`select count(*), coalesce(min(at), 0) from throttle_events where key=? and at >= ?`,
		key, cutoff).Scan(&n, &oldest); err != nil {
		return false, 0, err
	}
	if n > limit {
		// How long until the oldest event in the window falls out of it, which is when one more
		// would fit. The caller puts this in a Retry-After.
		retry := time.Until(time.Unix(0, oldest).Add(window))
		if retry < 0 {
			retry = 0
		}
		return false, retry, nil
	}
	return true, 0, nil
}
