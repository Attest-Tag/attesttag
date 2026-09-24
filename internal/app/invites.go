package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Getting somebody into the console.
//
// There are three ways in and they are all the same row in email_tokens, differing only in who
// the link names. An invitation can name a mailbox, in which case we mail it and only that
// mailbox may redeem it. It can name a Slack account, in which case the bot delivers it by
// direct message — the one case where we can put a link in the hands of the right person without
// knowing their address. Or it can name nobody, which is a share link: anybody holding it joins,
// bounded by a role, an expiry, a number of uses and — if its maker wants one — an email domain.
//
// The share link is the only one of the three that is bearer authority in the ordinary sense, so
// every bound it has is written down here rather than left to the screen that makes it.

// inviteReq is what the console posts to create an invitation or a share link. Which of the three
// it is depends on what it names, so there is one endpoint and one shape rather than three.
type inviteReq struct {
	Email       string `json:"email"`
	SlackUserID string `json:"slack_user_id"`
	Role        string `json:"role"`

	// Share links only.
	Share   bool   `json:"share"`
	Label   string `json:"label"`
	Domain  string `json:"domain"`
	MaxUses int    `json:"max_uses"`
	Days    int    `json:"days"`
}

const (
	maxInviteLinkDays = 30
	maxInviteLinkUses = 500
)

// invitee is who an invitation turned out to be for, once Slack has been asked.
type invitee struct {
	email   string
	slackID string
	name    string
	team    string
}

