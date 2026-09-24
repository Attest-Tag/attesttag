package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// challengeFrom reads the two-factor challenge a sign-in redirected to, or "" when it went
// anywhere else.
func challengeFrom(rec *httptest.ResponseRecorder) string {
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Path != "/admin/login/" {
		return ""
	}
	return strings.TrimPrefix(loc.Fragment, "challenge=")
}

// A sign-in that still owes its second factor has proved nothing but the first, so it changes
// nothing yet: an invitation it arrived holding used to be redeemed on the Slack or Microsoft
// session alone, before the code was asked for, and the membership stayed whether or not the code
// ever came. It waits in its cookie now. After the code an account with an organisation is sent
// to the join screen to accept it, as a password sign-in is; one with none is joined then, since
// the invitation is the only organisation it has to sign in to.
func TestAnInvitationWaitsForTheSecondFactor(t *testing.T) {
	b, mux, st := identityBot(t)
	fs := &fakeSlack{}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	old := slackOIDC
	slackOIDC = srv.URL
	t.Cleanup(func() { slackOIDC = old })
	ctx := context.Background()

	founder, org, _ := signedUp(t, b, mux, st, "founder@acme.test")
	raw := domainLinkFor(t, st, org, founder.ID, "") // a share link anybody may redeem
	invite := &http.Cookie{Name: inviteCookie, Value: raw}

	ana, ownOrg, session := signedUp(t, b, mux, st, "ana@elsewhere.test")
	secret, recovery := enrol(t, mux, session)
	st.AddIdentity(ctx, ana.ID, ProviderSlack, slackSubject("T9", "U9"))
	fs.info = map[string]any{"ok": true, "sub": "U9", "name": "Ana", "email": "ana@elsewhere.test",
		"https://slack.com/team_id": "T9", "https://slack.com/user_id": "U9"}
	// Enrolment spent this window's code, and the one after it is the last inside the drift
	// allowed, so the second sign-in below finishes with a recovery code.
	next, err := totpCodeAt(secret, totpStep(time.Now())+1)
	if err != nil {
		t.Fatal(err)
	}
	finish := func(challenge string, proof map[string]string) (int, map[string]any) {
		t.Helper()
		proof["challenge"] = challenge
		return post(t, mux, "/api/auth/two-factor", proof, invite)
	}

	rec := slackSignIn(t, mux, invite)
	challenge := challengeFrom(rec)
	if challenge == "" {
		t.Fatalf("the sign-in should have asked for the second factor, went to %q", rec.Header().Get("Location"))
	}
	if m, _ := st.Membership(ctx, ana.ID, org); m != nil {
		t.Fatal("the invitation was redeemed before the second factor was in")
	}
	if clearsInvite(rec) || linkUses(t, st, raw) != 0 {
		t.Fatal("the invitation should wait, whole, for the code")
	}
	status, body := finish(challenge, map[string]string{"code": next})
	if status != 200 || body["next"] != "/admin/join/" {
		t.Fatalf("after the code an account with an organisation goes to the join screen: %d %v", status, body)
	}
	if m, _ := st.Membership(ctx, ana.ID, org); m != nil {
		t.Error("the code joined an account that has an organisation; the join screen is where it accepts")
	}

	// An account in no organisation at all still gets its challenge, and joins once the code is in.
	if err := st.RemoveMember(ctx, ana.ID, ownOrg); err != nil {
		t.Fatal(err)
	}
	rec = slackSignIn(t, mux, invite)
	if challenge = challengeFrom(rec); challenge == "" {
		t.Fatalf("an account with no organisation but an invitation was refused before its code: %s", authError(rec))
	}
	if m, _ := st.Membership(ctx, ana.ID, org); m != nil {
		t.Fatal("the invitation was redeemed before the second factor was in")
	}
	status, body = finish(challenge, map[string]string{"recovery": recovery[0]})
	if m, _ := st.Membership(ctx, ana.ID, org); status != 200 || m == nil {
		t.Fatalf("after the code, the invitation should have joined the account: %d %v", status, body)
	}
}

// The same wait on Sign in with Microsoft.
func TestAnInvitationWaitsForTheSecondFactorOnMicrosoft(t *testing.T) {
	b, mux, f, st := microsoftBot(t, msSignInAnyOrganisation)
	ctx := context.Background()
	founder, org, _ := signedUp(t, b, mux, st, "founder@acme.test")
	invite := &http.Cookie{Name: inviteCookie, Value: domainLinkFor(t, st, org, founder.ID, "")}

	ana, _, session := signedUp(t, b, mux, st, "ana@elsewhere.test")
	enrol(t, mux, session)
	st.AddIdentity(ctx, ana.ID, ProviderMicrosoft, microsoftSubject(contosoTenant, anaOID))

	rec := microsoftSignIn(t, mux, f, nil, invite)
	if challengeFrom(rec) == "" {
		t.Fatalf("the sign-in should have asked for the second factor, went to %q", rec.Header().Get("Location"))
	}
	if m, _ := st.Membership(ctx, ana.ID, org); m != nil {
		t.Fatal("the invitation was redeemed before the second factor was in")
	}
}
