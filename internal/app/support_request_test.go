package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The help button in the header: the question is mailed to support with the asker as Reply-To,
// the message says who asked and from where, any member may ask — a viewer included — and ten a
// day is the ceiling because each one lands in a person's inbox.
func TestHelpRequestMailsSupportWithTheAskerAsReplyTo(t *testing.T) {
	helpRequests = newRateLimiter()
	b, mux, st := planBot(t, Config{FreePlanBudgetUSD: 5, SupportEmail: testSupportEmail})
	_, _, tok := seedOrg(t, st, RoleViewer)
	box := &mailbox{}
	b.mail = box
	ask := func(body string) (int, map[string]any) {
		return call(t, mux, "POST", "/api/support/message", body, "Bearer "+tok)
	}

	code, out := ask(`{"message":"  The bot stopped answering in #sales since this morning.\nIs it the budget?  ","page":"/admin/settings/"}`)
	if code != 200 || out["ok"] != true || out["delivered"] != true || out["support_email"] != testSupportEmail {
		t.Fatalf("a viewer asking for help: %d %v", code, out)
	}
	if len(box.sent) != 1 {
		t.Fatalf("%d messages sent, want 1", len(box.sent))
	}
	m := box.sent[0]
	if m.To != testSupportEmail || m.ReplyTo != "admin@example.com" {
		t.Errorf("to=%q reply-to=%q, want %q and the asker", m.To, m.ReplyTo, testSupportEmail)
	}
	if m.Subject != "Help request from Test Org: The bot stopped answering in #sales since this morning." {
		t.Errorf("subject = %q", m.Subject)
	}
	// The question is quoted line by line so a URL a stranger typed cannot become a clickable link
	// in the support inbox (quoteForMail).
	for _, want := range []string{"> The bot stopped answering in #sales since this morning.\n> Is it the budget?",
		"Test Admin <admin@example.com>, viewer", "Test Org", "free, $0.00 of $5.00 spent this month", "/admin/settings/",
		"Reply to this message"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("the message support reads lacks %q:\n%s", want, m.Body)
		}
	}

	// An empty question sends nothing and does not spend the quota; neither does a long one.
	if code, _ := ask(`{"message":"   "}`); code != 400 {
		t.Errorf("an empty question answered %d, want 400", code)
	}
	if code, _ := ask(`{"message":"` + strings.Repeat("x", helpMessageMax+1) + `"}`); code != 400 {
		t.Errorf("an over-long question answered %d, want 400", code)
	}
	if len(box.sent) != 1 {
		t.Errorf("%d messages sent after refused questions, want still 1", len(box.sent))
	}

	// Ten a day, counted per organisation.
	for i := 2; i <= helpRequestsPerOrgADay; i++ {
		if code, out := ask(`{"message":"another one"}`); code != 200 {
			t.Fatalf("question %d answered %d %v", i, code, out)
		}
	}
	if code, _ := ask(`{"message":"one too many"}`); code != http.StatusTooManyRequests {
		t.Errorf("question %d in a day answered %d, want 429", helpRequestsPerOrgADay+1, code)
	}
	if len(box.sent) != helpRequestsPerOrgADay {
		t.Errorf("%d messages sent, want %d", len(box.sent), helpRequestsPerOrgADay)
	}
}

// With no support address there is nobody to ask, so the route refuses and nothing is sent; and
// a mailer that fails is answered with the address, so the question is one mail away.
func TestHelpRequestNeedsAnAddressAndNamesItWhenMailFails(t *testing.T) {
	helpRequests = newRateLimiter()
	b, mux, st := planBot(t, Config{FreePlanBudgetUSD: 5})
	_, _, tok := seedOrg(t, st, RoleAdmin)
	box := &mailbox{}
	b.mail = box
	if code, _ := call(t, mux, "POST", "/api/support/message", `{"message":"hello"}`, "Bearer "+tok); code != 400 {
		t.Errorf("answered %d with no support address, want 400", code)
	}
	if len(box.sent) != 0 {
		t.Errorf("%d messages sent to nobody", len(box.sent))
	}

	b.cfg.SupportEmail = testSupportEmail
	b.mail = failingMailer{}
	code, out := call(t, mux, "POST", "/api/support/message", `{"message":"hello"}`, "Bearer "+tok)
	if msg, _ := out["error"].(string); code != http.StatusBadGateway || !strings.Contains(msg, testSupportEmail) {
		t.Errorf("a failed send answered %d %v, want 502 naming %s", code, out, testSupportEmail)
	}

	// Signed out, the button is not there to press.
	if code, _ := call(t, mux, "POST", "/api/support/message", `{"message":"hello"}`, ""); code != http.StatusUnauthorized {
		t.Errorf("a signed-out request answered %d, want 401", code)
	}
}

// The page comes from the browser and ends up in a mail, so only something shaped like a path
// survives, and the subject carries a trimmed first line of the question.
func TestHelpRequestPageAndSubject(t *testing.T) {
	for in, want := range map[string]string{
		"/admin/settings/":             "/admin/settings/",
		"  /admin/overview/  ":         "/admin/overview/",
		"https://evil.example/x":       "",
		"/admin/x\r\nBcc: a@b.c":       "",
		"/admin/two words":             "",
		"":                             "",
		"/" + strings.Repeat("a", 200): "",
	} {
		if got := helpPage(in); got != want {
			t.Errorf("helpPage(%q) = %q, want %q", in, got, want)
		}
	}
	long := "How do I connect   the GitHub App to a repository that lives in another organisation?\nMore."
	got := helpSubject("Acme", long)
	if !strings.HasPrefix(got, "Help request from Acme: How do I connect the GitHub App") || !strings.HasSuffix(got, "…") || strings.Contains(got, "More.") {
		t.Errorf("helpSubject = %q", got)
	}
}

type failingMailer struct{}

func (failingMailer) Configured() bool                 { return true }
func (failingMailer) Send(context.Context, Mail) error { return errors.New("resend answered 500") }

// A line that is only a URL becomes a link in a mail (textToHTML). Text the recipient did not
// write — a stranger's question landing in the support inbox, where real operator links arrive —
// must not get that treatment, so it is quoted line by line first and stays plain text.
func TestQuotedUserTextIsNotLinkified(t *testing.T) {
	raw := "https://attesttag-operator.example/operator/plan?org=ffff"
	if !strings.Contains(textToHTML(raw), `<a href=`) {
		t.Fatal("precondition: a bare URL line is expected to linkify on its own")
	}
	if strings.Contains(textToHTML(quoteForMail(raw)), `<a href=`) {
		t.Error("a quoted user URL still became a link in the mail")
	}
}
