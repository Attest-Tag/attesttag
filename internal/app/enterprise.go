package app

// The enterprise plan: an account the operator has written a deal for.
//
// The ladder in STRIPE_SIZES ends at "Talk to us". What the conversation ends in is this: the
// operator moves the account to the enterprise plan and writes its figures — how many people, how
// many fix jobs a month, what model credit it includes each month, what it costs — and how it
// pays. Three ways, and the operator picks one per account:
//
//   - A Stripe Price made for this account (price_id). The account's admin presses Subscribe on
//     Settings → Billing, pays through Checkout like any other plan, and the webhook keeps the
//     subscription on record — renewal date, card failures, cancellation — exactly as it does for
//     a size on the ladder.
//   - A link (pay_url): a Stripe Payment Link, a hosted invoice, anything with a page. Billing
//     shows a Pay button that opens it. Nothing here can see those payments, so they are
//     reconciled by hand.
//   - Neither: invoiced by hand, outside this product. Billing says so and offers nothing to press.
//
// Credit is the same credit every plan has: the operator grants it (operator_billing.go) and the
// account can buy more. The deal's included credit is a monthly allowance like a size's.
//
// What no payment ever does is move an account off this plan. movePlan refuses to, and a lapsed
// subscription mails support instead: the plan is the agreement, and a failed card or a cancelled
// subscription on an enterprise account is a conversation with a person, not a demotion to free.
//
// Everything that writes here is behind OPERATOR_SECRET. A member of the account reads the deal on
// Billing and can change none of it, which TestNoConsoleRouteCanMoveAPlanOrGrantCredit holds.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strings"
)

// statusInvoiced is the billing status of an enterprise account that pays outside Stripe
// subscriptions — by a link or by hand. It is live (BillingAccount.Active), so the deal's user
// ceiling and included credit apply, and it is deliberately not "comped": an invoiced account is
// being charged, just not by anything this deployment can see.
const statusInvoiced = "invoiced"

// Bounds on what one change may write. Not security controls — whoever holds the operator secret
// can write the figure twice — but a typed number with three extra zeros is the mistake these
// catch, and each says what it caught.
const (
	maxEnterpriseUsers   = 1_000_000
	maxEnterpriseJobs    = 1_000_000
	maxEnterpriseFeeUSD  = 10_000_000
	maxEnterpriseNote    = 500
	maxEnterprisePayURL  = 2000
	maxEnterprisePriceID = 255
)

// enterpriseChange is what the operator sends. Every field is a pointer so that absent means
// "leave it as it is": changing the size of a deal is `{"plan":"enterprise","users":600}`, not a
// resend of everything else, and a terminal command can say only the thing it is changing. To
// clear a text field, send it empty.
type enterpriseChange struct {
	// BudgetUSD is the monthly ceiling on model spend, as for pro. 0 means the deal sets none,
	// and the account's own monthly budget and its credit are what stop it.
	BudgetUSD   *float64 `json:"budget_usd"`
	Users       *int     `json:"users"`
	Jobs        *int     `json:"jobs"`
	FeeUSD      *float64 `json:"fee_usd"`
	Interval    *string  `json:"interval"`
	IncludedUSD *float64 `json:"included_usd"`
	PriceID     *string  `json:"price_id"`
	PayURL      *string  `json:"pay_url"`
	Note        *string  `json:"note"`
}

