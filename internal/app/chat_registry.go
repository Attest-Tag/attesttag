package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/slack-go/slack"
	"time"
)

// One process serves every connected Slack workspace through signed HTTP deliveries.
// Each Web API call uses the bot token of the workspace in the verified payload.
//
// ChatRegistry is that lookup. It hands back one *Chat per team, built lazily from its row in the
// teams table — for a Slack workspace, from the bot token sealed there — with the transport that
// row's platform needs. *Chat already owns six per-workspace caches (names,
// channels, emails, conversations, members), so one instance per team scopes all of them with
// no key changes.
//
// It is hydrated from the database, never from a live auth.test at boot: a single revoked
// install must not stop the process.
type ChatRegistry struct {
	mu     sync.Mutex
	byTeam map[string]*Chat
	cached map[string]time.Time // when each entry was hydrated
	store  *Store
	sealer *Sealer
	// msteams is the deployment's Teams client, shared by every Teams tenant's transport; nil
	// when the deployment has no Teams app registration.
	msteams *msteamsClient
}

// slackClientTTL bounds how stale a cached client may be.
//
// The cache had no expiry, which was right while one process held every client and invalidated
// its own on a reinstall. With two, a workspace that reconnects through instance A leaves
// instance B holding the revoked token until it restarts — so half the workspace's messages get
// an invalid_auth that nobody can explain, and reconnecting again does not fix it.
const slackClientTTL = 10 * time.Minute

func NewChatRegistry(st *Store, sealer *Sealer) *ChatRegistry {
	return &ChatRegistry{byTeam: map[string]*Chat{}, cached: map[string]time.Time{}, store: st, sealer: sealer}
}

// For returns the client for one team. An unknown, revoked or unsealable team is an error,
// never a fallback to some other team's token: answering workspace B with workspace A's
// credentials is the one failure mode this whole design exists to prevent.
func (r *ChatRegistry) For(ctx context.Context, teamID string) (*Chat, error) {
	if r == nil {
		return nil, fmt.Errorf("no Slack workspace is connected")
	}
	r.mu.Lock()
	sl, ok := r.byTeam[teamID]
	fresh := ok && time.Since(r.cached[teamID]) < slackClientTTL
	r.mu.Unlock()
	if fresh {
		return sl, nil
	}
	if teamID == "" {
		return nil, fmt.Errorf("no Slack workspace on this event")
	}

	if r.store == nil {
		return nil, fmt.Errorf("workspace %s is not connected", teamID)
	}
	t, err := r.store.Team(ctx, teamID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("workspace %s is not connected", teamID)
	}
	if t.Status != "active" {
		return nil, fmt.Errorf("workspace %s (%s) was disconnected", t.Name, teamID)
	}
	tr, err := r.transportFor(t)
	if err != nil {
		return nil, err
	}
	built := &Chat{
		t:         tr,
		Platform:  t.Platform,
		BotUserID: t.BotUserID,
		BotID:     t.BotID,
		TeamID:    t.TeamID,
		TeamName:  t.Name,
		OrgID:     t.OrgID,
	}
	r.mu.Lock()
	// Another goroutine may have built one while we were in the database; keep whichever
	// landed first so the caches are not split in two.
	if was, ok := r.byTeam[teamID]; ok && time.Since(r.cached[teamID]) < slackClientTTL {
		built = was
	} else {
		r.putLocked(teamID, built)
	}
	r.mu.Unlock()
	return built, nil
}

// transportFor builds the transport a workspace's platform needs. A platform this build does not
// know is an error, never a guess: a client for the wrong platform fails on every call, and
// guessing whose credentials a workspace takes is the one thing For exists to rule out.
func (r *ChatRegistry) transportFor(t *Team) (transport, error) {
	switch t.Platform {
	case platformSlack:
		token, err := r.sealer.Open(t.tokenEnc)
		if err != nil {
			return nil, fmt.Errorf("workspace %s: stored token could not be unsealed (MASTER_KEY changed?): %w", t.TeamID, err)
		}
		return &slackTransport{api: slack.New(string(token))}, nil
	case platformMSTeams:
		if r.msteams == nil {
			return nil, fmt.Errorf("workspace %s is on Microsoft Teams, and this deployment has no MSTEAMS_APP_ID", t.TeamID)
		}
		if t.ServiceURL == "" {
			return nil, fmt.Errorf("workspace %s has no Bot Framework service URL yet; it arrives with the tenant's first message", t.TeamID)
		}
		return r.msteams.transport(t.TeamID, t.ServiceURL), nil
	}
	return nil, fmt.Errorf("workspace %s is on %q, which this build cannot talk to", t.TeamID, t.Platform)
}

