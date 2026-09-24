package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// A share link is bearer authority in a way a personal invitation is not: whoever holds it joins.
// Everything that bounds it — the use count, the expiry, the domain — is therefore load-bearing,
// and each of them is checked here rather than trusted to the screen that offers them.

// The cap is the point of a capped link: it lets exactly that many people in and then stops,
// even though nothing about the link itself has changed.
func TestShareLinkStopsAtItsCap(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	orgID, byID, _ := seedOrg(t, st, RoleAdmin)

	raw, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Role: RoleViewer, CreatedBy: byID, MaxUses: 2, Label: "Engineering"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		got, err := st.TakeEmailToken(ctx, TokenInvite, raw)
		if err != nil {
			t.Fatalf("use %d was refused: %v", i, err)
		}
		if got.Uses != i {
			t.Errorf("use %d counted as %d", i, got.Uses)
		}
	}
	if _, err := st.TakeEmailToken(ctx, TokenInvite, raw); err == nil {
		t.Error("a link capped at two uses let a third person in")
	}
	// And it stops being offered as pending once it is spent.
	pending, _ := st.PendingInvitesFor(ctx, orgID)
	if len(pending) != 0 {
		t.Errorf("a spent link is still listed: %+v", pending)
	}
}

// An uncapped link keeps working, which is the whole of what "no limit" has to mean.
func TestUncappedShareLinkKeepsWorking(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	orgID, byID, _ := seedOrg(t, st, RoleAdmin)

	raw, err := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Role: RoleViewer, CreatedBy: byID, MaxUses: UsesUnlimited}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := st.TakeEmailToken(ctx, TokenInvite, raw); err != nil {
			t.Fatalf("use %d of an uncapped link was refused: %v", i+1, err)
		}
	}
	// Revoking is what ends it.
	pending, _ := st.PendingInvitesFor(ctx, orgID)
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want the one live link", pending)
	}
	if err := st.RevokeInviteByID(ctx, orgID, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TakeEmailToken(ctx, TokenInvite, raw); err == nil {
		t.Error("a revoked share link still let somebody in")
	}
}

// The zero value of MaxUses has to be the safe one. A caller that never thought about the field —
// every existing one, and the verification and reset links — must get a single-use link rather
// than one anybody can redeem forever.
func TestInviteDefaultsToSingleUse(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	orgID, byID, _ := seedOrg(t, st, RoleAdmin)

	raw, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "one@example.com", Role: RoleViewer, CreatedBy: byID}, time.Hour)
	if _, err := st.TakeEmailToken(ctx, TokenInvite, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TakeEmailToken(ctx, TokenInvite, raw); err == nil {
		t.Error("an invitation with no use count set was redeemable twice")
	}
	// The same for a link of another kind, whatever it asks for.
	reset, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenReset, UserID: byID,
		MaxUses: UsesUnlimited}, time.Hour)
	if _, err := st.TakeEmailToken(ctx, TokenReset, reset); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TakeEmailToken(ctx, TokenReset, reset); err == nil {
		t.Error("a password reset link asked for no limit and was given one")
	}
}

// Who a link will accept. This is the one guard a share link handed around in public still has,
// and three separate sign-in paths ask it, so it is answered in one place.
func TestWhoALinkAccepts(t *testing.T) {
	bound := EmailToken{Kind: TokenInvite, Email: "her@example.com"}
	if !bound.AcceptsEmail("HER@Example.com ") {
		t.Error("an invitation refused the address it was sent to over case and spacing")
	}
	if bound.AcceptsEmail("him@example.com") {
		t.Error("an invitation addressed to one mailbox was redeemable by another")
	}
	if bound.Shareable() {
		t.Error("an addressed invitation reported itself as a share link")
	}

	open := EmailToken{Kind: TokenInvite}
	if !open.Shareable() || !open.AcceptsEmail("anyone@anywhere.test") {
		t.Error("an open share link refused somebody")
	}

	walled := EmailToken{Kind: TokenInvite, Domain: "example.com"}
	if !walled.AcceptsEmail("new.person@EXAMPLE.com") {
		t.Error("a domain-restricted link refused an address in its domain")
	}
	for _, bad := range []string{"person@other.com", "person@notexample.com", "example.com", ""} {
		if walled.AcceptsEmail(bad) {
			t.Errorf("a link restricted to example.com accepted %q", bad)
		}
	}

	// An invitation to a Slack account carries the address Slack gave us, and is bound by it.
	slackBound := EmailToken{Kind: TokenInvite, Email: "them@example.com", SlackUserID: "U0123ABCDE"}
	if slackBound.Shareable() {
		t.Error("an invitation to a Slack account reported itself as a share link")
	}
	if slackBound.AcceptsEmail("someone@example.com") {
		t.Error("an invitation to a Slack account was redeemable by another address")
	}

	// One whose workspace withholds email is bound by the direct message alone. It must not
	// quietly become a share link — nothing else in the system may treat it as one.
	slackOnly := EmailToken{Kind: TokenInvite, SlackUserID: "U0123ABCDE"}
	if slackOnly.Shareable() {
		t.Error("an invitation with no address became a link anybody could redeem")
	}
}

