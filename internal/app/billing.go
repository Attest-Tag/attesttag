package app

// Self-serve billing: the routes a customer reaches, and the webhook that is the only thing
// allowed to say a payment happened.
//
// The shape to keep in mind while reading: nothing a browser sends moves money. A checkout route
// takes a size key or a preset amount, looks the real figure up here, and hands the browser to
// Stripe. Stripe takes the payment. Stripe then tells us, over a signed request, and that is where
// the ledger is written and the plan moves. A success page proves nothing and is treated as
// proving nothing.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// checkouts bounds how many Checkout Sessions one account, or one address, may open. Open signup
// plus a public checkout is a card-testing oracle: every declined attempt is a Radar event against
// our account, and enough of them restrict it. Counted per organisation AND per address, because
// the per-organisation limit alone misses the shape that matters — a hundred accounts, one session
// each. Shared across instances in bot.go, like the other limiters that are security controls.
var checkouts = newRateLimiter()

const (
	checkoutsPerOrgAnHour  = 12
	checkoutsPerAddrAnHour = 20
	// settlesPerOrgAnHour bounds the return-from-Stripe reads. Each is a request to Stripe on the
	// deployment's key, and Stripe rate-limits the account, not the caller, so an unbounded one was
	// a way for any member to spend the quota checkout and the portal depend on for everybody.
	settlesPerOrgAnHour = 20
)

// metadata keys copied onto every object Stripe will later hand back to us.
const (
	metaOrg   = "attesttag_org"   // the organisation's 32-hex public id, never the serial
	metaActor = "attesttag_actor" // who pressed the button, so the receipt goes to them
)

func (b *Bot) billingRoutes(mux *http.ServeMux) {
	// Unconfigured, none of this exists — the routes answer 404 like any path this deployment
	// does not serve, exactly as the operator API does. A self-host must not have a checkout
	// button that leads somewhere, and the webhook must not exist as an unauthenticated write.
	if !b.cfg.BillingEnabled() {
		return
	}
	checkouts.shareAcross(b.store)
	// Reading is every member's. /api/me already hands everyone the budget and the month's
	// spend, and "the bot has gone quiet, why" is a question anybody may have — the balance is
	// the answer. The Stripe identifiers are held back below without billing.manage.
	mux.HandleFunc("GET /api/billing", b.requireAdmin(b.handleBilling))
	mux.HandleFunc("POST /api/billing/checkout", b.requirePerm(PermBillingManage, b.handleBillingCheckout))
	mux.HandleFunc("POST /api/billing/portal", b.requirePerm(PermBillingManage, b.handleBillingPortal))
	mux.HandleFunc("POST /api/billing/size", b.requirePerm(PermBillingManage, b.handleBillingSize))
	mux.HandleFunc("POST /api/billing/size-request", b.requirePerm(PermBillingManage, b.handleBillingSizeRequest))
	// No session, no CSRF token, no permission — and therefore not behind requireAdmin, which
	// would impose all three and refuse every delivery. The authority here is a signature over
	// the exact bytes of the body. That also means auditedWrites does not wrap it, so every
	// effect below writes its own audit row; nothing else in this package has to remember that.
	mux.HandleFunc("POST /api/billing/webhook", b.handleStripeWebhook)
}

// ---- reading ----

type billingSizeView struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	PriceUSD float64 `json:"price_usd"`
	// IncludedUSD is the model credit this size hands over every month. Zero means the size
	// includes none and everything is bought as a top-up, which the console says rather than
	// drawing "$0.00 of credit a month".
	IncludedUSD float64 `json:"included_usd"`
	Available   bool    `json:"available"`
	// JobsPerMonth is the fix jobs a month the size is sold with. Zero means no figure was
	// printed for it, which the console renders as nothing rather than as "unlimited".
	JobsPerMonth int `json:"jobs_per_month"`
}

// jobsView is this calendar month's fix jobs against the figure the plan is sold with. Limit 0
// means no figure was printed for this plan and the console shows the count alone. Counted and
// shown, never enforced — see sizeJobLimits.
type jobsView struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
}

type billingSubscriptionView struct {
	Status            string  `json:"status"`
	Size              string  `json:"size"`
	SizeLabel         string  `json:"size_label"`
	UnitPriceUSD      float64 `json:"unit_price_usd"`
	Quantity          int64   `json:"quantity"`
	AmountUSD         float64 `json:"amount_usd"`
	PeriodStart       string  `json:"period_start"`
	PeriodEnd         string  `json:"period_end"`
	CancelAtPeriodEnd bool    `json:"cancel_at_period_end"`
	Comped            bool    `json:"comped"`
	// IncludedUSD is what this size adds to the balance at each renewal. On a comped account it
	// is what the size would include; nothing is invoiced, so nothing is granted.
	IncludedUSD float64 `json:"included_usd"`
	// A downgrade waiting for the period to end. Size above is still what they are ON and what
	// they are billed for until PendingAt; these two say what happens then. Without them the
	// screen forgets a scheduled downgrade the moment the page reloads, and somebody who is not
	// sure it took asks for it again.
	PendingSize      string `json:"pending_size,omitempty"`
	PendingSizeLabel string `json:"pending_size_label,omitempty"`
	PendingAt        string `json:"pending_at,omitempty"`
}

type billingView struct {
	Enabled      bool                     `json:"enabled"`
	CanManage    bool                     `json:"can_manage"`
	Plan         string                   `json:"plan"`
	Currency     string                   `json:"currency"`
	Subscription *billingSubscriptionView `json:"subscription"`
	Credit       struct {
		// BalanceUSD is prepaid credit only — money the customer bought, which never expires.
		// The allowance below is deliberately a separate figure: they are different kinds of
		// thing and a screen that added them up would be telling somebody their included credit
		// carries over, which it does not.
		BalanceUSD      float64 `json:"balance_usd"`
		Enforced        bool    `json:"enforced"`
		LowUSD          float64 `json:"low_threshold_usd"`
		OverdraftUSD    float64 `json:"overdraft_usd"`
		LifetimeTopUp   float64 `json:"lifetime_topup_usd"`
		LifetimeDebited float64 `json:"lifetime_debit_usd"`
		// AllowanceUSD is what is left of this period's included credit, AllowanceGrantedUSD what
		// the period started with, and AllowanceExpires when it lapses. Zero and empty for an
		// account with no plan, or a size that includes nothing.
		AllowanceUSD        float64 `json:"allowance_usd"`
		AllowanceGrantedUSD float64 `json:"allowance_granted_usd"`
		AllowanceExpires    string  `json:"allowance_expires"`
		// SpendableUSD is the two added together, which is the figure the overdraft floor is
		// actually compared against. On the wire so the console never has to add them itself and
		// get a different answer from the one the bot applies.
		SpendableUSD float64 `json:"spendable_usd"`
		// Metered is whether the floor applies at all — bought credit, or a live allowance.
		Metered bool `json:"metered"`
	} `json:"credit"`
	Budget struct {
		MonthlyBudgetUSD  float64 `json:"monthly_budget_usd"`
		PlatformBudgetUSD float64 `json:"platform_budget_usd"`
		EffectiveUSD      float64 `json:"effective_budget_usd"`
		MonthSpendUSD     float64 `json:"month_spend_usd"`
	} `json:"budget"`
	// PausedBy is the field the console must not work out for itself. Deriving "which limit
	// stopped us" from two numbers in the browser would be a second copy of the rule the bot
	// applies, and the two would drift. The server refuses the turn, so the server says why.
	PausedBy string `json:"paused_by"`
	// OwnModelKey: the organisation's model calls are on its own key (model_keys.go), so none of
	// them draws on the credit shown here. Billing says so rather than leaving a balance that never
	// moves to explain itself.
	OwnModelKey bool `json:"own_model_key"`
	// Users is the 30-day active count and the size's ceiling. Whether it is over is decided
	// here, beside paused_by and for the same reason: two copies of the rule would drift.
	// Nothing is refused for being over it — see the note on the console's banner.
	Users ActiveUserCount `json:"users"`
	// Jobs is the month's fix jobs against the plan's figure; see jobsView.
	Jobs jobsView `json:"jobs"`
	// SizeRequest is the last Talk to us request, when there has been one; see billing_request.go.
	SizeRequest *sizeRequestView `json:"size_request,omitempty"`
	// Enterprise is the account's deal when it is on the enterprise plan, and absent otherwise.
	// The console draws it instead of the size picker: an enterprise account has nothing on the
	// ladder to buy, and its figures are its own.
	Enterprise *enterpriseView   `json:"enterprise,omitempty"`
	Sizes      []billingSizeView `json:"sizes"`
	TopUp      struct {
		MinUSD  float64   `json:"min_usd"`
		MaxUSD  float64   `json:"max_usd"`
		Presets []float64 `json:"presets"`
	} `json:"topup"`
	Ledger []CreditEntry `json:"ledger"`
	// PendingCheckout is true when a payment has been taken and its webhook has not landed. The
	// console shows "updating…" and polls rather than showing a balance that is about to change.
	PendingCheckout bool   `json:"pending_checkout"`
	SupportEmail    string `json:"support_email"`
	// Stripe identifies this account to Stripe's own support, so it is held back from members
	// who cannot spend. Absent rather than empty, so the console can tell the two apart.
	Stripe *struct {
		CustomerID     string `json:"customer_id"`
		SubscriptionID string `json:"subscription_id"`
	} `json:"stripe,omitempty"`
}

