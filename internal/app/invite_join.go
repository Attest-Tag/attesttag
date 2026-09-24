package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
)

// Joining somebody else's organisation when you already have one of your own.
//
// This is the ordinary ending of the second person from one company signing up rather than being
// invited: they founded an organisation of nobody, found the workspace already taken, and are
// now holding an invitation to the real one. Accepting used to add the second membership and
// leave the first sitting in the switcher forever — an empty organisation with their name on it
// that they would have to remember never to open.
//
// So the join asks. It offers to leave the one they founded, and only when leaving it is
// obviously right: they are its only member and nothing has ever been put in it. Leaving detaches
// them from it and nothing more — the organisation and every row in it stay exactly where they
// are. Nothing here deletes anything, and their account is untouched: it is the same login, the
// same password, and after this it opens the organisation that invited them.

// ownOrg is the organisation the session is currently acting in, and whether this join can offer
// to leave it behind.
type ownOrg struct {
	// The organisation as the outside names it; the serial stays inside (see Org.PublicID).
	ID   string `json:"id"`
	Name string `json:"name"`
	// True when leaving costs nothing: sole member, nothing set up, nothing pending.
	Leavable bool `json:"leavable"`
	// Why not, when not — so the screen can be quiet rather than mysterious.
	Reason string `json:"reason,omitempty"`
	// The join key the leave itself needs. Unexported, so it cannot be marshalled by accident.
	orgID int64
}

// leavableOwnOrg decides whether to offer it. Every one of these is a reason somebody else would
// lose something, so the answer is no unless all of them say otherwise.
func (b *Bot) leavableOwnOrg(r *http.Request, me *AdminUser, joining int64) *ownOrg {
	ctx := r.Context()
	org, err := b.store.Org(ctx, me.OrgID)
	if err != nil || org == nil {
		return nil
	}
	own := &ownOrg{ID: org.PublicID, Name: org.Name, orgID: org.ID}
	if org.ID == joining {
		return nil // already there; there is nothing to leave
	}
	members, err := b.store.MembersOf(ctx, org.ID)
	if err != nil {
		return own
	}
	if len(members) > 1 {
		own.Reason = "Other people are in it."
		return own
	}
	if invites, err := b.store.PendingInvitesFor(ctx, org.ID); err == nil && len(invites) > 0 {
		own.Reason = "It has invitations out."
		return own
	}
	// "Nothing in it" is asked of the same counts the console's own front page reports, so the
	// two cannot come to disagree about whether an organisation is empty.
	o := b.store.OverviewStats(ctx, org.ID)
	if o.Teams > 0 || o.Docs > 0 || o.Bundles > 0 || o.Connections > 0 || o.Routines > 0 || o.Memories > 0 {
		own.Reason = "It has things set up in it."
		return own
	}
	own.Leavable = true
	return own
}

// handleInvitePreview is what the join screen reads before it asks anything: which organisation
// the invitation is for, and what leaving your own would mean. It spends nothing — the token is
// peeked at, never taken — so opening the screen twice does not burn the invitation.
func (b *Bot) handleInvitePreview(w http.ResponseWriter, r *http.Request) {
	me := adminFromCtx(r.Context())
	c, err := r.Cookie(inviteCookie)
	if err != nil || c.Value == "" {
		writeJSON(w, 400, map[string]any{"error": "That invitation link has expired. Ask for a new one."})
		return
	}
	t, err := b.store.PeekEmailToken(r.Context(), TokenInvite, c.Value)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	acct, _ := b.store.User(r.Context(), me.ID)
	if acct == nil {
		writeJSON(w, 401, map[string]any{"error": "sign in required"})
		return
	}
	// The same question accepting asks, so the screen says no before the button is pressed
	// rather than after. A domain link that only wants the address proved by our mail is not a
	// no to this person but a step they can take from here, so it is answered as one: the screen
	// offers to send the mail instead of showing an error with no way on.
	if why, unproven := invitationRefusal(t, acct); unproven {
		writeJSON(w, 200, map[string]any{"confirm_email": true, "blocked": why, "email": acct.Email})
		return
	} else if why != "" {
		writeJSON(w, 403, map[string]any{"error": why})
		return
	}
	org, _ := b.store.Org(r.Context(), t.OrgID)
	name, public := "", ""
	if org != nil {
		name, public = org.Name, org.PublicID
	}
	writeJSON(w, 200, map[string]any{"org": map[string]any{"id": public, "name": name},
		"role": t.Role, "email": t.Email, "shared": t.Shareable(),
		"own": b.leavableOwnOrg(r, me, t.OrgID)})
}

