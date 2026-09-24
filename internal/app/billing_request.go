package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// The size at the top of the ladder is a conversation, and on the Billing screen it is a button
// rather than a dead row. Picking "More than 250 users" and pressing Talk to us mails the support
// address — never the customer's own mail client, which would leave the account's figures in a
// draft they have to send themselves — with "Upgrade request from <organisation>" and what the
// person answering needs before they reply: who asked, the plan the account is on, how many
// people used the bot, what it spent this month, and anything the asker typed. Reply-To is the
// asker, so answering the mail answers them.
//
// Same shape and the same guards as the free plan's raise-budget request in plans.go: no support
// address means the route refuses rather than mailing a stranger, three a day per organisation
// because each one lands in a person's inbox, and the organisation remembers it asked so the
// screen can say so after a reload instead of offering the button as if nothing had happened.

const (
	sizeRequestKey         = "size_request_at"
	sizeRequestSizeKey     = "size_request_size"
	sizeRequestsPerOrgADay = 3
	sizeRequestNoteMax     = 1000
)

var sizeRequests = newRateLimiter()

type sizeRequestIn struct {
	Size string `json:"size"`
	Note string `json:"note"`
}

// sizeRequestView is what the Billing screen shows about the last request: when, and for which
// size, so the sentence under the button can say "you asked on …" rather than nothing.
type sizeRequestView struct {
	At        string `json:"at"`
	Size      string `json:"size"`
	SizeLabel string `json:"size_label"`
}

func (b *Bot) handleBillingSizeRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := adminFromCtx(ctx)
	var in sizeRequestIn
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	size, ok := b.cfg.SizeByKey(in.Size)
	if !ok {
		bad(w, errors.New("that is not a plan size this deployment offers"))
		return
	}
	// A size with a Price is bought on this screen. Asking a person to sell it by mail is the slow
	// road to the same place, and the inbox would be answering it forever.
	if size.PriceID != "" {
		bad(w, errors.New("that size is sold on this screen — pick it and continue to payment"))
		return
	}
	if b.cfg.SupportEmail == "" {
		bad(w, errors.New("this deployment publishes no support address; ask whoever runs it about a larger plan"))
		return
	}
	note := strings.TrimSpace(in.Note)
	if len(note) > sizeRequestNoteMax {
		bad(w, fmt.Errorf("the note is %d characters; keep it under %d", len(note), sizeRequestNoteMax))
		return
	}
	if ok, d := sizeRequests.allow("size-request:"+me.OrgPublic, sizeRequestsPerOrgADay, 24*time.Hour); !ok {
		tooMany(w, d)
		return
	}
	err := b.mail.Send(ctx, Mail{
		To: b.cfg.SupportEmail, ReplyTo: me.Email,
		Subject: "Upgrade request from " + me.OrgName,
		Body:    b.sizeRequestEmail(ctx, me, size, note),
	})
	slog.Info("plan size requested", "org", me.OrgPublic, "by", me.Email, "size", size.Key, "sent", err == nil)
	b.audit(r, "billing.size_requested", AuditEvent{TargetKind: "org", TargetID: me.OrgPublic, TargetName: me.OrgName,
		Details: auditDetails(map[string]any{"size": size.Key, "sent": err == nil})})
	if err != nil {
		// The request is the thing that must not be lost: say where it did not get to, so the
		// answer is one address away rather than a support page somebody has to go and find.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": fmt.Sprintf("The request could not be sent (%s). Write to %s and it will be picked up there.", err, b.cfg.SupportEmail)})
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	b.store.PutSetting(ctx, me.OrgID, sizeRequestKey, now)
	b.store.PutSetting(ctx, me.OrgID, sizeRequestSizeKey, size.Key)
	// delivered is false on a laptop with no RESEND_API_KEY, where the mailer only logs: the
	// console then names the address instead of promising an inbox somebody will never see.
	writeJSON(w, 200, map[string]any{"ok": true, "delivered": b.mail.Configured(),
		"support_email": b.cfg.SupportEmail, "requested_at": now})
}

// sizeRequestEmail is the message support reads: plain text, one fact a line, figures first, so
// whoever answers it does not have to open the operator page to know what they are looking at.
func (b *Bot) sizeRequestEmail(ctx context.Context, me *AdminUser, size Size, note string) string {
	st := b.settings.Get(ctx, me.OrgID)
	acct, _ := b.store.BillingAccountOf(ctx, me.OrgID)
	users := b.activeUsers(ctx, me.OrgID, true)
	spend, _ := b.store.MonthSpend(ctx, me.OrgID, "", "")
	jobs := b.store.JobsThisMonth(ctx, me.OrgID)

	plan := st.Plan
	if acct.Exists && acct.Active() {
		label := sizeLabels[acct.Size]
		if label == "" {
			label = acct.Size
		}
		plan = fmt.Sprintf("pro — %s at %s a month (%s)", label, creditAmount(acct.AmountMicros()), acct.Status)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s asked about a larger plan: %s.\n\n", me.OrgName, size.Label)
	fmt.Fprintf(&sb, "Asked by:      %s <%s>, from Settings → Billing\n", nonEmpty(me.Name, "(no name)"), me.Email)
	fmt.Fprintf(&sb, "Organisation:  %s (%s)\n", me.OrgName, me.OrgPublic)
	fmt.Fprintf(&sb, "Current plan:  %s\n", plan)
	fmt.Fprintf(&sb, "People:        %d used the bot in the last %d days", users.Users, users.WindowDays)
	if len(users.ByTeam) > 1 {
		var parts []string
		for _, t := range users.ByTeam {
			parts = append(parts, fmt.Sprintf("%s %d", nonEmpty(t.Name, t.TeamID), t.Users))
		}
		fmt.Fprintf(&sb, " (%s)", strings.Join(parts, ", "))
	}
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "This month:    $%.2f of model spend, %d fix jobs\n", spend, jobs)
	fmt.Fprintf(&sb, "Credit:        %s prepaid, %s of included credit left\n",
		creditAmount(acct.CreditBalanceMicros), creditAmount(acct.SpendableAllowanceMicros()))
	if note != "" {
		fmt.Fprintf(&sb, "\nTheir note:\n%s\n", quoteForMail(note))
	}
	sb.WriteString("\nReply to this message to answer them.\n")
	return sb.String()
}