// setEnterprise moves an organisation onto the enterprise plan, or changes the deal of one that is
// already on it. The one write behind the JSON route, the operator page and deploy/plan.sh.
func (b *Bot) setEnterprise(ctx context.Context, org *Org, ch enterpriseChange, by string) (EnterpriseTerms, error) {
	cur, err := b.store.EnterpriseTerms(ctx, org.ID)
	if err != nil {
		return EnterpriseTerms{}, err
	}
	t := cur
	t.OrgID, t.UpdatedBy = org.ID, by

	budget := 0.0
	if org.Plan == PlanEnterprise {
		budget = org.PlanBudgetUSD
	}
	if ch.BudgetUSD != nil {
		budget = *ch.BudgetUSD
	}
	if math.IsNaN(budget) || budget < 0 || budget > maxPlanBudgetUSD {
		return EnterpriseTerms{}, fmt.Errorf("budget_usd must be between 0 (no ceiling) and %d dollars a month", maxPlanBudgetUSD)
	}
	if ch.Users != nil {
		if *ch.Users < 0 || *ch.Users > maxEnterpriseUsers {
			return EnterpriseTerms{}, fmt.Errorf("users must be between 0 (no ceiling) and %d", maxEnterpriseUsers)
		}
		t.UserLimit = *ch.Users
	}
	if ch.Jobs != nil {
		if *ch.Jobs < 0 || *ch.Jobs > maxEnterpriseJobs {
			return EnterpriseTerms{}, fmt.Errorf("jobs must be between 0 (no figure) and %d a month", maxEnterpriseJobs)
		}
		t.JobLimit = *ch.Jobs
	}
	if ch.IncludedUSD != nil {
		minor, err := dollarsToMinor(*ch.IncludedUSD, maxGrantUSD, "included_usd")
		if err != nil {
			return EnterpriseTerms{}, err
		}
		t.IncludedMinor = minor
	}
	if ch.FeeUSD != nil {
		minor, err := dollarsToMinor(*ch.FeeUSD, maxEnterpriseFeeUSD, "fee_usd")
		if err != nil {
			return EnterpriseTerms{}, err
		}
		t.FeeMinor = minor
	}
	if ch.Interval != nil {
		switch iv := strings.ToLower(strings.TrimSpace(*ch.Interval)); iv {
		case "month", "year":
			t.FeeInterval = iv
		default:
			return EnterpriseTerms{}, errors.New(`interval must be "month" or "year"`)
		}
	}
	if t.FeeInterval == "" {
		t.FeeInterval = "month"
	}
	if ch.Note != nil {
		note := strings.TrimSpace(*ch.Note)
		if len(note) > maxEnterpriseNote {
			return EnterpriseTerms{}, fmt.Errorf("the note is %d characters; keep it under %d", len(note), maxEnterpriseNote)
		}
		t.Note = note
	}

	// How it pays. A price and a link are alternatives, so naming one clears the other unless the
	// same change names both — which is refused rather than guessed at.
	price, link := t.PriceID, t.PayURL
	if ch.PriceID != nil {
		price = strings.TrimSpace(*ch.PriceID)
		if ch.PayURL == nil && price != "" {
			link = ""
		}
	}
	if ch.PayURL != nil {
		link = strings.TrimSpace(*ch.PayURL)
		if ch.PriceID == nil && link != "" {
			price = ""
		}
	}
	if price != "" && link != "" {
		return EnterpriseTerms{}, errors.New("give a price_id to subscribe to or a pay_url to pay at, not both")
	}
	if err := validPriceID(price); err != nil {
		return EnterpriseTerms{}, err
	}
	if err := validPayURL(link); err != nil {
		return EnterpriseTerms{}, err
	}
	t.PriceID, t.PayURL = price, link

	// A subscription already being billed decides two things. One on another plan's price would go
	// on charging it under a deal that says something else, so it has to end first. And one on
	// this deal's price is how the deal is being paid, so its price cannot be swapped from here.
	var acct BillingAccount
	if b.cfg.BillingEnabled() {
		if acct, err = b.store.BillingAccountOf(ctx, org.ID); err != nil {
			return EnterpriseTerms{}, err
		}
	}
	live := acct.SubscriptionID != "" && acct.Active()
	switch {
	case live && acct.Size != sizeEnterprise:
		label := nonEmpty(sizeLabels[acct.Size], acct.Size)
		return EnterpriseTerms{}, fmt.Errorf("this account has a live subscription at Stripe (%s); cancel it first, or it goes on being charged beside the deal", nonEmpty(label, "a plan size"))
	case live && t.PriceID != cur.PriceID:
		return EnterpriseTerms{}, errors.New("this account is subscribed to its enterprise price at Stripe; cancel that subscription before changing how it pays")
	}

	// A new price is read from Stripe now, while the operator is looking, rather than when the
	// customer presses Subscribe and meets a typo. The fee and its interval then come from the
	// price, whatever was typed: the figure on the screen has to be the one that is charged, so
	// an unchanged price keeps the figures it was read with.
	switch {
	case t.PriceID == "":
	case t.PriceID != cur.PriceID:
		if !b.cfg.BillingEnabled() {
			return EnterpriseTerms{}, errors.New("this deployment has no Stripe keys, so there is no price to subscribe to; give a pay_url, or leave both out and invoice by hand")
		}
		p, err := b.pay.Price(ctx, t.PriceID)
		if err != nil {
			return EnterpriseTerms{}, fmt.Errorf("could not read %s from Stripe: %w", t.PriceID, err)
		}
		if err := enterprisePriceOK(p, b.cfg.BillingCurrency); err != nil {
			return EnterpriseTerms{}, err
		}
		t.FeeMinor, t.FeeInterval = p.UnitAmount, p.Interval
	default:
		t.FeeMinor, t.FeeInterval = cur.FeeMinor, cur.FeeInterval
	}

	// The plan first: it is the part that decides what the account may spend, and applyPlan is the
	// one place a plan moves. Only when something about it changed, so re-saving the deal does not
	// write a "plan changed" line into the account's log for a plan that did not change.
	if org.Plan != PlanEnterprise || budget != org.PlanBudgetUSD {
		if err := b.applyPlan(ctx, org, planChange{Plan: PlanEnterprise, BudgetUSD: budget,
			SetBudget: budget > 0, By: by}); err != nil {
			return EnterpriseTerms{}, err
		}
	}
	if err := b.store.PutEnterpriseTerms(ctx, t); err != nil {
		return EnterpriseTerms{}, err
	}
	t.Exists = true

	// The billing record, on a deployment that has one. What it says depends on how the deal is
	// paid: a live subscription keeps the record Stripe is writing; a price nobody has subscribed
	// to yet is a size with nothing billed, so nothing is included until they pay; anything else
	// is invoiced, which is live from now.
	if b.cfg.BillingEnabled() && !live {
		st := SubscriptionState{CustomerID: acct.CustomerID, Size: sizeEnterprise,
			UnitPriceMicros: minorToMicros(t.FeeMinor), Quantity: 1,
			Currency: nonEmpty(acct.Currency, b.cfg.BillingCurrency)}
		if t.PriceID == "" {
			st.Status = statusInvoiced
		}
		if err := b.store.PutSubscription(ctx, org.ID, st); err != nil {
			return EnterpriseTerms{}, err
		}
	}
	if b.cfg.BillingEnabled() {
		b.syncAllowance(ctx, org.ID)
	}
	b.settings.Invalidate(org.ID)
	// The account's own log, which its admins read: the deal's figures and how it is paid, never
	// the operator's note, which is written for the operator and may name people.
	b.auditOperator(ctx, org.ID, by, "plan.enterprise_terms", AuditEvent{
		TargetKind: "org", TargetID: org.PublicID, TargetName: org.Name,
		Details: auditDetails(map[string]any{"users": t.UserLimit, "jobs": t.JobLimit,
			"fee_usd": float64(t.FeeMinor) / 100, "interval": t.FeeInterval,
			"included_usd": float64(t.IncludedMinor) / 100, "paid_by": t.paidBy(),
			"price_id": t.PriceID, "budget_usd": budget})})
	slog.Info("enterprise terms set", "org", org.PublicID, "users", t.UserLimit, "jobs", t.JobLimit,
		"fee_minor", t.FeeMinor, "interval", t.FeeInterval, "included_minor", t.IncludedMinor,
		"paid_by", t.paidBy(), "budget_usd", budget, "by", by)
	return t, nil
}

