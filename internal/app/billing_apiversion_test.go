package app

// The shape a webhook ACTUALLY arrives in, and the size change that no longer needs a portal.
//
// Every other test in this package writes its fixtures in the 2024-06-20 spelling, which is what
// stripeAPIVersion pins — and a webhook is not rendered in that version. It is rendered in the
// version of the endpoint it is delivered to, taken from Stripe's account default on the day that
// endpoint was created. So a deployment could pass this whole suite and still read nothing: the
// account that found this was on 2026-08-26.dahlia, where three fields this package depends on
// have moved, and it showed a customer who had just paid $499 a plan with no size, no fee and no
// renewal date.
//
// These fixtures are the newer spelling. Both are kept working, because which one arrives is not
// something the code can choose.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// basilSubscription is a subscription event from 2025-04-30.basil onwards: the period is on the
// item, because items may now bill on different cycles, and no longer on the subscription.
func basilSubscription(eventID, evType, subID, customer, price string, start, end int64) string {
	return basilSubscriptionAs(eventID, evType, "active", subID, customer, price, start, end)
}

// basilSubscriptionAs is the same with the status spelled out, for the deliveries that arrive in
// the wrong order and describe a state the account has already left.
func basilSubscriptionAs(eventID, evType, status, subID, customer, price string, start, end int64) string {
	return fmt.Sprintf(`{"id":%q,"type":%q,"livemode":false,"data":{"object":{
		"id":%q,"object":"subscription","customer":%q,"status":%q,"cancel_at_period_end":false,
		"items":{"data":[{"quantity":1,"current_period_start":%d,"current_period_end":%d,
		  "price":{"id":%q,"currency":"usd","unit_amount":49900}}]}}}}`,
		eventID, evType, subID, customer, status, start, end, price)
}

// basilInvoice is an invoice from the same versions: it names its subscription under parent
// rather than at the top level, repeats that subscription's metadata there — the only place an
// invoice says whose it is — and names its Price under pricing rather than as `price`.
func basilInvoice(eventID, invoiceID, customer, subID, price, orgPublic, size string) string {
	return fmt.Sprintf(`{"id":%q,"type":"invoice.paid","livemode":false,"data":{"object":{
		"id":%q,"object":"invoice","customer":%q,"currency":"usd","billing_reason":"subscription_create",
		"parent":{"type":"subscription_details","subscription_details":{"subscription":%q,
		  "metadata":{"attesttag_org":%q,"size":%q}}},
		"lines":{"data":[{"period":{"start":1790020062,"end":1792612062},
		  "pricing":{"type":"price_details","price_details":{"price":%q}}}]}}}}`,
		eventID, invoiceID, customer, subID, orgPublic, size, price)
}

// The renewal date. It was read from the subscription, it now lives on the item, and reading only
// the old place is what left "Renews —" on an account Stripe was perfectly happy to describe.
func TestASubscriptionPeriodIsReadWhereverTheAPIVersionPutIt(t *testing.T) {
	const start, end = 1790020062, 1792612062
	for name, body := range map[string]string{
		"on the item (2025-04-30.basil and later)": basilSubscription("evt_basil", "customer.subscription.updated",
			"sub_v", "cus_v", "price_mid", start, end),
		"on the subscription (2024-06-20)": fmt.Sprintf(`{"id":"evt_old","type":"customer.subscription.updated","livemode":false,"data":{"object":{
			"id":"sub_v","object":"subscription","customer":"cus_v","status":"active",
			"current_period_start":%d,"current_period_end":%d,
			"items":{"data":[{"quantity":1,"price":{"id":"price_mid","currency":"usd","unit_amount":49900}}]}}}}`, start, end),
	} {
		t.Run(name, func(t *testing.T) {
			b, st := billingBot(t, billingCfg())
			org := testOrg(t, st, "Renewing Ltd")
			subscribedTo(t, st, org.ID, "cus_v", "25_100")

			if w := postWebhook(t, b, body, true); w.Code != 200 {
				t.Fatalf("answered %d: %s", w.Code, w.Body)
			}
			acct, _ := st.BillingAccountOf(context.Background(), org.ID)
			if acct.PeriodEnd != stripeTime(end) {
				t.Fatalf("period_end is %q, want %q — the console draws Renews from this", acct.PeriodEnd, stripeTime(end))
			}
			if acct.PeriodStart != stripeTime(start) {
				t.Fatalf("period_start is %q, want %q", acct.PeriodStart, stripeTime(start))
			}
			if acct.Size != "25_100" {
				t.Fatalf("size is %q after a subscription event naming price_mid", acct.Size)
			}
		})
	}
}

