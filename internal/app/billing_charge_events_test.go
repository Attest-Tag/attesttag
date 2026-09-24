package app

import (
	"context"
	"fmt"
	"testing"
)

// A refund or a dispute takes back what a top-up put in, found through the ledger row that credited
// the payment, and nothing else. Stripe's charge.refunded carries the running total, so each event
// takes only what is new; a dispute carries no customer and none of our metadata, yet still lands
// on the right account and is capped at what the payment has left; a won dispute gives back exactly
// what it took; and a refunded subscription fee — never credit — takes nothing.
func TestRefundsAndDisputesTakeBackOnlyWhatATopUpPutIn(t *testing.T) {
	b, st, _ := stripeBot(t)
	ctx := context.Background()
	org := testOrg(t, st, "Chargeback Ltd")
	balance := func() int64 {
		t.Helper()
		bal, _ := st.CreditBalance(ctx, org.ID)
		return bal
	}

	postWebhook(t, b, fmt.Sprintf(`{"id":"evt_topup","type":"checkout.session.completed","livemode":false,"data":{"object":{
		"id":"cs_t","object":"checkout.session","mode":"payment","payment_status":"paid","payment_intent":"pi_t",
		"amount_total":10000,"currency":"usd","customer":"cus_t","metadata":{"attesttag_org":%q}}}}`, org.PublicID), true)
	if got := balance(); got != usdToMicros(100) {
		t.Fatalf("the top-up credited %s, want $100.00", creditAmount(got))
	}

	for i, cumulative := range []int{1000, 2000} {
		postWebhook(t, b, fmt.Sprintf(`{"id":"evt_refund_%d","type":"charge.refunded","livemode":false,"data":{"object":{
			"id":"ch_t","object":"charge","amount_refunded":%d,"currency":"usd","customer":"cus_t","payment_intent":"pi_t"}}}`,
			i, cumulative), true)
	}
	if got := balance(); got != usdToMicros(80) {
		t.Errorf("two $10 partial refunds left %s, want $80.00", creditAmount(got))
	}

	postWebhook(t, b, `{"id":"evt_dispute","type":"charge.dispute.created","livemode":false,"data":{"object":{
		"id":"dp_1","object":"dispute","amount":10000,"charge":"ch_t","payment_intent":"pi_t","currency":"usd",
		"reason":"fraudulent","status":"needs_response","metadata":{}}}}`, true)
	if got := balance(); got != 0 {
		t.Errorf("a dispute for the whole payment left %s, want $0.00 — the $80 still in it", creditAmount(got))
	}

	postWebhook(t, b, `{"id":"evt_won","type":"charge.dispute.closed","livemode":false,"data":{"object":{
		"id":"dp_1","object":"dispute","amount":10000,"charge":"ch_t","payment_intent":"pi_t","currency":"usd",
		"status":"won","metadata":{}}}}`, true)
	if got := balance(); got != usdToMicros(80) {
		t.Errorf("a won dispute left %s, want $80.00 — exactly what it took back", creditAmount(got))
	}

	postWebhook(t, b, `{"id":"evt_fee_refund","type":"charge.refunded","livemode":false,"data":{"object":{
		"id":"ch_fee","object":"charge","amount_refunded":19900,"currency":"usd","customer":"cus_t","payment_intent":"pi_fee"}}}`, true)
	if got := balance(); got != usdToMicros(80) {
		t.Errorf("a refunded subscription fee took prepaid credit: %s left, want $80.00", creditAmount(got))
	}
}