// acceptInvitation joins an account that already exists to the organisation an invitation link
// names. It is the one set of terms for every way such an account can be holding a link — the join
// screen, and a Slack or Microsoft sign-in made while the link's cookie is still in the browser —
// because three copies drifted: a domain-limited link asked for a confirmed address on the join
// screen and nowhere else, so an account whose address nobody had confirmed signed in with Slack
// while holding one and was let in.
//
// The link is read before it is spent, so a refusal leaves it whole for the person it was meant
// for. The status says which kind of no it was: 403 when the link is not this account's to redeem
// as things stand, which its holder may be able to change; 400 when the link itself is gone —
// expired, withdrawn, used up, or no longer its maker's to give.
func (b *Bot) acceptInvitation(r *http.Request, acct *User, raw string) (*EmailToken, int, error) {
	ctx := r.Context()
	t, err := b.store.PeekEmailToken(ctx, TokenInvite, raw)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if why, _ := invitationRefusal(t, acct); why != "" {
		slog.Warn("invitation refused", "user", acct.ID, "org", t.OrgID, "invited", t.Addressee(), "why", why)
		return t, http.StatusForbidden, errors.New(why)
	}
	if t, err = b.store.TakeEmailToken(ctx, TokenInvite, raw); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if err := b.store.AddMembership(ctx, acct.ID, t.OrgID, t.Role, t.CreatedBy); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	slog.Info("invitation accepted", "user", acct.ID, "org", t.OrgID)
	// Recorded in the organisation being joined. On the join screen the session says who; a
	// sign-in has no session yet, so the account does.
	e := AuditEvent{OrgID: t.OrgID, TargetKind: "member", TargetID: acct.PublicID, TargetName: acct.Email,
		Details: auditDetails(map[string]any{"role": t.Role, "via": "invitation"})}
	if adminFromCtx(ctx) == nil {
		e.ActorID, e.ActorPublic, e.ActorEmail, e.ActorName = acct.ID, acct.PublicID, acct.Email, acct.Name
	}
	b.audit(r, "member.joined", e)
	return t, http.StatusOK, nil
}

// invitationRefusal says why this account may not redeem this invitation as things stand, or ""
// when it may. unproven is a refusal only for want of an address proved the way a domain link
// asks, which a confirmation mail puts right.
func invitationRefusal(t *EmailToken, acct *User) (why string, unproven bool) {
	// An invitation addressed to a mailbox belongs to that mailbox. Links get forwarded and sit in
	// inboxes and chat logs, so holding one is not being the person it was sent to.
	if !t.AcceptsEmail(acct.Email) {
		return inviteRefusal(t), false
	}
	// A domain link asks only that the address be in its domain, and the address on an account may
	// have been typed at open signup and never proved. The link is meant to admit colleagues who
	// can show they are, so the address has to have been proved — by something its redeemer could
	// not have arranged (confirmedForDomainLink).
	if t.domainLink() && !confirmedForDomainLink(acct, t.OrgID) {
		return "This link is limited to the " + t.Domain + " domain and needs your address confirmed by a link we email you. " +
			"Send one, open it, then accept.", true
	}
	return "", false
}