// The delivery order of one real $499 purchase: customer.subscription.updated (active) landed
// before customer.subscription.created (incomplete), and the stale snapshot won. It self-corrected
// there only because checkout.session.completed followed; a batch ending on it leaves an account
// that has paid reading `incomplete`, which Active() answers false to — no size change, and an
// offer to sell them a second subscription.
func TestAStaleIncompleteSnapshotNeverUnsetsALiveSubscription(t *testing.T) {
	const start, end = 1790020062, 1792612062
	b, st := billingBot(t, billingCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Reordered Ltd")
	subscribedTo(t, st, org.ID, "cus_r", "25_100")

	// The account goes live. `updated` carries Stripe's created timestamp :65.
	if w := postWebhook(t, b, basilSubscriptionAs("evt_upd", "customer.subscription.updated",
		"active", "sub_r", "cus_r", "price_mid", start, end), true); w.Code != 200 {
		t.Fatalf("the updated event answered %d: %s", w.Code, w.Body)
	}
	if acct, _ := st.BillingAccountOf(ctx, org.ID); acct.Status != "active" {
		t.Fatalf("status is %q after an active snapshot", acct.Status)
	}

	// And now the snapshot from before it went live, at :63, arriving second.
	if w := postWebhook(t, b, basilSubscriptionAs("evt_crt", "customer.subscription.created",
		"incomplete", "sub_r", "cus_r", "price_mid", start, end), true); w.Code != 200 {
		t.Fatalf("the created event answered %d: %s", w.Code, w.Body)
	}

	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Status != "active" {
		t.Fatalf("status is %q: a subscription that has gone live never returns to incomplete", acct.Status)
	}
	if !acct.Active() {
		t.Error("Active() is false, so the console offers a second subscription and refuses a size change")
	}
	if acct.PeriodEnd != stripeTime(end) {
		t.Errorf("the renewal date went with it: %q", acct.PeriodEnd)
	}
	after, _ := st.Org(ctx, org.ID)
	if after.Plan != PlanPro {
		t.Fatalf("plan is %q", after.Plan)
	}
	// Acknowledged rather than refused: replaying it will not make it newer, and a 5xx loop counts
	// toward Stripe disabling the endpoint.
	if !st.HasBillingEvent(ctx, "evt_crt") {
		t.Error("the ignored event was not recorded, so its redelivery is reconsidered from scratch")
	}
}

// The guard must not swallow the ordinary case. A subscription whose first payment genuinely has
// not gone through IS incomplete, and an account with no live subscription has to be told so.
func TestAGenuineIncompleteIsStillRecorded(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Pending Ltd")
	// Bound by customer only: no status yet, which is where a brand-new subscription starts.
	st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_p", Quantity: 1, Currency: "usd"})

	if w := postWebhook(t, b, basilSubscriptionAs("evt_new", "customer.subscription.created",
		"incomplete", "sub_p", "cus_p", "price_mid", 1790020062, 1792612062), true); w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Status != "incomplete" {
		t.Fatalf("status is %q, want incomplete: the first payment has not gone through", acct.Status)
	}
}

// Scoped to the subscription on record. `incomplete` for a DIFFERENT subscription is a second one
// being started — news rather than an echo — and this is also how a fresh subscription bought
// after a cancellation gets through instead of being read as a stale snapshot of the old one.
func TestIncompleteForAnotherSubscriptionIsNotAnEcho(t *testing.T) {
	b, st := billingBot(t, billingCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Second Ltd")
	st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_s", SubscriptionID: "sub_old",
		Status: "active", Size: "25_100", Quantity: 1, Currency: "usd"})

	if w := postWebhook(t, b, basilSubscriptionAs("evt_second", "customer.subscription.created",
		"incomplete", "sub_new", "cus_s", "price_mid", 1790020062, 1792612062), true); w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.SubscriptionID != "sub_new" || acct.Status != "incomplete" {
		t.Fatalf("a second subscription was read as an echo of the first: %q at %q",
			acct.SubscriptionID, acct.Status)
	}
}

