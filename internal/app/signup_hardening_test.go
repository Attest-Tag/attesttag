package app

import (
	"context"
	"strings"
	"testing"
)

// catchMailer keeps what would have been sent, so a test can read the message a person receives
// rather than the template it came from. It reports itself as configured: an unconfigured mailer
// changes what the console does, and this is here to watch mail, not to change the path.
type catchMailer struct{ sent []Mail }

func (c *catchMailer) Configured() bool { return true }
func (c *catchMailer) Send(_ context.Context, m Mail) error {
	c.sent = append(c.sent, m)
	return nil
}

// The reported walk-through, end to end: an organisation and a person both named
// "https://evil.com", the password "1234567890", and the verification mail that came back with
// the attacker's domain in it as something to click.
//
// Both halves are refused at the door now, and the third — the rendering — is proved separately
// in email_injection_test.go, because it is what still has to hold for a name that arrived from
// a Slack profile or an identity provider without passing this form.
func TestReportedSignupWalkthroughIsRefused(t *testing.T) {
	_, mux, _ := identityBotMode(t, SignupOpen)

	// Step 5 and 6: the password is the one from the report.
	code, out := post(t, mux, "/api/auth/signup", map[string]string{
		"email": "researcher@example.com", "password": "1234567890",
		"name": "Ada Lovelace", "org": "Acme"})
	if code != 400 {
		t.Fatalf("the password from the report was accepted: %d %v", code, out)
	}
	if msg, _ := out["error"].(string); strings.Contains(msg, "at least") {
		t.Errorf("refused for its length rather than for being predictable: %q", msg)
	}

	// Steps 2 and 3: the organisation's name, then the person's.
	code, out = post(t, mux, "/api/auth/signup", map[string]string{
		"email": "researcher@example.com", "password": "a quiet harbour lantern",
		"name": "Ada Lovelace", "org": "https://evil.com"})
	if code != 400 {
		t.Errorf("an organisation named after a web address: want 400, got %d %v", code, out)
	}
	// And the refusal came before anything was written: an account created and then abandoned
	// when its organisation was refused is an address that can never sign up again, because the
	// next attempt is told it is taken.
	if code, out := post(t, mux, "/api/auth/signup", map[string]string{
		"email": "researcher@example.com", "password": "a quiet harbour lantern",
		"name": "Ada Lovelace", "org": "Acme"}); code != 200 {
		t.Errorf("the refused signup left an orphaned account behind: %d %v", code, out)
	}
	code, out = post(t, mux, "/api/auth/signup", map[string]string{
		"email": "second@example.com", "password": "a quiet harbour lantern",
		"name": "https://evil.com", "org": "Acme Two"})
	if code != 400 {
		t.Errorf("a person named after a web address: want 400, got %d %v", code, out)
	}
}

// And the mail a real signup does send: one link, the one attest_tag generated.
func TestVerificationMailCarriesOnlyItsOwnLink(t *testing.T) {
	b, mux, _ := identityBotMode(t, SignupOpen)
	caught := &catchMailer{}
	b.mail = caught

	code, out := post(t, mux, "/api/auth/signup", map[string]string{
		"email": "ada@example.com", "password": "a quiet harbour lantern",
		"name": "Ada Lovelace", "org": "Analytical Engines"})
	if code != 200 {
		t.Fatalf("an ordinary signup was refused: %d %v", code, out)
	}
	if len(caught.sent) != 1 {
		t.Fatalf("want one verification mail, got %d", len(caught.sent))
	}
	m := caught.sent[0]
	if !strings.Contains(m.Body, "Hello Ada Lovelace,") {
		t.Errorf("the greeting lost the name:\n%s", m.Body)
	}
	html := textToHTML(m.Body)
	if n := strings.Count(html, "<a "); n != 1 {
		t.Errorf("want exactly one link in the verification mail, got %d:\n%s", n, html)
	}
	if !strings.Contains(html, `<a href="https://console.example.com/`) {
		t.Errorf("the one link is not the console's:\n%s", html)
	}
}

// Renaming is the other way in, and it runs through the same cleanName: an account founded
// under an honest name must not be able to become a link afterwards.
func TestRenamingCannotSmuggleALinkIn(t *testing.T) {
	_, _, st := identityBotMode(t, SignupOpen)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "ada@example.com", "Ada", "x")
	if err != nil {
		t.Fatal(err)
	}
	org, err := st.CreateOrg(ctx, "Analytical Engines", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RenameOrg(ctx, org.ID, "https://evil.com"); err == nil {
		t.Error("an organisation renamed itself into a web address")
	}
	if err := st.RenameOrg(ctx, org.ID, "Analytical Engines Ltd"); err != nil {
		t.Errorf("an ordinary rename was refused: %v", err)
	}
}