// topUpPresets are the amounts offered as buttons. A server-side list on purpose: the custom box
// is clamped, but a fixed set is what a scripted caller gets, and there is nothing to clamp.
func topUpPresets(cfg Config) []float64 {
	var out []float64
	for _, v := range []float64{100, 250, 500, 1000} {
		if v >= cfg.TopUpMinUSD && v <= cfg.TopUpMaxUSD {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		out = []float64{cfg.TopUpMinUSD}
	}
	return out
}

func (b *Bot) handleBilling(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := adminFromCtx(ctx)
	orgID := me.OrgID

	// settle=1 is the console coming back from Stripe. One read of the session, so a payment is
	// applied now rather than whenever the webhook arrives — it removes the race instead of
	// papering over it, and it is idempotent with the webhook because both key the ledger on the
	// same payment id.
	//
	// pending is what the console draws its "updating…" state from, and it is the honest answer
	// rather than an optimistic one: false means the money is already in the figures below, true
	// means it is not and the webhook is still the thing that will put it there.
	pending := false
	if r.URL.Query().Get("settle") == "1" && me.Permissions[PermBillingManage] {
		id := r.URL.Query().Get("session")
		// A return trip needs one or two. Past the limit the page shows pending and waits for the
		// webhook, which is what it does anyway whenever settling has not landed yet.
		if ok, _ := checkouts.allow("settle-org:"+me.OrgPublic, settlesPerOrgAnHour, time.Hour); ok {
			pending = id == "" || !b.settleCheckout(ctx, orgID, id)
		} else {
			pending = true
		}
	}

	acct, err := b.store.BillingAccountOf(ctx, orgID)
	if err != nil {
		fail(w, err)
		return
	}
	st := b.settings.Get(ctx, orgID)
	spend, _ := budgetSpend(ctx, b.store, orgID, st)
	ledger, _ := b.store.CreditLedger(ctx, orgID, 50)

	v := billingView{Enabled: true, CanManage: me.Permissions[PermBillingManage],
		Plan: st.Plan, Currency: b.cfg.BillingCurrency, SupportEmail: b.cfg.SupportEmail, Ledger: ledger,
		PendingCheckout: pending}
	v.Credit.BalanceUSD = microsToUSD(acct.CreditBalanceMicros)
	v.Credit.Enforced = acct.CreditEnforced
	v.Credit.LowUSD = b.cfg.CreditLowUSD
	v.Credit.OverdraftUSD = microsToUSD(maxOverdraftMicros)
	v.Credit.LifetimeTopUp = microsToUSD(acct.LifetimeTopUpMicros)
	v.Credit.LifetimeDebited = microsToUSD(acct.LifetimeDebitMicros)
	v.Credit.AllowanceUSD = microsToUSD(acct.SpendableAllowanceMicros())
	v.Credit.AllowanceGrantedUSD = microsToUSD(acct.AllowanceGrantedMicros)
	if acct.AllowanceLive() {
		v.Credit.AllowanceExpires = acct.AllowancePeriodEnd
	}
	v.Credit.SpendableUSD = microsToUSD(acct.SpendableMicros())
	v.Credit.Metered = acct.Metered()
	v.Budget.MonthlyBudgetUSD = st.MonthlyBudgetUSD
	v.Budget.PlatformBudgetUSD = st.PlatformBudgetUSD
	v.Budget.EffectiveUSD = st.EffectiveBudget()
	v.Budget.MonthSpendUSD = spend
	v.PausedBy = pausedBy(st, acct.Metered(), acct.SpendableMicros(), spend)
	v.OwnModelKey = st.OwnKey.Active()
	// With the per-workspace breakdown: this is the one screen where somebody asks why the total
	// is smaller than the parts, and the answer is that a person in two workspaces is one user.
	v.Users = b.activeUsers(ctx, orgID, true)
	v.Jobs = jobsView{Used: b.store.JobsThisMonth(ctx, orgID), Limit: jobLimitOf(acct, st.Plan)}
	if st.Plan == PlanEnterprise {
		terms, err := b.store.EnterpriseTerms(ctx, orgID)
		if err != nil {
			fail(w, err)
			return
		}
		// The deal's figure whether or not it has been paid for yet: it is what the account was
		// sold, and like every job figure it is printed rather than enforced.
		v.Jobs.Limit = terms.JobLimit
		v.Enterprise = enterpriseViewOf(terms, acct, v.CanManage)
	}
	if at := b.store.Setting(ctx, orgID, sizeRequestKey); at != "" {
		key := b.store.Setting(ctx, orgID, sizeRequestSizeKey)
		label := sizeLabels[key]
		if label == "" {
			label = key
		}
		v.SizeRequest = &sizeRequestView{At: at, Size: key, SizeLabel: label}
	}
	v.TopUp.MinUSD, v.TopUp.MaxUSD, v.TopUp.Presets = b.cfg.TopUpMinUSD, b.cfg.TopUpMaxUSD, topUpPresets(b.cfg)
	for _, size := range b.cfg.StripeSizes {
		v.Sizes = append(v.Sizes, billingSizeView{Key: size.Key, Label: size.Label,
			PriceUSD:    float64(size.AmountMinor) / 100,
			IncludedUSD: float64(size.IncludedMinor) / 100, Available: size.PriceID != "",
			JobsPerMonth: size.JobLimit})
	}
	if acct.Exists && acct.Status != "" {
		label := sizeLabels[acct.Size]
		if label == "" {
			label = acct.Size
		}
		v.Subscription = &billingSubscriptionView{Status: acct.Status, Size: acct.Size, SizeLabel: label,
			UnitPriceUSD: microsToUSD(acct.UnitPriceMicros), Quantity: acct.Quantity,
			AmountUSD: microsToUSD(acct.AmountMicros()), PeriodStart: acct.PeriodStart,
			PeriodEnd: acct.PeriodEnd, CancelAtPeriodEnd: acct.CancelAtPeriodEnd,
			Comped: acct.Status == "comped"}
		if size, ok := b.sizeOf(ctx, orgID, acct.Size); ok {
			v.Subscription.IncludedUSD = float64(size.IncludedMinor) / 100
		}
		b.fillPendingSize(ctx, acct, v.Subscription)
	}
	if v.CanManage && acct.Exists {
		v.Stripe = &struct {
			CustomerID     string `json:"customer_id"`
			SubscriptionID string `json:"subscription_id"`
		}{acct.CustomerID, acct.SubscriptionID}
	}
	writeJSON(w, 200, v)
}

// fillPendingSize asks Stripe whether a downgrade is waiting at the period boundary. Read here
// rather than remembered on this side: a stored copy is one more thing to drift, and the sentence
// it produces is a promise about money on a date.
//
// A failure is logged and dropped rather than returned. Stripe being unreachable is not a reason
// the billing screen cannot be looked at — the worst it costs is a pending downgrade not being
// mentioned on one load, and the account's own figures on this page do not come from Stripe.
func (b *Bot) fillPendingSize(ctx context.Context, acct BillingAccount, v *billingSubscriptionView) {
	if acct.SubscriptionID == "" || acct.Status == "comped" {
		return
	}
	p, err := b.pay.PendingPriceChange(ctx, acct.SubscriptionID)
	if err != nil {
		slog.Warn("could not read whether a size change is waiting", "sub", acct.SubscriptionID, "err", err)
		return
	}
	if !p.Waiting() {
		return
	}
	size, ok := b.cfg.SizeByPrice(p.PriceID)
	if !ok {
		// A price this deployment no longer sells. Say that something changes and when, rather
		// than naming a size nobody can look up or silently saying nothing.
		slog.Warn("a size change is waiting on a price this deployment does not sell",
			"sub", acct.SubscriptionID, "price", p.PriceID)
		v.PendingSize, v.PendingSizeLabel = "", "another plan size"
		v.PendingAt = stripeTime(p.AtUnix)
		return
	}
	v.PendingSize, v.PendingAt = size.Key, stripeTime(p.AtUnix)
	if v.PendingSizeLabel = sizeLabels[size.Key]; v.PendingSizeLabel == "" {
		v.PendingSizeLabel = size.Key
	}
}

// ---- buying ----

type checkoutRequest struct {
	Kind      string  `json:"kind"` // "subscription" | "enterprise" | "topup"
	Size      string  `json:"size"`
	AmountUSD float64 `json:"amount_usd"`
}

func (b *Bot) handleBillingCheckout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := adminFromCtx(ctx)
	var in checkoutRequest
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	// Buying wants a confirmed address, for the same reason inviting people and minting API keys
	// do: it is reaching out from the organisation, and this one reaches out with money. The
	// receipt also has to arrive somewhere real.
	//
	// Checked here rather than on the permission (needsVerifiedEmail), because that would apply
	// to the billing portal too — and the portal is where somebody CANCELS. Telling a paying
	// customer to go and find a verification mail before they may stop paying is the wrong
	// answer to that request, whatever it does for this one.
	if b.emailUnverified(ctx, me) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":        "Confirm your email address before buying: the link is in your inbox, and Settings → General can send it again.",
			"verify_email": true})
		return
	}
	if ok, d := checkouts.allow("checkout-org:"+me.OrgPublic, checkoutsPerOrgAnHour, time.Hour); !ok {
		tooMany(w, d)
		return
	}
	if ok, d := checkouts.allow("checkout-ip:"+clientIP(r), checkoutsPerAddrAnHour, time.Hour); !ok {
		tooMany(w, d)
		return
	}

	acct, err := b.store.BillingAccountOf(ctx, me.OrgID)
	if err != nil {
		fail(w, err)
		return
	}
	base := b.baseURL(r)
	// The trailing slash is load-bearing: the console is a static export served under /admin with
	// trailingSlash on, so the path without it 404s — on the one page somebody reaches straight
	// after paying, which is the worst possible place for a 404.
	success := base + "/admin/settings/?tab=billing&checkout=success&session={CHECKOUT_SESSION_ID}"
	cancel := base + "/admin/settings/?tab=billing&checkout=cancelled"

	req := CheckoutRequest{
		CustomerID: acct.CustomerID, CustomerEmail: me.Email, Currency: b.cfg.BillingCurrency,
		ClientRef: me.OrgPublic, SuccessURL: success, CancelURL: cancel,
		Metadata:       map[string]string{metaOrg: me.OrgPublic, metaActor: me.PublicID},
		IdempotencyKey: idempotencyKey(),
	}

	plan := b.settings.Get(ctx, me.OrgID).Plan
	switch in.Kind {
	case "subscription":
		// An enterprise account buys nothing off the ladder: its price is its deal, and a rung
		// bought beside it would be a second fee under a plan that says something else.
		if plan == PlanEnterprise {
			bad(w, errors.New("this account is on an enterprise plan; its price is set by agreement, so there is no plan size to buy here"))
			return
		}
		if acct.Active() && acct.SubscriptionID != "" {
			// A second subscription would be a second monthly charge. The answer is the portal,
			// where the existing one can be changed or cancelled, so say so rather than refusing
			// into a dead end.
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "this account already has a subscription; change or cancel it under Card and invoices", "portal": true})
			return
		}
		size, ok := b.cfg.SizeByKey(in.Size)
		if !ok || size.PriceID == "" {
			bad(w, errors.New("that is not a size this deployment sells; ask support for a quote"))
			return
		}
		// The size travels on the session, and on the subscription through subscription_data.
		// Without it activateSubscription has nothing to map back and every account arrives with
		// an empty size and a zero fee on its record — which is also what decides the monthly
		// credit allowance, so it is load-bearing twice over.
		req.Metadata["size"] = size.Key
		req.Mode, req.PriceID, req.Quantity = "subscription", size.PriceID, 1
	case "enterprise":
		// Subscribing to the deal's own Price. The Price comes from the deal the operator wrote,
		// never from the browser, which names only the kind — the same rule as a size key.
		if plan != PlanEnterprise {
			bad(w, errors.New("this account is not on an enterprise plan"))
			return
		}
		terms, err := b.store.EnterpriseTerms(ctx, me.OrgID)
		if err != nil {
			fail(w, err)
			return
		}
		if terms.PriceID == "" {
			bad(w, errors.New("this account's plan is not paid by subscription here; its Billing screen says how it is paid"))
			return
		}
		if acct.Active() && acct.SubscriptionID != "" {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "this account already has a subscription; change or cancel it under Card and invoices", "portal": true})
			return
		}
		// size travels on the subscription for the reason it does for a rung: it is what the
		// webhook maps back, and "enterprise" is what sends it to the deal for the figures.
		req.Metadata["size"] = sizeEnterprise
		req.Mode, req.PriceID, req.Quantity = "subscription", terms.PriceID, 1
	case "topup":
		// Never the amount the browser sent, beyond reading it: it is clamped here, against
		// figures only this process knows, before it reaches a payment provider.
		minor, err := topUpMinor(in.AmountUSD, b.cfg)
		if err != nil {
			bad(w, err)
			return
		}
		req.Mode, req.AmountMinor = "payment", minor
		req.Description = "attest_tag API credit"
	default:
		bad(w, errors.New(`kind must be "subscription", "enterprise" or "topup"`))
		return
	}

	sess, err := b.pay.Checkout(ctx, req)
	if err != nil {
		bad(w, err)
		return
	}
	b.audit(r, "billing.checkout_started", AuditEvent{TargetKind: "org", TargetID: me.OrgPublic, TargetName: me.OrgName,
		Details: auditDetails(map[string]any{"kind": in.Kind, "size": in.Size, "amount_usd": in.AmountUSD, "session": sess.ID})})
	writeJSON(w, 200, map[string]any{"url": sess.URL, "session": sess.ID})
}

