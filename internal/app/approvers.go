package app

import (
	"context"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Who can approve an access request, and what they can approve.
//
// An approver is not a name on a list. They hold a *role*, and the role lists what its members
// may grant from — whole bundles, and connections picked on their own, the same two shapes a
// channel's access takes — so "Approver" and "Super admin" are two tiers with different reach,
// and adding somebody to a tier is one edit rather than a per-person allowlist. Rank orders the
// tiers: a higher rank covers everything a lower one can.
//
// The roles are configuration, never something read out of a message. A turn can *hint* at who to
// ask — the requester tagged somebody — but the hint is intersected with the configured members,
// so text that nominates its own approver gains nothing. Who was tagged is deterministic input,
// whether the ask is an access request is the model's judgement, and who may say yes is neither.

// mentionIDRe reads ids out of a message. mentionRe erases mentions before the model sees them;
// this runs first, on the raw text. It also matches the <@U123|name> form, which mentionRe misses
// entirely. A Teams member's id is their Entra object id, which a Teams mention arrives as.
var mentionIDRe = regexp.MustCompile(`<@([UW][A-Z0-9]+|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:\|[^>]*)?>`)

// mentionedUsers returns the ids @-mentioned in a message, in order, once each.
func mentionedUsers(text string) []string {
	out := []string{}
	for _, m := range mentionIDRe.FindAllStringSubmatch(text, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

var userIDRe = regexp.MustCompile(`^[UW][A-Z0-9]{4,}$`)

// parseApprovers splits the tolerant list form — commas, semicolons, whitespace — into user ids
// (a Slack user id, or a Teams member's Entra object id) and email addresses. Anything that is
// neither comes back in bad, so a typo is reported rather than quietly shrinking the set of people
// who can approve.
func parseApprovers(s string) (ids, emails, bad []string) {
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		// People paste the mention rather than the raw id, and both should work.
		f = strings.TrimSuffix(strings.TrimPrefix(f, "<@"), ">")
		if i := strings.Index(f, "|"); i >= 0 {
			f = f[:i]
		}
		switch {
		// Before the Slack case, which upper-cases: an object id is lower case, and one
		// upper-cased would never match the person Teams says it is.
		case isEntraObjectID(strings.ToLower(f)):
			ids = appendOnce(ids, strings.ToLower(f))
		case userIDRe.MatchString(strings.ToUpper(f)):
			ids = appendOnce(ids, strings.ToUpper(f))
		case strings.Contains(f, "@") && strings.Contains(f[strings.Index(f, "@"):], "."):
			emails = appendOnce(emails, strings.ToLower(f))
		case f != "":
			bad = append(bad, f)
		}
	}
	return ids, emails, bad
}

func appendOnce(s []string, v string) []string {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}

// tier is one approval role with its members resolved to Slack ids and the access it grants.
// Everything the routing needs, in the order tiers should be tried.
type tier struct {
	Role    ApprovalRole
	Members []string // resolved Slack user ids
	Access  *Access  // what it can reach, grant-capable connections only
}

// tiers loads the approval roles, resolving member emails to ids and each role's grants to the
// access they give. A role with no grant-capable connection is still returned — its members can
// approve nothing, and saying so is better than pretending the role does not exist.
//
// Approvers are configured for the account but Slack user ids are per workspace, so an entry
// written as an email is resolved against sl — the workspace the request came from. A nil sl
// (a console read that names no workspace) keeps the id entries and drops the email ones.
func (a *Agent) tiers(ctx context.Context, orgID int64, sl *Chat) []tier {
	roles, err := a.store.ApprovalRoles(ctx, orgID)
	if err != nil {
		slog.Warn("read approval roles", "err", err)
		return nil
	}
	// An email approver is only ever resolved in one workspace, and only when there is exactly
	// one to choose from. With several connected, the directory of any one of them — a partner's,
	// a customer's — could put the approver's address on an account it controls, and that account
	// would then approve grants that spend this organisation's credentials. Entries added from
	// the console are resolved there, into a workspace the admin chose, and carry it.
	var single *Chat
	if sl != nil {
		if teams, _ := a.store.ActiveTeams(ctx); len(teams) > 0 {
			n := 0
			for _, tm := range teams {
				if tm.OrgID == orgID {
					n++
				}
			}
			if n == 1 {
				single = sl
			}
		}
	}
	out := make([]tier, 0, len(roles))
	for _, r := range roles {
		t := tier{Role: r}
		for _, m := range r.Resolved {
			if m.SlackUserID != "" {
				// Resolved into a workspace: a person there, and nowhere else.
				if sl == nil || m.TeamID == sl.TeamID {
					t.Members = appendOnce(t.Members, m.SlackUserID)
				}
				continue
			}
			ids, emails, bad := parseApprovers(m.Ref)
			for _, b := range bad {
				slog.Warn("ignoring an unreadable approver", "role", r.Name, "entry", b)
			}
			for _, id := range ids {
				t.Members = appendOnce(t.Members, id)
			}
			for _, e := range emails {
				if single == nil {
					if sl != nil {
						slog.Warn("an approver written as an email is not tied to a workspace and the organisation has several; re-add them from the console", "role", r.Name, "email", e)
					}
					continue
				}
				id, err := single.UserByEmail(ctx, e)
				if err != nil {
					slog.Warn("could not resolve an approver's email", "role", r.Name, "email", e, "err", err)
					continue
				}
				t.Members = appendOnce(t.Members, id)
			}
		}
		t.Access = a.roleGrants(ctx, orgID, r)
		out = append(out, t)
	}
	// Lowest rank first, so "the tier that covers this" is the least powerful one that does.
	slices.SortStableFunc(out, func(x, y tier) int { return x.Role.Rank - y.Role.Rank })
	return out
}

// roleGrants is the access one tier grants: every active connection in its bundles, and every one
// picked on its own — and nothing else. Being listed on the tier is the whole authorisation; an
// admin who wants a tier to reach less than a bundle lists the connections instead.
func (a *Agent) roleGrants(ctx context.Context, orgID int64, role ApprovalRole) *Access {
	return grantRules(ctx, a.store, orgID, role)
}

// grantRules does the work of roleGrants against a store, so the console can count a tier's
// grants the same way routing sees them. A connection listed both ways — in a bundle the tier
// has and on its own — is one rule; one that is inactive is none.
func grantRules(ctx context.Context, st *Store, orgID int64, role ApprovalRole) *Access {
	acc := &Access{ToolPacks: map[string]bool{}}
	seen := map[int64]bool{}
	add := func(cn *Connection, direct bool) {
		if cn == nil || seen[cn.ID] || cn.Status != "active" {
			return
		}
		seen[cn.ID] = true
		acc.Rules = append(acc.Rules, Rule{Conn: cn, Rank: 1, Notes: cn.Notes, Direct: direct})
	}
	for _, id := range role.BundleIDs {
		bl, err := st.Bundle(ctx, orgID, id)
		if err != nil || bl == nil {
			continue
		}
		for _, cn := range bl.Connections {
			add(cn, false)
		}
	}
	for _, id := range role.ConnectionIDs {
		cn, err := st.Connection(ctx, orgID, id)
		if err != nil {
			continue
		}
		add(cn, true)
	}
	return acc
}

// covers reports whether this tier can run every one of these calls.
func (a *Agent) covers(t tier, calls []grantStep) bool {
	if t.Access == nil || len(t.Access.Rules) == 0 {
		return false
	}
	for _, s := range calls {
		// A fix job reaches no host: what it does is branch a repository and open a draft pull
		// request on it. So the tier that may approve one is the tier that reaches that
		// repository — the same grant that would let it read the code.
		if s.Job != nil {
			if !tierHasRepo(t, s.Job.Repo) {
				return false
			}
			continue
		}
		// Plain parse, not parseGrantURL: routing asks "can this tier reach it", and mixing the
		// SSRF check in here would report a blocked host as "nobody can approve that". The guard
		// runs once before anything is recorded, and again in the proxy before anything is sent.
		u, err := url.Parse(strings.TrimSpace(s.URL))
		if err != nil {
			return false
		}
		conn, why := a.proxy.Match(t.Access, strings.ToUpper(s.Method), u)
		if why != "" || conn == nil {
			return false
		}
	}
	return true
}

// tierHasRepo reports whether one of a tier's own connections is this repository. Named rather
// than matched by host: every GitHub connection shares api.github.com, and "can reach GitHub" is
// not the same permission as "may open a pull request on this repository".
func tierHasRepo(t tier, repo string) bool {
	if t.Access == nil || repo == "" {
		return false
	}
	for _, r := range t.Access.Rules {
		if r.Conn != nil && strings.EqualFold(r.Conn.Repo, repo) {
			return true
		}
	}
	return false
}

// routeTo picks the tier that will be asked. The requester's tag decides between tiers that can
// all do the job; when the tagged person's tier cannot, it escalates to one that can, because a
// dead end helps nobody. Among tiers that cover it, the least powerful wins.
func (a *Agent) routeTo(ctx context.Context, c *Call, calls []grantStep) (*tier, []string) {
	ts := a.tiers(ctx, c.OrgID, c.SL)
	var covering []tier
	for _, t := range ts {
		if a.covers(t, calls) {
			covering = append(covering, t)
		}
	}
	if len(covering) == 0 {
		return nil, nil
	}
	// Prefer a tier the requester actually tagged, if it can do the whole thing.
	for _, t := range covering {
		for _, id := range c.Tagged {
			if slices.Contains(t.Members, id) && a.answerable(c, t, id) {
				return &t, []string{id}
			}
		}
	}
	// Otherwise the least powerful tier that covers it, and everyone in it.
	for i := range covering {
		if m := a.answerableMembers(c, covering[i]); len(m) > 0 {
			return &covering[i], m
		}
	}
	return nil, nil
}

func (a *Agent) answerable(c *Call, t tier, id string) bool {
	return c.allowSelfApprove || id != c.UserID
}

func (a *Agent) answerableMembers(c *Call, t tier) []string {
	out := []string{}
	for _, id := range t.Members {
		if a.answerable(c, t, id) {
			out = append(out, id)
		}
	}
	return out
}

// rankOf is the highest tier this person holds, and -1 when they hold none.
func (a *Agent) rankOf(ctx context.Context, orgID int64, sl *Chat, user string) int {
	best := -1
	for _, t := range a.tiers(ctx, orgID, sl) {
		if slices.Contains(t.Members, user) && t.Role.Rank > best {
			best = t.Role.Rank
		}
	}
	return best
}

// anyApprover reports whether approvals are configured at all. Empty means the feature is off:
// nothing is asked and nothing is granted.
func (a *Agent) anyApprover(ctx context.Context, orgID int64) bool {
	return a.approverIn(ctx, orgID, a.slacks.AnyFor(ctx, orgID))
}

// approverIn is the same question asked from inside a turn, which already knows its workspace and
// need not go looking for one. toolsFor asks it: an organisation with no tier that both has
// members and reaches something cannot approve anything, requestAccess says exactly that and
// stops, and advertising the tool there spends ~270 tokens a round on a call whose only possible
// outcome is that sentence.
func (a *Agent) approverIn(ctx context.Context, orgID int64, sl *Chat) bool {
	for _, t := range a.tiers(ctx, orgID, sl) {
		if len(t.Members) > 0 && len(t.Access.Rules) > 0 {
			return true
		}
	}
	return false
}

// describeApprovers is the startup line: the tiers that can actually approve something, or a note
// that none can.
func describeApprovers(ctx context.Context, a *Agent, orgID int64) string {
	if a == nil || a.store == nil {
		return "none configured"
	}
	parts := []string{}
	for _, t := range a.tiers(ctx, orgID, a.slacks.AnyFor(ctx, orgID)) {
		if len(t.Members) == 0 || len(t.Access.Rules) == 0 {
			continue
		}
		parts = append(parts, t.Role.Name)
	}
	if len(parts) == 0 {
		return "none configured"
	}
	return strings.Join(parts, ",")
}

// checkDMScope reports installs that cannot deliver approval cards. The card is a DM, and
// opening one with somebody who has never messaged the bot needs im:write. Without it every
// card silently fails to deliver, and the first sign is a requester saying nobody ever
// answered. Scopes are granted per install, so this reads what each workspace actually gave.
func (b *Bot) checkDMScope(ctx context.Context) {
	teams, _ := b.store.ActiveTeams(ctx)
	for _, t := range teams {
		if t.DMScope || !b.agent.anyApprover(ctx, t.OrgID) {
			continue
		}
		slog.Error("approval roles are configured but this workspace's install is missing the im:write bot scope, "+
			"so approval cards cannot be delivered there: every access request will fail to reach anyone. "+
			"Reconnect the workspace from /admin/workspaces to grant it", "team", t.TeamID, "name", t.Name)
	}
}