// The first invoice of a new subscription, arriving before anything has bound the customer.
//
// Stripe does not order its deliveries, and in the account that found this invoice.paid was the
// FIRST event of the twelve. An invoice carries no metadata of its own, so until it was read from
// parent.subscription_details the event resolved to no organisation at all and was filed as
// "unknown" — taking the size's monthly allowance with it, silently, on the month it was paid for.
func TestTheFirstInvoiceFindsItsOrganisationBeforeAnyCustomerIsBound(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Unbound Ltd")

	// Deliberately nothing recorded: no customer, no subscription, no size. This is the state a
	// brand-new account is in when the first invoice lands.
	if acct, _ := st.BillingAccountOf(ctx, org.ID); acct.CustomerID != "" {
		t.Fatalf("this test needs an account with no customer, got %q", acct.CustomerID)
	}

	body := basilInvoice("evt_unbound", "in_unbound", "cus_unbound", "sub_unbound", "price_25", org.PublicID, "upto_25")
	if w := postWebhook(t, b, body, true); w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}

	// The allowance is its own bucket now rather than a ledger movement, so this asks the
	// question the way the gate does: what may this account spend?
	if got := allowanceOf(t, st, org.ID); got != usdToMicros(20) {
		t.Fatalf("the first invoice granted %s of allowance, want the $20.00 the size includes", creditAmount(got))
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Size != "upto_25" {
		t.Errorf("size is %q: the invoice line names the price that was charged, which IS the size", acct.Size)
	}
	if acct.SubscriptionID != "sub_unbound" {
		t.Errorf("subscription id is %q: the invoice names it under parent", acct.SubscriptionID)
	}
	if acct.PeriodEnd == "" {
		t.Error("the invoice rolled no period, so the console has no renewal date to show")
	}
	assertLedgerMatchesBalance(t, st, org.ID)
}

// The same invoice in the old spelling still works. Nothing about the fix may depend on which
// version an endpoint happens to be on.
func TestTheOldInvoiceSpellingStillCredits(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	org := testOrg(t, st, "Old Ltd")
	subscribedTo(t, st, org.ID, "cus_old", "upto_25")

	if w := postWebhook(t, b, invoicePaid("evt_old_inv", "in_old", "cus_old", "price_25"), true); w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}
	if got := allowanceOf(t, st, org.ID); got != usdToMicros(20) {
		t.Fatalf("the 2024-06-20 spelling granted %s of allowance, want $20.00", creditAmount(got))
	}
}