// topUpMinor turns a requested top-up into minor units, refusing everything that is not a sum of
// money inside the deployment's own limits. Sub-minor-unit amounts are refused rather than rounded
// to zero: a customer who asked for one cent of credit should be told no, not charged nothing.
func topUpMinor(usd float64, cfg Config) (int64, error) {
	switch {
	case usd != usd, usd > 1e12, usd <= 0: // usd != usd is NaN
		return 0, errors.New("the amount must be a number of dollars")
	case usd < cfg.TopUpMinUSD:
		return 0, fmt.Errorf("the smallest top-up is $%.2f", cfg.TopUpMinUSD)
	case usd > cfg.TopUpMaxUSD:
		return 0, fmt.Errorf("the largest top-up is $%.2f; ask support for more than that", cfg.TopUpMaxUSD)
	}
	minor := int64(usd*100 + 0.5)
	if minor <= 0 {
		return 0, errors.New("the amount is too small to charge")
	}
	return minor, nil
}

func (b *Bot) handleBillingPortal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := adminFromCtx(ctx)
	acct, err := b.store.BillingAccountOf(ctx, me.OrgID)
	if err != nil {
		fail(w, err)
		return
	}
	if acct.CustomerID == "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "this account has not bought anything yet, so there is no card to manage"})
		return
	}
	url, err := b.pay.Portal(ctx, PortalRequest{CustomerID: acct.CustomerID,
		ReturnURL: b.baseURL(r) + "/admin/settings/?tab=billing"})
	if err != nil {
		bad(w, err)
		return
	}
	b.audit(r, "billing.portal_opened", AuditEvent{TargetKind: "org", TargetID: me.OrgPublic, TargetName: me.OrgName})
	writeJSON(w, 200, map[string]any{"url": url})
}

type sizeChangeRequest struct {
	Size string `json:"size"`
}

// handleBillingSize moves a live subscription to another size.
//
// It lives here rather than behind the card portal because the portal only offers a plan switch
// when its configuration has been given one, which is a setting in somebody's Stripe dashboard
// and not something this deployment can promise. The console offered a "Change" link into a
// portal that, unconfigured, shows nothing but "Cancel subscription" — an account could go up a
// size only by cancelling and buying again, losing the period it had paid for.
//
// The size is a key, never a price: a browser that could name the Price it moves to could name a
// $0 one. Everything below is looked up from STRIPE_SIZES on this side.
func (b *Bot) handleBillingSize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := adminFromCtx(ctx)
	var in sizeChangeRequest
	if err := decode(r, &in); err != nil {
		bad(w, err)
		return
	}
	size, ok := b.cfg.SizeByKey(in.Size)
	if !ok || size.PriceID == "" {
		bad(w, errors.New("that is not a size this deployment sells; ask support for a quote"))
		return
	}
	acct, err := b.store.BillingAccountOf(ctx, me.OrgID)
	if err != nil {
		fail(w, err)
		return
	}
	switch {
	case acct.Size == sizeEnterprise:
		// The deal is the size. It moves by agreement, and the operator writes the new one.
		writeJSON(w, http.StatusConflict, map[string]any{"error": "this account is on an enterprise plan, so its size is part of the agreement; ask support to change it"})
		return
	case acct.Status == "comped":
		// Granted rather than bought: there is no Stripe subscription to move, and an operator
		// is the only one who should be changing what a comped account is recorded as.
		writeJSON(w, http.StatusConflict, map[string]any{"error": "this plan was granted rather than bought, so there is no subscription to change; ask support"})
		return
	case !acct.Active() || acct.SubscriptionID == "":
		writeJSON(w, http.StatusConflict, map[string]any{"error": "there is no active subscription to change; pick a size below to start one"})
		return
	case acct.Size == size.Key:
		// Already there — a double-clicked button and a stale tab both land here and neither is
		// an error. It still has to reach Stripe, because picking the size you are already on
		// is the only way to call off a downgrade that has not happened yet, and only Stripe
		// knows whether one is waiting.
		res, err := b.pay.ChangeSubscriptionPrice(ctx, PriceChange{
			SubscriptionID: acct.SubscriptionID, PriceID: size.PriceID,
			ChargeNow: false, IdempotencyKey: sizeChangeKey(acct, size, time.Now()),
		})
		if err != nil {
			bad(w, err)
			return
		}
		if res.Released {
			b.audit(r, "billing.band_change_cancelled", AuditEvent{TargetKind: "org", TargetID: me.OrgPublic,
				TargetName: me.OrgName, Details: auditDetails(map[string]any{"size": size.Key})})
			slog.Info("billing size change cancelled", "org", me.OrgPublic, "size", size.Key)
		}
		writeJSON(w, 200, map[string]any{"size": size.Key, "charged": false, "cancelled": res.Released})
		return
	}

	from := acct.Size
	// Which way the move goes decides whether it takes money, so it is worked out once, here,
	// and everything downstream reads it rather than deciding again.
	chargeNow := sizeChangeCharges(acct.UnitPriceMicros, size)
	res, err := b.pay.ChangeSubscriptionPrice(ctx, PriceChange{
		SubscriptionID: acct.SubscriptionID, PriceID: size.PriceID, ChargeNow: chargeNow,
		IdempotencyKey: sizeChangeKey(acct, size, time.Now()),
	})
	if err != nil {
		switch {
		case needsCardAction(err):
			// There is no way through this from here. The card is asking for its owner, and an
			// API call has nowhere to put a 3-D Secure challenge — so answering with a bare
			// refusal would leave them clicking a button that can never work. Name the reason
			// and hand them somewhere they can finish it.
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"error": "this card asks its owner to confirm payments, and that cannot be done from this screen. " +
					"Open Card and invoices to authorise it there, or change to a card that does not ask. Your size has not changed.",
				"portal": true})
		case cardWasDeclined(err):
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"error":  err.Error() + " — your size has not changed. Update the card under Card and invoices, then try again.",
				"portal": true})
		default:
			bad(w, err)
		}
		return
	}
	if res.Scheduled {
		// Nothing moved and nothing was charged: they paid for this month and they keep it. So
		// the record must NOT move either — writing the smaller size here would take away
		// today, silently, the size and the allowance they have already bought until the
		// boundary. The subscription changes when Stripe rolls it over, and the webhook that
		// reports that is what moves the record.
		b.audit(r, "billing.band_change_scheduled", AuditEvent{TargetKind: "org", TargetID: me.OrgPublic,
			TargetName: me.OrgName, Details: auditDetails(map[string]any{"from": from, "to": size.Key,
				"at": stripeTime(res.Pending.AtUnix)})})
		slog.Info("billing size change scheduled", "org", me.OrgPublic, "from", from, "to", size.Key,
			"at", stripeTime(res.Pending.AtUnix))
		writeJSON(w, 200, map[string]any{
			// size is what they are on, which is unchanged. pending_size is what they move to.
			"size": from, "charged": false, "scheduled": true,
			"pending_size": size.Key, "pending_at": stripeTime(res.Pending.AtUnix)})
		return
	}
	// Written here as well as by the webhook, for the reason settleCheckout exists: the person
	// is looking at the screen now, and customer.subscription.updated arrives when it arrives.
	// The webhook is still the authority — it overwrites this with what Stripe actually charges,
	// including a period this side cannot know.
	st := SubscriptionState{CustomerID: acct.CustomerID, SubscriptionID: acct.SubscriptionID,
		Status: acct.Status, Size: size.Key, UnitPriceMicros: minorToMicros(size.AmountMinor),
		Quantity: max64(acct.Quantity, 1), Currency: acct.Currency,
		PeriodStart: acct.PeriodStart, PeriodEnd: acct.PeriodEnd,
		CancelAtPeriodEnd: acct.CancelAtPeriodEnd}
	if err := b.store.PutSubscription(ctx, me.OrgID, st); err != nil {
		fail(w, err)
		return
	}
	// The size moved, so what it includes moved with it. Same call the webhook will make when it
	// confirms; doing it here means the screen the customer is looking at is right immediately
	// rather than whenever Stripe gets round to telling us.
	b.syncAllowance(ctx, me.OrgID)
	b.settings.Invalidate(me.OrgID)
	b.audit(r, "billing.band_changed", AuditEvent{TargetKind: "org", TargetID: me.OrgPublic, TargetName: me.OrgName,
		Details: auditDetails(map[string]any{"from": from, "to": size.Key,
			"price_usd": float64(size.AmountMinor) / 100})})
	slog.Info("billing size changed", "org", me.OrgPublic, "from", from, "to", size.Key,
		"charged", res.Charged, "amount_minor", res.AmountMinor)
	// charged is the honest half, and the console says different things on either side of it:
	// an upgrade has already taken the money, a downgrade has only promised a smaller invoice.
	out := map[string]any{"size": size.Key, "charged": res.Charged}
	if res.Charged {
		out["amount_minor"], out["currency"] = res.AmountMinor, res.Currency
		if res.InvoiceURL != "" {
			out["invoice_url"] = res.InvoiceURL
		}
	}
	writeJSON(w, 200, out)
}

// sizeChangeCharges reports whether moving onto this size takes money now. Only going up does:
// the difference for the rest of a month already paid for is owed immediately, while going down
// is a credit, and a credit is not something to invoice — it comes off the next one.
//
// The preview and the charge must both come through here. Two places working out the direction
// separately is two places to get it wrong, and they would disagree about money.
func sizeChangeCharges(fromUnitMicros int64, to Size) bool {
	return minorToMicros(to.AmountMinor) > fromUnitMicros
}

