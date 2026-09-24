package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Transactional email: verification links, password resets and invitations.
//
// Resend over its plain HTTP API rather than the SDK — the whole client is one POST, and the
// marketing site already uses Resend, so this is the same account and the same verified domain
// rather than a second vendor to keep alive.
//
// The failure mode that matters is silence. A send that fails must never lose the thing it was
// carrying, so every caller is handed the link back and decides what to say; and a deployment
// with no key configured says so at boot instead of swallowing invitations for a week.

type Mail struct {
	To      string
	Subject string
	Body    string // plain text; the HTML part is derived from it
	// ReplyTo is where an answer belongs when that is not the sending address. The support
	// mailbox is the one place this matters: a request written by the server on somebody's
	// behalf has to answer them, not the no-reply address it left from.
	ReplyTo string
}

type Mailer interface {
	Send(ctx context.Context, m Mail) error
	// Configured reports whether mail can actually leave the process, so the console can tell
	// somebody "copy this link" instead of "check your inbox".
	Configured() bool
}

// ---- Resend ----

type resendMailer struct {
	key    string
	from   string
	client *http.Client
}

func (r *resendMailer) Configured() bool { return true }

func (r *resendMailer) Send(ctx context.Context, m Mail) error {
	if m.To == "" {
		return fmt.Errorf("no recipient")
	}
	payload := map[string]any{
		"from": r.from, "to": []string{m.To}, "subject": m.Subject,
		"text": m.Body, "html": textToHTML(m.Body),
	}
	if m.ReplyTo != "" {
		payload["reply_to"] = m.ReplyTo
	}
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the email provider: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode >= 300 {
		// Resend's own message is the useful half — an unverified sending domain says so here,
		// and that is the failure worth surfacing rather than a bare status code.
		var e struct{ Message string }
		json.Unmarshal(raw, &e)
		return fmt.Errorf("the email provider refused it (%d): %s", resp.StatusCode, nonEmpty(e.Message, truncate(string(raw), 200)))
	}
	return nil
}

// ---- a mailer for a laptop ----

// logMailer is what runs when no key is configured outside a managed deployment: it writes the
// message to the log so a developer can follow the link, and reports itself as unconfigured so
// the console offers the link to copy instead of promising an inbox.
type logMailer struct{}

func (logMailer) Configured() bool { return false }

// Send logs that mail would have gone out, and to whom. The body is not logged: it carries the
// reset, verification and invitation links, which are the credentials the mail exists to
// deliver, and a log line is read by more people than an inbox. MAIL_DEBUG=1 prints it for a
// developer following the link on their own machine.
func (logMailer) Send(ctx context.Context, m Mail) error {
	if os.Getenv("MAIL_DEBUG") == "1" {
		slog.Info("email (not sent: no RESEND_API_KEY)", "to", m.To, "subject", m.Subject, "body", m.Body)
		return nil
	}
	slog.Info("email (not sent: no RESEND_API_KEY; set MAIL_DEBUG=1 to log the body)", "to", m.To, "subject", m.Subject)
	return nil
}

// NewMailer picks one, and never refuses to start. Mail is how verification links, password
// resets and invitations reach people, so a deployment without a key is degraded — but it is a
// deployment that works for everyone already signed in, and half a bot is worth more than a
// container that crash-loops on a key somebody has not bought yet. A missing key is therefore a
// startup warning, loud enough to find in the logs, and the console reports the mailer as
// unconfigured so it offers links to copy by hand instead of promising an inbox. This is the
// opposite of NewSealer's refusal to invent a MASTER_KEY, and for the opposite reason: a
// throwaway key destroys stored credentials, while missing mail only inconveniences.
func NewMailer() Mailer {
	key := os.Getenv("RESEND_API_KEY")
	if key == "" {
		if managedDeployment() {
			slog.Warn("RESEND_API_KEY is not set: no email will be sent. Verification links, password " +
				"resets and invitations are logged instead of delivered, and the console offers them as " +
				"links to copy. Add the key from resend.com and redeploy to turn mail on")
		}
		return logMailer{}
	}
	from := os.Getenv("MAIL_FROM")
	if from == "" {
		slog.Warn("RESEND_API_KEY is set but MAIL_FROM is not, so no email will be sent. MAIL_FROM must " +
			"be an address on a domain verified at resend.com/domains, e.g. " +
			"\"attest_tag <no-reply@mail.example.com>\"")
		return logMailer{}
	}
	return &resendMailer{key: key, from: from, client: &http.Client{Timeout: 20 * time.Second}}
}

