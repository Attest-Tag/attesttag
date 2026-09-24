package app

// The enterprise plan: a deal the operator writes, which no payment may move and no member may
// change. Most of what can go wrong with it is a path that already exists for the ladder quietly
// doing the ladder's thing to an account that is not on it — a renewal moving it to pro, a
// cancellation moving it to free, an hourly sync refilling its credit — so most of these tests
// deliver the ordinary event and check that the deal survived it.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const enterpriseSecret = "correct-horse-battery-staple"

// acmePrice is a Price made for one enterprise account: $2,500 a month.
var acmePrice = PriceInfo{ID: "price_acme", Active: true, Currency: "usd", UnitAmount: 250000,
	Recurring: true, Interval: "month", IntervalN: 1}

// enterpriseBot is a deployment that sells the ladder, has an operator secret, and takes money
// through a fake that knows one enterprise Price.
func enterpriseBot(t *testing.T, cfg Config) (*Bot, *http.ServeMux, *Store, *fakePayments) {
	t.Helper()
	fixedMasterKey(t)
	cfg.OperatorSecret, cfg.SupportEmail = enterpriseSecret, testSupportEmail
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakePayments{prices: map[string]PriceInfo{acmePrice.ID: acmePrice}}
	b := &Bot{cfg: cfg, store: st, sealer: sealer, mail: logMailer{}, settings: newSettingsCache(st, cfg),
		resolver: NewResolver(st), pay: fake}
	b.slacks = NewChatRegistry(st, sealer)
	mux := http.NewServeMux()
	b.routes(mux, nil)
	return b, mux, st, fake
}

// putDeal is the operator's JSON route, as deploy/plan.sh calls it.
func putDeal(t *testing.T, mux *http.ServeMux, org *Org, body string) (int, map[string]any) {
	t.Helper()
	return call(t, mux, "PUT", "/api/operator/orgs/"+org.PublicID+"/plan", body, "Bearer "+enterpriseSecret)
}

func reloadOrg(t *testing.T, st *Store, id int64) *Org {
	t.Helper()
	o, err := st.Org(context.Background(), id)
	if err != nil || o == nil {
		t.Fatalf("org %d: %v", id, err)
	}
	return o
}

// The whole of the operator's path, and the property that makes it usable from a terminal: a
// change names only what it changes.
func TestTheOperatorWritesADealAndChangesItAFieldAtATime(t *testing.T) {
	b, mux, st, _ := enterpriseBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Acme Corp")

	code, out := putDeal(t, mux, org, `{"plan":"enterprise","users":400,"jobs":900,"included_usd":300,
		"budget_usd":2000,"fee_usd":3000,"interval":"year","note":"MSA signed 2026-09-22"}`)
	if code != 200 || out["plan"] != PlanEnterprise {
		t.Fatalf("moving to enterprise answered %d %v", code, out)
	}
	deal, _ := out["enterprise"].(map[string]any)
	if deal == nil || deal["users"] != float64(400) || deal["paid_by"] != "invoice" || deal["note"] != "MSA signed 2026-09-22" {
		t.Fatalf("the operator's view of the deal is %v", deal)
	}

	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Status != statusInvoiced || acct.Size != sizeEnterprise || !acct.Active() {
		t.Fatalf("an invoiced deal is recorded as %q/%q, active=%v", acct.Status, acct.Size, acct.Active())
	}
	if got := acct.SpendableAllowanceMicros(); got != usdToMicros(300) {
		t.Errorf("the deal includes $300 a month and the account holds %s", creditAmount(got))
	}
	set := b.settings.Get(ctx, org.ID)
	if set.PlatformBudgetUSD != 2000 || set.EffectiveBudget() != 2000 {
		t.Errorf("the deal's $2,000 budget reads as ceiling %v, effective %v", set.PlatformBudgetUSD, set.EffectiveBudget())
	}

	// Six hundred users, and nothing else touched.
	if code, out := putDeal(t, mux, org, `{"plan":"enterprise","users":600}`); code != 200 {
		t.Fatalf("changing the users answered %d %v", code, out)
	}
	terms, _ := st.EnterpriseTerms(ctx, org.ID)
	if terms.UserLimit != 600 || terms.JobLimit != 900 || terms.IncludedMinor != 30000 ||
		terms.FeeMinor != 300000 || terms.FeeInterval != "year" || terms.Note != "MSA signed 2026-09-22" {
		t.Fatalf("a change of users rewrote the rest of the deal: %+v", terms)
	}
	if o := reloadOrg(t, st, org.ID); o.PlanBudgetUSD != 2000 {
		t.Errorf("a change that did not name the budget moved it to %v", o.PlanBudgetUSD)
	}
	if u := b.activeUsers(ctx, org.ID, false); u.Limit != 600 {
		t.Errorf("the user ceiling the tile shows is %d, want the deal's 600", u.Limit)
	}
}