// leaveEnterprise is what moving an account off the enterprise plan does to the deal, called by
// applyPlan before anything is written. The deal goes, so nothing stale is waiting to come back,
// and an invoiced record ends so the deal's allowance and user ceiling stop with it.
//
// Refused while the deal is being paid by a live Stripe subscription: the plan would say free
// while Stripe went on charging the enterprise price. Cancel it first (the operator page does),
// and the account stays on enterprise until then.
func (b *Bot) leaveEnterprise(ctx context.Context, org *Org) error {
	if !b.cfg.BillingEnabled() {
		return b.store.DeleteEnterpriseTerms(ctx, org.ID)
	}
	acct, err := b.store.BillingAccountOf(ctx, org.ID)
	if err != nil {
		return err
	}
	if acct.Size == sizeEnterprise && acct.SubscriptionID != "" && acct.Active() {
		return errors.New("this account is still subscribed to its enterprise price at Stripe; cancel the subscription first, and move it once that has gone through")
	}
	if err := b.store.DeleteEnterpriseTerms(ctx, org.ID); err != nil {
		return err
	}
	if acct.Size == sizeEnterprise {
		status := acct.Status
		if status == statusInvoiced {
			status = "canceled"
		}
		if err := b.store.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: acct.CustomerID,
			SubscriptionID: acct.SubscriptionID, Status: status, Quantity: acct.Quantity,
			Currency: acct.Currency, PeriodStart: acct.PeriodStart, PeriodEnd: acct.PeriodEnd}); err != nil {
			return err
		}
		b.syncAllowance(ctx, org.ID)
	}
	return nil
}