// sizeChangeKey is the idempotency key for one size move — derived, never random. Now that an
// upgrade is charged on the spot, a random key per request would let a double-clicked button
// buy the same upgrade twice.
//
// What it names is the move: this subscription, off this size, onto that one. The ten-minute
// window is there because Stripe remembers a key for 24 hours, and without it a customer who
// went up, came back down and went up again inside a day would have that third move silently
// answered with the first one's result and never actually made. Ten minutes is far longer than
// the second a double click spans and far shorter than a change of mind. The read-back in
// ChangeSubscriptionPrice is what catches the remainder: a replay that lands anyway is seen as
// a subscription that did not move, and refused rather than recorded.
func sizeChangeKey(acct BillingAccount, to Size, now time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("size:%s:%s:%s:%d",
		acct.SubscriptionID, acct.Size, to.Key, now.Unix()/600)))
	return hex.EncodeToString(sum[:])
}

// idempotencyKey is one button press. Not security-critical — what stops a double CREDIT is
// credit_ledger.external_id, keyed on the payment itself — but it stops a double-clicked button
// opening two sessions somebody could then pay twice.
func idempotencyKey() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---- the webhook ----

func (b *Bot) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	// Under a maintenance freeze, tell Stripe to come back. This is the OPPOSITE of the Slack
	// handler, which acknowledges and drops (slack_http.go), and the reason inverts cleanly:
	// Slack disables an app that keeps failing and its events are cheap, while Stripe tolerates
	// a short outage and these events are money. Dropping one means the customer paid and got
	// nothing, with the only record of it at Stripe.
	if b.cfg.Maintenance {
		w.Header().Set("Retry-After", "300")
		http.Error(w, "maintenance", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, stripeBodyLimit)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		bad(w, errors.New("could not read the request body"))
		return
	}
	ev, err := b.pay.VerifyWebhook(raw, r.Header.Get("Stripe-Signature"), time.Now())
	if err != nil {
		// Throttled on FAILED signatures only, keyed by address. A request carrying a valid
		// signature is from Stripe by definition, and a throttle that turned those away would be
		// a mechanism for losing money.
		if ok, d := checkouts.allow("stripe-bad-sig:"+clientIP(r), 60, time.Hour); !ok {
			tooMany(w, d)
			return
		}
		slog.Warn("stripe webhook refused", "err", err, "ip", clientIP(r))
		bad(w, err)
		return
	}
	// A test-mode event delivered to a live endpoint, or the other way round. Acknowledged so it
	// is not retried for three days, recorded so a redelivery is recognised, and acted on never:
	// this is what stops a test webhook pointed at production crediting money nobody paid.
	if ev.Livemode != (stripeMode(b.cfg.StripeSecretKey) == "live") {
		slog.Warn("stripe webhook ignored: livemode does not match this deployment's key",
			"event", ev.ID, "type", ev.Type, "livemode", ev.Livemode)
		b.store.SeenBillingEvent(r.Context(), "stripe", ev.ID, ev.Type)
		writeJSON(w, 200, map[string]any{"ok": true, "ignored": "livemode"})
		return
	}
	// Detached: the work below moves money, and Stripe hanging up mid-request must not roll it
	// back into a state where the retry is the only record.
	ctx := context.WithoutCancel(r.Context())
	if err := b.applyStripeEvent(ctx, ev); err != nil {
		// 500, because it is the truth: the request was sound and the failure is ours. What brings
		// the event back is only that this is not a 2xx — Stripe retries any other answer, a 4xx
		// included, for up to three days — and an event is recorded only once everything it does
		// has happened, so the retry runs all of it again.
		slog.Error("stripe webhook failed", "event", ev.ID, "type", ev.Type, "err", err)
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// stripeObject is the union of the fields the handled events carry. One struct rather than six,
// because they overlap heavily and the alternative is six near-identical ones.
type stripeObject struct {
	ID            string            `json:"id"`
	Object        string            `json:"object"`
	Mode          string            `json:"mode"`
	Customer      string            `json:"customer"`
	Subscription  string            `json:"subscription"`
	PaymentIntent string            `json:"payment_intent"`
	PaymentStatus string            `json:"payment_status"`
	Status        string            `json:"status"`
	ClientRef     string            `json:"client_reference_id"`
	Currency      string            `json:"currency"`
	AmountTotal   int64             `json:"amount_total"`
	AmountReceive int64             `json:"amount_received"`
	AmountRefund  int64             `json:"amount_refunded"`
	Amount        int64             `json:"amount"`  // a dispute's amount
	Charge        string            `json:"charge"`  // a dispute's charge id, for the operator alert
	Metadata      map[string]string `json:"metadata"`
	CancelAtEnd   bool              `json:"cancel_at_period_end"`
	PeriodStart   int64             `json:"current_period_start"`
	PeriodEnd     int64             `json:"current_period_end"`
	BillingReason string            `json:"billing_reason"`
	Items         struct {
		Data []struct {
			Quantity int64 `json:"quantity"`
			Price    struct {
				ID       string `json:"id"`
				Currency string `json:"currency"`
				Amount   int64  `json:"unit_amount"`
			} `json:"price"`
			// The period, from 2025-04-30.basil onwards. It used to be on the subscription
			// itself, which is what PeriodStart/PeriodEnd above still read.
			PeriodStart int64 `json:"current_period_start"`
			PeriodEnd   int64 `json:"current_period_end"`
		} `json:"data"`
	} `json:"items"`
	// Parent is where an invoice names the subscription it belongs to, from 2025-04-30.basil
	// onwards. It also carries that subscription's metadata, which is the only place an invoice
	// says which organisation it is for — see orgForStripe.
	Parent struct {
		SubscriptionDetails struct {
			Subscription string            `json:"subscription"`
			Metadata     map[string]string `json:"metadata"`
		} `json:"subscription_details"`
	} `json:"parent"`
	Lines struct {
		Data []invoiceLine `json:"data"`
	} `json:"lines"`
}

// invoiceLine is one line of an invoice: what it charged or credited, at which Price, and for
// which stretch of time.
type invoiceLine struct {
	Amount int64                      `json:"amount"`
	Period struct{ Start, End int64 } `json:"period"`
	Price  struct {
		ID string `json:"id"`
	} `json:"price"`
	// Pricing is where an invoice line names its Price from 2025-04-30.basil onwards: the same
	// field, one level further down.
	Pricing struct {
		PriceDetails struct {
			Price string `json:"price"`
		} `json:"price_details"`
	} `json:"pricing"`
	// Whether the line charges or credits part of a period for a change made in the middle of it,
	// rather than being the subscription's own line for a whole period. basil moved the flag under
	// parent, onto whichever kind of item the line came from.
	Proration bool `json:"proration"`
	Parent    struct {
		SubscriptionItemDetails struct {
			Proration bool `json:"proration"`
		} `json:"subscription_item_details"`
		InvoiceItemDetails struct {
			Proration bool `json:"proration"`
		} `json:"invoice_item_details"`
	} `json:"parent"`
}

// The readers below exist because a webhook body is NOT rendered in the version pinned by
// stripeAPIVersion. That header governs the replies to calls this code makes; an event is
// rendered in the version its endpoint was created at, which is Stripe's account default and
// moves on its own. Four fields this file depends on moved in 2025-04-30.basil, so both
// spellings have to parse, and each moved field gets one reader rather than a conditional at
// every use. Each prefers the newer spelling and falls back to the older one, so an endpoint on
// either version is read correctly and neither has to be configured.

// subscriptionPeriod is the current period of a subscription object. basil moved it off the
// subscription and onto each item, because items may now bill on different cycles. We sell one
// item per subscription, so the first item's period is the subscription's.
func (o stripeObject) subscriptionPeriod() (start, end int64) {
	if len(o.Items.Data) > 0 {
		if it := o.Items.Data[0]; it.PeriodStart > 0 || it.PeriodEnd > 0 {
			return it.PeriodStart, it.PeriodEnd
		}
	}
	return o.PeriodStart, o.PeriodEnd
}

// subscriptionID is the subscription an invoice belongs to.
func (o stripeObject) subscriptionID() string {
	return nonEmpty(o.Parent.SubscriptionDetails.Subscription, o.Subscription)
}

func (ln invoiceLine) priceID() string { return nonEmpty(ln.Pricing.PriceDetails.Price, ln.Price.ID) }

func (ln invoiceLine) proration() bool {
	return ln.Proration || ln.Parent.SubscriptionItemDetails.Proration || ln.Parent.InvoiceItemDetails.Proration
}

// subscriptionLine is what a paid invoice says the subscription now is: the Price it charges,
// which is the size, and the period that pays for — zero when the invoice does not say.
//
// A first invoice or a renewal carries the subscription's own line for the whole period, and that
// line is the answer wherever it sits. An upgrade charged on the spot (proration_behavior=
// always_invoice) carries none: only two prorations over what is left of the period, and on a
// real sandbox invoice the FIRST of them is the credit for the unused time on the old Price, the
// second the charge for the new one — with a period that starts at the moment of the change, in
// both API versions. Reading the first line wrote the size being left back over the one just
// bought. So without its own line the size is the line that charges, and the period is left to the
// subscription events, which carry the real one.
func (o stripeObject) subscriptionLine() (priceID string, start, end int64) {
	charged := ""
	for _, ln := range o.Lines.Data {
		if !ln.proration() {
			return ln.priceID(), ln.Period.Start, ln.Period.End
		}
		if ln.Amount > 0 {
			charged = ln.priceID()
		}
	}
	return charged, 0, 0
}

func (b *Bot) applyStripeEvent(ctx context.Context, ev StripeEvent) error {
	// Stripe delivers at least once, and redelivers events it already had a 2xx for. An event on
	// record has had its whole effect — every path records it last, once what it does is done — so
	// it is skipped. Replaying one is not harmless: a positive event redelivered after a later change
	// would write back the state it describes.
	if b.store.HasBillingEvent(ctx, ev.ID) {
		return nil
	}
	var o stripeObject
	if err := json.Unmarshal(ev.Data.Object, &o); err != nil {
		// Recorded and accepted: a body we cannot parse will not parse on the fourth retry
		// either, and a 500 loop counts toward Stripe disabling the endpoint.
		slog.Warn("stripe event object could not be read", "event", ev.ID, "type", ev.Type, "err", err)
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		return nil
	}
	if strings.HasPrefix(ev.Type, "charge.dispute.") {
		// A chargeback. Tell the operator no matter what — the money and Stripe's dispute fee are
		// at stake and the window to respond is short — even when the dispute object does not
		// carry enough to tie it to an account here.
		b.alertOperator(fmt.Sprintf("a Stripe dispute (%s, status %q) on charge %s for %d %s — respond in the Stripe dashboard",
			o.ID, o.Status, o.Charge, o.Amount, strings.ToUpper(o.Currency)))
	}
	switch ev.Type {
	case "charge.refunded", "charge.dispute.created", "charge.dispute.closed":
		return b.applyChargeEvent(ctx, ev, o)
	}
	org, err := b.orgForStripe(ctx, o)
	if err != nil {
		return err
	}
	if org == nil {
		// Not ours, or an organisation that has since been deleted. Retrying will not make it
		// ours, so it is recorded and acknowledged rather than failed forever.
		slog.Info("stripe event for an unknown organisation", "event", ev.ID, "type", ev.Type, "customer", o.Customer)
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		return nil
	}

	switch ev.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		if o.Mode == "payment" {
			if o.Metadata[metaOrg] == "" {
				// Not a top-up this server created: a Payment Link, or an enterprise pay_url
				// payment. Crediting it would turn a fee we never priced into spendable model
				// credit, and a link carrying ?client_reference_id would credit an account nobody
				// chose to pay for. Only our own checkout carries metaOrg on the session.
				b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
				return nil
			}
			if o.PaymentStatus != "paid" {
				// A session that completed without settling — a delayed payment method. The
				// async_payment_succeeded event will carry the same payment intent later, and
				// key the ledger on it, so this one simply waits.
				b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
				return nil
			}
			return b.creditTopUp(ctx, org, ev, o.PaymentIntent, o.AmountTotal, o.Currency, "Card payment")
		}
		return b.activateSubscription(ctx, org, ev, o)
	case "payment_intent.succeeded":
		// Stripe fires this for subscription invoices too, and those must never become credit.
		// Our own metadata is what tells a top-up apart from a monthly fee.
		if o.Metadata[metaOrg] == "" {
			b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
			return nil
		}
		// Same key as the session above, so whichever arrives first credits and the other is a
		// no-op. amount_received is what settled; amount_total on a session is set before payment.
		return b.creditTopUp(ctx, org, ev, o.ID, o.AmountReceive, o.Currency, "Card payment")
	case "invoice.paid":
		return b.rollSubscriptionPeriod(ctx, org, ev, o)
	case "invoice.payment_failed":
		return b.markPastDue(ctx, org, ev, o)
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		return b.syncSubscription(ctx, org, ev, o)
	default:
		// Unknown types are recorded and accepted. A 5xx on one Stripe happens to start sending
		// counts toward the endpoint being disabled, which would take the ones that matter with it.
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		return nil
	}
}