// The operator's secret is the only way in. A console session — even an admin's — is not it.
func TestOnlyTheOperatorSecretWritesADeal(t *testing.T) {
	_, mux, st, _ := enterpriseBot(t, includedCfg())
	orgID, _, tok := seedOrg(t, st, RoleAdmin)
	org := reloadOrg(t, st, orgID)
	for _, auth := range []string{"", "Bearer wrong", "Bearer " + tok} {
		if code, _ := call(t, mux, "PUT", "/api/operator/orgs/"+org.PublicID+"/plan",
			`{"plan":"enterprise","users":10}`, auth); code != 401 {
			t.Errorf("auth %q wrote a deal: %d", auth, code)
		}
	}
	if o := reloadOrg(t, st, orgID); o.Plan != PlanFree {
		t.Fatalf("a refused request moved the plan to %s", o.Plan)
	}
}

// Enterprise is a plan before it is a way of paying, so it works on a deployment with no Stripe
// keys — every self-host — and refuses only the one thing that needs Stripe.
func TestADealNeedsNoStripeUnlessItIsPaidBySubscription(t *testing.T) {
	b, mux, st, _ := enterpriseBot(t, Config{})
	ctx := context.Background()
	org := testOrg(t, st, "Self Hosted Ltd")
	if code, out := putDeal(t, mux, org, `{"plan":"enterprise","users":50,"budget_usd":500}`); code != 200 {
		t.Fatalf("a deal without Stripe answered %d %v", code, out)
	}
	if set := b.settings.Get(ctx, org.ID); set.Plan != PlanEnterprise || set.EffectiveBudget() != 500 {
		t.Fatalf("the deal reads as plan %q with budget %v", set.Plan, set.EffectiveBudget())
	}
	if acct, _ := st.BillingAccountOf(ctx, org.ID); acct.Exists {
		t.Error("a deployment with no billing grew a billing row: an org with none must behave as before")
	}
	if code, _ := putDeal(t, mux, org, `{"plan":"enterprise","price_id":"price_acme"}`); code != 400 {
		t.Errorf("a price with no Stripe keys to subscribe through answered %d", code)
	}
}

