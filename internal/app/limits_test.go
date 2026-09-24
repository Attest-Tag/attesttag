package app

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Signup is open; these are the bounds on what an organisation founded a minute ago can do.

// stubMailer stands in for a configured Resend, so the verification gate is exercised.
type stubMailer struct{ sent []Mail }

func (m *stubMailer) Send(ctx context.Context, mail Mail) error {
	m.sent = append(m.sent, mail)
	return nil
}
func (m *stubMailer) Configured() bool { return true }

func TestUnverifiedFounderCannotReachOut(t *testing.T) {
	b, mux, st := identityBot(t)
	b.mail = &stubMailer{}
	u, _, token := signedUp(t, b, mux, st, "founder@example.com")
	ctx := context.Background()

	code, body := authReq(t, mux, "POST", "/api/console/invites", map[string]string{"email": "friend@example.com"}, token)
	if code != 403 || body["verify_email"] != true {
		t.Fatalf("an unverified founder sent an invitation: %d %v", code, body)
	}
	code, body = authReq(t, mux, "POST", "/api/api-keys", map[string]string{"name": "ci"}, token)
	if code != 403 || body["verify_email"] != true {
		t.Fatalf("an unverified founder minted an API key: %d %v", code, body)
	}
	// Connecting a workspace does not wait: the install proves something stronger than a mail
	// link — that the person pressing Allow administers the workspace — and reaches nobody else.
	if w := do(t, mux, "GET", "/slack/install", token); !strings.HasPrefix(w.Header().Get("Location"), "https://slack.com/oauth/v2/authorize?") {
		t.Fatalf("an unverified founder was held off the workspace install: %d %q", w.Code, w.Header().Get("Location"))
	}
	// The setup walk still says the address is unconfirmed — a password reset and the invite
	// step rest on it — and carries the resend button, without holding the install on it.
	if _, body := authReq(t, mux, "GET", "/api/onboarding", nil, token); body["email_unverified"] != true {
		t.Fatalf("the setup walk did not report the unconfirmed address: %v", body)
	}
	// Reading, and the things that reach nobody else, still work.
	if code, _ := authReq(t, mux, "GET", "/api/settings", nil, token); code != 200 {
		t.Fatalf("an unverified founder cannot even read: %d", code)
	}
	if err := st.SetEmailVerified(ctx, u.ID, emailByMail, 0); err != nil {
		t.Fatal(err)
	}
	if code, body := authReq(t, mux, "POST", "/api/console/invites", map[string]string{"email": "friend@example.com"}, token); code != 200 {
		t.Fatalf("a verified founder could not invite: %d %v", code, body)
	}
	if _, body := authReq(t, mux, "GET", "/api/onboarding", nil, token); body["email_unverified"] != false {
		t.Fatalf("the setup walk still asks a verified founder to confirm: %v", body)
	}
}

func TestSignupsAreThrottledPerCaller(t *testing.T) {
	_, mux, _ := identityBot(t)
	for i := 0; i < signupsPerIPPerHour; i++ {
		email := "founder" + string(rune('a'+i)) + "@example.com"
		if code, body := post(t, mux, "/api/auth/signup", map[string]string{"email": email, "password": "correct horse battery", "org": "Org"}); code != 200 {
			t.Fatalf("signup %d = %d: %v", i+1, code, body)
		}
	}
	if code, _ := post(t, mux, "/api/auth/signup", map[string]string{"email": "one-more@example.com", "password": "correct horse battery", "org": "Org"}); code != 429 {
		t.Fatalf("the %dth signup from one address was accepted: %d", signupsPerIPPerHour+1, code)
	}
}