// Inviting the same person twice replaces the first link. Making a second share link must not:
// they are different links to different people, and superseding on "addressed to nobody" would
// have every new one silently kill the last.
func TestSupersedingOnlyAppliesToAPerson(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	orgID, byID, _ := seedOrg(t, st, RoleAdmin)

	first, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "again@example.com", Role: RoleViewer, CreatedBy: byID}, time.Hour)
	second, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "again@example.com", Role: RoleEditor, CreatedBy: byID}, time.Hour)
	if _, err := st.TakeEmailToken(ctx, TokenInvite, first); err == nil {
		t.Error("re-inviting somebody left the first link live")
	}
	if _, err := st.TakeEmailToken(ctx, TokenInvite, second); err != nil {
		t.Errorf("the invitation just sent was refused: %v", err)
	}

	// The same for two invitations to one Slack account.
	slackA, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		SlackUserID: "U0123ABCDE", Role: RoleViewer, CreatedBy: byID}, time.Hour)
	slackB, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		SlackUserID: "U0123ABCDE", Role: RoleViewer, CreatedBy: byID}, time.Hour)
	if _, err := st.TakeEmailToken(ctx, TokenInvite, slackA); err == nil {
		t.Error("re-inviting a Slack account left the first link live")
	}
	if _, err := st.TakeEmailToken(ctx, TokenInvite, slackB); err != nil {
		t.Errorf("the Slack invitation just sent was refused: %v", err)
	}

	// Two share links coexist.
	linkA, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Role: RoleViewer, CreatedBy: byID, MaxUses: UsesUnlimited, Label: "Design"}, time.Hour)
	linkB, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Role: RoleViewer, CreatedBy: byID, MaxUses: UsesUnlimited, Label: "Support"}, time.Hour)
	for name, raw := range map[string]string{"the first": linkA, "the second": linkB} {
		if _, err := st.TakeEmailToken(ctx, TokenInvite, raw); err != nil {
			t.Errorf("%s share link was killed by the other: %v", name, err)
		}
	}
}

// Revoking is keyed on the row, so one tenant cannot cancel another's by guessing a number.
func TestRevokeByIDIsPerOrganisation(t *testing.T) {
	st, ctx := testStore(t), context.Background()
	orgID, byID, _ := seedOrg(t, st, RoleAdmin)
	otherUser, _ := st.CreateUser(ctx, "other@example.com", "Other", "")
	otherOrg, _ := st.CreateOrg(ctx, "Other Org", otherUser.ID)

	raw, _ := st.NewEmailToken(ctx, EmailToken{Kind: TokenInvite, OrgID: orgID,
		Email: "mine@example.com", Role: RoleViewer, CreatedBy: byID}, time.Hour)
	pending, _ := st.PendingInvitesFor(ctx, orgID)
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	if err := st.RevokeInviteByID(ctx, otherOrg.ID, pending[0].ID); err == nil {
		t.Error("another organisation revoked our invitation")
	}
	if _, err := st.PeekEmailToken(ctx, TokenInvite, raw); err != nil {
		t.Errorf("the invitation was cancelled anyway: %v", err)
	}
	if err := st.RevokeInviteByID(ctx, orgID, pending[0].ID); err != nil {
		t.Fatalf("revoking our own: %v", err)
	}
	if _, err := st.TakeEmailToken(ctx, TokenInvite, raw); err == nil {
		t.Error("a revoked invitation was still redeemable")
	}
	// Revoking twice says so rather than reporting a second success.
	if err := st.RevokeInviteByID(ctx, orgID, pending[0].ID); err == nil {
		t.Error("revoking an already-revoked invitation reported success")
	}
}

// A share link's bounds are decided where it is made, not where it is offered. Anything the
// console can ask for that would outlive the policy is clamped here.
func TestShareLinkBoundsAreClamped(t *testing.T) {
	if !looksLikeDomain("example.com") || !looksLikeDomain("mail.example.co.uk") {
		t.Error("a real domain was rejected")
	}
	for _, bad := range []string{"", "example", "@example.com", "exa mple.com", "example.", ".com", "a/b.com"} {
		if looksLikeDomain(bad) {
			t.Errorf("%q was accepted as an email domain", bad)
		}
	}
}

