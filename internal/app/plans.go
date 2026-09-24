package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Plans. Signup is open, so every organisation starts on the free plan: a fixed monthly model
// budget on the shared key that its own Settings page cannot raise. Pro is the plan the operator
// moves an account to by hand (operator.go); after that its monthly budget is its own to set,
// under the operator's ceiling as before. There is deliberately no self-serve upgrade: the
// console, the Slack refusal and the alert all say to write to support, and a person decides.

//
// Enterprise is the third, and like pro only the operator moves an account onto it. It is a deal
// rather than a rung: the account's own user ceiling, fix jobs, included credit and fee, and how it
// pays, written into enterprise_terms (enterprise.go). Nothing a payment does moves an account on
// or off it — see movePlan — because the plan is the agreement, and a card that fails or a
// subscription somebody cancels is a conversation with a person rather than a demotion.

const (
	PlanFree       = "free"
	PlanPro        = "pro"
	PlanEnterprise = "enterprise"
)

var plans = []string{PlanFree, PlanPro, PlanEnterprise}

// askHint is the "and here is where to ask" half of every budget refusal. One function, because
// the sentence is written into the console, into Slack and into the alert channel, and wording
// that drifts between the three is a support request that never arrives.
//
// Empty address, empty clause. A deployment that publishes no support address — which is every
// self-host until somebody sets SUPPORT_EMAIL — must not send its users to an inbox belonging to
// whoever happened to build the binary. The refusal still says the budget is fixed; it just
// stops short of naming a stranger.
func askHint(email string) string {
	if email == "" {
		return " The budget for this plan is fixed here; ask whoever runs this deployment."
	}
	return " To raise it, email " + email + "."
}

// planOf reads a stored value defensively: anything unrecognised is free, the restrictive
// answer. A typo in the database must not hand an account an unlimited budget.
func planOf(v string) string {
	switch v {
	case PlanPro, PlanEnterprise:
		return v
	}
	return PlanFree
}

// defaultProBudgetUSD is what a pro account may spend a month when the operator moved it
// without naming a figure.
const defaultProBudgetUSD = 25

// budgetCeiling is the monthly spend a plan permits on the shared key. Pro: the budget the
// operator granted with the plan (orgs.plan_budget_usd), or the deployment-wide ceiling when
// none was — the grant is the operator's own decision about this account, so it is not held
// under the deployment-wide figure. Free: the free plan's fixed budget, or the deployment
// ceiling when that is lower. 0 means no ceiling.
//
// creditFunded is the case none of that was written for: an account paying for its own model
// spend out of prepaid credit. The operator's ceiling exists because free accounts spend on a key
// somebody else pays for, and that stops being true the moment a customer has bought the credit —
// a $25 deployment ceiling standing in front of a $500 balance would stop the bot for a customer
// who has already paid, which is the worst refusal this product can produce. So the ceiling does
// not apply, EffectiveBudget falls through to the account's own monthly_budget_usd (0 = none),
// and the balance is what actually stops it (billing.go, routines.go).
//
// It also beats an operator grant, deliberately: a $50 grant made by hand on an account that has
// since paid for $500 is stale, and an operator who wants a paying customer held down sets the
// account's own monthly budget or moves it back to free.
//
// Enterprise is the grant and nothing else. A figure is part of the deal the operator wrote, so
// neither bought credit nor the deployment ceiling moves it; no figure means the deal sets no
// ceiling, and the account's own monthly budget and its credit are what stop it. The deployment
// ceiling is for accounts nobody has made an agreement with, which is the one thing an
// enterprise account is not.
func budgetCeiling(plan string, grant float64, cfg Config, creditFunded bool) float64 {
	if plan == PlanEnterprise {
		return grant
	}
	if creditFunded && plan == PlanPro {
		return 0
	}
	ceiling := cfg.PlatformMonthlyBudgetUSDPerOrg
	if plan == PlanPro {
		if grant > 0 {
			return grant
		}
		return ceiling
	}
	if cfg.FreePlanBudgetUSD <= 0 {
		return ceiling
	}
	if ceiling <= 0 || cfg.FreePlanBudgetUSD < ceiling {
		return cfg.FreePlanBudgetUSD
	}
	return ceiling
}

// raiseBudgetHint is the sentence that follows any budget refusal a free account meets: what
// the plan allows and where to ask for more. Empty for a pro account, whose budget is its own
// setting and whose remedy is the console.
func (s Settings) raiseBudgetHint() string {
	if s.Plan != PlanFree || s.PlatformBudgetUSD <= 0 {
		return ""
	}
	return fmt.Sprintf("Free accounts have a $%.2f monthly budget.%s", s.PlatformBudgetUSD, askHint(s.SupportEmail))
}