// confirmedForDomainLink says whether this account's address was confirmed in a way a
// domain-limited link of org accepts: a link we mailed to it; single sign-on, whose identity
// provider speaks for the domain its organisation proved by DNS, which is the address's own; or an
// invitation from org itself, which could have invited the address anyway.
//
// Not an invitation from anywhere else: its link goes back to whoever sent it, so anybody who
// founds an organisation can invite any address and redeem the link themselves. And not Slack's
// word, which is whatever the workspace's own admin or SAML IdP says. Either one is still a
// confirmed address everywhere else, where the question is only whether somebody stands behind it.
//
// An address confirmed before the source was recorded ("") is accepted, by decision (migration
// 0026): nothing says how it was confirmed, and those accounts keep working as they did.
func confirmedForDomainLink(u *User, org int64) bool {
	if !u.EmailVerified {
		return false
	}
	switch u.emailVerifiedBy {
	case emailByMail, emailBySSO, "":
		return true
	case emailByInvite:
		return u.emailVerifiedOrg == org
	}
	return false
}

// joinOnSignIn redeems the invitation a Slack or Microsoft sign-in arrived holding, for an account
// that already exists, and reports whether the sign-in should finish on the join screen rather
// than where it would have gone. A refusal leaves the link and its cookie where they are, and the
// join screen reads them back and says why — the way a password sign-in holding the same link
// already ends there. A link that is gone is let go of: there is nothing on that screen to act on.
//
// Only on a sign-in that is whole, though. An account with a second factor has proved nothing yet
// but its Slack or Microsoft session, and a membership made on that alone stayed whether or not
// the code ever came. Its invitation waits in the cookie instead (waiting), and the code takes it
// from there (handleTwoFactorLogin).
func (b *Bot) joinOnSignIn(w http.ResponseWriter, r *http.Request, acct *User, invite string) (joinScreen, waiting bool, err error) {
	e, err := b.store.TOTPEnrolment(r.Context(), acct.ID)
	if err != nil {
		return false, false, err
	}
	if e != nil && e.Confirmed {
		return false, true, nil
	}
	_, status, aerr := b.acceptInvitation(r, acct, invite)
	if aerr != nil && status == http.StatusForbidden {
		return true, false, nil
	}
	if aerr != nil {
		slog.Warn("invitation not accepted on sign-in", "user", acct.ID, "err", aerr)
	}
	http.SetCookie(w, &http.Cookie{Name: inviteCookie, Value: "", Path: "/", MaxAge: -1})
	return false, false, nil
}

// inNoOrganisation says whether an account belongs to no organisation at all. The only place such
// an account can sign in to is the organisation an invitation it holds is for.
func (b *Bot) inNoOrganisation(ctx context.Context, userID int64) bool {
	ms, err := b.store.MembershipsFor(ctx, userID)
	return err == nil && len(ms) == 0
}

// leaveOwnOrg detaches somebody from the organisation they founded and points their live session
// at the one they have just joined. The membership row goes; the organisation and everything in
// it stay. Re-checked here rather than trusted from the request: the preview that offered this
// was a different request, and what it saw could have changed since.
func (b *Bot) leaveOwnOrg(w http.ResponseWriter, r *http.Request, me *AdminUser, joined int64) {
	own := b.leavableOwnOrg(r, me, joined)
	if own == nil || !own.Leavable {
		return
	}
	if err := b.store.RemoveMember(r.Context(), me.ID, own.orgID); err != nil {
		slog.Warn("could not leave the organisation founded on sign-up", "user", me.ID, "org", own.ID, "err", err)
		return
	}
	// The session still names the organisation they have just left, and a session in an
	// organisation you are no longer a member of authenticates as nobody: move it, or the next
	// page load signs them out.
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := b.store.SetSessionOrg(r.Context(), c.Value, joined); err != nil {
			slog.Warn("could not move the session to the joined organisation", "user", me.ID, "err", err)
		}
	}
	slog.Info("left the organisation founded on sign-up", "user", me.ID, "org", own.ID, "joined", joined)
}