// ---- what the messages say ----

// textToHTML is deliberately minimal: these are short, plain messages, and a templating layer
// for six of them would be more to get wrong than to gain. Links are the only markup.
//
// A link is a line holding nothing but a URL — which is how every template below writes one, and
// deliberately not "a URL anywhere in the text". These bodies carry names people chose for
// themselves: their own, their organisation's, the address an invitation went to. Linking every
// URL in the text meant an organisation named "https://evil.com" was greeted with a working link
// to it, inside a message signed by attest_tag and sent from its verified domain — a phishing
// page wearing our envelope. Whatever such a value says, it stays text in the sentence it was
// written into; only the line a template put there alone becomes something to click.
var linkLineRe = regexp.MustCompile(`^https?://[^\s<>"\x27]+$`)

// quoteForMail marks a block of user-typed text so textToHTML leaves it as plain text. A line that
// is only a URL becomes a clickable link (see textToHTML), which for text the recipient did not
// write — a stranger's support question or upgrade note landing in the operator's inbox — is a way
// to plant a look-alike link, e.g. one to the operator page. Prefixing each line with "> " both
// reads as a quote and stops the line being "solely a URL", so nothing in it is linked.
func quoteForMail(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}

func textToHTML(s string) string {
	var b strings.Builder
	b.WriteString(`<div style="font:14px/1.6 -apple-system,Segoe UI,Roboto,sans-serif;color:#1c1c1e">`)
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteString("<br>")
		}
		// html.EscapeString rather than the three-way replacer this used to carry: the quotes
		// matter once a URL is going into an href, and getting that from the standard library
		// is better than remembering to.
		if u := strings.TrimSpace(line); linkLineRe.MatchString(u) {
			esc := html.EscapeString(u)
			b.WriteString(`<a href="` + esc + `">` + esc + `</a>`)
			continue
		}
		b.WriteString(html.EscapeString(line))
	}
	b.WriteString(`</div>`)
	return b.String()
}

// mailValue renders something a person chose — their name, their organisation's, an address —
// into a mail body. One thing has to be true of it by the time textToHTML sees it: it occupies
// no line of its own, because that is what a link looks like. Names typed into the console come
// through cleanName, which already forbids the control characters that would do it; a name from
// a Slack profile or an identity provider arrives as its owner left it, so the guarantee is
// made here rather than assumed. The cap is on the same argument: a name is a name, and one
// long enough to be a paragraph is one being used as a paragraph.
func mailValue(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
	return cutAtRune(strings.Join(fields, " "), 120)
}

func verifyEmail(name, link string) string {
	return greeting(name) + `Confirm this address and your account is ready:

` + link + `

The link works once and expires in 24 hours. If you did not create an account, ignore this — nothing was set up.`
}

func resetEmail(name, link string) string {
	return greeting(name) + `Someone asked to reset the password on this account. Set a new one here:

` + link + `

The link works once and expires in an hour. If it was not you, ignore this: your password has not changed.`
}

func inviteEmail(by *AdminUser, to, link string) string {
	who := mailValue(nonEmpty(by.Name, by.Email))
	to = mailValue(to)
	return `Hello,

` + who + ` has invited you to ` + mailValue(by.OrgName) + ` on attest_tag.

` + link + `

The link works once and expires in seven days. It was sent to ` + to + `; if you were not expecting it, ignore it.`
}

