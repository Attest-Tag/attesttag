package app

// The operator's money actions: grant credit without a payment, set the size on a comped
// subscription, cancel a real one, and the index that lists every account.
//
// All of it is behind OPERATOR_SECRET and none of it is reachable from a console session. That
// separation is the point rather than an accident of where the code lives: a member who could
// move their own plan or grant their own credit would make the whole commercial model advisory.
// TestNoConsoleRouteCanMoveAPlanOrGrantCredit asserts it.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxGrantUSD bounds one hand-granted amount. Not a security control — whoever holds the operator
// secret can press the button twice — but a typed figure with three extra zeros in it is the
// mistake this catches, and a second press is a deliberate act.
const maxGrantUSD = 100000

// grantCredit is the one write behind every "give this account credit" route. It writes a ledger
// row like any other movement, so a grant and a payment are the same kind of thing on the
// statement and the balance is still a plain sum.
//
// It turns the credit floor ON, like a payment does. A comped account with $100 stops at zero the
// same way a paying one does — which is the whole reason for granting a figure rather than a plan.
func (b *Bot) grantCredit(ctx context.Context, org *Org, amountUSD float64, note, by string) (float64, error) {
	if math.IsNaN(amountUSD) || math.IsInf(amountUSD, 0) || amountUSD == 0 {
		return 0, errors.New("the amount must be a number of dollars, and not zero")
	}
	if amountUSD > maxGrantUSD || amountUSD < -maxGrantUSD {
		return 0, fmt.Errorf("one grant may be at most $%d; press it twice if you mean it", maxGrantUSD)
	}
	kind, enforce := creditGrant, true
	if amountUSD < 0 {
		// Taking credit back is a correction, not a grant, and it must not switch enforcement on
		// for an account that never had any.
		kind, enforce = creditAdjustment, false
	}
	micros := int64(math.Round(amountUSD * microsPerUSD))
	bal, err := b.store.MoveCredit(ctx, CreditMovement{OrgID: org.ID, Kind: kind, Micros: micros,
		ExternalID: kind + ":" + idempotencyKey(), Note: note, Actor: by,
		Currency: b.cfg.BillingCurrency, Enforce: enforce})
	if err != nil {
		return 0, err
	}
	b.store.ClearAlert(ctx, org.ID, alertLowCredit)
	b.settings.Invalidate(org.ID)
	b.auditOperator(ctx, org.ID, by, "billing.credit."+kind+"ed", AuditEvent{
		TargetKind: "org", TargetID: org.PublicID, TargetName: org.Name,
		Details: auditDetails(map[string]any{"amount_usd": amountUSD, "balance_usd": microsToUSD(bal), "note": note})})
	return microsToUSD(bal), nil
}

// compSize records a size, and its fee, on an account with no Stripe subscription behind it. It
// is what "on Pro because we said so" looks like in the record: status comped, so a reader can
// tell it from an account that is actually being charged, and every other page reads it the same
// way a paid one.
func (b *Bot) compSize(ctx context.Context, org *Org, sizeKey, by string) error {
	acct, err := b.store.BillingAccountOf(ctx, org.ID)
	if err != nil {
		return err
	}
	if acct.SubscriptionID != "" {
		return errors.New("this account has a live subscription at Stripe; change the size there, or cancel it first")
	}
	// An enterprise account's size is its deal. A rung of the ladder recorded over it would leave
	// the plan saying enterprise and the record saying something else.
	if org.Plan == PlanEnterprise {
		return errors.New("this account is on the enterprise plan; change its users in the enterprise deal instead")
	}
	size, ok := b.cfg.SizeByKey(sizeKey)
	if !ok {
		return errors.New("that is not a size this deployment sells")
	}
	if err := b.store.PutSubscription(ctx, org.ID, SubscriptionState{
		CustomerID: acct.CustomerID, Status: "comped", Size: size.Key,
		UnitPriceMicros: minorToMicros(size.AmountMinor), Quantity: 1,
		Currency: b.cfg.BillingCurrency}); err != nil {
		return err
	}
	// A comped size includes what the size includes. The operator gave them the plan; giving them
	// the plan and not what it comes with would be a surprise on the first screen they look at.
	b.syncAllowance(ctx, org.ID)
	b.settings.Invalidate(org.ID)
	b.auditOperator(ctx, org.ID, by, "billing.size.set", AuditEvent{
		TargetKind: "org", TargetID: org.PublicID, TargetName: org.Name,
		Details: auditDetails(map[string]any{"size": size.Key, "fee_usd": float64(size.AmountMinor) / 100, "comped": true})})
	return nil
}

