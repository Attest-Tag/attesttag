package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Stripe never brings a cancelled subscription back — a customer who returns gets a new one with a
// new id — so nothing that arrives after the cancellation may: not the console's return trip
// replayed inside its hour, not the checkout redelivered (under its own id or a fresh one), not a
// late invoice.paid, and not a live-looking subscription snapshot delivered out of order.
func TestAnEndedSubscriptionStaysEnded(t *testing.T) {
	b, st, fake := stripeBot(t)
	ctx := context.Background()
	org := testOrg(t, st, "Churn Ltd")
	ps, pe := time.Now().Add(-time.Hour).Unix(), time.Now().Add(30*24*time.Hour).Unix()

	checkout := fmt.Sprintf(`{"id":"evt_cs","type":"checkout.session.completed","livemode":false,"data":{"object":{
		"id":"cs_S","object":"checkout.session","mode":"subscription","customer":"cus_S","subscription":"sub_S",
		"currency":"usd","metadata":{"attesttag_org":%q}}}}`, org.PublicID)
	postWebhook(t, b, checkout, true)
	if o, _ := st.Org(ctx, org.ID); o.Plan != PlanPro {
		t.Fatalf("the checkout did not put the account on pro: %s", o.Plan)
	}
	postWebhook(t, b, fmt.Sprintf(`{"id":"evt_del","type":"customer.subscription.deleted","livemode":false,"data":{"object":{
		"id":"sub_S","object":"subscription","customer":"cus_S","status":"canceled","metadata":{"attesttag_org":%q},
		"items":{"data":[{"quantity":1,"price":{"id":"price_small","currency":"usd","unit_amount":19900},
		"current_period_start":%d,"current_period_end":%d}]}}}}`, org.PublicID, ps, pe), true)

	stillEnded := func(after string) {
		t.Helper()
		o, _ := st.Org(ctx, org.ID)
		acct, _ := st.BillingAccountOf(ctx, org.ID)
		if o.Plan != PlanFree || acct.Status != "canceled" {
			t.Errorf("after %s: plan=%s status=%s, want free and canceled", after, o.Plan, acct.Status)
		}
	}
	stillEnded("the cancellation")

	fake.session = CheckoutSession{ID: "cs_S", Status: "complete", PaymentStatus: "paid", Mode: "subscription",
		SubscriptionID: "sub_S", CustomerID: "cus_S", Created: time.Now().Add(-20 * time.Minute).Unix(),
		Metadata: map[string]string{metaOrg: org.PublicID}}
	if b.settleCheckout(ctx, org.ID, "cs_S") {
		t.Error("the return trip for an ended subscription settled")
	}
	stillEnded("a settle replayed inside the hour")

	postWebhook(t, b, checkout, true)
	postWebhook(t, b, strings.Replace(checkout, `"evt_cs"`, `"evt_cs_resent"`, 1), true)
	stillEnded("the checkout redelivered")

	postWebhook(t, b, fmt.Sprintf(`{"id":"evt_inv","type":"invoice.paid","livemode":false,"data":{"object":{
		"id":"in_late","object":"invoice","customer":"cus_S","currency":"usd",
		"parent":{"subscription_details":{"subscription":"sub_S","metadata":{"attesttag_org":%q}}},
		"lines":{"data":[{"period":{"start":%d,"end":%d},"price":{"id":"price_small"}}]}}}}`, org.PublicID, ps, pe), true)
	stillEnded("a late invoice.paid")

	postWebhook(t, b, fmt.Sprintf(`{"id":"evt_upd","type":"customer.subscription.updated","livemode":false,"data":{"object":{
		"id":"sub_S","object":"subscription","customer":"cus_S","status":"active","metadata":{"attesttag_org":%q},
		"items":{"data":[{"quantity":1,"price":{"id":"price_small","currency":"usd","unit_amount":19900},
		"current_period_start":%d,"current_period_end":%d}]}}}}`, org.PublicID, ps, pe), true)
	stillEnded("an active snapshot delivered out of order")
}