// sizeOf is SizeByKey for one account: the configured ladder, or the account's own deal when the
// key is the enterprise one. Everything that turns a size into a fee, an allowance or a ceiling
// asks this, so an enterprise account is read by the same code as every other.
func (b *Bot) sizeOf(ctx context.Context, orgID int64, key string) (Size, bool) {
	if key != sizeEnterprise {
		return b.cfg.SizeByKey(key)
	}
	t, err := b.store.EnterpriseTerms(ctx, orgID)
	if err != nil || !t.Exists {
		return Size{}, false
	}
	return t.size(), true
}

// sizeByPrice is SizeByPrice for one account, for the events that name a Price and not a size: a
// Price this deployment sells, or this account's own enterprise Price.
func (b *Bot) sizeByPrice(ctx context.Context, orgID int64, priceID string) (Size, bool) {
	if size, ok := b.cfg.SizeByPrice(priceID); ok {
		return size, true
	}
	if priceID == "" {
		return Size{}, false
	}
	t, err := b.store.EnterpriseTerms(ctx, orgID)
	if err != nil || !t.Exists || t.PriceID != priceID {
		return Size{}, false
	}
	return t.size(), true
}

// enterpriseLapsed tells the operator that an enterprise subscription has stopped being paid, once
// per change of status. The account stays on its plan — movePlan will not demote it — so without
// this the first anybody heard of it would be the next invoice that never came.
func (b *Bot) enterpriseLapsed(ctx context.Context, org *Org, status string) {
	b.alertOperator(fmt.Sprintf("the enterprise subscription for %s (%s) is now %s; the account stays on the enterprise plan until the operator moves it", org.Name, org.PublicID, status))
	if b.cfg.SupportEmail == "" {
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "The enterprise subscription for %s is now %s.\n\n", org.Name, status)
	sb.WriteString("The account is still on the enterprise plan: a payment never moves an enterprise account, so it keeps its\n")
	sb.WriteString("deal until somebody decides otherwise. Its included credit has stopped with the subscription.\n\n")
	fmt.Fprintf(&sb, "Organisation:  %s (%s)\n", org.Name, org.PublicID)
	if base := publicBaseURL(ctx, b.store, b.cfg); base != "" {
		fmt.Fprintf(&sb, "Operator page: %s/operator/plan?org=%s\n", base, org.PublicID)
	}
	if err := b.mail.Send(ctx, Mail{To: b.cfg.SupportEmail,
		Subject: "Enterprise subscription " + status + ": " + org.Name, Body: sb.String()}); err != nil {
		slog.Warn("could not mail support about a lapsed enterprise subscription", "org", org.PublicID, "err", err)
	}
}