// cancelSubscription stops a real subscription at Stripe and lets the webhook do the rest. It
// deliberately does not write the plan itself: Stripe answering customer.subscription.deleted is
// what moves it, so there is one path into applyPlan and no chance of this page and that event
// disagreeing about what happened.
func (b *Bot) cancelSubscription(ctx context.Context, org *Org, by string) error {
	acct, err := b.store.BillingAccountOf(ctx, org.ID)
	if err != nil {
		return err
	}
	if acct.SubscriptionID == "" {
		return errors.New("this account has no subscription at Stripe")
	}
	if err := b.pay.CancelSubscription(ctx, acct.SubscriptionID); err != nil {
		return err
	}
	b.auditOperator(ctx, org.ID, by, "billing.subscription.canceled", AuditEvent{
		TargetKind: "org", TargetID: org.PublicID, TargetName: org.Name,
		Details: auditDetails(map[string]any{"subscription": acct.SubscriptionID, "by": "operator"})})
	return nil
}

// ---- the JSON twins, for deploy/plan.sh and a terminal ----

func (b *Bot) handleOperatorCredit(w http.ResponseWriter, r *http.Request) {
	if !b.cfg.BillingEnabled() {
		http.NotFound(w, r)
		return
	}
	var in struct {
		AmountUSD float64 `json:"amount_usd"`
		Note      string  `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	org, err := b.store.OrgByPublicID(r.Context(), r.PathValue("id"))
	if err != nil || org == nil {
		writeJSON(w, 404, map[string]any{"error": errNoSuchOrg.Error()})
		return
	}
	bal, err := b.grantCredit(r.Context(), org, in.AmountUSD, nonEmpty(in.Note, "Granted by the operator"), operatorActor)
	if err != nil {
		bad(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": org.PublicID, "name": org.Name, "credit_balance_usd": bal})
}

func (b *Bot) handleOperatorSize(w http.ResponseWriter, r *http.Request) {
	if !b.cfg.BillingEnabled() {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Size string `json:"size"`
	}
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	org, err := b.store.OrgByPublicID(r.Context(), r.PathValue("id"))
	if err != nil || org == nil {
		writeJSON(w, 404, map[string]any{"error": errNoSuchOrg.Error()})
		return
	}
	if err := b.compSize(r.Context(), org, in.Size, operatorActor); err != nil {
		bad(w, err)
		return
	}
	writeJSON(w, 200, b.operatorOrgJSON(r.Context(), r, *org))
}

// ---- the dashboard ----

// handleOperatorIndex is the list of accounts. Like the plan page it shows nothing at all until
// the secret is in hand: a page that named every tenant to whoever held the URL would be worse
// than the single-account page it grew out of, not better.
func (b *Bot) handleOperatorIndex(w http.ResponseWriter, r *http.Request) {
	if b.cfg.OperatorSecret == "" {
		http.NotFound(w, r)
		return
	}
	ok, why := b.operatorOK(r)
	v := operatorPlanView{Index: true, Remembered: ok, Query: strings.TrimSpace(r.URL.Query().Get("q"))}
	if why == "unauthorized" {
		b.forgetOperator(w, r)
		v.Error = "This browser's operator cookie no longer matches the secret. Enter it again."
	}
	if ok {
		b.fillOperatorIndex(r.Context(), &v)
	}
	b.renderOperatorPlan(w, http.StatusOK, v)
}

// handleOperatorUnlock takes the secret, remembers it in this browser, and shows the list. It
// changes nothing about any account — it is the door, not a decision.
func (b *Bot) handleOperatorUnlock(w http.ResponseWriter, r *http.Request) {
	if b.cfg.OperatorSecret == "" {
		http.NotFound(w, r)
		return
	}
	v := operatorPlanView{Index: true, Query: strings.TrimSpace(r.PostFormValue("q"))}
	ok, why := b.operatorOK(r)
	if !ok {
		status := http.StatusUnauthorized
		switch why {
		case "throttled":
			status, v.Error = http.StatusTooManyRequests, "Too many attempts from this address. Try again in an hour."
		case "none":
			v.Error = "The operator secret is needed."
		default:
			b.forgetOperator(w, r)
			v.Error = "That is not the operator secret."
		}
		b.renderOperatorPlan(w, status, v)
		return
	}
	if val := operatorCookieValue(b.cfg.OperatorSecret, time.Now().Add(operatorCookieTTL)); val != "" {
		http.SetCookie(w, &http.Cookie{Name: operatorCookie, Value: val, Path: "/operator",
			HttpOnly: true, MaxAge: int(operatorCookieTTL / time.Second), SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
	}
	v.Remembered = true
	b.fillOperatorIndex(r.Context(), &v)
	b.renderOperatorPlan(w, http.StatusOK, v)
}

func (b *Bot) fillOperatorIndex(ctx context.Context, v *operatorPlanView) {
	orgs, err := b.store.Orgs(ctx)
	if err != nil {
		v.Error = err.Error()
		return
	}
	q := strings.ToLower(v.Query)
	// An address as well as a name, because the support mailbox knows who wrote and not always
	// which account they meant — the same reason ?email= exists on the JSON route.
	var byEmail map[int64]bool
	if strings.Contains(q, "@") {
		byEmail = map[int64]bool{}
		if u, _ := b.store.UserByEmail(ctx, v.Query); u != nil {
			ms, _ := b.store.MembershipsFor(ctx, u.ID)
			for _, m := range ms {
				byEmail[m.OrgID] = true
			}
		}
	}
	for _, o := range orgs {
		switch {
		case byEmail != nil:
			if !byEmail[o.ID] {
				continue
			}
		case q != "":
			if !strings.Contains(strings.ToLower(o.Name), q) && !strings.Contains(strings.ToLower(o.Slug), q) && o.PublicID != v.Query {
				continue
			}
		}
		row := operatorOrgRow{ID: o.PublicID, Name: o.Name, Slug: o.Slug, Plan: o.Plan}
		row.SpendUSD, _ = b.store.MonthSpend(ctx, o.ID, "", "")
		row.BudgetUSD = b.settings.Get(ctx, o.ID).EffectiveBudget()
		members, _ := b.store.MembersOf(ctx, o.ID)
		row.Members = len(members)
		active := b.activeUsers(ctx, o.ID, false)
		row.ActiveUsers, row.OverLimit = active.Users, active.Over
		if b.cfg.BillingEnabled() {
			if acct, err := b.store.BillingAccountOf(ctx, o.ID); err == nil && acct.Exists {
				row.CreditUSD, row.CreditOn, row.Subscription = microsToUSD(acct.CreditBalanceMicros), acct.CreditEnforced, acct.Status
			}
		}
		v.Orgs = append(v.Orgs, row)
	}
	v.Billing = b.cfg.BillingEnabled()
}

// ---- the forms ----

// operatorForm is the preamble every POST on this page shares: the secret, the cookie, and the
// organisation. It returns nil when it has already written a response.
func (b *Bot) operatorForm(w http.ResponseWriter, r *http.Request) (*Org, *operatorPlanView) {
	return b.operatorFormFor(w, r, true)
}

// operatorFormFor is operatorForm for a form that may or may not need billing. The money forms do
// — without Stripe keys there is no credit to grant — and the enterprise deal does not.
func (b *Bot) operatorFormFor(w http.ResponseWriter, r *http.Request, needBilling bool) (*Org, *operatorPlanView) {
	if b.cfg.OperatorSecret == "" || (needBilling && !b.cfg.BillingEnabled()) {
		http.NotFound(w, r)
		return nil, nil
	}
	id := strings.TrimSpace(r.PostFormValue("org"))
	v := &operatorPlanView{OrgID: id}
	ok, why := b.operatorOK(r)
	if !ok {
		status := http.StatusUnauthorized
		switch why {
		case "throttled":
			status, v.Error = http.StatusTooManyRequests, "Too many attempts from this address. Try again in an hour."
		case "none":
			v.Error = "The operator secret is needed."
		default:
			b.forgetOperator(w, r)
			v.Error = "That is not the operator secret."
		}
		b.renderOperatorPlan(w, status, *v)
		return nil, nil
	}
	if val := operatorCookieValue(b.cfg.OperatorSecret, time.Now().Add(operatorCookieTTL)); val != "" && r.PostFormValue("secret") != "" {
		http.SetCookie(w, &http.Cookie{Name: operatorCookie, Value: val, Path: "/operator",
			HttpOnly: true, MaxAge: int(operatorCookieTTL / time.Second), SameSite: http.SameSiteLaxMode, Secure: b.secureCookies(r)})
	}
	v.Remembered = true
	org, err := b.store.OrgByPublicID(r.Context(), id)
	if err != nil || org == nil {
		v.Error = "There is no organisation with that id."
		b.renderOperatorPlan(w, http.StatusNotFound, *v)
		return nil, nil
	}
	return org, v
}

// finish re-reads the account and renders, so the page a press returns to is the state the press
// produced rather than the one it started from.
func (b *Bot) finish(w http.ResponseWriter, ctx context.Context, v *operatorPlanView, status int) {
	b.fillOperatorOrg(ctx, v)
	b.renderOperatorPlan(w, status, *v)
}

func (b *Bot) handleOperatorCreditForm(w http.ResponseWriter, r *http.Request) {
	org, v := b.operatorForm(w, r)
	if org == nil {
		return
	}
	amount, err := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("amount_usd")), 64)
	if err != nil {
		v.Error = "The amount has to be a number of dollars."
		b.finish(w, r.Context(), v, http.StatusBadRequest)
		return
	}
	bal, err := b.grantCredit(r.Context(), org, amount, nonEmpty(strings.TrimSpace(r.PostFormValue("note")), "Granted by the operator"), operatorActor)
	if err != nil {
		v.Error = err.Error()
		b.finish(w, r.Context(), v, http.StatusBadRequest)
		return
	}
	v.Notice = fmt.Sprintf("$%.2f of credit granted to %s. The balance is now $%.2f, and the credit floor is on.", amount, org.Name, bal)
	b.finish(w, r.Context(), v, http.StatusOK)
}

func (b *Bot) handleOperatorSizeForm(w http.ResponseWriter, r *http.Request) {
	org, v := b.operatorForm(w, r)
	if org == nil {
		return
	}
	if err := b.compSize(r.Context(), org, r.PostFormValue("size"), operatorActor); err != nil {
		v.Error = err.Error()
		b.finish(w, r.Context(), v, http.StatusBadRequest)
		return
	}
	v.Notice = org.Name + " is recorded on that size, comped — nothing is being charged for it."
	b.finish(w, r.Context(), v, http.StatusOK)
}

func (b *Bot) handleOperatorCancelForm(w http.ResponseWriter, r *http.Request) {
	org, v := b.operatorForm(w, r)
	if org == nil {
		return
	}
	if err := b.cancelSubscription(r.Context(), org, operatorActor); err != nil {
		v.Error = err.Error()
		b.finish(w, r.Context(), v, http.StatusBadRequest)
		return
	}
	v.Notice = "The subscription is cancelled at Stripe. The plan moves when their confirmation arrives, usually within seconds."
	b.finish(w, r.Context(), v, http.StatusOK)
}

// handleOperatorEnterpriseForm is the Enterprise card: the whole deal, as the form shows it.
func (b *Bot) handleOperatorEnterpriseForm(w http.ResponseWriter, r *http.Request) {
	org, v := b.operatorFormFor(w, r, false)
	if org == nil {
		return
	}
	ch, err := enterpriseChangeFromForm(r)
	if err != nil {
		v.Error = err.Error()
		b.finish(w, r.Context(), v, http.StatusBadRequest)
		return
	}
	t, err := b.setEnterprise(r.Context(), org, ch, operatorActor)
	if err != nil {
		v.Error = err.Error()
		b.finish(w, r.Context(), v, http.StatusBadRequest)
		return
	}
	how := map[string]string{"subscription": "it subscribes from its Billing screen",
		"link": "its Billing screen offers the link to pay at", "invoice": "it is invoiced by hand"}[t.paidBy()]
	v.Notice = fmt.Sprintf("%s is on the enterprise plan, and %s.", org.Name, how)
	b.finish(w, r.Context(), v, http.StatusOK)
}

// enterpriseChangeFromForm reads the Enterprise card. The form always shows the whole deal, so
// every field is sent and every field is set: an emptied number is 0 and an emptied text field is
// cleared, which is what the person who emptied it meant.
func enterpriseChangeFromForm(r *http.Request) (enterpriseChange, error) {
	num := func(name, label string) (*float64, error) {
		raw := strings.TrimSpace(r.PostFormValue(name))
		if raw == "" {
			zero := 0.0
			return &zero, nil
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("%s has to be a number", label)
		}
		return &f, nil
	}
	whole := func(name, label string) (*int, error) {
		f, err := num(name, label)
		if err != nil {
			return nil, err
		}
		if *f != math.Trunc(*f) || *f > math.MaxInt32 || *f < math.MinInt32 {
			return nil, fmt.Errorf("%s has to be a whole number", label)
		}
		n := int(*f)
		return &n, nil
	}
	text := func(name string) *string {
		v := strings.TrimSpace(r.PostFormValue(name))
		return &v
	}
	var ch enterpriseChange
	var err error
	if ch.Users, err = whole("users", "Users"); err != nil {
		return ch, err
	}
	if ch.Jobs, err = whole("jobs", "Fix jobs a month"); err != nil {
		return ch, err
	}
	if ch.IncludedUSD, err = num("included_usd", "Included credit"); err != nil {
		return ch, err
	}
	if ch.BudgetUSD, err = num("budget_usd", "The monthly budget"); err != nil {
		return ch, err
	}
	if ch.FeeUSD, err = num("fee_usd", "The fee"); err != nil {
		return ch, err
	}
	ch.Interval, ch.PriceID, ch.PayURL, ch.Note = text("interval"), text("price_id"), text("pay_url"), text("note")
	if *ch.Interval == "" {
		ch.Interval = nil
	}
	return ch, nil
}
