package app

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// stripeBot is a billing bot whose Stripe is a fake that still verifies webhook signatures for
// real, so one fixture drives both the webhook and the settle-on-return path.
func stripeBot(t *testing.T) (*Bot, *Store, *fakePayments) {
	t.Helper()
	cfg := billingCfg()
	st := testStore(t)
	fake := &fakePayments{}
	return &Bot{cfg: cfg, store: st, settings: newSettingsCache(st, cfg), pay: fake, mail: logMailer{}}, st, fake
}

// client_reference_id is the payer's to set on a Stripe Payment Link, by adding it to the URL. A
// session that names an account there and nowhere else is a payment this server never started —
// an enterprise fee, say — so returning from it credits nothing.
func TestSettleIgnoresAClientReferenceIDItDidNotSet(t *testing.T) {
	b, st, fake := stripeBot(t)
	ctx := context.Background()
	org := testOrg(t, st, "Link Payer Ltd")
	fake.session = CheckoutSession{ID: "cs_link", Status: "complete", Mode: "payment", PaymentStatus: "paid",
		PaymentIntent: "pi_link", AmountMinor: 3_000_000, Currency: "usd", ClientRef: org.PublicID,
		Created: time.Now().Unix(), Metadata: map[string]string{}}
	if b.settleCheckout(ctx, org.ID, "cs_link") {
		t.Error("a session naming the account only by client_reference_id settled")
	}
	if bal, _ := st.CreditBalance(ctx, org.ID); bal != 0 {
		t.Errorf("a Payment Link payment became %s of credit", creditAmount(bal))
	}
}

// An account's billing is its own customer and subscription. Neither a checkout naming it only by
// client_reference_id, nor an event carrying its metadata beside a different customer, may write
// another customer over them — that is how its billing could be pointed at somebody else's
// subscription and then cancelled from under it.
func TestAWebhookCannotHandAnAccountASecondCustomer(t *testing.T) {
	b, st, _ := stripeBot(t)
	ctx := context.Background()
	org := testOrg(t, st, "Bound Ltd")
	now := time.Now()
	postWebhook(t, b, fmt.Sprintf(`{"id":"evt_own","type":"customer.subscription.created","livemode":false,"data":{"object":{
		"id":"sub_OWN","object":"subscription","customer":"cus_OWN","status":"active","metadata":{"attesttag_org":%q},
		"items":{"data":[{"quantity":1,"price":{"id":"price_mid","currency":"usd","unit_amount":49900},
		"current_period_start":%d,"current_period_end":%d}]}}}}`, org.PublicID, now.Add(-24*time.Hour).Unix(), now.Add(29*24*time.Hour).Unix()), true)

	postWebhook(t, b, fmt.Sprintf(`{"id":"evt_link","type":"checkout.session.completed","livemode":false,"data":{"object":{
		"id":"cs_LINK","object":"checkout.session","mode":"subscription","customer":"cus_LINK","subscription":"sub_LINK",
		"client_reference_id":%q,"currency":"usd","metadata":{}}}}`, org.PublicID), true)
	postWebhook(t, b, fmt.Sprintf(`{"id":"evt_second","type":"customer.subscription.created","livemode":false,"data":{"object":{
		"id":"sub_SECOND","object":"subscription","customer":"cus_SECOND","status":"active","metadata":{"attesttag_org":%q},
		"items":{"data":[{"quantity":1,"price":{"id":"price_mid","currency":"usd","unit_amount":49900}}]}}}}`, org.PublicID), true)

	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.CustomerID != "cus_OWN" || acct.SubscriptionID != "sub_OWN" {
		t.Fatalf("the account's billing points at %q / %q, want its own cus_OWN / sub_OWN", acct.CustomerID, acct.SubscriptionID)
	}
}