// orgForStripe resolves the organisation an event concerns, and refuses the one shape that is an
// attack rather than a mistake: a checkout naming organisation B for a customer already bound to
// organisation A.
//
// The id arrives from Stripe, so it is trusted only because the signature verified — it looks like
// user input in the payload and is not. It is the 32-hex public id; the serial never leaves this
// process.
func (b *Bot) orgForStripe(ctx context.Context, o stripeObject) (*Org, error) {
	// An invoice carries no metadata of its own — ours goes on the session, the payment intent
	// and the subscription — so its only claim to an organisation is the subscription's, which
	// it repeats under parent. Reading it here is what stops the FIRST invoice of a new
	// subscription being filed as "unknown organisation": Stripe does not order its deliveries,
	// so invoice.paid routinely arrives before the subscription event that binds the customer,
	// and until this line that event went to nobody — taking the size's included credit with it.
	//
	// Only our own metadata names an organisation. client_reference_id does not: our checkout sets
	// it, but so can whoever pays a Stripe Payment Link, by adding ?client_reference_id= to the URL,
	// and reading it as a claim let a payer name any organisation they knew the public id of.
	// Metadata is set by this server's checkout alone — on the session, the subscription and the
	// payment intent — so it is the one claim a payer cannot make.
	public := nonEmpty(o.Metadata[metaOrg], o.Parent.SubscriptionDetails.Metadata[metaOrg])
	byCustomer, err := b.store.BillingAccountByCustomer(ctx, o.Customer)
	if err != nil {
		return nil, err
	}
	if public != "" {
		org, err := b.store.OrgByPublicID(ctx, public)
		if err != nil {
			return nil, err
		}
		if org == nil {
			return nil, nil
		}
		if byCustomer.Exists && byCustomer.OrgID != org.ID {
			// Credit neither. One Stripe customer belongs to one account here, and an event
			// saying otherwise is either a mistake worth a human or somebody trying to attach a
			// payment to a workspace that is not theirs.
			slog.Error("stripe event names an organisation that does not own its customer; ignoring",
				"claimed", public, "customer", o.Customer)
			b.alertOperator("a Stripe event named an organisation that does not own its customer: " + o.Customer)
			return nil, nil
		}
		// And the other direction: an organisation already bound to a customer is not handed a
		// second one by an event. Our checkout reuses the account's customer once it has one, so a
		// different customer naming this account is not ours — and accepting it would write that
		// customer and its subscription over the real ones, which is how the account's own billing
		// could be pointed at somebody else's and then cancelled from under it.
		if o.Customer != "" {
			acct, err := b.store.BillingAccountOf(ctx, org.ID)
			if err != nil {
				return nil, err
			}
			if acct.CustomerID != "" && acct.CustomerID != o.Customer {
				slog.Error("stripe event brings a second customer to an organisation that has one; ignoring",
					"org", public, "has", acct.CustomerID, "event_customer", o.Customer)
				b.alertOperator("a Stripe event tried to bind customer " + o.Customer + " to an account already on " + acct.CustomerID)
				return nil, nil
			}
		}
		return org, nil
	}
	if byCustomer.Exists {
		return b.store.Org(ctx, byCustomer.OrgID)
	}
	return nil, nil
}

// alertOperator is a log line loud enough to find, for the handful of conditions that mean a
// person should look. Not mail: these are deployment problems, not tenant ones.
func (b *Bot) alertOperator(msg string) { slog.Error("billing: " + msg) }

// alignBudgetToPlan keeps the account's own monthly budget in step with what its plan includes.
//
// The two numbers had nothing to do with each other and that was wrong in one direction in
// particular: an account whose plan includes $250 a month, sitting at the $20 the deployment
// happens to default to, is stopped at $20. The customer is paying for credit the guard rail will
// not let them reach, and nothing on the screen explains why the bot went quiet with $230 of their
// own allowance unspent.
//
// So the plan sets it, on the two occasions it is safe to:
//
//   - The plan size moved. The budget moves with it, over a figure somebody typed if there is one,
//     because a guard rail chosen for one plan is not a guard rail for a different one. Somebody
//     who upgrades and wants their old figure back can type it again; somebody who downgrades and
//     keeps a budget from the larger plan has a guard rail that no longer guards anything.
//   - Nobody has ever set one. Then there is no human decision to override and the plan's figure
//     is simply the better default than the deployment's.
//
// Between those, it is left alone. A figure an admin typed on the plan they are on is theirs.
//
// Never above what the plan includes, which is the whole point: it is a ceiling on spending the
// account's own allowance, not a licence to spend past it into prepaid credit. Raising it beyond
// that is a deliberate act and stays one.
func (b *Bot) alignBudgetToPlan(ctx context.Context, orgID, grantedBefore, grantedAfter int64) {
	if grantedAfter <= 0 {
		// No plan, or one that includes nothing. A lapsed subscription keeps whatever figure it
		// had — EffectiveBudget clamps it to the free plan's ceiling anyway, so there is nothing
		// to correct and overwriting it would be a change nobody asked for.
		return
	}
	moved := grantedAfter != grantedBefore
	if !moved && b.store.Setting(ctx, orgID, "monthly_budget_usd") != "" {
		return // their own figure, on the plan they are on
	}
	want := microsToUSD(grantedAfter)
	if err := b.store.PutSetting(ctx, orgID, "monthly_budget_usd",
		strconv.FormatFloat(want, 'f', -1, 64)); err != nil {
		slog.Warn("could not align the monthly budget to the plan", "org", orgID, "err", err)
		return
	}
	b.settings.Invalidate(orgID)
	slog.Info("monthly budget aligned to the plan", "org", orgID, "budget_usd", want, "size_moved", moved)
}

// ---- the effects ----

func (b *Bot) creditTopUp(ctx context.Context, org *Org, ev StripeEvent, paymentID string, amountMinor int64, currency, note string) error {
	if paymentID == "" || amountMinor <= 0 {
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		return nil
	}
	if !b.currencyOK(ctx, ev, currency) {
		return nil
	}
	bal, err := b.store.MoveCredit(ctx, CreditMovement{OrgID: org.ID, Kind: creditTopUp,
		Micros: minorToMicros(amountMinor), ExternalID: paymentID, Note: note, Actor: "stripe",
		Currency: currency, Enforce: true})
	if errors.Is(err, ErrAlreadyCredited) {
		// The other event for the same payment got here first. That is the design working.
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		return nil
	}
	if err != nil {
		return err
	}
	b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
	// The account was warned it was running low and has now fixed it; the next time it runs low
	// is news again.
	b.store.ClearAlert(ctx, org.ID, alertLowCredit)
	b.settings.Invalidate(org.ID)
	b.auditSystem(ctx, org.ID, "billing.credit.topped_up", AuditEvent{TargetKind: "org", TargetID: org.PublicID, TargetName: org.Name,
		Details: auditDetails(map[string]any{"amount_usd": microsToUSD(minorToMicros(amountMinor)),
			"balance_usd": microsToUSD(bal), "external_id": paymentID})})
	slog.Info("credit topped up", "org", org.PublicID, "usd", microsToUSD(minorToMicros(amountMinor)), "balance_usd", microsToUSD(bal))
	return nil
}

// applyChargeEvent takes back what a refunded or disputed top-up put in, and gives a won dispute
// back. The ledger row that credited the payment decides both whose it is and the most there is
// to take: a dispute carries no customer and none of our metadata (so resolving it the way other
// events are resolved found nobody, and a chargeback cost nothing), and a refund of a subscription
// fee was never credit, so it must not come out of prepaid credit. A payment with no top-up on the
// ledger is simply recorded; the operator has already heard about a dispute either way.
func (b *Bot) applyChargeEvent(ctx context.Context, ev StripeEvent, o stripeObject) error {
	orgID, credited, ok, err := b.store.CreditedPayment(ctx, o.PaymentIntent)
	if err != nil {
		return err
	}
	var org *Org
	if ok {
		if org, err = b.store.Org(ctx, orgID); err != nil {
			return err
		}
	}
	if org == nil {
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		return nil
	}
	// What has already come back out of this payment: the refunds, keyed by charge and running
	// total, and the dispute debits. The ledger stores both as negative amounts.
	taken, err := b.store.LedgerByExternalID(ctx, org.ID, creditRefund)
	if err != nil {
		return err
	}
	charge := nonEmpty(o.Charge, o.ID) // a refund event's object is the charge; a dispute names it
	var refunded int64
	for ext, m := range taken {
		if strings.HasPrefix(ext, "refund:"+charge+":") {
			refunded -= m
		}
	}
	switch ev.Type {
	case "charge.refunded":
		// amount_refunded is the charge's running total, not this refund: debit only what is new
		// since the refunds already taken, and never more than the payment credited. Debiting the
		// total each time took a second partial refund twice.
		delta := min(minorToMicros(o.AmountRefund), credited) - refunded
		if delta <= 0 {
			b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
			return nil
		}
		return b.creditRefund(ctx, org, ev, fmt.Sprintf("refund:%s:%d", charge, o.AmountRefund), delta, o.Currency)
	case "charge.dispute.created":
		// Stop the disputed amount being spendable while the dispute is open — at most what the
		// payment still has in it — keyed on the dispute so it lands once.
		amt := min(minorToMicros(o.Amount), credited-refunded)
		if amt <= 0 {
			b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
			return nil
		}
		return b.creditRefund(ctx, org, ev, "dispute:"+o.ID, amt, o.Currency)
	case "charge.dispute.closed":
		// Won: give back exactly what dispute.created took. Lost or otherwise closed: it stands.
		back := -taken["dispute:"+o.ID]
		if o.Status != "won" || back <= 0 {
			b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
			return nil
		}
		return b.creditTopUp(ctx, org, ev, "dispute-won:"+o.ID, back/(microsPerUSD/100), o.Currency, "Dispute resolved in your favour")
	}
	b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
	return nil
}