// Inviting a Slack account, which is the one case where a link can be put into the right hands
// without knowing an address. The id is resolved against the organisation's own workspaces, the
// bot delivers the link by direct message, and what Slack says about the account decides whether
// there is anybody to invite at all.
func TestInvitingASlackAccount(t *testing.T) {
	b, mux, st := installTestBot(t)
	ctx := context.Background()

	var dms []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/chat.postMessage":
			r.ParseForm()
			dms = append(dms, r.Form.Get("channel")+"|"+r.Form.Get("text"))
			w.Write([]byte(`{"ok":true,"channel":"D1","ts":"1.0"}`))
		case "/api/users.info":
			r.ParseForm()
			switch r.Form.Get("user") {
			case "U01REAL2345":
				w.Write([]byte(`{"ok":true,"user":{"id":"U01REAL2345","team_id":"T_ACME","name":"rea",
					"profile":{"email":"Real.Person@Example.com","real_name":"Real Person"}}}`))
			case "U01BOT23456":
				w.Write([]byte(`{"ok":true,"user":{"id":"U01BOT23456","team_id":"T_ACME","is_bot":true,"profile":{}}}`))
			case "U01GONE2345":
				w.Write([]byte(`{"ok":true,"user":{"id":"U01GONE2345","team_id":"T_ACME","deleted":true,"profile":{}}}`))
			default:
				w.Write([]byte(`{"ok":false,"error":"user_not_found"}`))
			}
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	orgID, _, token := seedOrg(t, st, RoleAdmin)
	st.SetEmailVerified(ctx, 1, emailByMail, 0)
	enc, _ := b.sealer.Seal([]byte("xoxb-acme"))
	if err := st.SaveTeam(ctx, &Team{TeamID: "T_ACME", OrgID: orgID, Name: "Acme", DMScope: true}, enc); err != nil {
		t.Fatal(err)
	}
	b.slacks = testRegistry(&Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))},
		TeamID: "T_ACME", OrgID: orgID, BotUserID: "UBOT"})

	// The ordinary case: a real person in a connected workspace.
	code, out := authReq(t, mux, "POST", "/api/console/invites",
		map[string]string{"slack_user_id": "U01REAL2345", "role": RoleEditor}, token)
	if code != 200 {
		t.Fatalf("inviting a Slack account = %d: %v", code, out)
	}
	if out["via"] != "dm" || out["delivered"] != true {
		t.Errorf("delivery reported as via=%v delivered=%v, want a sent DM", out["via"], out["delivered"])
	}
	link, _ := out["link"].(string)
	if link == "" {
		t.Fatal("no link came back")
	}
	// It went to that person, and the message carries the link rather than telling them to go
	// looking for it.
	if len(dms) != 1 {
		t.Fatalf("sent %d direct messages, want one", len(dms))
	}
	if !strings.HasPrefix(dms[0], "U01REAL2345|") || !strings.Contains(dms[0], link) {
		t.Errorf("the direct message is %q, want the link sent to U01REAL2345", dms[0])
	}

	// The invitation is bound to the address Slack gave us, so a forwarded link is no use to
	// anybody else — and it remembers the Slack account, so the console can name and revoke it.
	pending, _ := st.PendingInvitesFor(ctx, orgID)
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want the one invitation", pending)
	}
	if got := pending[0]; got.SlackUserID != "U01REAL2345" || got.Email != "real.person@example.com" {
		t.Errorf("invitation = %+v, want it bound to both the Slack id and the address", got)
	}
	if pending[0].Shareable() {
		t.Error("an invitation to a Slack account is not a share link")
	}
	if !pending[0].AcceptsEmail("real.person@example.com") || pending[0].AcceptsEmail("someone@example.com") {
		t.Error("the invitation is not bound to the address Slack reported")
	}

	// Nobody to invite, in three different ways.
	for _, c := range []struct{ id, want string }{
		{"U01BOT23456", "bot"},
		{"U01GONE2345", "deactivated"},
		{"U01NOBODY12", "no Slack account with that id"},
		{"not-an-id", "does not look like a Slack user id"},
	} {
		code, out := authReq(t, mux, "POST", "/api/console/invites",
			map[string]string{"slack_user_id": c.id, "role": RoleViewer}, token)
		if code != 400 {
			t.Errorf("inviting %s = %d, want a refusal: %v", c.id, code, out)
			continue
		}
		if msg, _ := out["error"].(string); !strings.Contains(msg, c.want) {
			t.Errorf("inviting %s said %q, want it to mention %q", c.id, msg, c.want)
		}
	}
	if len(dms) != 1 {
		t.Errorf("a refused invitation still sent a message: %v", dms)
	}
}