// resolveSlackInvitee finds a Slack user id among the workspaces this organisation has connected
// and reports what Slack says about the account.
//
// The console asks for an id without saying which workspace it is in, and an organisation may
// have several: the id is looked for in each until one answers. Asking only the first would
// refuse every member of the second workspace, and asking every workspace on the deployment
// would be asking other tenants about ids that mean nothing to them.
func (b *Bot) resolveSlackInvitee(ctx context.Context, orgID int64, id string) (*invitee, error) {
	id = strings.ToUpper(strings.TrimSpace(id))
	if !userIDRe.MatchString(id) {
		return nil, errors.New("that does not look like a Slack user id — they start with U or W, like U0123ABCDEF")
	}
	teams, err := b.store.Teams(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var connected bool
	for _, t := range teams {
		if t.Status != "active" {
			continue
		}
		connected = true
		sl, err := b.slacks.For(ctx, t.TeamID)
		if err != nil {
			slog.Warn("could not open a workspace to resolve an invitee", "team", t.TeamID, "err", err)
			continue
		}
		// Straight from Slack rather than the cache: whether an account is a bot, a guest or
		// deactivated decides whether we hand it console access, and a stale yes is the kind of
		// answer that ages badly.
		uf, err := sl.UserFacts(ctx, id)
		if err != nil {
			continue // not a member of this workspace; try the next one
		}
		switch {
		case uf.Deleted:
			return nil, errors.New("that Slack account is deactivated")
		case uf.Bot:
			return nil, errors.New("that is a bot account — invite the person who runs it instead")
		}
		return &invitee{email: normalEmail(uf.Email), slackID: id, name: sl.UserName(ctx, id), team: t.TeamID}, nil
	}
	if !connected {
		return nil, errors.New("no Slack workspace is connected to this organisation yet, so there is nobody to look that id up against")
	}
	return nil, errors.New("no Slack account with that id is in a workspace connected here — check the id, or invite them by email")
}

// dmInviteLink hands the link to a Slack account directly. chat.postMessage takes a user id as
// the channel and opens the IM itself, which is the same trick the approval cards use.
func (b *Bot) dmInviteLink(ctx context.Context, teamID string, to *invitee, by *AdminUser, link string) error {
	sl, err := b.slacks.For(ctx, teamID)
	if err != nil {
		return err
	}
	who := strings.TrimSpace(by.Name)
	if who == "" {
		who = by.Email
	}
	text := fmt.Sprintf("%s has invited you to the *%s* console on attest_tag.\n\n<%s|Open the invitation> — it is yours alone and expires in seven days.",
		who, by.OrgName, link)
	if _, err := sl.PostText(ctx, to.slackID, "", text); err != nil {
		return err
	}
	return nil
}

// createInvite is the whole of "invite somebody", shared by the console endpoint and the
// onboarding walk. It decides who the link is for, mints it, and tries to deliver it — and it
// returns the link whichever way delivery went, because a send that fails must not lose an
// invitation that is already written.
func (b *Bot) createInvite(r *http.Request, me *AdminUser, in inviteReq) (map[string]any, int, error) {
	ctx := r.Context()
	if in.Role == "" {
		in.Role = RoleViewer
	}
	// A role you hand out is one you hold. Checked before anything is written or sent, so a
	// refusal costs no mail and no rate-limit budget.
	if !canAssignRole(me.Permissions, in.Role, b.store.CustomRoleMap(ctx, me.OrgID)) {
		return nil, http.StatusForbidden, errors.New("You can only invite somebody to a role you hold yourself.")
	}
	if in.Share {
		return b.createInviteLink(r, me, in)
	}

	tok := EmailToken{Kind: TokenInvite, OrgID: me.OrgID, Role: in.Role, CreatedBy: me.ID, MaxUses: 1}
	var to invitee
	switch {
	case strings.TrimSpace(in.SlackUserID) != "":
		got, err := b.resolveSlackInvitee(ctx, me.OrgID, in.SlackUserID)
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
		to = *got
		// Bound to the address when Slack will tell us one, because that is what makes the link
		// useless to anybody it gets forwarded to. When the workspace withholds it — no
		// users:read.email — the direct message is the only binding there is, and a DM is
		// already delivery to exactly one person. Recorded either way, so the invitation can be
		// listed and revoked as the person it was meant for.
		tok.Email, tok.SlackUserID = to.email, to.slackID
	default:
		to.email = normalEmail(in.Email)
		if !looksLikeEmail(to.email) {
			return nil, http.StatusBadRequest, errors.New("that does not look like an email address or a Slack user id")
		}
		tok.Email = to.email
	}

	// Invitations are mail, or a direct message, from this deployment to any address given.
	// Bounded per organisation and per recipient, or an organisation founded a minute ago is a
	// relay. Keyed on whatever names the recipient, so a Slack id cannot be used to sidestep the
	// per-address limit.
	key := to.email
	if key == "" {
		key = to.slackID
	}
	if ok, d := invites.allow("to:"+key, invitesPerAddressADay, 24*time.Hour); !ok {
		return nil, http.StatusTooManyRequests, retryAfter(d)
	}
	if ok, d := invites.allow("org:"+strconv.FormatInt(me.OrgID, 10), invitesPerOrgPerHour, time.Hour); !ok {
		return nil, http.StatusTooManyRequests, retryAfter(d)
	}

	link, err := b.mintInvite(r, tok)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := map[string]any{"ok": true, "link": link, "to": to.displayName()}

	// Deliver it where we can. A Slack invitation goes by direct message even when we know the
	// address: it is the channel the sender chose, and it arrives where they already talk.
	switch {
	case to.slackID != "":
		if err := b.dmInviteLink(ctx, to.team, &to, me, link); err != nil {
			slog.Warn("could not DM an invitation", "to", to.slackID, "err", err)
			out["via"], out["delivered"] = "dm", false
			out["delivery_error"] = "Slack would not deliver the message (" + err.Error() +
				"). The Slack app may be missing the `im:write` and `chat:write` scopes — pass the link on yourself for now."
		} else {
			out["via"], out["delivered"] = "dm", true
		}
	default:
		err := b.mail.Send(ctx, Mail{
			To: to.email, Subject: "You have been invited to " + me.OrgName + " on attest_tag",
			Body: inviteEmail(me, to.email, link),
		})
		// A deployment with no mail key logs the message instead of sending it, and Send reports
		// no error for it — so delivery is only a promise worth making when there is a mailer
		// behind it. Otherwise this reads exactly like a bounce: here is the link, pass it on.
		out["via"], out["delivered"] = "email", err == nil && b.mail.Configured()
		switch {
		case err != nil:
			out["delivery_error"] = errText(err)
		case !b.mail.Configured():
			out["delivery_error"] = "This deployment has no email provider configured, so nothing was sent — pass this link on yourself."
		}
	}
	slog.Info("invitation created", "org", me.OrgID, "by", me.ID, "to", tok.Addressee(),
		"role", in.Role, "via", out["via"], "delivered", out["delivered"])
	b.audit(r, "member.invited", AuditEvent{TargetKind: "invite", TargetName: tok.Addressee(),
		Details: auditDetails(map[string]any{"role": in.Role, "via": out["via"], "delivered": out["delivered"]})})
	return out, http.StatusOK, nil
}

// createInviteLink mints a link addressed to nobody.
//
// Everything that makes this safe to hand around is decided here. The role is capped to what its
// maker holds, as every invitation is. The expiry and the use count are bounded so that "forever"
// and "unlimited" are choices somebody made rather than what you get by leaving a field blank.
// The domain, when given, is the difference between a link that anyone who finds it can redeem
// and one only your colleagues can — and it is enforced at redemption, not here.
func (b *Bot) createInviteLink(r *http.Request, me *AdminUser, in inviteReq) (map[string]any, int, error) {
	days := in.Days
	if days <= 0 {
		days = 7
	}
	if days > maxInviteLinkDays {
		days = maxInviteLinkDays
	}
	// The console sends 0 for "no limit", which is the natural thing for an empty field to mean
	// on that screen; the store wants it said explicitly.
	uses := in.MaxUses
	switch {
	case uses <= 0:
		uses = UsesUnlimited
	case uses > maxInviteLinkUses:
		uses = maxInviteLinkUses
	}
	domain := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(in.Domain, "@")))
	if domain != "" && !looksLikeDomain(domain) {
		return nil, http.StatusBadRequest, errors.New("that does not look like an email domain — give something like example.com, or leave it empty for anyone")
	}
	label, err := cleanName(nonEmpty(strings.TrimSpace(in.Label), "Shared invite link"), 60)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	// A share link is worth more than a single invitation, so it counts against the organisation
	// the same way — one tenant cannot mint a hundred of them in a minute.
	if ok, d := invites.allow("org:"+strconv.FormatInt(me.OrgID, 10), invitesPerOrgPerHour, time.Hour); !ok {
		return nil, http.StatusTooManyRequests, retryAfter(d)
	}
	link, err := b.mintInviteTTL(r, EmailToken{Kind: TokenInvite, OrgID: me.OrgID, Role: in.Role,
		CreatedBy: me.ID, MaxUses: uses, Label: label, Domain: domain},
		time.Duration(days)*24*time.Hour)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	slog.Info("invite link created", "org", me.OrgID, "by", me.ID, "role", in.Role,
		"days", days, "max_uses", uses, "domain", domain)
	// The terms of the link, never the link: it is a credential until it is spent.
	b.audit(r, "member.invite_link_created", AuditEvent{TargetKind: "invite", TargetName: label,
		Details: auditDetails(map[string]any{"role": in.Role, "days": days, "max_uses": uses, "domain": domain})})
	return map[string]any{"ok": true, "link": link, "via": "link", "delivered": false,
		"to": label}, http.StatusOK, nil
}