// enterpriseView is the deal as the account sees it on Billing. The operator's note is not in it.
type enterpriseView struct {
	UserLimit   int     `json:"user_limit"`
	JobLimit    int     `json:"job_limit"`
	FeeUSD      float64 `json:"fee_usd"`
	Interval    string  `json:"interval"`
	IncludedUSD float64 `json:"included_usd"`
	// PaidBy is "subscription", "link" or "invoice"; see EnterpriseTerms.paidBy.
	PaidBy string `json:"paid_by"`
	// PayURL is the link, for members who may pay. Absent otherwise.
	PayURL string `json:"pay_url,omitempty"`
	// Subscribed is a live Stripe subscription to the deal's price: the Subscribe button's job is
	// done, and the renewal date is on the subscription.
	Subscribed bool `json:"subscribed"`
}

func enterpriseViewOf(t EnterpriseTerms, acct BillingAccount, canManage bool) *enterpriseView {
	v := &enterpriseView{UserLimit: t.UserLimit, JobLimit: t.JobLimit, FeeUSD: float64(t.FeeMinor) / 100,
		Interval: nonEmpty(t.FeeInterval, "month"), IncludedUSD: float64(t.IncludedMinor) / 100,
		PaidBy:     t.paidBy(),
		Subscribed: acct.Size == sizeEnterprise && acct.SubscriptionID != "" && acct.Active()}
	if canManage {
		v.PayURL = t.PayURL
	}
	return v
}

// enterprisePriceOK is the check on a Price before a customer is sent to subscribe to it.
func enterprisePriceOK(p PriceInfo, currency string) error {
	switch {
	case !p.Active:
		return fmt.Errorf("%s is archived at Stripe; nobody can subscribe to it", p.ID)
	case !p.Recurring:
		return fmt.Errorf("%s is a one-off price; the deal needs a recurring one, or invoice the one-off by hand", p.ID)
	case p.Tiered || p.UnitAmount <= 0:
		return fmt.Errorf("%s has no single amount; the deal needs a flat price", p.ID)
	case p.IntervalN != 1 || (p.Interval != "month" && p.Interval != "year"):
		return fmt.Errorf("%s renews every %d %s; the deal needs a monthly or a yearly price", p.ID, p.IntervalN, p.Interval)
	case !strings.EqualFold(p.Currency, currency):
		return fmt.Errorf("%s is in %s and this deployment holds %s", p.ID, p.Currency, currency)
	}
	return nil
}

func validPriceID(id string) error {
	if id == "" {
		return nil
	}
	if !strings.HasPrefix(id, "price_") || len(id) > maxEnterprisePriceID || strings.ContainsAny(id, " \t\r\n/?#") {
		return errors.New("price_id must be a Stripe Price id, price_…")
	}
	return nil
}

// validPayURL keeps the link a page somebody can be sent to: https, a host, nothing that runs. It
// is the operator's own link, and the console renders it as a button, so the check is less about
// trust than about a paste that picked up the wrong thing.
func validPayURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(raw) > maxEnterprisePayURL {
		return errors.New("pay_url must be an https:// link to the page the account pays at")
	}
	return nil
}

// dollarsToMinor reads a figure the operator typed into minor units, refusing what is not money.
func dollarsToMinor(usd, max float64, field string) (int64, error) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 || usd > max {
		return 0, fmt.Errorf("%s must be between 0 and %.0f dollars", field, max)
	}
	return int64(math.Round(usd * 100)), nil
}