// An upgrade charged on the spot is paid by an invoice with no line for the subscription itself,
// only two prorations over the rest of the period. These are shaped like the real one Stripe's
// sandbox raised for a size change, in both spellings: the credit for the unused time on the OLD
// Price comes first, and both lines' period starts at the moment of the change. Reading the first
// line wrote the size being left back over the one just bought, and moved the period's start to
// the change — which the next upgrade's share of its allowance is measured from.
func TestAnUpgradeInvoiceKeepsTheNewSizeAndTheRealPeriod(t *testing.T) {
	const start, end = 1790020062, 1792612062 // the subscription's own period
	const changed = start + 7*24*3600         // upgraded a week in
	for name, object := range map[string]string{
		"2025-04-30.basil and later": fmt.Sprintf(`
			"parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_up"}},
			"lines":{"data":[
			  {"amount":-15300,"period":{"start":%[1]d,"end":%[2]d},
			   "pricing":{"type":"price_details","price_details":{"price":"price_small"}},
			   "parent":{"type":"subscription_item_details","subscription_item_details":{"proration":true}}},
			  {"amount":38400,"period":{"start":%[1]d,"end":%[2]d},
			   "pricing":{"type":"price_details","price_details":{"price":"price_mid"}},
			   "parent":{"type":"subscription_item_details","subscription_item_details":{"proration":true}}}]}`, changed, end),
		"2024-06-20": fmt.Sprintf(`
			"subscription":"sub_up",
			"lines":{"data":[
			  {"amount":-15300,"type":"invoiceitem","proration":true,"period":{"start":%[1]d,"end":%[2]d},"price":{"id":"price_small"}},
			  {"amount":38400,"type":"invoiceitem","proration":true,"period":{"start":%[1]d,"end":%[2]d},"price":{"id":"price_mid"}}]}`, changed, end),
	} {
		t.Run(name, func(t *testing.T) {
			b, st := billingBot(t, billingCfg())
			ctx := context.Background()
			org := testOrg(t, st, "Upgraded Ltd")
			// What the console recorded once Stripe had taken the upgrade's payment.
			st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_up", SubscriptionID: "sub_up",
				Status: "active", Size: "25_100", UnitPriceMicros: usdToMicros(499), Quantity: 1, Currency: "usd",
				PeriodStart: stripeTime(start), PeriodEnd: stripeTime(end)})

			body := fmt.Sprintf(`{"id":"evt_upgrade","type":"invoice.paid","livemode":false,"data":{"object":{
				"id":"in_upgrade","object":"invoice","customer":"cus_up","currency":"usd",
				"billing_reason":"subscription_update",%s}}}`, object)
			if w := postWebhook(t, b, body, true); w.Code != 200 {
				t.Fatalf("answered %d: %s", w.Code, w.Body)
			}
			acct, _ := st.BillingAccountOf(ctx, org.ID)
			if acct.Size != "25_100" || acct.UnitPriceMicros != usdToMicros(499) {
				t.Errorf("the upgrade's own invoice put the record back on %q at %s, want 25_100 at $499.00",
					acct.Size, creditAmount(acct.UnitPriceMicros))
			}
			if acct.PeriodStart != stripeTime(start) || acct.PeriodEnd != stripeTime(end) {
				t.Errorf("the period became %s to %s, want the subscription's own %s to %s",
					acct.PeriodStart, acct.PeriodEnd, stripeTime(start), stripeTime(end))
			}
		})
	}

	// Whatever else an invoice carries, the subscription's own line is the one that names the
	// size and the period, wherever it sits among the prorations.
	var renewal stripeObject
	if err := json.Unmarshal([]byte(`{"lines":{"data":[
		{"amount":-15300,"proration":true,"period":{"start":1,"end":2},"price":{"id":"price_small"}},
		{"amount":38400,"proration":true,"period":{"start":1,"end":2},"price":{"id":"price_mid"}},
		{"amount":49900,"proration":false,"period":{"start":3,"end":4},"price":{"id":"price_mid"}}]}}`), &renewal); err != nil {
		t.Fatal(err)
	}
	if price, s, e := renewal.subscriptionLine(); price != "price_mid" || s != 3 || e != 4 {
		t.Errorf("a renewal carrying prorations read %q for %d to %d, want its own line: price_mid for 3 to 4", price, s, e)
	}
}

// The screenshot this was reported from. PutSubscription writes every column, so any path that
// builds a SubscriptionState out of partial knowledge publishes blanks over whatever the webhooks
// had already established — and the settle path runs on the FIRST page somebody sees after
// paying, which is the worst possible moment to forget what they bought.
func TestSettlingAPurchaseNeverBlanksWhatTheWebhookKnows(t *testing.T) {
	cfg := billingCfg()
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Settled Ltd")
	fake := &fakePayments{}
	b := &Bot{cfg: cfg, store: st, settings: newSettingsCache(st, cfg), pay: fake, mail: logMailer{}}

	// The webhooks got here first and the record is complete.
	st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_settle",
		SubscriptionID: "sub_settle", Status: "active", Quantity: 1, Currency: "usd"})
	if w := postWebhook(t, b, basilSubscription("evt_pre", "customer.subscription.updated",
		"sub_settle", "cus_settle", "price_mid", 1790020062, 1792612062), true); w.Code != 200 {
		t.Fatalf("the subscription event answered %d: %s", w.Code, w.Body)
	}
	if before, _ := st.BillingAccountOf(ctx, org.ID); before.PeriodEnd == "" {
		t.Fatal("the subscription event recorded no period, so this test cannot tell whether settling erased one")
	}

	fake.session = CheckoutSession{ID: "cs_settle", Status: "complete", PaymentStatus: "paid", Mode: "subscription", SubscriptionID: "sub_settle",
		CustomerID: "cus_settle", Currency: "usd", ClientRef: org.PublicID,
		Metadata: map[string]string{metaOrg: org.PublicID, "size": "25_100"}}
	if !b.settleCheckout(ctx, org.ID, "cs_settle") {
		t.Fatal("a completed subscription session did not settle")
	}

	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Size != "25_100" {
		t.Errorf("size is %q after settling: the session's own metadata names it", acct.Size)
	}
	if acct.UnitPriceMicros != usdToMicros(499) {
		t.Errorf("monthly fee is %s after settling, want $499.00", creditAmount(acct.UnitPriceMicros))
	}
	if acct.PeriodEnd != stripeTime(1792612062) {
		t.Errorf("settling erased the renewal date the webhook had already recorded: %q", acct.PeriodEnd)
	}
}