func (b *Bot) creditRefund(ctx context.Context, org *Org, ev StripeEvent, key string, micros int64, currency string) error {
	if micros <= 0 {
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		return nil
	}
	// Enforce is deliberately false: a refund must not switch the credit floor on for an account
	// that never had it. And the balance is allowed to go negative — clamping a refund at zero
	// would make a chargeback a way of getting free model spend.
	bal, err := b.store.MoveCredit(ctx, CreditMovement{OrgID: org.ID, Kind: creditRefund,
		Micros: -micros, ExternalID: key, Note: "Refunded at Stripe", Actor: "stripe",
		Currency: currency})
	if errors.Is(err, ErrAlreadyCredited) {
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		return nil
	}
	if err != nil {
		return err
	}
	b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
	b.auditSystem(ctx, org.ID, "billing.credit.refunded", AuditEvent{TargetKind: "org", TargetID: org.PublicID, TargetName: org.Name,
		Details: auditDetails(map[string]any{"amount_usd": microsToUSD(micros), "balance_usd": microsToUSD(bal)})})
	return nil
}

// currencyOK refuses money in a currency this deployment does not hold. A Price created in the
// wrong currency would otherwise credit 5,000 of somebody else's minor units as 5,000 of ours,
// which is a gift rather than a rounding error.
func (b *Bot) currencyOK(ctx context.Context, ev StripeEvent, currency string) bool {
	if currency == "" || strings.EqualFold(currency, b.cfg.BillingCurrency) {
		return true
	}
	slog.Error("stripe event is in another currency; crediting nothing",
		"event", ev.ID, "got", currency, "want", b.cfg.BillingCurrency)
	b.alertOperator("a payment arrived in " + currency + " on a deployment holding " + b.cfg.BillingCurrency)
	b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
	return false
}

func minorToMicros(minor int64) int64 { return minor * (microsPerUSD / 100) }

// activateSubscription records a completed subscription checkout. A Checkout Session says who
// bought and which size, and says nothing about the period — so the period is carried forward
// from whatever the subscription events have already established rather than overwritten with
// nothing. PutSubscription writes every column, so a field this event does not know has to be
// filled in here or it is erased; that erasure is what left a freshly paid account reading
// "Renews —" next to a subscription Stripe was perfectly happy to describe.
func (b *Bot) activateSubscription(ctx context.Context, org *Org, ev StripeEvent, o stripeObject) error {
	if b.subscriptionEnded(ctx, o.Subscription) {
		slog.Info("stripe checkout for a subscription that has ended; recorded, not reactivated",
			"org", org.PublicID, "event", ev.ID, "subscription", o.Subscription)
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		return nil
	}
	acct, err := b.store.BillingAccountOf(ctx, org.ID)
	if err != nil {
		return err
	}
	st := SubscriptionState{CustomerID: o.Customer, SubscriptionID: nonEmpty(o.Subscription, acct.SubscriptionID),
		Status: "active", Quantity: max64(acct.Quantity, 1),
		Currency:          nonEmpty(o.Currency, b.cfg.BillingCurrency),
		Size:              acct.Size,
		UnitPriceMicros:   acct.UnitPriceMicros,
		PeriodStart:       acct.PeriodStart,
		PeriodEnd:         acct.PeriodEnd,
		CancelAtPeriodEnd: acct.CancelAtPeriodEnd}
	// Both spellings. Subscriptions created before the rename carry metadata["band"] at Stripe,
	// and nothing on this side can rewrite what Stripe is holding — so the old key is read until
	// those subscriptions have renewed out of existence.
	if size, ok := b.sizeOf(ctx, org.ID, nonEmpty(o.Metadata["size"], o.Metadata["band"])); ok {
		st.Size, st.UnitPriceMicros = size.Key, minorToMicros(size.AmountMinor)
	}
	if err := b.store.PutSubscription(ctx, org.ID, st); err != nil {
		return err
	}
	// Recorded last, once the plan has moved: a recorded event is skipped on redelivery
	// (applyStripeEvent), so recording it before a step that can still fail would lose that step.
	if err := b.movePlan(ctx, org, PlanPro, ev); err != nil {
		return err
	}
	b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
	return nil
}

func (b *Bot) syncSubscription(ctx context.Context, org *Org, ev StripeEvent, o stripeObject) error {
	cur, err := b.store.BillingAccountOf(ctx, org.ID)
	if err != nil {
		return err
	}
	periodStart, periodEnd := o.subscriptionPeriod()
	st := SubscriptionState{CustomerID: o.Customer, SubscriptionID: o.ID, Status: o.Status,
		CancelAtPeriodEnd: o.CancelAtEnd, Quantity: 1,
		PeriodStart: stripeTime(periodStart), PeriodEnd: stripeTime(periodEnd)}
	if len(o.Items.Data) > 0 {
		it := o.Items.Data[0]
		st.Quantity, st.Currency = max64(it.Quantity, 1), it.Price.Currency
		// The price is the authority on which size this is. A price this deployment does not
		// sell leaves the size as it was rather than believing an unknown one.
		if size, ok := b.sizeByPrice(ctx, org.ID, it.Price.ID); ok {
			st.Size, st.UnitPriceMicros = size.Key, minorToMicros(size.AmountMinor)
		} else {
			st.Size, st.UnitPriceMicros = cur.Size, minorToMicros(it.Price.Amount)
		}
	}
	if ev.Type == "customer.subscription.deleted" {
		st.Status, st.SubscriptionID = "canceled", ""
	}
	// A live-looking snapshot of a subscription already seen to end is a late delivery of how it
	// was, not news: Stripe does not bring an ended subscription back. Writing it would put a
	// cancelled account back on its plan.
	ends := subscriptionEnds(st.Status)
	if !ends && b.subscriptionEnded(ctx, o.ID) {
		slog.Info("stripe subscription event for a subscription that has ended; ignoring",
			"org", org.PublicID, "event", ev.ID, "type", ev.Type, "said", st.Status)
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		return nil
	}
	// Recorded and acknowledged, acted on never — the same answer this file gives an event for an
	// organisation it does not have, and for the same reason: replaying it will not make it newer.
	if supersededSnapshot(cur, st.SubscriptionID, st.Status) {
		slog.Info("stripe subscription event describes a state already superseded; ignoring",
			"org", org.PublicID, "event", ev.ID, "type", ev.Type, "was", cur.Status, "said", st.Status)
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		return nil
	}
	if err := b.store.PutSubscription(ctx, org.ID, st); err != nil {
		return err
	}
	if ends {
		b.markSubscriptionEnded(ctx, o.ID)
	}
	b.auditSystem(ctx, org.ID, "billing.subscription."+subscriptionVerb(ev.Type), AuditEvent{
		TargetKind: "org", TargetID: org.PublicID, TargetName: org.Name,
		Details: auditDetails(map[string]any{"status": st.Status, "size": st.Size, "cancel_at_period_end": st.CancelAtPeriodEnd})})

	// Before the plan moves, so that an account arriving on a new size has its allowance by the
	// time anything reads it. This is the call that fixes a size change, which fires
	// customer.subscription.updated and no invoice at all.
	b.syncAllowance(ctx, org.ID)

	switch st.Status {
	case "active", "trialing":
		err = b.movePlan(ctx, org, PlanPro, ev)
	case "canceled", "unpaid", "incomplete_expired":
		// The credit survives: it is prepaid money and still theirs, and credit_enforced never
		// goes back to 0, so the floor keeps binding on what is left.
		//
		// An enterprise account is not moved (movePlan) — so somebody has to hear about it, once,
		// on the change rather than on every redelivery.
		if org.Plan == PlanEnterprise && cur.Status != st.Status {
			b.enterpriseLapsed(ctx, org, st.Status)
		}
		err = b.movePlan(ctx, org, PlanFree, ev)
	default:
		// past_due and incomplete deliberately change nothing. Stripe's dunning retries a failed
		// card for weeks, and cutting a paying customer off on the first decline is the wrong
		// failure.
		b.settings.Invalidate(org.ID)
	}
	if err != nil {
		return err
	}
	// Last, once everything above has happened; see activateSubscription.
	b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
	return nil
}

// supersededSnapshot is "this event describes the subscription as it was BEFORE the state already
// recorded" — the one case where Stripe's unordered delivery is not a tie to break but a state
// that cannot be returned to.
//
// A subscription begins `incomplete`, which means its first invoice has not been paid. When it is
// paid Stripe moves it to `active` or `trialing` and never moves it back: every later failure has
// a name of its own — `past_due`, `unpaid`, `incomplete_expired`, `canceled`. So an `incomplete`
// snapshot of a subscription this account already holds as live is, by construction, the
// `created` event arriving after the `updated` one.
//
// Not hypothetical. The four real deliveries for one $499 purchase arrived invoice.paid,
// subscription.updated (active, created at :65), subscription.created (incomplete, created at
// :63) — and the last one won. It self-corrected there only because checkout.session.completed
// happened to follow; a batch ending on that snapshot leaves an account that has paid reading
// `incomplete`, which Active() answers false to, which refuses a size change and offers to sell
// them a SECOND subscription.
//
// Deliberately narrow. Only `incomplete` is treated this way, because it is the only status whose
// arrow points one way: past_due and active genuinely alternate as a card fails and recovers, and
// a rule about those would drop real news. And it is scoped to the subscription id on record, so
// `incomplete` for a different one is a second subscription being started — news rather than an
// echo — and a fresh subscription bought after a cancellation is never mistaken for a stale
// snapshot of the old one.
func supersededSnapshot(cur BillingAccount, subID, status string) bool {
	if status != "incomplete" || subID == "" || cur.SubscriptionID != subID {
		return false
	}
	return cur.Status == "active" || cur.Status == "trialing"
}

// A subscription that has ended stays ended. Stripe never revives one — a customer who comes back
// gets a new subscription with a new id — so a positive event or a settle for an ended one is a
// replay or a late delivery, never news. The account's own row cannot say so: a cancellation clears
// its subscription id, so that a new checkout is offered. The ending is remembered in
// billing_events instead, under a key no Stripe event id can collide with.
func endedSubscriptionKey(subID string) string { return "ended:" + subID }