// raiseBudgetEmail is what support reads when a free account presses the button under its
// monthly budget. It carries the operator's link, which is the whole reason the message is
// written here: the console used to open a draft in the requester's own mail client with that
// link inside it, and the account asking for a plan change has no business holding the URL
// that grants one. The reply address is the person who asked, so answering the mail answers
// them — which is what "the reply moves the account to Pro" means.
func raiseBudgetEmail(by *AdminUser, budget, spend float64, link string) string {
	who, orgName, byEmail := mailValue(nonEmpty(by.Name, by.Email)), mailValue(by.OrgName), mailValue(by.Email)
	return fmt.Sprintf(`Hello,

%s asked to raise the monthly budget for %s on attest_tag.

Organisation: %s (id %s)
Plan: free, $%.2f a month, $%.2f spent so far this month
Asked by: %s <%s>

Move the account to Pro here:

%s

Replying to this message answers %s.`, who, orgName, orgName, by.OrgPublic, budget, spend, who, byEmail, link, byEmail)
}

func greeting(name string) string {
	if n := mailValue(name); n != "" {
		return "Hello " + n + ",\n\n"
	}
	return "Hello,\n\n"
}

// errText renders an error for a JSON field without turning nil into "".
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// looksLikeEmail is a deliberately loose check. The address is proved by sending to it, not by a
// regular expression, so this only catches the obvious typo before we spend a send on it.
var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s.]+(\.[^@\s.]+)+$`)

func looksLikeEmail(s string) bool {
	return len(s) <= 254 && emailRe.MatchString(s)
}

// urlEscape is for putting a message into a redirect's query string.
func urlEscape(s string) string { return url.QueryEscape(s) }

// ---- billing ----

// The billing mail. Plain text like everything else here, and deliberately short: each of these
// is read by somebody who wants one fact — it worked, it did not work, you are about to stop —
// and a paragraph of reassurance around that fact is a paragraph between them and it.
//
// None of them carries a Stripe identifier, a card, or anything a person could act on if the mail
// went to the wrong inbox. The console is where the detail is, and these link to it.

func topUpReceiptEmail(orgName string, amountUSD, balanceUSD float64, consoleURL string) string {
	orgName = mailValue(orgName)
	return fmt.Sprintf(`Hello,

$%.2f of API credit has been added to %s.

The balance is now $%.2f. Credit is drawn at the provider's price as the bot answers,
and the console shows every call it came from:

%s

The receipt and the invoice are in the card portal, linked from the same page.`,
		amountUSD, orgName, balanceUSD, consoleURL)
}

func subscriptionStartedEmail(orgName, sizeLabel string, amountUSD float64, periodEnd, consoleURL string) string {
	orgName = mailValue(orgName)
	renews := "It renews monthly until you cancel"
	if periodEnd != "" {
		renews = "It renews on " + periodEnd + " and every month after that, until you cancel"
	}
	return fmt.Sprintf(`Hello,

%s is now on the Pro plan: %s, $%.2f a month.

%s, which you can do on the same screen you subscribed on:

%s

Model spend is separate and is drawn from API credit, at the provider's price.`,
		orgName, sizeLabel, amountUSD, renews, consoleURL)
}

func paymentFailedEmail(orgName string, amountUSD float64) string {
	orgName = mailValue(orgName)
	return fmt.Sprintf(`Hello,

The $%.2f monthly payment for %s did not go through.

The card issuer will be asked again over the next few days, so there is usually nothing
to do. Nothing has been switched off and the bot is still answering. If you would rather
fix it now, update the card under Settings → Billing.

If it keeps failing, the account goes back to the free plan and keeps whatever credit it
has already bought. Nothing is deleted.`, amountUSD, orgName)
}

func lowCreditEmail(orgName string, balanceUSD, thresholdUSD float64, consoleURL string) string {
	orgName = mailValue(orgName)
	return fmt.Sprintf(`Hello,

%s has $%.2f of API credit left, which is under the $%.2f you asked to be told about.

The bot is still answering. It stops when the credit is spent, and says so in the channel
rather than going quiet. Topping up is one screen:

%s`, orgName, balanceUSD, thresholdUSD, consoleURL)
}