// A price is read from Stripe when it is named, so the typo is the operator's to see rather than
// the customer's to meet at checkout, and the fee on the screen is the one that is charged.
func TestAnEnterprisePriceIsCheckedWhenItIsNamed(t *testing.T) {
	_, mux, st, fake := enterpriseBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Picky Ltd")
	bad := map[string]PriceInfo{
		"price_archived": {ID: "price_archived", Currency: "usd", UnitAmount: 100, Recurring: true, Interval: "month", IntervalN: 1},
		"price_oneoff":   {ID: "price_oneoff", Active: true, Currency: "usd", UnitAmount: 100},
		"price_eur":      {ID: "price_eur", Active: true, Currency: "eur", UnitAmount: 100, Recurring: true, Interval: "month", IntervalN: 1},
		"price_weekly":   {ID: "price_weekly", Active: true, Currency: "usd", UnitAmount: 100, Recurring: true, Interval: "week", IntervalN: 1},
		"price_tiered":   {ID: "price_tiered", Active: true, Currency: "usd", Tiered: true, Recurring: true, Interval: "month", IntervalN: 1},
	}
	for id, p := range bad {
		fake.prices[id] = p
		if code, out := putDeal(t, mux, org, `{"plan":"enterprise","price_id":"`+id+`"}`); code != 400 {
			t.Errorf("%s was accepted: %d %v", id, code, out)
		}
	}
	for _, body := range []string{
		`{"plan":"enterprise","price_id":"price_missing"}`,                                   // Stripe has never heard of it
		`{"plan":"enterprise","price_id":"prod_123"}`,                                        // not a price at all
		`{"plan":"enterprise","pay_url":"javascript:alert(1)"}`,                              // not a page
		`{"plan":"enterprise","pay_url":"http://pay.example/x"}`,                             // not https
		`{"plan":"enterprise","price_id":"price_acme","pay_url":"https://buy.stripe.com/x"}`, // both
		`{"plan":"enterprise","users":-1}`,
		`{"plan":"enterprise","budget_usd":1e9}`,
	} {
		if code, _ := putDeal(t, mux, org, body); code != 400 {
			t.Errorf("%s answered %d, want 400", body, code)
		}
	}
	if o := reloadOrg(t, st, org.ID); o.Plan != PlanFree {
		t.Fatalf("refused deals moved the plan to %s", o.Plan)
	}

	// A good one. The typed fee loses to the price's, and the account waits for its admin to
	// subscribe: nothing is live, so nothing is included yet.
	if code, out := putDeal(t, mux, org, `{"plan":"enterprise","price_id":"price_acme","fee_usd":1,"included_usd":100}`); code != 200 {
		t.Fatalf("a good price answered %d %v", code, out)
	}
	terms, _ := st.EnterpriseTerms(ctx, org.ID)
	if terms.FeeMinor != 250000 || terms.FeeInterval != "month" || terms.paidBy() != "subscription" {
		t.Fatalf("the deal records %+v, want the price's $2,500 a month", terms)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Active() || acct.Size != sizeEnterprise || acct.SpendableAllowanceMicros() != 0 {
		t.Fatalf("an unpaid subscription deal is %q/%q with %s included", acct.Status, acct.Size,
			creditAmount(acct.SpendableAllowanceMicros()))
	}

	// Naming a link swaps the way it pays; the price goes with it rather than lingering.
	if code, _ := putDeal(t, mux, org, `{"plan":"enterprise","pay_url":"https://buy.stripe.com/test_abc"}`); code != 200 {
		t.Fatal("switching to a link was refused")
	}
	if terms, _ = st.EnterpriseTerms(ctx, org.ID); terms.PriceID != "" || terms.paidBy() != "link" {
		t.Fatalf("after naming a link the deal is %+v", terms)
	}
	if acct, _ = st.BillingAccountOf(ctx, org.ID); acct.Status != statusInvoiced {
		t.Errorf("a deal paid at a link is %q, want invoiced", acct.Status)
	}
}

// The customer's half: Subscribe opens Checkout on the deal's own Price, named by the deal and
// never by the browser — and nothing on the ladder can be bought beside it.
func TestAnEnterpriseAccountSubscribesToItsOwnPrice(t *testing.T) {
	b, mux, st, fake := enterpriseBot(t, includedCfg())
	org := testOrg(t, st, "Subscriber Corp")
	checkout := func(kind string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/billing/checkout", strings.NewReader(`{"kind":"`+kind+`","size":"upto_25"}`))
		r = r.WithContext(context.WithValue(r.Context(), userKey, &AdminUser{ID: 1, OrgID: org.ID,
			OrgPublic: org.PublicID, OrgName: org.Name, PublicID: "actor", Email: "admin@example.com",
			Permissions: map[Permission]bool{PermBillingManage: true}}))
		w := httptest.NewRecorder()
		b.handleBillingCheckout(w, r)
		return w
	}
	if w := checkout("enterprise"); w.Code != 400 {
		t.Errorf("a free account opened an enterprise checkout: %d", w.Code)
	}
	if code, _ := putDeal(t, mux, org, `{"plan":"enterprise","price_id":"price_acme"}`); code != 200 {
		t.Fatal("could not write the deal")
	}
	b.settings.Invalidate(org.ID)
	if w := checkout("enterprise"); w.Code != 200 {
		t.Fatalf("the enterprise checkout answered %d: %s", w.Code, w.Body)
	}
	if fake.checkout.Mode != "subscription" || fake.checkout.PriceID != "price_acme" ||
		fake.checkout.Metadata["size"] != sizeEnterprise || fake.checkout.Metadata[metaOrg] != org.PublicID {
		t.Fatalf("checkout was opened as %+v", fake.checkout)
	}
	if w := checkout("subscription"); w.Code != 400 {
		t.Errorf("an enterprise account bought a rung of the ladder: %d", w.Code)
	}
	// Invoiced by hand: there is nothing to subscribe to.
	if code, _ := putDeal(t, mux, org, `{"plan":"enterprise","price_id":""}`); code != 200 {
		t.Fatal("could not clear the price")
	}
	if w := checkout("enterprise"); w.Code != 400 {
		t.Errorf("an invoiced deal opened a checkout: %d", w.Code)
	}
}

// The property the whole plan rests on. Every event below moves a ladder account's plan; none of
// them may move this one — and a lapse is told to a person, once.
func TestStripeNeverMovesAnEnterpriseAccount(t *testing.T) {
	b, mux, st, _ := enterpriseBot(t, includedCfg())
	mail := &catchMailer{}
	b.mail = mail
	ctx := context.Background()
	org := testOrg(t, st, "Steady Corp")
	if code, _ := putDeal(t, mux, org, `{"plan":"enterprise","price_id":"price_acme","included_usd":250,"users":300}`); code != 200 {
		t.Fatal("could not write the deal")
	}
	start, end := time.Now().Unix(), time.Now().AddDate(0, 1, 0).Unix()

	// Checkout completes: the session names the organisation and binds the customer.
	done := fmt.Sprintf(`{"id":"evt_done","type":"checkout.session.completed","livemode":false,"data":{"object":{
		"id":"cs_e","object":"checkout.session","mode":"subscription","customer":"cus_ent","subscription":"sub_ent",
		"client_reference_id":%q,"metadata":{"attesttag_org":%q,"size":"enterprise"}}}}`, org.PublicID, org.PublicID)
	if w := postWebhook(t, b, done, true); w.Code != 200 {
		t.Fatalf("checkout.session.completed answered %d: %s", w.Code, w.Body)
	}
	if w := postWebhook(t, b, basilSubscription("evt_sub", "customer.subscription.created", "sub_ent", "cus_ent", "price_acme", start, end), true); w.Code != 200 {
		t.Fatalf("customer.subscription.created answered %d: %s", w.Code, w.Body)
	}
	if o := reloadOrg(t, st, org.ID); o.Plan != PlanEnterprise {
		t.Fatalf("paying for the deal moved the account to %s", o.Plan)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Status != "active" || acct.Size != sizeEnterprise || acct.SubscriptionID != "sub_ent" {
		t.Fatalf("the paid deal is recorded as %q/%q/%q", acct.Status, acct.Size, acct.SubscriptionID)
	}
	if got := acct.SpendableAllowanceMicros(); got != usdToMicros(250) {
		t.Errorf("the paid deal includes %s, want $250", creditAmount(got))
	}
	if acct.UnitPriceMicros != usdToMicros(2500) {
		t.Errorf("the fee on record is %s, want the deal's $2,500", creditAmount(acct.UnitPriceMicros))
	}

	// Cancelled at Stripe. A ladder account goes to free here; this one stays, and support hears.
	gone := basilSubscriptionAs("evt_gone", "customer.subscription.deleted", "canceled", "sub_ent", "cus_ent", "price_acme", start, end)
	if w := postWebhook(t, b, gone, true); w.Code != 200 {
		t.Fatalf("customer.subscription.deleted answered %d: %s", w.Code, w.Body)
	}
	postWebhook(t, b, strings.Replace(gone, "evt_gone", "evt_gone_again", 1), true)
	if o := reloadOrg(t, st, org.ID); o.Plan != PlanEnterprise {
		t.Fatalf("a cancelled subscription moved the enterprise account to %s", o.Plan)
	}
	acct, _ = st.BillingAccountOf(ctx, org.ID)
	if acct.Status != "canceled" || acct.SpendableAllowanceMicros() != 0 {
		t.Errorf("after cancelling: status %q, %s still included", acct.Status, creditAmount(acct.SpendableAllowanceMicros()))
	}
	lapses := 0
	for _, m := range mail.sent {
		if m.To == testSupportEmail && strings.Contains(m.Subject, "Enterprise subscription canceled") {
			lapses++
		}
	}
	if lapses != 1 {
		t.Errorf("support was told about the lapse %d times, want once", lapses)
	}
}

// Paying "by invoice" means an invoice somebody sent from the Stripe dashboard, and its events
// arrive like any other. They name no subscription, so they renew nothing — for any account.
func TestAnInvoiceWithNoSubscriptionRenewsNothing(t *testing.T) {
	b, mux, st, _ := enterpriseBot(t, includedCfg())
	ctx := context.Background()
	oneOff := func(evType, id string) string {
		return fmt.Sprintf(`{"id":%q,"type":%q,"livemode":false,"data":{"object":{
			"id":"in_manual","object":"invoice","customer":"cus_hand","currency":"usd",
			"lines":{"data":[{"period":{"start":1790000000,"end":1790000000}}]}}}}`, id, evType)
	}

	// An invoiced deal whose customer is known to us, from a top-up.
	org := testOrg(t, st, "Invoiced Corp")
	postWebhook(t, b, topUpEventFrom("evt_tu", org.PublicID, "pi_tu", "cus_hand", 10000), true)
	if code, _ := putDeal(t, mux, org, `{"plan":"enterprise","users":20}`); code != 200 {
		t.Fatal("could not write the deal")
	}
	for _, ev := range []string{oneOff("invoice.payment_failed", "evt_fail"), oneOff("invoice.paid", "evt_paid")} {
		if w := postWebhook(t, b, ev, true); w.Code != 200 {
			t.Fatalf("a one-off invoice event answered %d: %s", w.Code, w.Body)
		}
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Status != statusInvoiced || acct.SubscriptionID != "" || acct.PeriodEnd != "" {
		t.Fatalf("a one-off invoice rewrote the deal's record: %q, sub %q, period %q", acct.Status, acct.SubscriptionID, acct.PeriodEnd)
	}

	// And the bug it was: a free account that once topped up, paying a one-off invoice, was moved
	// to pro and recorded as subscribed.
	free := testOrg(t, st, "One Off Ltd")
	postWebhook(t, b, topUpEventFrom("evt_tu2", free.PublicID, "pi_tu2", "cus_once", 10000), true)
	paid := strings.Replace(oneOff("invoice.paid", "evt_paid2"), "cus_hand", "cus_once", 1)
	if w := postWebhook(t, b, paid, true); w.Code != 200 {
		t.Fatalf("answered %d", w.Code)
	}
	if o := reloadOrg(t, st, free.ID); o.Plan != PlanFree {
		t.Errorf("a one-off invoice moved a free account to %s", o.Plan)
	}
	if acct, _ := st.BillingAccountOf(ctx, free.ID); acct.Active() {
		t.Errorf("a one-off invoice left a free account reading %q", acct.Status)
	}
}

// Leaving takes the deal with it — and is refused while Stripe is still charging for it.
func TestLeavingEnterpriseTakesTheDealWithIt(t *testing.T) {
	b, mux, st, _ := enterpriseBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Leaver Corp")
	if code, _ := putDeal(t, mux, org, `{"plan":"enterprise","users":80,"included_usd":100,"budget_usd":900}`); code != 200 {
		t.Fatal("could not write the deal")
	}
	if code, out := putDeal(t, mux, org, `{"plan":"pro","budget_usd":50}`); code != 200 {
		t.Fatalf("moving to pro answered %d %v", code, out)
	}
	if terms, _ := st.EnterpriseTerms(ctx, org.ID); terms.Exists {
		t.Error("the deal outlived the plan")
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Active() || acct.Size != "" || acct.SpendableAllowanceMicros() != 0 {
		t.Errorf("after leaving: %q/%q with %s included", acct.Status, acct.Size, creditAmount(acct.SpendableAllowanceMicros()))
	}
	if set := b.settings.Get(ctx, org.ID); set.Plan != PlanPro || set.EffectiveBudget() != 50 {
		t.Errorf("after leaving the account is %q with budget %v", set.Plan, set.EffectiveBudget())
	}

	// Subscribed to the deal's price: leaving would leave Stripe charging it.
	sub := testOrg(t, st, "Still Paying Corp")
	if code, _ := putDeal(t, mux, sub, `{"plan":"enterprise","price_id":"price_acme"}`); code != 200 {
		t.Fatal("could not write the deal")
	}
	st.PutSubscription(ctx, sub.ID, SubscriptionState{CustomerID: "cus_sp", SubscriptionID: "sub_sp",
		Status: "active", Size: sizeEnterprise, Quantity: 1})
	if code, _ := putDeal(t, mux, sub, `{"plan":"free"}`); code != 400 {
		t.Errorf("leaving with a live enterprise subscription answered %d", code)
	}
	if code, _ := putDeal(t, mux, sub, `{"plan":"enterprise","price_id":"price_other"}`); code != 400 {
		t.Errorf("swapping the price under a live subscription answered %d", code)
	}
	if o := reloadOrg(t, st, sub.ID); o.Plan != PlanEnterprise {
		t.Errorf("a refused move left the account on %s", o.Plan)
	}

	// And the other way: a ladder subscription must end before a deal replaces it.
	ladder := testOrg(t, st, "Ladder Corp")
	st.PutSubscription(ctx, ladder.ID, SubscriptionState{CustomerID: "cus_l", SubscriptionID: "sub_l",
		Status: "active", Size: "upto_25", Quantity: 1})
	if code, _ := putDeal(t, mux, ladder, `{"plan":"enterprise","users":100}`); code != 400 {
		t.Errorf("a deal was written over a live ladder subscription: %d", code)
	}
}

// The regression the deal would have made worse: an allowance with no Stripe period behind it —
// comped, or invoiced — was given a fresh period on every sync, and a fresh period is a renewal.
func TestAnAllowanceWithNoStripePeriodIsNotRefilledHourly(t *testing.T) {
	b, mux, st, _ := enterpriseBot(t, includedCfg())
	ctx := context.Background()
	// Ten days into the month, with this much left. The period end is moved too: the old code
	// worked out "a month from now" on every call, and two calls inside one second agree with
	// each other — so a test that only syncs twice straight away could not tell it from the fix.
	spend := func(orgID int64, leftUSD float64) {
		if _, err := st.db.ExecContext(ctx, `update billing_accounts set allowance_micros=?, allowance_period_end=? where org_id=?`,
			usdToMicros(leftUSD), time.Now().UTC().AddDate(0, 0, 20).Format(time.DateTime), orgID); err != nil {
			t.Fatal(err)
		}
	}

	comped := testOrg(t, st, "Comped Ltd")
	if err := b.compSize(ctx, comped, "upto_25", "operator@example.com"); err != nil {
		t.Fatal(err)
	}
	invoiced := testOrg(t, st, "Invoiced Ltd")
	if code, _ := putDeal(t, mux, invoiced, `{"plan":"enterprise","included_usd":300}`); code != 200 {
		t.Fatal("could not write the deal")
	}
	spend(comped.ID, 3)
	spend(invoiced.ID, 40)
	b.syncAllowances(ctx) // the hourly catch-all
	b.syncAllowances(ctx)
	if got := allowanceOf(t, st, comped.ID); got != usdToMicros(3) {
		t.Errorf("the hourly sync refilled a comped allowance to %s; $3 was left", creditAmount(got))
	}
	if got := allowanceOf(t, st, invoiced.ID); got != usdToMicros(40) {
		t.Errorf("the hourly sync refilled an invoiced deal's allowance to %s; $40 was left", creditAmount(got))
	}

	// Once the month is over, the next one starts in full.
	st.db.ExecContext(ctx, `update billing_accounts set allowance_period_end=? where org_id=?`,
		time.Now().UTC().AddDate(0, 0, -1).Format(time.DateTime), invoiced.ID)
	b.syncAllowances(ctx)
	if got := allowanceOf(t, st, invoiced.ID); got != usdToMicros(300) {
		t.Errorf("a new month began with %s, want the deal's $300", creditAmount(got))
	}
}

// What the account's own Billing screen is told: the deal's figures, how it pays, and nothing
// the operator wrote for themselves.
func TestTheBillingScreenShowsTheDealAndNotTheNote(t *testing.T) {
	b, mux, st, _ := enterpriseBot(t, includedCfg())
	org := testOrg(t, st, "Screen Corp")
	if code, _ := putDeal(t, mux, org, `{"plan":"enterprise","users":250,"jobs":700,"included_usd":150,
		"fee_usd":1200,"pay_url":"https://buy.stripe.com/test_xyz","note":"call Dana before renewal"}`); code != 200 {
		t.Fatal("could not write the deal")
	}
	v := billingViewFor(t, b, org)
	if v.Plan != PlanEnterprise || v.Enterprise == nil {
		t.Fatalf("the billing screen sees plan %q and deal %v", v.Plan, v.Enterprise)
	}
	e := v.Enterprise
	if e.UserLimit != 250 || e.JobLimit != 700 || e.IncludedUSD != 150 || e.FeeUSD != 1200 ||
		e.PaidBy != "link" || e.PayURL != "https://buy.stripe.com/test_xyz" || e.Subscribed {
		t.Fatalf("the deal on the billing screen is %+v", e)
	}
	if v.Users.Limit != 250 || v.Jobs.Limit != 700 {
		t.Errorf("the tiles read %d users and %d jobs, want the deal's 250 and 700", v.Users.Limit, v.Jobs.Limit)
	}

	r := httptest.NewRequest("GET", "/api/billing", nil)
	r = r.WithContext(context.WithValue(r.Context(), userKey, &AdminUser{ID: 2, OrgID: org.ID,
		OrgPublic: org.PublicID, OrgName: org.Name, PublicID: "viewer", Permissions: map[Permission]bool{}}))
	w := httptest.NewRecorder()
	b.handleBilling(w, r)
	if strings.Contains(w.Body.String(), "call Dana") {
		t.Error("the operator's note reached the account's billing screen")
	}
	if strings.Contains(w.Body.String(), "buy.stripe.com") {
		t.Error("a member who cannot pay was handed the payment link")
	}
	// Nor its audit log, which the account's admins read.
	events, _ := st.AuditEvents(context.Background(), org.ID, AuditFilter{Limit: 50})
	for _, ev := range events {
		if strings.Contains(fmt.Sprint(ev.Details), "call Dana") {
			t.Errorf("the note reached the account's audit log in %q", ev.Action)
		}
	}
}

// The operator page's form, which always posts the whole deal.
func TestTheOperatorPageWritesADeal(t *testing.T) {
	b, mux, st, _ := enterpriseBot(t, includedCfg())
	org := testOrg(t, st, "Form Corp")
	form := "org=" + org.PublicID + "&users=120&jobs=300&included_usd=75&budget_usd=0&price_id=&pay_url=&fee_usd=900&interval=year&note=from+the+form"
	r := httptest.NewRequest("POST", "/operator/enterprise", strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Authorization", "Bearer "+enterpriseSecret)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "is on the enterprise plan") {
		t.Fatalf("the form answered %d: %s", w.Code, w.Body)
	}
	terms, _ := st.EnterpriseTerms(context.Background(), org.ID)
	if terms.UserLimit != 120 || terms.JobLimit != 300 || terms.IncludedMinor != 7500 ||
		terms.FeeMinor != 90000 || terms.FeeInterval != "year" || terms.Note != "from the form" {
		t.Fatalf("the form wrote %+v", terms)
	}
	// budget 0 on enterprise is "no ceiling", not "unlimited written into their own setting".
	if set := b.settings.Get(context.Background(), org.ID); set.PlatformBudgetUSD != 0 {
		t.Errorf("a deal with no budget has ceiling %v", set.PlatformBudgetUSD)
	}
	// The page shows the deal back, and no longer offers to comp a rung over it.
	g := httptest.NewRequest("GET", "/operator/plan?org="+org.PublicID, nil)
	g.Header.Set("Authorization", "Bearer "+enterpriseSecret)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, g)
	if !strings.Contains(w.Body.String(), "Save the deal") || strings.Contains(w.Body.String(), "Record a size, comped") {
		t.Errorf("the operator page for an enterprise account reads wrong:\n%s", w.Body)
	}
}

// setPlan alone must not produce an enterprise account with no deal, and a ladder rung must not be
// comped over a deal.
func TestEnterpriseComesOnlyWithItsDeal(t *testing.T) {
	b, mux, st, _ := enterpriseBot(t, includedCfg())
	org := testOrg(t, st, "Half Corp")
	if _, err := b.setPlan(context.Background(), org.PublicID, PlanEnterprise, 0, "op"); err == nil {
		t.Error("setPlan moved an account to enterprise with no deal")
	}
	if code, _ := putDeal(t, mux, org, `{"plan":"enterprise"}`); code != 200 {
		t.Fatal("an empty deal was refused; every figure in it is optional")
	}
	if err := b.compSize(context.Background(), reloadOrg(t, st, org.ID), "upto_25", "op"); err == nil {
		t.Error("a ladder size was comped over an enterprise deal")
	}
}

func TestEnterpriseBudgetIsTheDealsAndNothingElse(t *testing.T) {
	cfg := Config{PlatformMonthlyBudgetUSDPerOrg: 25, FreePlanBudgetUSD: 5}
	if got := budgetCeiling(PlanEnterprise, 2000, cfg, false); got != 2000 {
		t.Errorf("an enterprise grant of 2000 reads as %v", got)
	}
	// Bought credit does not lift a figure that is part of the deal, as it does a pro grant.
	if got := budgetCeiling(PlanEnterprise, 2000, cfg, true); got != 2000 {
		t.Errorf("credit lifted the deal's budget to %v", got)
	}
	// No figure is no ceiling — not the deployment's $25, which is for accounts nobody has a deal with.
	if got := budgetCeiling(PlanEnterprise, 0, cfg, false); got != 0 {
		t.Errorf("a deal with no budget is held to %v", got)
	}
	if planOf(PlanEnterprise) != PlanEnterprise || planOf("Enterprise ") != PlanFree {
		t.Error("planOf does not read the stored plan exactly")
	}
}