// putLocked is the only way a client enters the cache, so nothing can leave the timestamp
// behind — an entry with no timestamp reads as infinitely stale and would be rebuilt on every
// call, which on a registry built directly (the tests do) means rebuilt from a store that is
// not there.
func (r *ChatRegistry) putLocked(teamID string, sl *Chat) {
	if r.cached == nil {
		r.cached = map[string]time.Time{}
	}
	r.byTeam[teamID] = sl
	r.cached[teamID] = time.Now()
}

// Put places a client directly, for a registry assembled rather than hydrated.
func (r *ChatRegistry) Put(teamID string, sl *Chat) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.putLocked(teamID, sl)
}

// Evict drops a cached client: on reinstall (a new token), on disconnect, and on
// app_uninstalled / tokens_revoked.
func (r *ChatRegistry) Evict(teamID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.byTeam, teamID)
	delete(r.cached, teamID)
	r.mu.Unlock()
}

// Active is what the fan-out loops iterate: scope sync, the routine scheduler, document
// ingest, the job reconciler. A team whose client cannot be built is logged and skipped
// rather than aborting the sweep for everyone else.
func (r *ChatRegistry) Active(ctx context.Context) []*Chat {
	if r == nil || r.store == nil {
		// A registry built for a test holds its clients directly and has no store behind it.
		if r == nil {
			return nil
		}
		out := make([]*Chat, 0, len(r.byTeam))
		seen := map[*Chat]bool{}
		for _, sl := range r.byTeam {
			if !seen[sl] {
				seen[sl] = true
				out = append(out, sl)
			}
		}
		return out
	}
	teams, err := r.store.ActiveTeams(ctx)
	if err != nil {
		slog.Error("list teams", "err", err)
		return nil
	}
	out := make([]*Chat, 0, len(teams))
	for _, t := range teams {
		sl, err := r.For(ctx, t.TeamID)
		if err != nil {
			slog.Warn("skipping workspace", "team", t.TeamID, "err", err)
			continue
		}
		out = append(out, sl)
	}
	return out
}

// Any returns some connected workspace. It exists only for the handful of console reads that
// need a Slack client but not a particular one (looking a user up by email when the caller
// has not said which workspace they mean); it is never used on the message path.
func (r *ChatRegistry) Any(ctx context.Context) *Chat {
	all := r.Active(ctx)
	if len(all) == 0 {
		return nil
	}
	return all[0]
}

// AnyFor is Any narrowed to one organisation. Any() walks every connected workspace in the
// deployment, so using it to resolve a name or a channel means asking an arbitrary other
// tenant's Slack about ids that mean nothing there — and, when the id happens to exist in both,
// answering with the wrong company's user. Anything that already knows whose request it is
// should use this instead.
func (r *ChatRegistry) AnyFor(ctx context.Context, orgID int64) *Chat {
	for _, sl := range r.Active(ctx) {
		if sl.OrgID == orgID {
			return sl
		}
	}
	return nil
}

// Token hands back one workspace's raw bot token. It exists for files.slack.com downloads,
// which are plain HTTP rather than Web API calls and so cannot go through *Chat.
func (r *ChatRegistry) Token(ctx context.Context, teamID string) (string, error) {
	if r == nil || r.store == nil {
		return "", fmt.Errorf("no Slack workspace is connected")
	}
	t, err := r.store.Team(ctx, teamID)
	if err != nil {
		return "", err
	}
	if t == nil || t.Status != "active" {
		return "", fmt.Errorf("workspace %s is not connected", teamID)
	}
	if t.Platform != platformSlack {
		return "", fmt.Errorf("workspace %s is not on Slack, so it has no bot token to download with", teamID)
	}
	tok, err := r.sealer.Open(t.tokenEnc)
	return string(tok), err
}

// orgOfChat is the organisation behind a client, tolerating a nil one so a loop that could not
// build a client still has something to key an alert on rather than panicking.
func orgOfChat(sl *Chat) int64 {
	if sl == nil {
		return 0
	}
	return sl.OrgID
}