func (i invitee) displayName() string {
	switch {
	case i.name != "":
		return i.name
	case i.email != "":
		return i.email
	}
	return i.slackID
}

// mintInvite writes the row and returns the URL somebody can open.
func (b *Bot) mintInvite(r *http.Request, t EmailToken) (string, error) {
	return b.mintInviteTTL(r, t, inviteTTL2)
}

func (b *Bot) mintInviteTTL(r *http.Request, t EmailToken, ttl time.Duration) (string, error) {
	raw, err := b.store.NewEmailToken(r.Context(), t, ttl)
	if err != nil {
		return "", err
	}
	return b.baseURL(r) + "/invite/" + raw, nil
}

// looksLikeDomain is deliberately loose: it is a guard against a typo becoming a link nobody can
// redeem, not an attempt to decide what is a real domain.
func looksLikeDomain(d string) bool {
	if len(d) < 4 || len(d) > 253 || strings.ContainsAny(d, " @/\\") {
		return false
	}
	dot := strings.LastIndex(d, ".")
	return dot > 0 && dot < len(d)-1
}

// rateLimited carries a limiter's wait back to the handler, so a throttled invitation answers
// with the same Retry-After header every other throttled route sends rather than a bare 429.
type rateLimited struct{ d time.Duration }

func (e rateLimited) Error() string {
	return fmt.Sprintf("Too many invitations at once. Try again in %d minutes.", int(e.d.Minutes())+1)
}

func retryAfter(d time.Duration) error { return rateLimited{d} }

// describeInvite is one pending invitation as the console lists it. The Slack id is resolved to a
// name here rather than in the browser: the console has no Slack token, and a list of raw U… ids
// is a list nobody can act on.
func (b *Bot) describeInvite(ctx context.Context, orgID int64, t EmailToken) map[string]any {
	kind := "email"
	who := t.Email
	switch {
	case t.Shareable():
		kind, who = "link", nonEmpty(t.Label, "Shared invite link")
	case t.SlackUserID != "":
		kind = "slack"
		if sl := b.slacks.AnyFor(ctx, orgID); sl != nil {
			if name := sl.UserName(ctx, t.SlackUserID); name != "" && name != t.SlackUserID {
				who = name
			}
		}
		if who == "" {
			who = t.SlackUserID
		}
	}
	by := ""
	if t.CreatedBy != 0 {
		if u, _ := b.store.User(ctx, t.CreatedBy); u != nil {
			by = nonEmpty(u.Name, u.Email)
		}
	}
	return map[string]any{
		"id": t.ID, "kind": kind, "who": who, "email": t.Email, "slack_user_id": t.SlackUserID,
		"role": t.Role, "label": t.Label, "domain": t.Domain, "max_uses": t.MaxUses,
		"uses": t.Uses, "expires_at": t.ExpiresAt, "created_at": t.CreatedAt, "invited_by": by,
	}
}

// inviteRefusal says why a link will not be redeemed by the person holding it, in terms of what
// they can do about it. A link addressed to a mailbox names it — they need to know which account
// to sign in as. A link narrowed to a domain names the domain, which is not a secret to anyone
// who has the link anyway.
func inviteRefusal(t *EmailToken) string {
	if t.Email != "" {
		return "That invitation was sent to " + t.Email + ". Sign in as that account to accept it, or ask for a new invitation."
	}
	if t.Domain != "" {
		return "That link only accepts " + t.Domain + " addresses. Use your " + t.Domain + " address, or ask for an invitation of your own."
	}
	return "That invitation cannot be accepted by this account. Ask for a new one."
}