// A subscription checkout that beats every webhook still names the size, because the session
// carries it. Without this the settle path is only ever a downgrade of what is on screen.
func TestSettlingAheadOfTheWebhookStillNamesTheSize(t *testing.T) {
	cfg := billingCfg()
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Early Ltd")
	fake := &fakePayments{session: CheckoutSession{ID: "cs_early", Status: "complete", PaymentStatus: "paid", Mode: "subscription",
		SubscriptionID: "sub_early", CustomerID: "cus_early", Currency: "usd", ClientRef: org.PublicID,
		Metadata: map[string]string{metaOrg: org.PublicID, "size": "25_100"}}}
	b := &Bot{cfg: cfg, store: st, settings: newSettingsCache(st, cfg), pay: fake, mail: logMailer{}}

	if !b.settleCheckout(ctx, org.ID, "cs_early") {
		t.Fatal("the session did not settle")
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Size != "25_100" || acct.UnitPriceMicros != usdToMicros(499) {
		t.Fatalf("size %q at %s, want 25_100 at $499.00", acct.Size, creditAmount(acct.UnitPriceMicros))
	}
	after, _ := st.Org(ctx, org.ID)
	if after.Plan != PlanPro {
		t.Fatalf("plan is %q after a settled subscription", after.Plan)
	}
}

// ---- changing size ----

func sizeChange(t *testing.T, b *Bot, org *Org, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/billing/size", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), userKey, &AdminUser{ID: 1, OrgID: org.ID,
		OrgPublic: org.PublicID, OrgName: org.Name, PublicID: "actor",
		Permissions: map[Permission]bool{PermBillingManage: true}}))
	w := httptest.NewRecorder()
	b.handleBillingSize(w, r)
	return w
}

func TestChangingSizeMovesTheSubscriptionAndTheRecord(t *testing.T) {
	cfg := billingCfg()
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Upgrading Ltd")
	fake := &fakePayments{}
	b := &Bot{cfg: cfg, store: st, settings: newSettingsCache(st, cfg), pay: fake, mail: logMailer{}}
	st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_up", SubscriptionID: "sub_up",
		Status: "active", Size: "under_25", UnitPriceMicros: usdToMicros(199), Quantity: 1,
		Currency: "usd", PeriodEnd: stripeTime(1792612062)})

	if w := sizeChange(t, b, org, `{"size":"25_100"}`); w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}
	if fake.changedSub != "sub_up" || fake.changedPrice != "price_mid" {
		t.Fatalf("Stripe was asked to move %q to %q, want sub_up to price_mid", fake.changedSub, fake.changedPrice)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Size != "25_100" || acct.UnitPriceMicros != usdToMicros(499) {
		t.Fatalf("the record says %q at %s", acct.Size, creditAmount(acct.UnitPriceMicros))
	}
	// The period is not this side's to invent, and it is not this side's to throw away either.
	if acct.PeriodEnd != stripeTime(1792612062) {
		t.Errorf("changing size erased the renewal date: %q", acct.PeriodEnd)
	}
	events, _ := st.AuditEvents(ctx, org.ID, AuditFilter{Limit: 20})
	found := false
	for _, e := range events {
		if e.Action == "billing.band_changed" {
			found = true
			if d := string(e.Details); !strings.Contains(d, "under_25") || !strings.Contains(d, "25_100") {
				t.Errorf("the audit row does not say what changed: %s", d)
			}
		}
	}
	if !found {
		t.Error("no billing.band_changed audit row")
	}
}

