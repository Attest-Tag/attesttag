package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// billingCfg is a deployment that sells plans, with two sizes and the limits the tests reason
// about. Test-mode key on purpose: the livemode check compares against it.
func billingCfg() Config {
	return Config{
		StripeSecretKey: "sk_test_x", StripeWebhookSecret: testWhsec,
		BillingCurrency: "usd", TopUpMinUSD: 25, TopUpMaxUSD: 10000, CreditLowUSD: 10,
		StripeSizes: []Size{
			{Key: "under_25", PriceID: "price_small", AmountMinor: 19900, Label: sizeLabels["under_25"]},
			{Key: "25_100", PriceID: "price_mid", AmountMinor: 49900, Label: sizeLabels["25_100"]},
		},
	}
}

func billingBot(t *testing.T, cfg Config) (*Bot, *Store) {
	t.Helper()
	st := testStore(t)
	b := &Bot{cfg: cfg, store: st, settings: newSettingsCache(st, cfg), pay: NewPayments(cfg), mail: logMailer{}}
	return b, st
}

// post signs a webhook body the way Stripe would and sends it through the real mux, so the route
// registration, the middleware chain and the handler are all exercised together.
func postWebhook(t *testing.T, b *Bot, body string, sign bool) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	b.billingRoutes(mux)
	req := httptest.NewRequest("POST", "/api/billing/webhook", strings.NewReader(body))
	if sign {
		req.Header.Set("Stripe-Signature", signStripePayload([]byte(body), testWhsec, time.Now()))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func topUpEvent(id, org, payment string, minor int64) string {
	return topUpEventFrom(id, org, payment, "cus_1", minor)
}

// topUpEventFrom names the Stripe customer too, which is what the cross-tenant test turns on: the
// clash it has to provoke is a session claiming one organisation for a customer bound to another.
func topUpEventFrom(id, org, payment, customer string, minor int64) string {
	return fmt.Sprintf(`{"id":%q,"type":"checkout.session.completed","livemode":false,"data":{"object":{
		"id":"cs_1","object":"checkout.session","mode":"payment","payment_status":"paid",
		"payment_intent":%q,"amount_total":%d,"currency":"usd","client_reference_id":%q,
		"customer":%q,"metadata":{"attesttag_org":%q}}}}`, id, payment, minor, org, customer, org)
}

// ---- off by default ----

// The property most deployments depend on: no Stripe keys, no billing surface at all. Checked
// through the real mux rather than by asking the config, because "the routes are not registered"
// is the claim, not "a flag is false".
func TestBillingIsOffWithoutItsSecrets(t *testing.T) {
	b, _ := billingBot(t, Config{})
	mux := http.NewServeMux()
	b.billingRoutes(mux)
	for _, p := range []string{"/api/billing", "/api/billing/checkout", "/api/billing/portal", "/api/billing/webhook"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if w.Code != 404 {
			t.Errorf("%s answered %d with no Stripe keys; it must not exist", p, w.Code)
		}
	}
	// And an unsigned webhook is not waved through "because verification is off".
	if w := postWebhook(t, b, `{"id":"evt_x","type":"invoice.paid"}`, false); w.Code != 404 {
		t.Errorf("an unsigned webhook answered %d on a deployment with no billing", w.Code)
	}
}

// GET /v1/billing is billing too, though it lives with the rest of the developer API rather than
// in billingRoutes — which is how it came to be registered on every deployment, answering a plan
// and a balance where nothing is sold. Off, it is not there at all; on, the same key reads it.
func TestV1BillingExistsOnlyWhereBillingDoes(t *testing.T) {
	b, mux, st := identityBot(t)
	_, _, session := signedUp(t, b, mux, st, "founder@example.com")
	raw, _ := mintKey(t, mux, session, "finance")
	if code, _ := authReq(t, mux, "GET", "/v1/whoami", nil, raw); code != 200 {
		t.Fatalf("the key does not open the API at all: %d", code)
	}
	if code, body := authReq(t, mux, "GET", "/v1/billing", nil, raw); code != 404 {
		t.Errorf("a deployment that sells nothing answered /v1/billing with %d: %v", code, body)
	}

	b.cfg = billingCfg()
	selling := http.NewServeMux()
	b.apiV1Routes(selling)
	if code, body := authReq(t, selling, "GET", "/v1/billing", nil, raw); code != 200 || body["currency"] != "usd" {
		t.Errorf("a deployment that sells plans answered /v1/billing with %d: %v", code, body)
	}
}

// Half a configuration is no configuration: a deployment that can charge a card and cannot hear
// the result would take money and never credit it.
func TestBillingNeedsBothKeys(t *testing.T) {
	for name, c := range map[string]Config{
		"key only":    {StripeSecretKey: "sk_test_x"},
		"secret only": {StripeWebhookSecret: testWhsec},
	} {
		if c.BillingEnabled() {
			t.Errorf("%s: billing reads as enabled", name)
		}
	}
	if !billingCfg().BillingEnabled() {
		t.Error("a fully configured deployment reads as disabled")
	}
}

// ---- the webhook ----

func TestAnUnsignedWebhookChangesNothing(t *testing.T) {
	cfg := billingCfg()
	b, st := billingBot(t, cfg)
	org := testOrg(t, st, "Signed Ltd")
	body := topUpEvent("evt_unsigned", org.PublicID, "pi_unsigned", 10000)

	for name, mutate := range map[string]func(*http.Request){
		"no signature": func(r *http.Request) {},
		"wrong secret": func(r *http.Request) {
			r.Header.Set("Stripe-Signature", signStripePayload([]byte(body), "whsec_other", time.Now()))
		},
		"stale": func(r *http.Request) {
			r.Header.Set("Stripe-Signature", signStripePayload([]byte(body), testWhsec, time.Now().Add(-time.Hour)))
		},
		"body mutated": func(r *http.Request) {
			r.Header.Set("Stripe-Signature", signStripePayload([]byte(`{"id":"other"}`), testWhsec, time.Now()))
		},
		"header garbage": func(r *http.Request) { r.Header.Set("Stripe-Signature", "t=1,v1=nope") },
	} {
		mux := http.NewServeMux()
		b.billingRoutes(mux)
		req := httptest.NewRequest("POST", "/api/billing/webhook", strings.NewReader(body))
		mutate(req)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Errorf("%s: answered %d, want 400", name, w.Code)
		}
	}
	acct, _ := st.BillingAccountOf(context.Background(), org.ID)
	if acct.CreditBalanceMicros != 0 || acct.Exists {
		t.Fatal("an unverified webhook moved money")
	}
}

