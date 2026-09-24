package app

import (
	"strings"
	"testing"
)

// A name a person chose for themselves is text, never a link. It used to be both: textToHTML
// linked every URL it could find anywhere in the body, so founding an organisation called
// "https://evil.com" produced "Hello <a href="https://evil.com">https://evil.com</a>," inside a
// verification mail signed by attest_tag and sent from its verified domain. Nothing was
// executed and no session was stolen; the whole exploit was the envelope, which is the part
// worth protecting.
func TestUserValuesNeverBecomeLinks(t *testing.T) {
	const evil = "https://evil.com"
	link := "https://app.attesttag.com/verify/abc123"

	// paymentFailedEmail is the one that carries no link of its own: nothing in it needs
	// pressing, so the count it is measured against is zero rather than one.
	bodies := map[string]string{
		"verify":       verifyEmail(evil, link),
		"reset":        resetEmail(evil, link),
		"invite":       inviteEmail(&AdminUser{Name: evil, Email: "a@b.com", OrgName: evil}, evil, link),
		"raise budget": raiseBudgetEmail(&AdminUser{Name: evil, Email: "a@b.com", OrgName: evil, OrgPublic: "x"}, 5, 1, link),
		"top up":       topUpReceiptEmail(evil, 10, 20, link),
		"subscribed":   subscriptionStartedEmail(evil, "1-10 people", 49, "1 Oct", link),
		"failed":       paymentFailedEmail(evil, 49),
		"low credit":   lowCreditEmail(evil, 1, 5, link),
	}
	for name, body := range bodies {
		html := textToHTML(body)
		want := 1
		if name == "failed" {
			want = 0
		}
		// The one link the template meant to send is still a link...
		if want == 1 && !strings.Contains(html, `<a href="`+link+`">`) {
			t.Errorf("%s: the message lost its own link:\n%s", name, html)
		}
		// ...and nothing the caller supplied became one.
		if strings.Contains(html, `href="`+evil) || strings.Contains(html, `href='`+evil) {
			t.Errorf("%s: a user-supplied value was rendered as a link:\n%s", name, html)
		}
		if n := strings.Count(html, "<a "); n != want {
			t.Errorf("%s: want %d links, got %d:\n%s", name, want, n, html)
		}
	}
}

// The line-of-its-own rule only holds if no value can occupy a line of its own. cleanName
// refuses control characters, but a name off a Slack profile or an identity provider is written
// to the database as its owner left it, so mailValue is what actually makes it true.
func TestMailValueCannotOpenALine(t *testing.T) {
	for _, in := range []string{
		"Bo\nhttps://evil.com\nBo",
		"Bo\r\nhttps://evil.com",
		"Bo\u2028https://evil.com",
		"Bo\thttps://evil.com",
		"Bo\x00https://evil.com",
	} {
		got := mailValue(in)
		if strings.ContainsAny(got, "\n\r") {
			t.Errorf("mailValue(%q) kept a line break: %q", in, got)
		}
		html := textToHTML(greeting(in) + "text\n\nhttps://app.attesttag.com/x\n")
		if strings.Contains(html, `href="https://evil.com"`) {
			t.Errorf("mailValue(%q) still reached an href:\n%s", in, html)
		}
	}
	if got := mailValue("  Ada   Lovelace  "); got != "Ada Lovelace" {
		t.Errorf("an ordinary name should survive intact, got %q", got)
	}
	if n := len(mailValue(strings.Repeat("x", 4000))); n > 120 {
		t.Errorf("a name long enough to be a paragraph should be cut, got %d bytes", n)
	}
}

// Escaping is the other half: the body is plain text, so anything that looks like markup in it
// is somebody's idea of a joke rather than markup.
func TestMailBodyIsEscaped(t *testing.T) {
	html := textToHTML(greeting(`<img src=x onerror=alert(1)>`) + "hello")
	if strings.Contains(html, "<img") {
		t.Errorf("a tag in a name reached the HTML part:\n%s", html)
	}
	if !strings.Contains(html, "&lt;img") {
		t.Errorf("want the tag escaped, got:\n%s", html)
	}
}

// And the front door: a name is not a web address. This is the input half of the same fix, and
// it holds for organisations, people and invite-link labels alike, because they share cleanName.
func TestNamesCannotBeWebAddresses(t *testing.T) {
	for _, bad := range []string{
		"https://evil.com", "http://evil.com", "HTTPS://EVIL.COM",
		"Acme https://evil.com", "javascript:alert(1)", "data:text/html,x",
	} {
		if _, err := cleanName(bad, 80); err == nil {
			t.Errorf("cleanName(%q) should have been refused", bad)
		}
	}
	// A company that is named after its domain is still a company.
	for _, ok := range []string{"acme.com", "Acme Inc.", "Ada's Team", "研究室", "O'Brien & Sons"} {
		if _, err := cleanName(ok, 80); err != nil {
			t.Errorf("cleanName(%q) should have been allowed: %v", ok, err)
		}
	}
}