func TestInvitesAreThrottledAndNamesAreClean(t *testing.T) {
	b, mux, st := identityBot(t)
	_, _, token := signedUp(t, b, mux, st, "founder@example.com")
	// A name that would be a second header line in the invitation's subject.
	if code, _ := authReq(t, mux, "PUT", "/api/org", map[string]string{"name": "Acme\r\nBcc: victim@example.com"}, token); code != 400 {
		t.Fatalf("an organisation name with a line break was accepted: %d", code)
	}
	if code, _ := authReq(t, mux, "PUT", "/api/org", map[string]string{"name": strings.Repeat("x", 81)}, token); code != 400 {
		t.Fatalf("an 81-character organisation name was accepted: %d", code)
	}
	if _, err := st.CreateOrg(context.Background(), "Evil\x00Corp", 1); err == nil {
		t.Fatal("a control character in an organisation name was accepted")
	}
	for i := 0; i < invitesPerAddressADay; i++ {
		if code, body := authReq(t, mux, "POST", "/api/console/invites", map[string]string{"email": "same@example.com"}, token); code != 200 {
			t.Fatalf("invite %d = %d %v", i+1, code, body)
		}
	}
	if code, _ := authReq(t, mux, "POST", "/api/console/invites", map[string]string{"email": "same@example.com"}, token); code != 429 {
		t.Fatalf("the %dth invitation to one address in a day was sent: %d", invitesPerAddressADay+1, code)
	}
	for i := invitesPerAddressADay; i < invitesPerOrgPerHour; i++ {
		email := "person" + strings.Repeat("x", i) + "@example.com"
		if code, body := authReq(t, mux, "POST", "/api/console/invites", map[string]string{"email": email}, token); code != 200 {
			t.Fatalf("invite %d = %d %v", i+1, code, body)
		}
	}
	if code, _ := authReq(t, mux, "POST", "/api/console/invites", map[string]string{"email": "last@example.com"}, token); code != 429 {
		t.Fatalf("the %dth invitation from one organisation in an hour was sent: %d", invitesPerOrgPerHour+1, code)
	}
}

func TestPerOrgCapsAndRoutineFloor(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for i := 0; i < maxRoutinesPerOrg; i++ {
		if _, err := st.AddRoutine(ctx, Routine{OrgID: 7, TeamID: "T1", Channel: "C1", Cron: "0 9 * * *", TZ: "UTC", Prompt: "digest"}); err != nil {
			t.Fatalf("routine %d: %v", i+1, err)
		}
	}
	if _, err := st.AddRoutine(ctx, Routine{OrgID: 7, TeamID: "T1", Channel: "C1", Cron: "0 9 * * *", TZ: "UTC", Prompt: "one more"}); err == nil {
		t.Fatal("the cap on routines did not hold")
	}
	if _, err := st.AddRoutine(ctx, Routine{OrgID: 8, TeamID: "T1", Channel: "C1", Cron: "0 9 * * *", TZ: "UTC", Prompt: "another org"}); err != nil {
		t.Fatalf("one organisation's cap stopped another: %v", err)
	}
	if !routineTooFrequent("* * * * *", "UTC") || !routineTooFrequent("*/5 * * * *", "UTC") {
		t.Error("a schedule under the floor was allowed")
	}
	if routineTooFrequent("*/15 * * * *", "UTC") || routineTooFrequent("0 9 * * 1-5", "UTC") {
		t.Error("a schedule at or above the floor was refused")
	}
	every := "* * * * *"
	if err := st.UpdateRoutine(ctx, 8, 51, RoutinePatch{Cron: &every}); err == nil {
		t.Error("a routine was moved to every minute")
	}
	for i := 0; i < maxMemoriesPerOrg; i++ {
		if err := st.AddMemory(ctx, 7, "T1", "team:T1", "fact", "U1"); err != nil {
			t.Fatalf("memory %d: %v", i+1, err)
		}
	}
	if err := st.AddMemory(ctx, 7, "T1", "team:T1", "fact", "U1"); err == nil {
		t.Fatal("the cap on memories did not hold")
	}
	if _, err := st.CreateBundle(ctx, 7, "b", "U1"); err != nil {
		t.Fatalf("a bundle under the cap was refused: %v", err)
	}
}

func TestSplitCommand(t *testing.T) {
	argv, err := SplitCommand(`pytest -x "tests/unit" --tb='short one'`)
	if err != nil || len(argv) != 4 || argv[2] != "tests/unit" || argv[3] != "--tb=short one" {
		t.Fatalf("argv = %v, %v", argv, err)
	}
	for _, bad := range []string{"make test; curl evil", "npm test | sh", "echo $HOME", "cat < /etc/passwd", "a && b", "`id`", "x \\ y", "unclosed 'quote"} {
		if _, err := SplitCommand(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if argv, err := SplitCommand("   "); err != nil || argv != nil {
		t.Errorf("blank = %v, %v", argv, err)
	}
}

// A reset between tests: the limiters are process-wide, and every test signs up from the same
// address.
func resetLimiters() {
	signups = newRateLimiter()
	invites = newRateLimiter()
}

var _ = time.Second