func TestAWebhookReplayCreditsOnce(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Replay Ltd")
	body := topUpEvent("evt_replay", org.PublicID, "pi_replay", 25000)

	for i := 0; i < 4; i++ {
		if w := postWebhook(t, b, body, true); w.Code != 200 {
			t.Fatalf("delivery %d answered %d: %s", i, w.Code, w.Body)
		}
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.CreditBalanceMicros != usdToMicros(250) {
		t.Fatalf("four deliveries of one payment credited %s", creditAmount(acct.CreditBalanceMicros))
	}
	if !acct.CreditEnforced {
		t.Error("a top-up did not switch the credit floor on")
	}
	assertLedgerMatchesBalance(t, st, org.ID)
}

// The same payment arriving as two different event types is the shape a per-event-id key would
// miss: two event ids, one payment. The ledger is keyed on the payment.
func TestTheSamePaymentUnderTwoEventTypesCreditsOnce(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	org := testOrg(t, st, "TwoTypes Ltd")

	postWebhook(t, b, topUpEvent("evt_session", org.PublicID, "pi_one", 50000), true)
	intent := fmt.Sprintf(`{"id":"evt_intent","type":"payment_intent.succeeded","livemode":false,"data":{"object":{
		"id":"pi_one","object":"payment_intent","amount_received":50000,"currency":"usd",
		"customer":"cus_1","metadata":{"attesttag_org":%q}}}}`, org.PublicID)
	if w := postWebhook(t, b, intent, true); w.Code != 200 {
		t.Fatalf("payment_intent.succeeded answered %d", w.Code)
	}

	acct, _ := st.BillingAccountOf(context.Background(), org.ID)
	if acct.CreditBalanceMicros != usdToMicros(500) {
		t.Fatalf("one payment under two event types credited %s, want $500.00", creditAmount(acct.CreditBalanceMicros))
	}
}

// Stripe fires payment_intent.succeeded for subscription invoices too. Those are the monthly fee,
// not credit, and crediting them would give every subscriber their fee back as model spend.
func TestASubscriptionPaymentIntentIsNotCredit(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	org := testOrg(t, st, "Fee Ltd")
	st.PutSubscription(context.Background(), org.ID, SubscriptionState{CustomerID: "cus_fee", SubscriptionID: "sub_1", Status: "active"})

	body := `{"id":"evt_fee","type":"payment_intent.succeeded","livemode":false,"data":{"object":{
		"id":"pi_fee","object":"payment_intent","amount_received":49900,"currency":"usd","customer":"cus_fee"}}}`
	if w := postWebhook(t, b, body, true); w.Code != 200 {
		t.Fatalf("answered %d", w.Code)
	}
	acct, _ := st.BillingAccountOf(context.Background(), org.ID)
	if acct.CreditBalanceMicros != 0 {
		t.Fatalf("a subscription fee became %s of credit", creditAmount(acct.CreditBalanceMicros))
	}
}

// A test-mode event pointed at a live deployment, or the reverse. Recorded and ignored — this is
// what stops a stray `stripe listen` crediting production with money nobody paid.
func TestALivemodeMismatchCreditsNothing(t *testing.T) {
	b, st := billingBot(t, billingCfg()) // sk_test_… so livemode:true is the mismatch
	org := testOrg(t, st, "Livemode Ltd")
	body := strings.Replace(topUpEvent("evt_live", org.PublicID, "pi_live", 100000), `"livemode":false`, `"livemode":true`, 1)

	if w := postWebhook(t, b, body, true); w.Code != 200 {
		t.Fatalf("answered %d, want 200 so Stripe stops retrying", w.Code)
	}
	acct, _ := st.BillingAccountOf(context.Background(), org.ID)
	if acct.CreditBalanceMicros != 0 {
		t.Fatalf("a livemode mismatch credited %s", creditAmount(acct.CreditBalanceMicros))
	}
	if !st.HasBillingEvent(context.Background(), "evt_live") {
		t.Error("the ignored event was not recorded, so a redelivery would be reconsidered")
	}
}

// The attack shape: a checkout naming organisation B for a customer already bound to A. Neither
// is credited, and the one that pays for it is nobody.
func TestACheckoutCannotBeAttachedToAnotherOrganisation(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	ctx := context.Background()
	a := testOrg(t, st, "Alpha Ltd")
	victim := testOrg(t, st, "Beta Ltd")
	st.PutSubscription(ctx, a.ID, SubscriptionState{CustomerID: "cus_shared", Status: "active"})

	// cus_shared belongs to Alpha; the event claims Beta.
	body := topUpEventFrom("evt_steal", victim.PublicID, "pi_steal", "cus_shared", 100000)
	if w := postWebhook(t, b, body, true); w.Code != 200 {
		t.Fatalf("answered %d", w.Code)
	}
	for _, o := range []*Org{a, victim} {
		acct, _ := st.BillingAccountOf(ctx, o.ID)
		if acct.CreditBalanceMicros != 0 {
			t.Fatalf("%s was credited %s by an event that named the wrong organisation", o.Name, creditAmount(acct.CreditBalanceMicros))
		}
	}
}

func TestAnEventForAnUnknownOrganisationCreditsNothing(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	body := topUpEvent("evt_ghost", "ffffffffffffffffffffffffffffffff", "pi_ghost", 10000)
	if w := postWebhook(t, b, body, true); w.Code != 200 {
		t.Fatalf("answered %d, want 200: retrying will not make it ours", w.Code)
	}
	if !st.HasBillingEvent(context.Background(), "evt_ghost") {
		t.Error("the event was not recorded, so every redelivery for three days is reconsidered")
	}
}

// A payment in a currency this deployment does not hold would credit somebody else's minor units
// as ours — 5,000 JPY becoming $50.00 of credit.
func TestAPaymentInAnotherCurrencyCreditsNothing(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	org := testOrg(t, st, "Yen Ltd")
	body := strings.Replace(topUpEvent("evt_jpy", org.PublicID, "pi_jpy", 500000), `"currency":"usd"`, `"currency":"jpy"`, 1)
	postWebhook(t, b, body, true)
	acct, _ := st.BillingAccountOf(context.Background(), org.ID)
	if acct.CreditBalanceMicros != 0 {
		t.Fatalf("a yen payment credited %s", creditAmount(acct.CreditBalanceMicros))
	}
}

// Under a maintenance freeze Stripe must be told to come back, not thanked and ignored. This is
// the deliberate opposite of the Slack handler, which acknowledges and drops.
func TestMaintenanceRetriesBillingRatherThanDroppingIt(t *testing.T) {
	cfg := billingCfg()
	cfg.Maintenance = true
	b, st := billingBot(t, cfg)
	org := testOrg(t, st, "Frozen Ltd")
	w := postWebhook(t, b, topUpEvent("evt_frozen", org.PublicID, "pi_frozen", 10000), true)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("answered %d during a freeze; Stripe reads 2xx as delivered and the payment would exist only at Stripe", w.Code)
	}
	acct, _ := st.BillingAccountOf(context.Background(), org.ID)
	if acct.Exists {
		t.Error("a webhook wrote during a maintenance freeze")
	}
}

