package app

// The write half of active users: deciding that somebody counts, and recording it once a day.
//
// There is exactly one call site, in bot.go's incoming(), and that is the design rather than an
// accident. incoming() is the single entry point for a human turn and has already refused bots,
// the bot's own id and empty users before anything here runs; routines, the playground and
// investigations build a Call directly and never pass through it, which is what makes "machine
// activity is not a user" true without a flag anybody has to remember to set.

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// markedToday is the "we have already counted this person today" set, so a busy channel is one
// upsert a day per container rather than one per turn. Per process and not shared: another
// container writing the same row is an upsert that increments the turn count, which is correct,
// and the cost of missing that optimisation across instances is one write.
//
// Keyed on the day as well as the person, so it expires itself at midnight UTC without a sweep.
var markedToday sync.Map // "org:identity:day" -> struct{}

// countsAsUser is the whole of "is this somebody we bill for", in one place so that the rule can
// be read and tested rather than inferred from a condition at a call site.
//
// Three exclusions, each a decision rather than a defensive check:
//
//   - the bot's own id, which a self-test turn is rewritten to before it is recorded;
//   - 'email:<channel>', the forwarded-mail lane, which is keyed per channel and not per sender
//     precisely so that it is not a person — counting it would bill an account for its own inbox;
//   - the empty id, which incoming() has already refused but which must not become a user here
//     if that ever changes.
func countsAsUser(user, botUserID string) bool {
	return user != "" && user != botUserID && !isEmailRequester(user)
}

// markActive counts one person's turn. It is called for its effect and never for its answer: a
// counter that could fail a turn would be a counter that takes the product down, so everything
// here is best-effort and logged.
//
// The email is what lets one human in two connected workspaces count once, and it is only asked
// for where the install granted users:read.email — teams.email_scope, probed per install. Without
// the scope users.info omits the address entirely, so asking would be a Slack call per person per
// ten minutes that could never return anything.
func (b *Bot) markActive(ctx context.Context, sl *Chat, user string) {
	if sl == nil || sl.OrgID == 0 || sl.TeamID == "" || user == "" {
		return
	}
	m := ActiveMark{OrgID: sl.OrgID, TeamID: sl.TeamID, SlackUser: user}
	key := m.identity() + ":" + today()
	if _, seen := markedToday.Load(key); seen {
		// Already counted today by this container. The turn count on the row is a nicety, not
		// the figure anybody is billed on, so it is the right thing to trade away here.
		return
	}

	if t, err := b.store.Team(ctx, sl.TeamID); err == nil && t != nil && t.EmailScope {
		// The same users.info cache mayUseBot reads: one call per person per userInfoTTL,
		// shared with the domain gate and with name resolution.
		if uf, err := sl.user(ctx, user, false); err == nil {
			m.Email = uf.Email
		}
	}

	if err := b.store.MarkActive(ctx, m); err != nil {
		slog.Warn("could not record an active user", "org", sl.OrgID, "team", sl.TeamID, "err", err)
		return
	}
	markedToday.Store(key, struct{}{})
}

// ---- reading, for the surfaces ----

// ActiveUserCount is what every surface shows: the figure, the size's limit, and whether the one
// is past the other. Computed here rather than in the console for the same reason paused_by is —
// two copies of "are they over" would drift, and the one on the screen would be the wrong one.
type ActiveUserCount struct {
	// Users is the deduped count over the window.
	Users int `json:"users"`
	// Limit is the size's ceiling, 0 for a size that does not have one (and for an account with
	// no subscription at all, where there is nothing to be over).
	Limit int  `json:"limit"`
	Over  bool `json:"over"`
	// WindowDays is on the wire so a reader never has to assume 30, and so changing it is one
	// change rather than one per surface.
	WindowDays int               `json:"window_days"`
	ByTeam     []TeamActiveUsers `json:"by_team,omitempty"`
}

// activeUsers assembles that for one organisation. byTeam is optional because the console wants
// the breakdown and /v1 and the operator index do not — a per-workspace group-by on every row of
// a listing is work nobody asked for.
func (b *Bot) activeUsers(ctx context.Context, orgID int64, byTeam bool) ActiveUserCount {
	out := ActiveUserCount{WindowDays: int(activeUserWindow / (24 * time.Hour))}
	n, err := b.store.ActiveUsers(ctx, orgID, activeUserWindow)
	if err != nil {
		slog.Warn("could not count active users", "org", orgID, "err", err)
		return out
	}
	out.Users = n
	// The limit comes from the size the account is actually on. An account with no subscription
	// has no size and therefore no limit: a free account is not "over" anything, and saying it
	// was would be telling somebody to upgrade from a plan they have not bought.
	if acct, err := b.store.BillingAccountOf(ctx, orgID); err == nil {
		limit := userLimitOf(acct, b.cfg)
		// An enterprise deal's ceiling is its own, and it holds from the day the deal is written
		// rather than from the first payment: it is what the account was sold. Shown, never
		// enforced, like every ceiling here.
		if t, err := b.store.EnterpriseTerms(ctx, orgID); err == nil && t.Exists {
			limit = t.UserLimit
		}
		if limit > 0 {
			out.Limit = limit
			out.Over = n > limit
		}
	}
	if byTeam {
		out.ByTeam, _ = b.store.ActiveUsersByTeam(ctx, orgID, activeUserWindow)
	}
	return out
}

// nameActiveUsers puts a display name on each row, in place.
//
// Resolved here and never stored, which is the whole point of keeping only a hash of the address
// in a table that outlives the tenant's retention policy: the list is readable while somebody is
// looking at it, and the database is not a directory of their staff when nobody is.
//
// Concurrent and time-boxed, because the obvious serial loop is a trap. DisplayName is cached per
// workspace and shared with everything else that asks, so a warm console resolves the whole list
// from memory — but the FIRST load after a restart has a cache miss per person, and each miss is
// a live users.info. Measured at about 1.4s each: a hundred people serially is a page that never
// finishes. A person whose name has not arrived when the deadline passes is shown as their raw
// Slack id, which is what an unreachable workspace and a deleted account already fall back to, and
// the next load has the cache.
const (
	nameLookupWorkers  = 8
	nameLookupDeadline = 4 * time.Second
)

func (b *Bot) nameActiveUsers(ctx context.Context, rows []ActiveUserRow) {
	ctx, cancel := context.WithTimeout(ctx, nameLookupDeadline)
	defer cancel()

	// One handle per workspace, resolved up front: b.slacks.For takes a lock and may build a
	// client, and doing that inside the workers would serialise them on exactly the thing they
	// are trying to do in parallel.
	handles := map[string]*Chat{}
	for i := range rows {
		if _, ok := handles[rows[i].TeamID]; !ok {
			sl, _ := b.slacks.For(ctx, rows[i].TeamID)
			handles[rows[i].TeamID] = sl
		}
		// The fallback, written first so that every row has a name even if nothing below runs.
		rows[i].Name = rows[i].SlackUser
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < nameLookupWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				sl := handles[rows[i].TeamID]
				if sl == nil || rows[i].SlackUser == "" {
					continue
				}
				if name := sl.DisplayName(ctx, rows[i].SlackUser); name != "" {
					rows[i].Name = name
				}
			}
		}()
	}
	for i := range rows {
		select {
		case jobs <- i:
		case <-ctx.Done():
			// Out of time. Stop handing out work; what is already in flight finishes or gives
			// up on the same context, and the rest keep the id they were seeded with.
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}