func subscriptionEnds(status string) bool {
	return status == "canceled" || status == "incomplete_expired"
}

func (b *Bot) subscriptionEnded(ctx context.Context, subID string) bool {
	return subID != "" && b.store.HasBillingEvent(ctx, endedSubscriptionKey(subID))
}

func (b *Bot) markSubscriptionEnded(ctx context.Context, subID string) {
	if subID != "" {
		b.store.SeenBillingEvent(ctx, "stripe", endedSubscriptionKey(subID), "subscription.ended")
	}
}

func subscriptionVerb(eventType string) string {
	switch eventType {
	case "customer.subscription.created":
		return "activated"
	case "customer.subscription.deleted":
		return "canceled"
	}
	return "updated"
}

func (b *Bot) rollSubscriptionPeriod(ctx context.Context, org *Org, ev StripeEvent, o stripeObject) error {
	// An invoice with no subscription behind it is not a renewal. It is an invoice somebody sent
	// by hand from the Stripe dashboard — which is exactly how an enterprise deal paid "by
	// invoice" gets paid — and treating it as one wrote "active", a period and a plan off a
	// one-off payment. Recorded, and nothing renewed.
	if o.subscriptionID() == "" {
		slog.Info("stripe invoice paid with no subscription behind it; recorded, nothing renewed",
			"org", org.PublicID, "invoice", o.ID)
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		b.auditSystem(ctx, org.ID, "billing.invoice_paid", AuditEvent{TargetKind: "org", TargetID: org.PublicID,
			TargetName: org.Name, Details: auditDetails(map[string]any{"invoice": o.ID})})
		return nil
	}
	// A paid invoice for a subscription that has ended renews nothing: it is a final proration, or
	// a late delivery, and writing "active" off it would put a cancelled account back on its plan.
	if b.subscriptionEnded(ctx, o.subscriptionID()) {
		slog.Info("stripe invoice paid for a subscription that has ended; recorded, nothing renewed",
			"org", org.PublicID, "invoice", o.ID, "subscription", o.subscriptionID())
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		return nil
	}
	acct, err := b.store.BillingAccountOf(ctx, org.ID)
	if err != nil {
		return err
	}
	st := SubscriptionState{CustomerID: nonEmpty(o.Customer, acct.CustomerID),
		SubscriptionID: nonEmpty(o.subscriptionID(), acct.SubscriptionID), Status: "active",
		Size: acct.Size, UnitPriceMicros: acct.UnitPriceMicros, Quantity: max64(acct.Quantity, 1),
		Currency: acct.Currency, PeriodStart: acct.PeriodStart, PeriodEnd: acct.PeriodEnd,
		CancelAtPeriodEnd: acct.CancelAtPeriodEnd}
	price, start, end := o.subscriptionLine()
	if end > 0 {
		st.PeriodStart, st.PeriodEnd = stripeTime(start), stripeTime(end)
	}
	// What the line was charged at is what the size IS, so an invoice that arrives before the
	// subscription event — Stripe orders nothing — still names the size rather than leaving the
	// record blank until the other event turns up.
	if size, ok := b.sizeByPrice(ctx, org.ID, price); ok {
		st.Size, st.UnitPriceMicros = size.Key, minorToMicros(size.AmountMinor)
	}
	// The fee itself is still not credit: an account that pays $199 does not get $199 to spend,
	// it gets the allowance its size includes, which is a separate figure in STRIPE_SIZES. What
	// this event does is roll the period, and then hand over that allowance for the month it
	// just paid for.
	if err := b.store.PutSubscription(ctx, org.ID, st); err != nil {
		return err
	}
	b.syncAllowance(ctx, org.ID)
	if err := b.movePlan(ctx, org, PlanPro, ev); err != nil {
		return err
	}
	// Last, once everything above has happened; see activateSubscription.
	b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
	return nil
}

// syncAllowance brings the account's monthly allowance into line with the size it is on and the
// period it is in. Safe and cheap to call from anything that touched the subscription, which is
// why it is called from everything that does.
//
// It replaces a grant that hung off invoice.paid alone. That was wrong in a way nobody would spot
// until a customer did: a size change produces no invoice — Stripe prorates onto the next one — so
// an account that upgraded saw its new plan promising $100 of credit next to a balance of $0.00,
// with nothing in the logs to say why and nothing to do but wait a month. An allowance is not an
// event that may or may not arrive; it is a property of the size and the period, and this reads it
// as one.
//
// An account with no live subscription resolves to zero, which is also how a cancelled plan stops
// being metered rather than being metered against an allowance it no longer has.
// syncAllowances is the hourly catch-all behind the leader lease. Every other caller is an event,
// and the failure this covers is the event that never arrived.
func (b *Bot) syncAllowances(ctx context.Context) {
	orgs, err := b.store.OrgsWithAllowanceDrift(ctx)
	if err != nil {
		slog.Warn("could not list accounts to sync allowances for", "err", err)
		return
	}
	for _, id := range orgs {
		b.syncAllowance(ctx, id)
	}
}

func (b *Bot) syncAllowance(ctx context.Context, orgID int64) {
	acct, err := b.store.BillingAccountOf(ctx, orgID)
	if err != nil {
		slog.Warn("could not read the billing account to sync its allowance", "org", orgID, "err", err)
		return
	}
	// An unpaid renewal hands over nothing. Stripe moves a past_due subscription's period on while
	// its dunning retries the card, and reading that as a renewal granted a month of included credit
	// nobody had paid for. The allowance already granted stays until its own period ends; the next
	// one arrives with invoice.paid, which is the payment.
	if acct.Status == "past_due" {
		return
	}
	var want int64
	periodEnd := acct.PeriodEnd
	if acct.Active() {
		if size, ok := b.sizeOf(ctx, orgID, acct.Size); ok {
			want = minorToMicros(size.IncludedMinor)
		}
	}
	// An enterprise deal includes a month's credit whatever its fee's interval: a yearly price
	// must neither hand over a year of it at once nor pin one month's to a year.
	monthly := acct.Size == sizeEnterprise
	// A comped size has no Stripe period, and an account whose allowance is pinned to nothing
	// would never expire it. A month is the honest reading of "included each month" for a plan
	// nobody is invoicing through Stripe — and it is ONE month: a live period is kept until it
	// lapses, and only then does the next one start. Working out "a month from now" afresh on
	// every call moved the period on every call, and SyncAllowance reads a moved period as a
	// renewal — so the hourly catch-all refilled a comped account's allowance every hour.
	if want > 0 && (periodEnd == "" || monthly) {
		if acct.AllowanceLive() {
			periodEnd = acct.AllowancePeriodEnd
		} else {
			periodEnd = time.Now().UTC().AddDate(0, 1, 0).Format(time.DateTime)
		}
	}
	before := acct.SpendableAllowanceMicros()
	if err := b.store.SyncAllowance(ctx, orgID, acct.Size, periodEnd, want); err != nil {
		slog.Warn("could not sync the allowance", "org", orgID, "err", err)
		return
	}
	after, _ := b.store.BillingAccountOf(ctx, orgID)
	// Not over an enterprise budget the operator wrote: that figure is part of the deal, and the
	// plan's allowance moving is no reason to overwrite it. Without one, the deal is aligned like
	// any size, so an account sitting at the deployment default is not stopped short of the
	// credit its deal includes.
	if plan, grant := b.store.OrgPlan(ctx, orgID); plan != PlanEnterprise || grant <= 0 {
		b.alignBudgetToPlan(ctx, orgID, acct.AllowanceGrantedMicros, after.AllowanceGrantedMicros)
	}
	if after.AllowanceMicros == before && after.AllowancePeriodEnd == acct.AllowancePeriodEnd {
		return // nothing moved; not worth a log line or an audit row every webhook
	}
	// The low-credit warning is news again once there is credit, exactly as after a top-up, and
	// the cached billing facts no longer know whether this account is metered.
	b.store.ClearAlert(ctx, orgID, alertLowCredit)
	b.settings.Invalidate(orgID)
	slog.Info("plan allowance synced", "org", orgID, "size", acct.Size,
		"granted_usd", microsToUSD(after.AllowanceGrantedMicros),
		"left_usd", microsToUSD(after.AllowanceMicros), "expires", after.AllowancePeriodEnd)
}

func (b *Bot) markPastDue(ctx context.Context, org *Org, ev StripeEvent, o stripeObject) error {
	// The same line as rollSubscriptionPeriod's: a one-off invoice that failed says nothing about
	// a subscription, and marking the account past_due over it would leave an invoiced deal
	// reading past_due after the invoice was paid, since that payment renews nothing either.
	if o.subscriptionID() == "" {
		b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
		b.auditSystem(ctx, org.ID, "billing.payment_failed", AuditEvent{Outcome: auditFailed,
			TargetKind: "org", TargetID: org.PublicID, TargetName: org.Name,
			Details: auditDetails(map[string]any{"invoice": o.ID})})
		return nil
	}
	acct, err := b.store.BillingAccountOf(ctx, org.ID)
	if err != nil || !acct.Exists {
		return err
	}
	st := SubscriptionState{CustomerID: acct.CustomerID, SubscriptionID: acct.SubscriptionID,
		Status: "past_due", Size: acct.Size, UnitPriceMicros: acct.UnitPriceMicros,
		Quantity: acct.Quantity, Currency: acct.Currency, PeriodStart: acct.PeriodStart,
		PeriodEnd: acct.PeriodEnd, CancelAtPeriodEnd: acct.CancelAtPeriodEnd}
	if err := b.store.PutSubscription(ctx, org.ID, st); err != nil {
		return err
	}
	b.store.SeenBillingEvent(ctx, "stripe", ev.ID, ev.Type)
	b.settings.Invalidate(org.ID)
	b.auditSystem(ctx, org.ID, "billing.payment_failed", AuditEvent{Outcome: auditFailed,
		TargetKind: "org", TargetID: org.PublicID, TargetName: org.Name,
		Details: auditDetails(map[string]any{"amount_usd": float64(o.AmountTotal) / 100})})
	b.mailBillingOwner(ctx, org, "attest_tag: a payment did not go through",
		paymentFailedEmail(org.Name, float64(o.AmountTotal)/100))
	return nil
}

// movePlan is the only way a payment changes a plan, and it goes through the same choke point an
// operator does (applyPlan) so the cache drop, the config version bump, the cleared request and
// the audit row all still happen.
//
// SetBudget is false. setPlan defaults a zero budget to defaultProBudgetUSD and writes it to the
// account's own setting; doing that from here would cap a customer who has just paid for $500 of
// credit at $25 a month and overwrite a guard rail they chose themselves.
//
// Nor does a payment ever move an enterprise account, in either direction. Its plan is the deal the
// operator wrote; a subscription that renews confirms it and one that lapses is a conversation
// (enterpriseLapsed), and letting either rewrite the plan would turn a card decline into a
// demotion to free, or a renewal into a demotion to pro.
func (b *Bot) movePlan(ctx context.Context, org *Org, plan string, ev StripeEvent) error {
	if org.Plan == plan || org.Plan == PlanEnterprise {
		b.settings.Invalidate(org.ID)
		return nil
	}
	return b.applyPlan(ctx, org, planChange{Plan: plan, SetBudget: false, System: true, By: "stripe:" + ev.ID})
}