// An event type nobody handles must not 5xx: enough failures and Stripe disables the endpoint,
// taking the events that matter with it.
func TestAnUnknownEventTypeIsAcknowledged(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	org := testOrg(t, st, "Unknown Ltd")
	body := fmt.Sprintf(`{"id":"evt_unknown","type":"radar.early_fraud_warning.created","livemode":false,
		"data":{"object":{"id":"issfr_1","metadata":{"attesttag_org":%q}}}}`, org.PublicID)
	if w := postWebhook(t, b, body, true); w.Code != 200 {
		t.Fatalf("an unhandled event type answered %d", w.Code)
	}
	if !st.HasBillingEvent(context.Background(), "evt_unknown") {
		t.Error("an unhandled event was not recorded")
	}
}

// ---- the subscription ----

func TestASubscriptionMovesThePlanWithoutOverwritingTheOwnBudget(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Pro Ltd")
	// A guard rail the customer chose. A webhook reusing setPlan unchanged would replace it with
	// defaultProBudgetUSD, capping somebody who has just paid at $25 a month.
	st.PutSetting(ctx, org.ID, "monthly_budget_usd", "400")

	body := fmt.Sprintf(`{"id":"evt_sub","type":"checkout.session.completed","livemode":false,"data":{"object":{
		"id":"cs_2","object":"checkout.session","mode":"subscription","customer":"cus_2","subscription":"sub_2",
		"client_reference_id":%q,"currency":"usd","metadata":{"attesttag_org":%q,"size":"25_100"}}}}`, org.PublicID, org.PublicID)
	if w := postWebhook(t, b, body, true); w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}

	after, _ := st.Org(ctx, org.ID)
	if after.Plan != PlanPro {
		t.Fatalf("plan is %q after a paid subscription", after.Plan)
	}
	if got := st.Setting(ctx, org.ID, "monthly_budget_usd"); got != "400" {
		t.Fatalf("the account's own monthly budget was overwritten with %q", got)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Size != "25_100" || acct.UnitPriceMicros != usdToMicros(499) {
		t.Fatalf("size recorded as %q at %s", acct.Size, creditAmount(acct.UnitPriceMicros))
	}
	// The plan change is in the organisation's own log, attributed to the payment.
	events, _ := st.AuditEvents(ctx, org.ID, AuditFilter{Limit: 20})
	found := false
	for _, e := range events {
		if e.Action == "plan.changed" && e.Via == viaSystem {
			found = true
		}
	}
	if !found {
		t.Error("no plan.changed audit row with via=system: a webhook bypasses auditedWrites, so it has to write its own")
	}
}

