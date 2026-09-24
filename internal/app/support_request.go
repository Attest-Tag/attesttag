package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// The help button in the console's header: type a question, press Send, and the server mails it
// to the support address with the asker on Reply-To. It takes the same road as the Upgrade and
// Talk to us requests (plans.go, billing_request.go) rather than opening a mailto: — the message
// arrives saying who asked, from which organisation and which page, without anybody finishing a
// draft in their own mail client, and answering it answers them.
//
// Any signed-in member may ask. A question is not a change to the account, so there is no
// permission it could sensibly sit behind, and a viewer stuck on a page is exactly who needs it.
// What stands between the button and a flooded inbox is the ceiling below, the same guard the
// other two carry, and the session: the Reply-To is the address the account signed in with,
// not one typed into the form.

const (
	helpRequestsPerOrgADay = 10
	helpMessageMax         = 4000
	helpSubjectSnippet     = 60
)

var helpRequests = newRateLimiter()

type helpRequestIn struct {
	Message string `json:"message"`
	// Page is the console path the question was asked from, so the answer can start from the
	// screen the person was looking at. Display only; see helpPage.
	Page string `json:"page"`
}

func (b *Bot) handleHelpRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := adminFromCtx(ctx)
	// No address, no button. A deployment that named no inbox has nobody to put the question in
	// front of, and sending it anyway would mail a stranger. The console hides the button on the
	// same condition.
	if b.cfg.SupportEmail == "" {
		bad(w, errors.New("this deployment publishes no support address; ask whoever runs it"))
		return
	}
	var in helpRequestIn
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	msg := strings.TrimSpace(in.Message)
	if msg == "" {
		bad(w, errors.New("write your question first"))
		return
	}
	if len(msg) > helpMessageMax {
		bad(w, fmt.Errorf("the message is %d characters; keep it under %d", len(msg), helpMessageMax))
		return
	}
	// Counted after the checks above, so a refused empty message does not spend the day's quota.
	if ok, d := helpRequests.allow("help-request:"+me.OrgPublic, helpRequestsPerOrgADay, 24*time.Hour); !ok {
		tooMany(w, d)
		return
	}
	page := helpPage(in.Page)
	err := b.mail.Send(ctx, Mail{
		To: b.cfg.SupportEmail, ReplyTo: me.Email,
		Subject: helpSubject(me.OrgName, msg),
		Body:    b.helpRequestEmail(ctx, me, msg, page),
	})
	slog.Info("help requested", "org", me.OrgPublic, "by", me.Email, "sent", err == nil)
	// The length and not the text: the question is between the asker and support, and the
	// organisation's audit log is read by more people than either.
	b.audit(r, "support.help_requested", AuditEvent{TargetKind: "org", TargetID: me.OrgPublic, TargetName: me.OrgName,
		Details: auditDetails(map[string]any{"sent": err == nil, "chars": len(msg), "page": page})})
	if err != nil {
		// The question is the thing that must not be lost: say where it did not get to, so the
		// answer is one address away rather than a support page somebody has to go and find.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": fmt.Sprintf("The message could not be sent (%s). Write to %s and it will be picked up there.", err, b.cfg.SupportEmail)})
		return
	}
	// delivered is false on a laptop with no RESEND_API_KEY, where the mailer only logs: the
	// console then names the address instead of promising an inbox somebody will never see.
	writeJSON(w, 200, map[string]any{"ok": true, "delivered": b.mail.Configured(), "support_email": b.cfg.SupportEmail})
}

// helpSubject puts the start of the question in the subject, so the support inbox reads as a
// list of questions rather than a column of identical "Help request" lines.
func helpSubject(org, msg string) string {
	s := strings.Join(strings.Fields(firstLine(msg)), " ")
	if cut, more := cutRunes(s, helpSubjectSnippet); more {
		s = strings.TrimRightFunc(cut, unicode.IsSpace) + "…"
	}
	return "Help request from " + org + ": " + s
}

// helpPage keeps the page only when it looks like one: a path, short, with no spaces or control
// characters. It comes from the browser and ends up in a mail, so anything else is dropped
// rather than repaired.
func helpPage(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p[0] != '/' || len(p) > 200 || strings.ContainsFunc(p, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return ""
	}
	return p
}

// helpRequestEmail is the message support reads: the question first, because it is what the
// mail is for, then one fact a line — the same layout as the upgrade requests — so whoever
// answers does not have to open the operator page to know who is asking.
func (b *Bot) helpRequestEmail(ctx context.Context, me *AdminUser, msg, page string) string {
	st := b.settings.Get(ctx, me.OrgID)
	spend, _ := b.store.MonthSpend(ctx, me.OrgID, "", "")
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s asked for help from the console.\n\n", nonEmpty(me.Name, me.Email))
	sb.WriteString(quoteForMail(msg))
	sb.WriteString("\n\n")
	fmt.Fprintf(&sb, "Asked by:      %s <%s>, %s\n", nonEmpty(me.Name, "(no name)"), me.Email, nonEmpty(me.Role, "member"))
	fmt.Fprintf(&sb, "Organisation:  %s (%s)\n", me.OrgName, me.OrgPublic)
	if budget := st.EffectiveBudget(); budget > 0 {
		fmt.Fprintf(&sb, "Plan:          %s, $%.2f of $%.2f spent this month\n", st.Plan, spend, budget)
	} else {
		fmt.Fprintf(&sb, "Plan:          %s, $%.2f spent this month\n", st.Plan, spend)
	}
	if page != "" {
		fmt.Fprintf(&sb, "Page:          %s\n", page)
	}
	sb.WriteString("\nReply to this message to answer them.\n")
	return sb.String()
}