// stripeTime renders a Unix second as the text timestamp every other column in this database uses.
func stripeTime(secs int64) string {
	if secs <= 0 {
		return ""
	}
	return time.Unix(secs, 0).UTC().Format(time.DateTime)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ---- settling on the way back ----

// settleMaxAge is how recent a checkout session must be for the return-from-Stripe shortcut to
// apply it. It only has to cover the round trip from Stripe back to the console; anything older is
// a replay, not a return.
const settleMaxAge = time.Hour

// settleCheckout applies one session the console has just come back from, without waiting for its
// webhook. Idempotent with the webhook because both key the ledger on the payment, so whichever
// runs first credits and the other is a no-op.
//
// It refuses a session that does not name this organisation: the session id travels in a URL the
// browser controls, so it is a claim, not a fact.
// It returns whether the payment is now reflected in this organisation's figures. False means
// "not yet", not "never": the webhook is still coming, and the console says so rather than
// showing a balance it is about to contradict.
func (b *Bot) settleCheckout(ctx context.Context, orgID int64, sessionID string) bool {
	sess, err := b.pay.Session(ctx, sessionID)
	if err != nil {
		slog.Warn("could not read the checkout session back", "session", sessionID, "err", err)
		return false
	}
	org, err := b.store.Org(ctx, orgID)
	if err != nil || org == nil {
		return false
	}
	// Our metadata only, for the reason orgForStripe gives: client_reference_id is the payer's to
	// set on a Payment Link, so a session naming this account there and nowhere else is a payment
	// this server never started — an enterprise fee, say — and crediting it would turn it into
	// spendable model credit that the webhook, rightly, refuses to.
	if sess.Metadata[metaOrg] != org.PublicID {
		slog.Warn("a checkout session was offered to an organisation it does not belong to", "session", sessionID)
		return false
	}
	// Settle only a session from the checkout the customer just came back from. Without this, the
	// endpoint would re-apply any past session of theirs: replaying the one from a larger plan
	// (or from before a cancellation) restored that size — Status is forced to "active" and the
	// size is taken from the session's own metadata — and the hourly allowance sync then topped
	// the credit back up. The webhook remains the authority for later changes; this is only the
	// first-screen shortcut, and a stale session is not that.
	if sess.Created > 0 && time.Since(time.Unix(sess.Created, 0)) > settleMaxAge {
		slog.Warn("ignoring a stale checkout session on settle", "session", sessionID, "age", time.Since(time.Unix(sess.Created, 0)).Round(time.Second))
		return false
	}
	// Only a finished checkout is settled. An open session has not been paid for, and an expired
	// one never will be; the webhook is the authority on either if that changes.
	if sess.Status != "complete" {
		return false
	}
	ev := StripeEvent{ID: "settle:" + sess.ID, Type: "checkout.session.completed"}
	switch {
	case sess.Mode == "payment" && sess.PaymentStatus == "paid":
		// creditTopUp swallows a repeat, so "the webhook got here first" also counts as settled.
		return b.creditTopUp(ctx, org, ev, sess.PaymentIntent, sess.AmountMinor, sess.Currency, "Card payment") == nil
	case sess.Mode == "subscription" && sess.SubscriptionID != "" &&
		(sess.PaymentStatus == "paid" || sess.PaymentStatus == "no_payment_required"):
		// A subscription this account has already seen end is never brought back by a return
		// trip: Stripe does not revive a cancelled subscription (a new one gets a new id), so a
		// session naming one is a replay from before the cancellation.
		if b.subscriptionEnded(ctx, sess.SubscriptionID) {
			slog.Warn("ignoring a settle for a subscription that has ended", "session", sessionID, "subscription", sess.SubscriptionID)
			return false
		}
		// Everything this session does not say is carried forward. PutSubscription writes every
		// column, and this path runs on the FIRST page the customer sees after paying: filling
		// it from the session alone published a blank record over a correct one and greeted them
		// with "Size —, Monthly fee $0.00, Renews —" for the plan they had just bought.
		acct, err := b.store.BillingAccountOf(ctx, org.ID)
		if err != nil {
			return false
		}
		st := SubscriptionState{CustomerID: nonEmpty(sess.CustomerID, acct.CustomerID),
			SubscriptionID: nonEmpty(sess.SubscriptionID, acct.SubscriptionID),
			Status:         "active", Quantity: max64(acct.Quantity, 1),
			Currency:          nonEmpty(sess.Currency, b.cfg.BillingCurrency),
			Size:              acct.Size,
			UnitPriceMicros:   acct.UnitPriceMicros,
			PeriodStart:       acct.PeriodStart,
			PeriodEnd:         acct.PeriodEnd,
			CancelAtPeriodEnd: acct.CancelAtPeriodEnd}
		// The size is the one thing the session does know, and it is the reason this path can
		// beat the webhook without showing less than the webhook would.
		if size, ok := b.sizeOf(ctx, org.ID, nonEmpty(sess.Metadata["size"], sess.Metadata["band"])); ok {
			st.Size, st.UnitPriceMicros = size.Key, minorToMicros(size.AmountMinor)
		}
		if err := b.store.PutSubscription(ctx, org.ID, st); err != nil {
			return false
		}
		return b.movePlan(ctx, org, PlanPro, ev) == nil
	}
	// A delayed payment method, or a session that was never completed. Either way the figures
	// have not moved and saying they have would be a lie the next reload contradicts.
	return false
}

// ---- who hears about it ----

// mailBillingOwner sends one billing message to the person most likely to be able to act on it:
// whoever pressed the button, if the event remembers them, and the founder otherwise.
//
// Deliberately not every admin. These are receipts and card failures, not incidents, and a mail
// that fans out to eight people is one that eight people assume somebody else is handling.
func (b *Bot) mailBillingOwner(ctx context.Context, org *Org, subject, body string) {
	to := b.billingContact(ctx, org)
	if to == "" {
		return
	}
	if err := b.mail.Send(ctx, Mail{To: to, Subject: subject, Body: body}); err != nil {
		// Never fatal to the caller: the money has already moved and the ledger already says so.
		slog.Warn("billing mail not sent", "org", org.PublicID, "err", err)
	}
}

func (b *Bot) billingContact(ctx context.Context, org *Org) string {
	if org.CreatedBy != 0 {
		if u, err := b.store.User(ctx, org.CreatedBy); err == nil && u != nil {
			return u.Email
		}
	}
	return ""
}

// billingURL is where every billing mail and every Slack refusal points. One function, because a
// path that drifts between them is a support request that arrives at the wrong screen.
func (b *Bot) billingURL(ctx context.Context) string {
	base := publicBaseURL(ctx, b.store, b.cfg)
	if base == "" {
		return ""
	}
	return base + "/admin/settings/?tab=billing"
}

// ---- housekeeping ----

// runBilling is billing's once-per-deployment loop, behind the leader lease like retention. Three
// jobs, in this order because the first produces the numbers the second reads:
//
//  1. roll the hour's spend up into each account's statement;
//  2. warn any account that is running low, once per window;
//  3. drop webhook dedup keys older than Stripe would still be retrying.
//
// Modelled on runRetention: body first, then the select, so the work happens at boot as well as
// on the tick. Nothing here is on a request path, and nothing here takes money — a loop that
// could would be a loop that runs twice if the lease ever overlapped.
func (b *Bot) runBilling(ctx context.Context) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		b.rollUpCredit(ctx)
		b.warnLowCredit(ctx)
		// Stripe gives up retrying after about three days. Keeping the keys a month either side
		// of that costs nothing and means a very late redelivery is still recognised.
		b.store.SweepBillingEvents(ctx, 30*24*time.Hour)
		// Not the tenant's retention policy — see the note in retention.go for why this table is
		// not on that list. This is the table's own ceiling, long enough to answer what an
		// account was being charged for a year ago and to draw a trend from.
		b.store.SweepActiveUsers(ctx, activeUserRetention)
		// Housekeeping, not policy: every read already treats a lapsed allowance as spent, so
		// nothing depends on this having run. It stops a row in the database claiming credit the
		// gate would refuse.
		b.store.ExpireAllowances(ctx)
		b.syncAllowances(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (b *Bot) rollUpCredit(ctx context.Context) {
	orgs, err := b.store.OrgsWithUnrolledDebit(ctx)
	if err != nil {
		slog.Error("credit roll-up could not list accounts", "err", err)
		return
	}
	at := time.Now()
	for _, id := range orgs {
		org, err := b.store.Org(ctx, id)
		if err != nil || org == nil {
			continue
		}
		written, err := b.store.RollUpDebit(ctx, id, org.PublicID, at)
		if err != nil {
			slog.Error("credit roll-up failed", "org", org.PublicID, "err", err)
			continue
		}
		if written > 0 {
			slog.Info("credit spend written up", "org", org.PublicID, "usd", microsToUSD(written))
		}
	}
}

// warnLowCredit tells an account it is running out, in the channel it nominated and to the person
// who pays. AlertOnce is what keeps it to once a window, counted in the database so several
// instances cannot each send their own, and ClearAlert on a top-up is what makes the next time
// news again.
func (b *Bot) warnLowCredit(ctx context.Context) {
	orgs, err := b.store.OrgIDs(ctx)
	if err != nil {
		return
	}
	low := creditLowMicros(b.cfg)
	if low <= 0 {
		return
	}
	for _, id := range orgs {
		acct, err := b.store.BillingAccountOf(ctx, id)
		if err != nil || !acct.Exists || !acct.CreditEnforced {
			continue
		}
		if acct.CreditBalanceMicros > low {
			continue
		}
		if !b.store.AlertOnce(ctx, id, alertLowCredit, lowCreditWindow) {
			continue
		}
		org, err := b.store.Org(ctx, id)
		if err != nil || org == nil {
			continue
		}
		bal := microsToUSD(acct.CreditBalanceMicros)
		slog.Info("credit is low", "org", org.PublicID, "balance_usd", bal)
		b.mailBillingOwner(ctx, org, "attest_tag: your API credit is running low",
			lowCreditEmail(org.Name, bal, b.cfg.CreditLowUSD, b.billingURL(ctx)))
		// And in the room, if they nominated one. The same alert channel the budget warnings
		// use, so "the bot is about to stop" always arrives in the same place.
		if ch := b.settings.Get(ctx, id).AlertChannel; ch != "" {
			b.agent.postAlert(ctx, id, fmt.Sprintf(":credit_card: API credit is down to $%.2f. The bot stops when it runs out — top up under Settings → Billing.", bal))
		}
	}
}