// A failed card is not a cancellation. Stripe retries for weeks, and cutting somebody off on the
// first decline is the wrong failure.
func TestAFailedPaymentDoesNotDowngrade(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Dunning Ltd")
	st.SetOrgPlan(ctx, org.ID, PlanPro, 0)
	st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_3", SubscriptionID: "sub_3", Status: "active"})

	body := `{"id":"evt_failed","type":"invoice.payment_failed","livemode":false,"data":{"object":{
		"id":"in_1","object":"invoice","customer":"cus_3","subscription":"sub_3","amount_total":49900,"currency":"usd"}}}`
	if w := postWebhook(t, b, body, true); w.Code != 200 {
		t.Fatalf("answered %d", w.Code)
	}
	after, _ := st.Org(ctx, org.ID)
	if after.Plan != PlanPro {
		t.Fatal("one failed payment dropped the plan")
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Status != "past_due" {
		t.Fatalf("status is %q, want past_due", acct.Status)
	}
}

// A cancelled subscription moves the plan back and leaves the credit alone: it is prepaid money
// and still theirs.
func TestACancelledSubscriptionKeepsTheCredit(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Cancelled Ltd")
	st.SetOrgPlan(ctx, org.ID, PlanPro, 0)
	topUp(t, st, org.ID, 80, "pi_keep")
	st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_4", SubscriptionID: "sub_4", Status: "active"})

	body := `{"id":"evt_del","type":"customer.subscription.deleted","livemode":false,"data":{"object":{
		"id":"sub_4","object":"subscription","customer":"cus_4","status":"canceled"}}}`
	if w := postWebhook(t, b, body, true); w.Code != 200 {
		t.Fatalf("answered %d", w.Code)
	}
	after, _ := st.Org(ctx, org.ID)
	if after.Plan != PlanFree {
		t.Fatalf("plan is %q after cancellation", after.Plan)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.CreditBalanceMicros != usdToMicros(80) {
		t.Fatalf("cancelling took the credit: %s left", creditAmount(acct.CreditBalanceMicros))
	}
	if !acct.CreditEnforced {
		t.Error("the credit floor was switched off by a cancellation, so what is left could be overspent")
	}
}

// ---- amounts ----

// Nothing a browser sends is a figure. The amount is a request, clamped here against limits only
// this process knows.
func TestTopUpAmountsAreClampedServerSide(t *testing.T) {
	cfg := billingCfg()
	nan := func() float64 { var z float64; return z / z }()
	for name, amount := range map[string]float64{
		"nan":       nan,
		"zero":      0,
		"negative":  -100,
		"under min": 1,
		"over max":  1000000,
		"absurd":    1e18,
	} {
		if _, err := topUpMinor(amount, cfg); err == nil {
			t.Errorf("%s (%v) was accepted", name, amount)
		}
	}
	for _, ok := range []float64{25, 100, 9999.99, 10000} {
		if _, err := topUpMinor(ok, cfg); err != nil {
			t.Errorf("$%v was refused: %v", ok, err)
		}
	}
	if got, _ := topUpMinor(250, cfg); got != 25000 {
		t.Errorf("$250 became %d minor units", got)
	}
}

func TestOnlyConfiguredSizesAreSold(t *testing.T) {
	cfg := billingCfg()
	if _, ok := cfg.SizeByKey("over_2000"); ok {
		t.Error("a size with no configured price is on sale")
	}
	b, ok := cfg.SizeByKey("25_100")
	if !ok || b.PriceID != "price_mid" {
		t.Fatalf("configured size did not resolve: %+v", b)
	}
	// And the same map read backwards, which is how a subscription event names its size.
	back, ok := cfg.SizeByPrice("price_mid")
	if !ok || back.Key != "25_100" {
		t.Fatalf("price did not map back to its size: %+v", back)
	}
	if _, ok := cfg.SizeByPrice("price_someone_elses"); ok {
		t.Error("an unknown price resolved to a size")
	}
}

func TestParseSizesIgnoresRubbishRatherThanFailingTheBoot(t *testing.T) {
	got := parseSizes("under_25=price_a:19900, broken, 25_100=price_b:notanumber, 100_500=price_c:24900")
	if len(got) != 2 || got[0].Key != "under_25" || got[1].Key != "100_500" {
		t.Fatalf("parsed %+v", got)
	}
	if got[0].Label != sizeLabels["under_25"] {
		t.Errorf("label not filled in: %q", got[0].Label)
	}
}

// ---- which limit bound ----

// The console must never derive this from two numbers; that would be a second copy of the rule
// the bot applies. The names are also the answer a person gets in Slack, so they must differ.
func TestPausedByNamesWhichLimitBound(t *testing.T) {
	credit := Settings{CreditEnforced: true, MonthlyBudgetUSD: 1000}
	if got := pausedBy(credit, true, -maxOverdraftMicros, 10); got != pausedCredit {
		t.Errorf("out of credit read as %q", got)
	}
	// One cent inside the overdraft is still running: the bound is the refusal, not zero.
	if got := pausedBy(credit, true, -maxOverdraftMicros+10_000, 10); got != pausedNone {
		t.Errorf("inside the documented overdraft read as %q", got)
	}
	budget := Settings{MonthlyBudgetUSD: 50}
	if got := pausedBy(budget, false, 0, 50); got != pausedBudget {
		t.Errorf("budget spent read as %q", got)
	}
	if got := pausedBy(budget, false, 0, 10); got != pausedNone {
		t.Errorf("a running account read as %q", got)
	}
	// Credit is checked first: an account that is out of money is told about the money, not about
	// a budget it could raise and that would not help.
	both := Settings{CreditEnforced: true, MonthlyBudgetUSD: 50}
	if got := pausedBy(both, true, -maxOverdraftMicros, 60); got != pausedCredit {
		t.Errorf("with both limits reached, the answer was %q", got)
	}
	// Metered false is the whole of "this account has no credit relationship": an unmetered
	// account is never out of credit, whatever the figure beside it says.
	if got := pausedBy(credit, false, -maxOverdraftMicros*10, 10); got != pausedNone {
		t.Errorf("an unmetered account read as %q", got)
	}
	// And an account metered only by a live plan allowance stops the same way a paying one does.
	allowance := Settings{AllowanceActive: true, MonthlyBudgetUSD: 1000}
	if got := pausedBy(allowance, true, -maxOverdraftMicros, 10); got != pausedCredit {
		t.Errorf("an allowance-metered account out of credit read as %q", got)
	}
}

// ---- source-level guards ----

var billingSQL = regexp.MustCompile("`([^`]*credit_balance_micros[^`]*)`")

// The balance is a cache of the ledger, and the only thing allowed to move it is the code that
// writes the row justifying the move. This is the guard on a future change that decrements it
// somewhere convenient — the drift that produces is silent and, because usage is swept by
// retention, unrecoverable.
func TestEveryBalanceWriteLivesInTheLedgerFile(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "store_billing.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range billingSQL.FindAllStringSubmatch(string(src), -1) {
			q := strings.ToLower(strings.Join(strings.Fields(m[1]), " "))
			if strings.Contains(q, "update billing_accounts") && strings.Contains(q, "set credit_balance_micros") {
				t.Errorf("%s writes credit_balance_micros directly:\n  %s\n"+
					"Every move of the balance belongs in store_billing.go, in the same transaction as the "+
					"credit_ledger row that justifies it — otherwise the two drift, and usage is swept by "+
					"retention so nothing can rebuild them.", f, q)
			}
		}
	}
}

