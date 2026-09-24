package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// The console's half of personal memory. Everything here answers one question -- which Slack
// account is the person holding this session -- and refuses to guess when it cannot tell.

// personalStore is one of the caller's own Slack accounts, named for the page.
type personalStore struct {
	TeamID      string `json:"team_id"`
	TeamName    string `json:"team_name"`
	SlackUserID string `json:"slack_user_id"`
	key         personalKey
}

// slackSelves are the Slack accounts this console session has proved it controls, in workspaces
// this organisation has connected. It is resolved per request from user_identities, and that is
// the point of it.
//
// AdminUser.UserID is right there on the session and is the wrong field twice over. It is marked
// display only, and slackIDOf -- which fills it -- returns a bare user id with the workspace
// stripped, then falls back to an id from a workspace this organisation never connected. Slack
// only promises user ids are unique inside a workspace, so keying somebody's private notes on it
// can address a different person entirely. Fine for rendering a mention; not for this.
//
// Matching by email is out for the reason store_identity.go gives: the address in a Slack
// sign-in is set by the workspace's own admin, so adopting an account because the addresses
// agree is a takeover. Here it would hand one person another's notes.
func (b *Bot) slackSelves(ctx context.Context, u *AdminUser) []personalStore {
	if u == nil || u.OrgID == 0 {
		return nil
	}
	ids, err := b.store.Identities(ctx, u.ID)
	if err != nil {
		return nil
	}
	teams, err := b.store.Teams(ctx, u.OrgID)
	if err != nil {
		return nil
	}
	named := map[string]string{}
	for _, t := range teams {
		named[t.TeamID] = t.Name
	}
	var out []personalStore
	for _, i := range ids {
		// A Slack identity, or a Microsoft one standing for the same person in a Teams tenant.
		team, user, ok := chatAccount(i)
		if !ok {
			continue
		}
		name, connected := named[team]
		if !connected {
			// A Slack account in a workspace this organisation has not connected. The session's
			// OrgID is the authority on what it may see, not the identity.
			continue
		}
		out = append(out, personalStore{TeamID: team, TeamName: name, SlackUserID: user,
			key: personalKey{orgID: u.OrgID, teamID: team, owner: user}})
	}
	return out
}

// selfFor picks the caller's account in one workspace. Writes go through this rather than
// trusting a team_id in the body: the caller says which of their own accounts, never whose.
func (b *Bot) selfFor(ctx context.Context, u *AdminUser, teamID string) (personalStore, bool) {
	selves := b.slackSelves(ctx, u)
	if teamID == "" && len(selves) == 1 {
		return selves[0], true
	}
	for _, s := range selves {
		if s.TeamID == teamID {
			return s, true
		}
	}
	return personalStore{}, false
}

type personalMemoryJSON struct {
	ID        int64  `json:"id"`
	TeamID    string `json:"team_id"`
	TeamName  string `json:"team_name"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// personalMemoryRoutes are gated on holding a session and nothing else. There is deliberately no
// permission here: memory.manage is an admin-and-editor permission, so requiring it would mean a
// viewer cannot keep their own notes while an editor can. A permission is something one person
// holds and another does not, and "you are you" is not that. requireAdmin already carries CSRF,
// the organisation's sign-in policy and the two-factor gate.
//
// There is no route here that takes a user id, and no admin listing. An organisation admin cannot
// read anybody's notes through this API or any other, which is what strictly private means.
func (b *Bot) personalMemoryRoutes(mux *http.ServeMux, a func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/personal-memories", a(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		selves := b.slackSelves(ctx, adminFromCtx(ctx))
		out := []personalMemoryJSON{}
		for _, self := range selves {
			notes, err := b.store.PersonalMemories(ctx, self.key)
			if err != nil {
				fail(w, err)
				return
			}
			for _, n := range notes {
				out = append(out, personalMemoryJSON{ID: n.ID, TeamID: self.TeamID, TeamName: self.TeamName,
					Text: n.Text, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt})
			}
		}
		// An empty identities list is the ordinary case for somebody who signed up with a
		// password and never connected Slack -- not an error, and not a reason to guess.
		writeJSON(w, 200, map[string]any{"identities": selves, "memories": out})
	}))

	mux.HandleFunc("POST /api/personal-memories", a(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var in struct{ TeamID, Text string }
		if err := decode(r, &in); err != nil || strings.TrimSpace(in.Text) == "" {
			bad(w, fmt.Errorf("text is required"))
			return
		}
		self, ok := b.selfFor(ctx, adminFromCtx(ctx), in.TeamID)
		if !ok {
			writeJSON(w, 404, map[string]any{"error": "that is not one of your connected Slack accounts"})
			return
		}
		if _, err := b.store.AddPersonalMemory(ctx, self.key, in.Text); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, map[string]any{"ok": true})
	}))

	mux.HandleFunc("PUT /api/personal-memories/{id}", a(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var in struct{ Text string }
		if err := decode(r, &in); err != nil || strings.TrimSpace(in.Text) == "" {
			bad(w, fmt.Errorf("text is required"))
			return
		}
		b.eachSelf(ctx, w, pathID(r, "id"), func(k personalKey, id int64) (bool, error) {
			return b.store.UpdatePersonalMemory(ctx, k, id, in.Text)
		})
	}))

	mux.HandleFunc("DELETE /api/personal-memories/{id}", a(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		b.eachSelf(ctx, w, pathID(r, "id"), func(k personalKey, id int64) (bool, error) {
			return b.store.DeletePersonalMemory(ctx, k, id)
		})
	}))
}

// eachSelf runs a keyed write against each of the caller's own accounts until one of them owns
// that id. Somebody else's note is a 404 rather than a 403: an id that is not yours is missing,
// which is the boundary the organisation's own memory routes already keep.
func (b *Bot) eachSelf(ctx context.Context, w http.ResponseWriter, id int64, do func(personalKey, int64) (bool, error)) {
	if id == 0 {
		writeJSON(w, 404, map[string]any{"error": "no such note"})
		return
	}
	for _, self := range b.slackSelves(ctx, adminFromCtx(ctx)) {
		found, err := do(self.key, id)
		if err != nil {
			fail(w, err)
			return
		}
		if found {
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	writeJSON(w, 404, map[string]any{"error": "no such note"})
}