// validateBudget is the console's check on monthly_budget_usd before it is stored: a number,
// and no higher than the plan allows. It used to be stored unread and clamped silently at
// spend time, which showed a free account a budget it did not have.
func (b *Bot) validateBudget(ctx context.Context, orgID int64, v string) error {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return errors.New("monthly_budget_usd must be a number of dollars, 0 or more")
	}
	st := b.settings.Get(ctx, orgID)
	if st.PlatformBudgetUSD > 0 && f > st.PlatformBudgetUSD {
		if st.Plan == PlanFree {
			return fmt.Errorf("free accounts have a $%.2f monthly budget.%s", st.PlatformBudgetUSD, askHint(st.SupportEmail))
		}
		return fmt.Errorf("the most an account may spend here is $%.2f a month", st.PlatformBudgetUSD)
	}
	return nil
}

// ---- asking for more ----

// planRequests bounds how often one account may ask for a bigger budget. The ask puts a message
// in a person's inbox at the press of a button, and a button that sends mail is one somebody
// eventually holds down.
var planRequests = newRateLimiter()

const planRequestsPerOrgADay = 3

// planRequestKey remembers that an upgrade was asked for, so the console can say so instead of
// offering the button again. It is per organisation rather than per browser: the banner is on
// every page, and a second press from the next page would be a second message in a person's
// inbox saying what the first one said. Cleared whenever the plan moves.
const planRequestKey = "plan_request_at"

// handleRaiseBudgetRequest is the button under a free account's monthly budget. It sends the
// support request that used to be a `mailto:` the console opened in the requester's mail
// client: the same facts and the same operator link, except the link is written into the
// message here rather than into a draft the requester reads. Nothing about the account's own
// screen should show it the URL that moves plans.
//
// Behind PermSettingsManage, the permission on the field it is asking to open: the answer moves
// the account to Pro and unlocks monthly_budget_usd, so the person asking should be one who
// could then set it. The mail leaves from the deployment's own sending address with the
// requester on Reply-To, which is also why this sits behind a session at all — the address is
// the one they signed in with, proved by the account rather than typed into a form.
func (b *Bot) handleRaiseBudgetRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := adminFromCtx(ctx)
	st := b.settings.Get(ctx, me.OrgID)
	if st.Plan != PlanFree {
		bad(w, fmt.Errorf("this account is already on the %s plan; its monthly budget is its own to set", st.Plan))
		return
	}
	// On a deployment that sells plans from the console this route is the wrong answer: it asks a
	// person to do by hand, over hours, what a card does in thirty seconds, and leaves whoever
	// reads that inbox answering it forever. The console stops offering the button, and this
	// catches the stale tab, the old bookmark and the script.
	if b.cfg.BillingEnabled() {
		bad(w, errors.New("this deployment sells plans from the console — open Settings → Billing and pick a size"))
		return
	}
	// No address, no button. This route exists to put a message in somebody's inbox, and a
	// deployment that named no inbox has nobody to put it in — sending it anyway would mail a
	// stranger. The console hides the button on the same condition (env.support_email).
	if b.cfg.SupportEmail == "" {
		bad(w, errors.New("this deployment publishes no support address; ask whoever runs it to raise the budget"))
		return
	}
	if ok, d := planRequests.allow("plan-request:"+me.OrgPublic, planRequestsPerOrgADay, 24*time.Hour); !ok {
		tooMany(w, d)
		return
	}
	spend, _ := b.store.MonthSpend(ctx, me.OrgID, "", "")
	link := b.baseURL(r) + "/operator/plan?org=" + me.OrgPublic
	err := b.mail.Send(ctx, Mail{
		To: b.cfg.SupportEmail, ReplyTo: me.Email,
		Subject: "Raise the monthly budget for " + me.OrgName,
		Body:    raiseBudgetEmail(me, st.EffectiveBudget(), spend, link),
	})
	slog.Info("budget raise requested", "org", me.OrgPublic, "by", me.Email, "sent", err == nil)
	if err == nil {
		b.store.PutSetting(ctx, me.OrgID, planRequestKey, time.Now().UTC().Format(time.RFC3339))
	}
	if err != nil {
		// The request is the thing that must not be lost. Say where it did not get to, so the
		// answer is one address away rather than a support page somebody has to go and find.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": fmt.Sprintf("The request could not be sent (%s). Write to %s and it will be picked up there.", err, b.cfg.SupportEmail)})
		return
	}
	// delivered is false on a laptop with no RESEND_API_KEY, where the mailer only logs: the
	// console then names the address instead of promising an inbox somebody will never see.
	writeJSON(w, 200, map[string]any{"ok": true, "delivered": b.mail.Configured(), "support_email": b.cfg.SupportEmail})
}