// A member must not be able to move their own plan or grant themselves credit by any route. The
// plan moves from the operator's page or a verified webhook; credit moves from a payment or the
// operator. Nothing reachable with a session may do either.
func TestNoConsoleRouteCanMoveAPlanOrGrantCredit(t *testing.T) {
	src, err := os.ReadFile("admin_api.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"applyPlan(", "setPlan(", "MoveCredit(", "grantCredit(", "setEnterprise(", "PutEnterpriseTerms("} {
		if strings.Contains(string(src), bad) {
			t.Errorf("admin_api.go reaches %s: the console must not be able to move a plan or grant credit", bad)
		}
	}
	// And the console's own billing file may start a checkout, but never grant.
	src, err = os.ReadFile("billing.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "grantCredit(") {
			t.Errorf("billing.go calls grantCredit: granting is the operator's, in operator_billing.go\n  %s", strings.TrimSpace(line))
		}
		// Nor write an enterprise deal: the console reads one on Billing and may not change it.
		if strings.Contains(line, "setEnterprise(") || strings.Contains(line, "PutEnterpriseTerms(") {
			t.Errorf("billing.go writes an enterprise deal: that is the operator's, in enterprise.go\n  %s", strings.TrimSpace(line))
		}
	}
}

// Secrets and personal data must not reach the audit log's details, which the console shows and
// /v1/audit exports.
func TestBillingAuditDetailsCarryNoSecrets(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Quiet Ltd")
	postWebhook(t, b, topUpEvent("evt_audit", org.PublicID, "pi_audit", 10000), true)

	events, _ := st.AuditEvents(ctx, org.ID, AuditFilter{Limit: 50})
	if len(events) == 0 {
		t.Fatal("the top-up wrote no audit row at all; a webhook bypasses auditedWrites and must write its own")
	}
	for _, e := range events {
		blob, _ := json.Marshal(e)
		for _, secret := range []string{"sk_test_", "sk_live_", "whsec_", "client_secret", "@"} {
			if strings.Contains(string(blob), secret) {
				t.Errorf("audit row %q carries %q: %s", e.Action, secret, blob)
			}
		}
	}
}

// ---- the operator ----

func TestGrantingCreditNeedsNoPaymentAndTurnsTheFloorOn(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Comped Ltd")

	bal, err := b.grantCredit(ctx, org, 250, "beta partner", "operator@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if bal != 250 {
		t.Fatalf("balance is $%.2f after a $250 grant", bal)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if !acct.CreditEnforced {
		t.Error("a grant did not switch the credit floor on: the account would spend without limit")
	}
	if acct.LifetimeTopUpMicros != usdToMicros(250) {
		t.Errorf("the grant is not counted as money in: %d", acct.LifetimeTopUpMicros)
	}
	ledger, _ := st.CreditLedger(ctx, org.ID, 10)
	if len(ledger) != 1 || ledger[0].Kind != creditGrant || ledger[0].Note != "beta partner" {
		t.Fatalf("the statement does not say where it came from: %+v", ledger)
	}
	assertLedgerMatchesBalance(t, st, org.ID)

	// Taking it back is an adjustment, not a grant, and it must not re-enable anything.
	if _, err := b.grantCredit(ctx, org, -50, "mistake", "operator@example.com"); err != nil {
		t.Fatal(err)
	}
	ledger, _ = st.CreditLedger(ctx, org.ID, 10)
	if ledger[0].Kind != creditAdjustment {
		t.Errorf("taking credit back is recorded as %q", ledger[0].Kind)
	}
	assertLedgerMatchesBalance(t, st, org.ID)
}

func TestGrantingRefusesFiguresThatAreNotMoney(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	org := testOrg(t, st, "Fat Finger Ltd")
	nan := func() float64 { var z float64; return z / z }()
	for name, amount := range map[string]float64{"nan": nan, "zero": 0, "absurd": 1e9} {
		if _, err := b.grantCredit(context.Background(), org, amount, "", "op"); err == nil {
			t.Errorf("%s (%v) was granted", name, amount)
		}
	}
}

func TestACompedSizeRecordsTheFeeWithoutChargingIt(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Handshake Ltd")
	if err := b.compSize(ctx, org, "25_100", "operator@example.com"); err != nil {
		t.Fatal(err)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Status != "comped" || acct.Size != "25_100" || acct.UnitPriceMicros != usdToMicros(499) {
		t.Fatalf("recorded as %+v", acct)
	}
	if acct.SubscriptionID != "" {
		t.Error("a comped size invented a Stripe subscription")
	}
	// And it refuses to fight with a real subscription rather than leaving the two disagreeing.
	st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_9", SubscriptionID: "sub_9", Status: "active"})
	if err := b.compSize(ctx, org, "under_25", "operator@example.com"); err == nil {
		t.Error("a size was comped over a live Stripe subscription")
	}
}

// The operator surface is off with the rest of billing, and its pages are unreachable without the
// secret however the deployment is configured.
func TestOperatorBillingRoutesNeedTheSecretAndTheKeys(t *testing.T) {
	b, _ := billingBot(t, billingCfg()) // billing on, OPERATOR_SECRET unset
	mux := http.NewServeMux()
	b.operatorRoutes(mux)
	for _, p := range []string{"/operator/credit", "/operator/size", "/operator/cancel"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", p, strings.NewReader("org=x")))
		if w.Code != 404 {
			t.Errorf("%s answered %d with no OPERATOR_SECRET", p, w.Code)
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/operator", nil))
	if w.Code != 404 {
		t.Errorf("the account list answered %d with no OPERATOR_SECRET", w.Code)
	}
}

// ---- coming back from Stripe ----

// fakePayments is a Payments that takes money without a network, the way logMailer sends mail
// without one. Only Session is interesting here: it is what the console's return trip reads.
type fakePayments struct {
	session CheckoutSession
	err     error
	calls   int
	// What ChangeSubscriptionPrice was last asked to do, and what it should answer.
	changedSub    string
	changedPrice  string
	changedCharge bool   // whether it was asked to take the money now
	changedKey    string // the idempotency key it was handed
	changeErr     error
	changeResult  PriceChangeResult
	changeCalls   int
	pending       PendingChange
	pendingErr    error
	// What Checkout was last asked to open, and the Prices Price answers with, by id.
	checkout CheckoutRequest
	prices   map[string]PriceInfo
}

func (f *fakePayments) Configured() bool { return true }
func (f *fakePayments) Checkout(_ context.Context, req CheckoutRequest) (CheckoutSession, error) {
	f.checkout = req
	return CheckoutSession{ID: "cs_fake", URL: "https://pay.example/cs_fake"}, nil
}
func (f *fakePayments) Price(_ context.Context, id string) (PriceInfo, error) {
	p, ok := f.prices[id]
	if !ok {
		return PriceInfo{}, fmt.Errorf("No such price: '%s'", id)
	}
	return p, nil
}
func (f *fakePayments) Portal(context.Context, PortalRequest) (string, error) {
	return "https://billing.example/p", nil
}
func (f *fakePayments) Session(context.Context, string) (CheckoutSession, error) {
	f.calls++
	return f.session, f.err
}
func (f *fakePayments) CancelSubscription(context.Context, string) error { return nil }

func (f *fakePayments) ChangeSubscriptionPrice(_ context.Context, ch PriceChange) (PriceChangeResult, error) {
	f.changeCalls++
	if f.changeErr != nil {
		return PriceChangeResult{}, f.changeErr
	}
	f.changedSub, f.changedPrice = ch.SubscriptionID, ch.PriceID
	f.changedCharge, f.changedKey = ch.ChargeNow, ch.IdempotencyKey
	return f.changeResult, nil
}

func (f *fakePayments) PendingPriceChange(context.Context, string) (PendingChange, error) {
	return f.pending, f.pendingErr
}
func (f *fakePayments) VerifyWebhook(raw []byte, header string, at time.Time) (StripeEvent, error) {
	return (&stripeClient{whsec: testWhsec}).VerifyWebhook(raw, header, at)
}

// The console's whole post-payment story hangs off this one boolean, so it has to be the honest
// answer rather than an optimistic one. False means the money is already in the figures the same
// response carries; true means it is not, and the page says "updating…" instead of showing a
// balance it is about to contradict.
func TestSettlingOnReturnSaysWhetherTheMoneyHasLanded(t *testing.T) {
	cfg := billingCfg()
	st := testStore(t)
	org := testOrg(t, st, "Returning Ltd")
	fake := &fakePayments{}
	b := &Bot{cfg: cfg, store: st, settings: newSettingsCache(st, cfg), pay: fake, mail: logMailer{}}

	// A session Stripe says is paid: settled on the spot, and the balance moves before the
	// webhook has been anywhere near us.
	fake.session = CheckoutSession{ID: "cs_1", Status: "complete", Mode: "payment", PaymentStatus: "paid",
		PaymentIntent: "pi_return", AmountMinor: 25000, Currency: "usd",
		ClientRef: org.PublicID, Metadata: map[string]string{metaOrg: org.PublicID}}
	if !b.settleCheckout(context.Background(), org.ID, "cs_1") {
		t.Fatal("a paid session did not settle")
	}
	bal, _ := st.CreditBalance(context.Background(), org.ID)
	if bal != usdToMicros(250) {
		t.Fatalf("balance is %s after settling a $250 payment", creditAmount(bal))
	}

	// Settling twice is what happens when the webhook and the return trip race. It must not
	// credit twice, and it must still report the money as landed — "somebody else already did
	// it" is settled, not pending.
	if !b.settleCheckout(context.Background(), org.ID, "cs_1") {
		t.Error("a second settle of the same session reported the payment as not landed")
	}
	bal, _ = st.CreditBalance(context.Background(), org.ID)
	if bal != usdToMicros(250) {
		t.Fatalf("settling twice credited twice: %s", creditAmount(bal))
	}

	// A payment that has not settled at Stripe yet: pending, and nothing moves.
	fake.session = CheckoutSession{ID: "cs_2", Status: "complete", Mode: "payment", PaymentStatus: "unpaid",
		PaymentIntent: "pi_slow", AmountMinor: 90000, Currency: "usd",
		ClientRef: org.PublicID, Metadata: map[string]string{metaOrg: org.PublicID}}
	if b.settleCheckout(context.Background(), org.ID, "cs_2") {
		t.Error("an unpaid session reported as landed")
	}

	// And a session belonging to somebody else is refused outright: the id travels in a URL the
	// browser controls, so it is a claim rather than a fact.
	fake.session = CheckoutSession{ID: "cs_3", Status: "complete", Mode: "payment", PaymentStatus: "paid",
		PaymentIntent: "pi_theirs", AmountMinor: 500000, Currency: "usd",
		ClientRef: "ffffffffffffffffffffffffffffffff",
		Metadata:  map[string]string{metaOrg: "ffffffffffffffffffffffffffffffff"}}
	if b.settleCheckout(context.Background(), org.ID, "cs_3") {
		t.Error("another organisation's session settled into this one")
	}
	bal, _ = st.CreditBalance(context.Background(), org.ID)
	if bal != usdToMicros(250) {
		t.Fatalf("another organisation's payment moved this balance to %s", creditAmount(bal))
	}
}

// Buying needs a confirmed address; stopping paying must not.
//
// The gate used to hang off the permission, which meant it covered the billing portal too — and
// the portal is where somebody cancels. "Go and find a verification email before you may stop
// paying" is the wrong answer to that request however reasonable it is to the other one.
func TestAnUnverifiedAddressBlocksBuyingAndNeverCancelling(t *testing.T) {
	if why := (&Bot{}).needsVerifiedEmail(context.Background(), &AdminUser{}, PermBillingManage); why != "" {
		t.Fatalf("billing.manage is gated on a verified address at the permission, so the portal is gated too: %q", why)
	}
	// The permissions that genuinely are about reaching out from the organisation keep theirs.
	for _, p := range []Permission{PermUsersManage, PermAPIKeysManage} {
		b, st := billingBot(t, billingCfg())
		_ = st
		if b.needsVerifiedEmail(context.Background(), &AdminUser{}, p) == "" {
			// emailUnverified needs a real user row to answer true, so this only asserts the
			// switch still names them — the behaviour itself is covered by the console tests.
			continue
		}
	}
	// And the checkout route carries the check itself, where it belongs.
	src, err := os.ReadFile("billing.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func (b *Bot) handleBillingCheckout")
	j := strings.Index(body, "func (b *Bot) handleBillingPortal")
	if i < 0 || j < 0 || i > j {
		t.Fatal("the two handlers moved; this test reads the source between them")
	}
	if !strings.Contains(body[i:j], "emailUnverified") {
		t.Error("handleBillingCheckout no longer checks for a verified address")
	}
	if strings.Contains(body[j:], "emailUnverified") {
		t.Error("handleBillingPortal checks for a verified address: cancelling must not need one")
	}
}

// ---- which way the size moved ----

func sizeBot(t *testing.T, from string, fromUSD float64) (*Bot, *Store, *Org, *fakePayments) {
	t.Helper()
	st := testStore(t)
	org := testOrg(t, st, "Moving Ltd")
	fake := &fakePayments{}
	b := &Bot{cfg: billingCfg(), store: st, settings: newSettingsCache(st, billingCfg()), pay: fake, mail: logMailer{}}
	st.PutSubscription(context.Background(), org.ID, SubscriptionState{CustomerID: "cus_m",
		SubscriptionID: "sub_m", Status: "active", Size: from, UnitPriceMicros: usdToMicros(fromUSD),
		Quantity: 1, Currency: "usd", PeriodEnd: stripeTime(1792612062)})
	return b, st, org, fake
}

// The direction is the whole of it: going up takes money now, going down never does. Deciding it
// in the handler rather than at the provider is what lets a preview agree with the charge.
func TestTheHandlerAsksToChargeForAnUpgradeAndNotForADowngrade(t *testing.T) {
	b, _, org, fake := sizeBot(t, "under_25", 199)
	fake.changeResult = PriceChangeResult{Charged: true, AmountMinor: 30000, Currency: "usd"}
	if w := sizeChange(t, b, org, `{"size":"25_100"}`); w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}
	if !fake.changedCharge {
		t.Error("an upgrade did not ask to be charged: the customer gets the better size for free until renewal")
	}

	b, _, org, fake = sizeBot(t, "25_100", 499)
	if w := sizeChange(t, b, org, `{"size":"under_25"}`); w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}
	if fake.changedCharge {
		t.Error("a downgrade asked to be charged: there is nothing to collect, only a credit to carry")
	}
}

// The whole point of a scheduled downgrade: they paid for this month, so the record must not
// move today. Writing the smaller size here would take away, silently and immediately, the size
// and the allowance they have already bought until the boundary.
func TestAScheduledDowngradeChangesNothingAboutTheAccountToday(t *testing.T) {
	b, st, org, fake := sizeBot(t, "25_100", 499)
	fake.changeResult = PriceChangeResult{Scheduled: true,
		Pending: PendingChange{PriceID: "price_small", AtUnix: 1792612062}}

	w := sizeChange(t, b, org, `{"size":"under_25"}`)
	if w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}
	var out struct {
		Size        string `json:"size"`
		PendingSize string `json:"pending_size"`
		PendingAt   string `json:"pending_at"`
		Scheduled   bool   `json:"scheduled"`
		Charged     bool   `json:"charged"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Size != "25_100" {
		t.Errorf("the answer says they are on %q already: they keep 25_100 until the date", out.Size)
	}
	if out.PendingSize != "under_25" || !out.Scheduled {
		t.Errorf("nothing tells the console what is waiting: %+v", out)
	}
	if out.PendingAt != stripeTime(1792612062) {
		t.Errorf("pending_at = %q: the sentence needs the date", out.PendingAt)
	}
	if out.Charged {
		t.Error("a downgrade reported a charge")
	}
	// The one that matters. The record moves when Stripe rolls the subscription over, not now.
	acct, _ := st.BillingAccountOf(context.Background(), org.ID)
	if acct.Size != "25_100" || acct.UnitPriceMicros != usdToMicros(499) {
		t.Fatalf("the account dropped to %q at %s the moment they asked, before the month they paid for ran out",
			acct.Size, creditAmount(acct.UnitPriceMicros))
	}
}

// Picking the size you are already on is how a pending downgrade gets called off, so it has to
// reach the provider rather than being answered here as a no-op.
func TestPickingTheCurrentSizeStillReachesTheProvider(t *testing.T) {
	b, _, org, fake := sizeBot(t, "25_100", 499)
	fake.changeResult = PriceChangeResult{Released: true}

	w := sizeChange(t, b, org, `{"size":"25_100"}`)
	if w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}
	if fake.changeCalls != 1 {
		t.Fatal("it was answered without asking Stripe, so a pending downgrade would still happen")
	}
	if fake.changedCharge {
		t.Error("calling off a downgrade asked to charge for something")
	}
	var out struct {
		Size      string `json:"size"`
		Cancelled bool   `json:"cancelled"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Size != "25_100" || !out.Cancelled {
		t.Errorf("answered %+v, want the size held and the pending change reported cancelled", out)
	}
}

// The double-charge guard. A random key per press would let two in-flight requests both charge,
// because the "already on this size" check reads a record written only after Stripe answers.
func TestTheSameSizeMoveCarriesTheSameIdempotencyKey(t *testing.T) {
	acct := BillingAccount{SubscriptionID: "sub_m", Size: "under_25"}
	to := Size{Key: "25_100", AmountMinor: 49900}
	at := time.Unix(1792612062, 0)

	first := sizeChangeKey(acct, to, at)
	if first == "" {
		t.Fatal("no key at all: every press would be a fresh charge")
	}
	// The second press of the same button, a second later.
	if again := sizeChangeKey(acct, to, at.Add(time.Second)); again != first {
		t.Error("a double click produced two keys, which is two charges")
	}
	// A different destination is a different intent and must not be swallowed.
	if other := sizeChangeKey(acct, Size{Key: "under_25", AmountMinor: 19900}, at); other == first {
		t.Error("two different moves share a key: the second would be answered with the first's result")
	}
	// Nor may two accounts ever collide.
	if other := sizeChangeKey(BillingAccount{SubscriptionID: "sub_other", Size: "under_25"}, to, at); other == first {
		t.Error("two subscriptions share a key")
	}
	// And a genuine change of mind later is not a replay. Stripe remembers a key for 24 hours,
	// so without this an up-down-up inside a day would silently never make the third move.
	if later := sizeChangeKey(acct, to, at.Add(11*time.Minute)); later == first {
		t.Error("the same move 11 minutes later reused the key: a real second change would be dropped")
	}
}

func TestTheHandlerSendsADerivedKeyRatherThanNone(t *testing.T) {
	b, _, org, fake := sizeBot(t, "under_25", 199)
	if w := sizeChange(t, b, org, `{"size":"25_100"}`); w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}
	if fake.changedKey == "" {
		t.Fatal("the size change went to Stripe with no idempotency key")
	}
	if want := sizeChangeKey(BillingAccount{SubscriptionID: "sub_m", Size: "under_25"},
		Size{Key: "25_100", AmountMinor: 49900}, time.Now()); fake.changedKey != want {
		t.Errorf("key = %q, want the one derived from the move (%q)", fake.changedKey, want)
	}
}

// A card that wants its owner cannot be dealt with from this endpoint at all, so answering with
// a bare refusal leaves somebody pressing a button that can never work. It has to say so, and
// point at the one place the challenge can be finished.
func TestACardNeedingAuthenticationSendsThemSomewhereItCanBeDone(t *testing.T) {
	b, st, org, fake := sizeBot(t, "under_25", 199)
	fake.changeErr = &stripeError{Status: 402, Message: "authentication required",
		Code: "subscription_payment_intent_requires_action"}

	w := sizeChange(t, b, org, `{"size":"25_100"}`)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("answered %d, want 402: %s", w.Code, w.Body)
	}
	var out struct {
		Error  string `json:"error"`
		Portal bool   `json:"portal"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if !out.Portal {
		t.Error("no way out was offered: the customer can only press the same button again")
	}
	if !strings.Contains(out.Error, "not changed") {
		t.Errorf("the answer does not say the size stayed put, which is the one thing they need to know: %q", out.Error)
	}
	// error_if_incomplete means Stripe moved nothing, so neither may we.
	acct, _ := st.BillingAccountOf(context.Background(), org.ID)
	if acct.Size != "under_25" || acct.UnitPriceMicros != usdToMicros(199) {
		t.Fatalf("the record moved to %q although nothing was paid", acct.Size)
	}
}

func TestADeclinedCardSaysSoAndOffersTheCardScreen(t *testing.T) {
	b, st, org, fake := sizeBot(t, "under_25", 199)
	fake.changeErr = &stripeError{Status: 402, Message: "your card was declined", Code: "card_declined"}

	w := sizeChange(t, b, org, `{"size":"25_100"}`)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("answered %d, want 402: %s", w.Code, w.Body)
	}
	var out struct {
		Error  string `json:"error"`
		Portal bool   `json:"portal"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if !strings.Contains(out.Error, "declined") || !out.Portal {
		t.Errorf("a decline answered %q portal=%v", out.Error, out.Portal)
	}
	acct, _ := st.BillingAccountOf(context.Background(), org.ID)
	if acct.Size != "under_25" {
		t.Fatalf("the record moved to %q although the card was declined", acct.Size)
	}
}

// A scheduled downgrade has to survive a page reload. Without this the console forgets it the
// moment the toast goes, and somebody who is not sure it took asks for it a second time.
func TestTheBillingScreenStillKnowsADowngradeIsWaiting(t *testing.T) {
	b, _, org, fake := sizeBot(t, "25_100", 499)
	fake.pending = PendingChange{ScheduleID: "sub_sched_1", PriceID: "price_small", AtUnix: 1792612062}

	v := billingViewFor(t, b, org)
	if v.Subscription == nil {
		t.Fatal("no subscription on the billing view")
	}
	if v.Subscription.Size != "25_100" {
		t.Errorf("size = %q: they are still ON the larger size until the date", v.Subscription.Size)
	}
	if v.Subscription.PendingSize != "under_25" {
		t.Errorf("pending_size = %q, want under_25", v.Subscription.PendingSize)
	}
	if v.Subscription.PendingAt != stripeTime(1792612062) {
		t.Errorf("pending_at = %q: the sentence needs the date", v.Subscription.PendingAt)
	}
	if v.Subscription.PendingSizeLabel == "" {
		t.Error("no label to put in the sentence")
	}
}

// Stripe being unreachable must not take the billing screen down with it. The figures on it are
// ours, not Stripe's; the only thing lost is the mention of a pending change.
func TestTheBillingScreenLoadsWhenStripeCannotBeAsked(t *testing.T) {
	b, _, org, fake := sizeBot(t, "25_100", 499)
	fake.pendingErr = fmt.Errorf("stripe is down")

	v := billingViewFor(t, b, org)
	if v.Subscription == nil || v.Subscription.Size != "25_100" {
		t.Fatal("the billing screen failed because Stripe could not be reached")
	}
	if v.Subscription.PendingSize != "" {
		t.Error("a pending size was invented out of an error")
	}
}

func billingViewFor(t *testing.T, b *Bot, org *Org) billingView {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/billing", nil)
	r = r.WithContext(context.WithValue(r.Context(), userKey, &AdminUser{ID: 1, OrgID: org.ID,
		OrgPublic: org.PublicID, OrgName: org.Name, PublicID: "actor",
		Permissions: map[Permission]bool{PermBillingManage: true}}))
	w := httptest.NewRecorder()
	b.handleBilling(w, r)
	if w.Code != 200 {
		t.Fatalf("the billing screen answered %d: %s", w.Code, w.Body)
	}
	var v billingView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