// The size is a key and never a price. A browser that could name the Price it moves to could name
// a $0 one, and everything below is looked up on this side.
func TestChangingSizeRefusesWhatThisDeploymentDoesNotSell(t *testing.T) {
	cfg := billingCfg()
	st := testStore(t)
	org := testOrg(t, st, "Creative Ltd")
	fake := &fakePayments{}
	b := &Bot{cfg: cfg, store: st, settings: newSettingsCache(st, cfg), pay: fake, mail: logMailer{}}
	st.PutSubscription(context.Background(), org.ID, SubscriptionState{CustomerID: "cus_c",
		SubscriptionID: "sub_c", Status: "active", Size: "under_25", Quantity: 1, Currency: "usd"})

	for name, body := range map[string]string{
		"a size with no price here": `{"size":"unlimited"}`,
		"a Stripe price id":         `{"size":"price_free"}`,
		"nothing at all":            `{"size":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			if w := sizeChange(t, b, org, body); w.Code == 200 {
				t.Fatalf("accepted %s", body)
			}
			if fake.changedSub != "" {
				t.Fatalf("Stripe was called anyway, for %q", fake.changedPrice)
			}
		})
	}
}

func TestChangingSizeNeedsASubscriptionToChange(t *testing.T) {
	cfg := billingCfg()
	st := testStore(t)
	ctx := context.Background()
	fake := &fakePayments{}
	b := &Bot{cfg: cfg, store: st, settings: newSettingsCache(st, cfg), pay: fake, mail: logMailer{}}

	// Nothing bought at all.
	free := testOrg(t, st, "Free Ltd")
	if w := sizeChange(t, b, free, `{"size":"25_100"}`); w.Code != 409 {
		t.Fatalf("an account with no subscription answered %d, want 409: %s", w.Code, w.Body)
	}

	// Granted rather than bought: there is no Stripe subscription behind it, and an operator is
	// the only one who should be changing what it is recorded as.
	comped := testOrg(t, st, "Comped Ltd")
	st.PutSubscription(ctx, comped.ID, SubscriptionState{Status: "comped", Size: "under_25",
		UnitPriceMicros: usdToMicros(199), Quantity: 1, Currency: "usd"})
	if w := sizeChange(t, b, comped, `{"size":"25_100"}`); w.Code != 409 {
		t.Fatalf("a comped account answered %d, want 409: %s", w.Code, w.Body)
	}
	if fake.changedSub != "" {
		t.Fatal("Stripe was called for an account with no subscription")
	}
	acct, _ := st.BillingAccountOf(ctx, comped.ID)
	if acct.Size != "under_25" {
		t.Fatalf("a comped account's size moved to %q", acct.Size)
	}
}

// If Stripe refuses, nothing here may claim it happened. The record is what the console draws,
// and a size it shows that Stripe is not charging is worse than an error message.
func TestARefusedSizeChangeChangesNothing(t *testing.T) {
	cfg := billingCfg()
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Declined Ltd")
	fake := &fakePayments{changeErr: fmt.Errorf("your card was declined")}
	b := &Bot{cfg: cfg, store: st, settings: newSettingsCache(st, cfg), pay: fake, mail: logMailer{}}
	st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_d", SubscriptionID: "sub_d",
		Status: "active", Size: "under_25", UnitPriceMicros: usdToMicros(199), Quantity: 1, Currency: "usd"})

	w := sizeChange(t, b, org, `{"size":"25_100"}`)
	if w.Code == 200 {
		t.Fatal("a refused change answered 200")
	}
	var out struct{ Error string }
	json.Unmarshal(w.Body.Bytes(), &out)
	if !strings.Contains(out.Error, "declined") {
		t.Errorf("the reason Stripe gave was not passed on: %q", out.Error)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Size != "under_25" || acct.UnitPriceMicros != usdToMicros(199) {
		t.Fatalf("the record moved to %q at %s although Stripe refused", acct.Size, creditAmount(acct.UnitPriceMicros))
	}
}
