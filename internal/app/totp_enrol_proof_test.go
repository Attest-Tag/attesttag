package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// postAs runs one authenticated JSON request through the real routes.
func postAs(t *testing.T, mux *http.ServeMux, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// backdateSession ages every session this account holds, which is how a stolen cookie differs
// from a person who has just signed in.
func backdateSession(t *testing.T, st *Store, userID int64, by time.Duration) {
	t.Helper()
	when := time.Now().UTC().Add(-by).Format(time.DateTime)
	if _, err := st.db.ExecContext(context.Background(),
		`update admin_sessions set created_at=? where id=?`, when, userID); err != nil {
		t.Fatal(err)
	}
}

// proveIdentity stands in front of every change that would let a stolen session keep the account:
// turning the second factor off, reissuing recovery codes, setting a first password. For an
// account that signed in with Slack and has never set a password — the common case — the only
// proof it accepts is a session minted in the last ten minutes, because a thief holding a cookie
// found on an unattended console cannot show one.
//
// Enrolling a second factor asked for nothing, and it was the entrance to all of it: enrol your
// own authenticator and proveIdentity answers "code" from then on, so the same stolen session can
// then set a first password with a code from your own phone and keep the account for good.
func TestEnrollingTwoFactorNeedsProof(t *testing.T) {
	_, mux, st := installTestBot(t)
	_, userID, tok := seedOrg(t, st, RoleAdmin)
	// proveIdentity's lockout is a package-level counter keyed by user id, and every test here
	// starts from a fresh store whose first user is id 1 — so failures this test makes on
	// purpose would otherwise be inherited by the next one.
	defer logins.reset(proofKey(userID))
	logins.reset(proofKey(userID))

	// Signed in a moment ago: that is the proof, and there is nothing to type.
	if w := postAs(t, mux, "/api/account/totp/start", tok, "{}"); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"secret"`) {
		t.Fatalf("a fresh session could not enrol: %d %s", w.Code, w.Body.String())
	}

	// The same session, older than the window. This is the request that used to hand back a
	// secret to anybody holding the cookie.
	backdateSession(t, st, userID, 20*time.Minute)
	w := postAs(t, mux, "/api/account/totp/start", tok, "{}")
	if w.Code != 403 {
		t.Fatalf("a stale session was given an enrolment secret: %d %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "Sign in again") {
		t.Errorf("the refusal does not say how to proceed: %s", body)
	}
	if strings.Contains(w.Body.String(), `"secret"`) {
		t.Error("the refusal handed back the secret anyway")
	}

	// And the door it was the entrance to stays shut: with no authenticator to answer with, a
	// first password cannot be set either.
	if w := postAs(t, mux, "/api/auth/password", tok,
		`{"current":"000000","password":"a-long-enough-new-password"}`); w.Code == 200 {
		t.Fatal("a stale session set a first password")
	}
}

// An account that has a password proves it the way every other change in account.go asks: by
// typing it. The point is that the answer is never "nothing".
func TestEnrollingTwoFactorAsksAnAccountWithAPassword(t *testing.T) {
	_, mux, st := installTestBot(t)
	ctx := context.Background()
	_, userID, tok := seedOrg(t, st, RoleAdmin)

	hash, err := hashPassword("the-account-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetPassword(ctx, userID, hash); err != nil {
		t.Fatal(err)
	}
	defer logins.reset(proofKey(userID))
	logins.reset(proofKey(userID))
	backdateSession(t, st, userID, 20*time.Minute)

	if w := postAs(t, mux, "/api/account/totp/start", tok, "{}"); w.Code != 403 {
		t.Fatalf("enrolment without the password = %d, want 403: %s", w.Code, w.Body.String())
	}
	if w := postAs(t, mux, "/api/account/totp/start", tok, `{"password":"wrong"}`); w.Code != 403 {
		t.Fatalf("enrolment with the wrong password = %d, want 403", w.Code)
	}
	w := postAs(t, mux, "/api/account/totp/start", tok, `{"password":"the-account-password"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"secret"`) {
		t.Fatalf("enrolment with the right password = %d: %s", w.Code, w.Body.String())
	}
}

// The screen has to know which field to put up before it asks, or it guesses — which is how a
// person ends up typing a password into a box that will only accept a code.
func TestAccountSaysWhatProofEnrollingWillAskFor(t *testing.T) {
	_, mux, st := installTestBot(t)
	_, _, tok := seedOrg(t, st, RoleAdmin)
	w := do(t, mux, "GET", "/api/account", tok)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"proof":"recent"`) {
		t.Fatalf("GET /api/account = %d, and does not say what proof enrolling needs: %s", w.Code, w.Body.String())
	}
}
