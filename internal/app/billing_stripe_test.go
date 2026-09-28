package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testWhsec = "whsec_test_secret_value"

func sigHeader(t *testing.T, body []byte, secret string, at time.Time) string {
	t.Helper()
	return signStripePayload(body, secret, at)
}

func TestStripeSignatureAcceptsAFreshOne(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"checkout.session.completed"}`)
	now := time.Now()
	if err := verifyStripeSignature(body, sigHeader(t, body, testWhsec, now), testWhsec, now, stripeTolerance); err != nil {
		t.Fatalf("a signature this process just made was refused: %v", err)
	}
}

// The mistake that looks like working code. If the signed message were "t.<timestamp>.<body>" —
// the header's field name included — every genuine delivery would be refused and every test that
// signed the same wrong way would pass. So the expected MAC here is computed from first
// principles rather than by calling our own signer.
func TestStripeSignatureIsOverTimestampDotBody(t *testing.T) {
	body := []byte(`{"id":"evt_2","type":"invoice.paid"}`)
	at := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(at.Unix(), 10)

	mac := hmac.New(sha256.New, []byte(testWhsec))
	mac.Write([]byte(ts + "." + string(body)))
	right := hex.EncodeToString(mac.Sum(nil))

	wrong := hmac.New(sha256.New, []byte(testWhsec))
	wrong.Write([]byte("t=" + ts + "." + string(body)))
	withFieldName := hex.EncodeToString(wrong.Sum(nil))

	if err := verifyStripeSignature(body, "t="+ts+",v1="+right, testWhsec, at, stripeTolerance); err != nil {
		t.Fatalf("the documented payload was refused: %v", err)
	}
	if err := verifyStripeSignature(body, "t="+ts+",v1="+withFieldName, testWhsec, at, stripeTolerance); err == nil {
		t.Fatal("a MAC over \"t=<ts>.<body>\" was accepted: the field name is not part of the signed payload")
	}
}

func TestStripeSignatureRefusesATimestampOutsideTheTolerance(t *testing.T) {
	body := []byte(`{"id":"evt_3","type":"charge.refunded"}`)
	now := time.Now()
	// Stale: a captured delivery replayed later.
	old := sigHeader(t, body, testWhsec, now.Add(-6*time.Minute))
	if err := verifyStripeSignature(body, old, testWhsec, now, stripeTolerance); err == nil {
		t.Error("a six-minute-old signature was accepted")
	}
	// And from the future, which is not a generous clock — it is a signature somebody chose the
	// expiry of.
	ahead := sigHeader(t, body, testWhsec, now.Add(6*time.Minute))
	if err := verifyStripeSignature(body, ahead, testWhsec, now, stripeTolerance); err == nil {
		t.Error("a signature timestamped six minutes in the future was accepted")
	}
}

func TestStripeSignatureRefusesATamperedBody(t *testing.T) {
	body := []byte(`{"id":"evt_4","type":"checkout.session.completed","amount_total":500}`)
	now := time.Now()
	h := sigHeader(t, body, testWhsec, now)
	tampered := []byte(strings.Replace(string(body), "500", "50000", 1))
	if err := verifyStripeSignature(tampered, h, testWhsec, now, stripeTolerance); err == nil {
		t.Fatal("a body edited after signing was accepted")
	}
}

func TestStripeSignatureRefusesAMalformedHeaderOrAWrongSecret(t *testing.T) {
	body := []byte(`{"id":"evt_5","type":"invoice.paid"}`)
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	good := sigHeader(t, body, testWhsec, now)

	for name, h := range map[string]string{
		"empty":                     "",
		"no timestamp":              "v1=deadbeef",
		"no v1":                     "t=" + ts,
		"timestamp is not a number": "t=yesterday,v1=deadbeef",
		"v1 is not hex":             "t=" + ts + ",v1=zzzz",
		"nothing parseable":         "garbage",
	} {
		if err := verifyStripeSignature(body, h, testWhsec, now, stripeTolerance); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := verifyStripeSignature(body, good, "whsec_a_different_secret", now, stripeTolerance); err == nil {
		t.Error("a signature made with another secret was accepted")
	}
	if err := verifyStripeSignature(body, good, "", now, stripeTolerance); err == nil {
		t.Error("verification passed with no secret configured: an unconfigured deployment must refuse, not wave through")
	}
}

// Rotating a webhook secret means both are live for a while, and Stripe sends a v1 for each. Any
// one matching is a pass, or a rotation is an outage.
func TestStripeSignatureAcceptsAnyOfSeveralV1s(t *testing.T) {
	body := []byte(`{"id":"evt_6","type":"invoice.paid"}`)
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testWhsec))
	mac.Write([]byte(ts + "." + string(body)))
	real := hex.EncodeToString(mac.Sum(nil))

	h := "t=" + ts + ",v1=" + strings.Repeat("ab", 32) + ",v1=" + real
	if err := verifyStripeSignature(body, h, testWhsec, now, stripeTolerance); err != nil {
		t.Fatalf("a header carrying an old and a new signature was refused: %v", err)
	}
}

// The form encoding is the other half that has no type system behind it. Checked against a real
// HTTP server because the bracket spelling is what Stripe parses, not what we meant.
func TestCheckoutFormEncoding(t *testing.T) {
	var got url.Values
	var idem, version, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got, idem = r.PostForm, r.Header.Get("Idempotency-Key")
		version, auth = r.Header.Get("Stripe-Version"), r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{"id": "cs_1", "url": "https://pay.example/cs_1", "mode": "payment"})
	}))
	defer srv.Close()
	c := &stripeClient{key: "sk_test_x", whsec: testWhsec, currency: "usd", base: srv.URL, client: srv.Client()}

	_, err := c.Checkout(context.Background(), CheckoutRequest{
		Mode: "payment", AmountMinor: 25000, Currency: "usd", ClientRef: "abc123",
		Metadata:   map[string]string{"attesttag_org": "abc123"},
		SuccessURL: "https://app.example/ok", CancelURL: "https://app.example/no", IdempotencyKey: "key-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"mode":                                         "payment",
		"line_items[0][price_data][unit_amount]":       "25000",
		"line_items[0][price_data][currency]":          "usd",
		"client_reference_id":                          "abc123",
		"metadata[attesttag_org]":                      "abc123",
		"payment_intent_data[metadata][attesttag_org]": "abc123",
	} {
		if got.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.Get(k), want)
		}
	}
	if idem != "key-1" || version != stripeAPIVersion || auth != "Bearer sk_test_x" {
		t.Errorf("headers: idempotency=%q version=%q auth=%q", idem, version, auth)
	}

	// A subscription carries its metadata on subscription_data instead — Stripe does not copy a
	// session's metadata onto the subscription, and without this a renewal is unattributable.
	_, err = c.Checkout(context.Background(), CheckoutRequest{
		Mode: "subscription", PriceID: "price_x", Quantity: 1, ClientRef: "abc123",
		Metadata:   map[string]string{"attesttag_org": "abc123"},
		SuccessURL: "https://app.example/ok", CancelURL: "https://app.example/no",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("line_items[0][price]") != "price_x" || got.Get("line_items[0][quantity]") != "1" {
		t.Errorf("subscription line item: %v", got)
	}
	if got.Get("subscription_data[metadata][attesttag_org]") != "abc123" {
		t.Error("the organisation is not on subscription_data[metadata]: renewal events will not name it")
	}
}

func TestCheckoutSurfacesStripesOwnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		w.Write([]byte(`{"error":{"message":"No such price: 'price_gone'","code":"resource_missing"}}`))
	}))
	defer srv.Close()
	c := &stripeClient{key: "sk_test_x", base: srv.URL, client: srv.Client()}
	_, err := c.Checkout(context.Background(), CheckoutRequest{Mode: "subscription", PriceID: "price_gone",
		SuccessURL: "https://x", CancelURL: "https://y"})
	if err == nil || !strings.Contains(err.Error(), "No such price") {
		t.Fatalf("Stripe's own message should be the one surfaced, got %v", err)
	}
}

func TestStripeModeReadsTheKeyPrefix(t *testing.T) {
	if stripeMode("sk_live_abc") != "live" || stripeMode("sk_test_abc") != "test" || stripeMode("") != "test" {
		t.Fatal("a live key must not be mistaken for a test one, and an absent key must not read as live")
	}
	if stripeMode("rk_live_abc") != "live" || stripeMode("rk_test_abc") != "test" {
		t.Fatal("a restricted key must read as the mode its prefix names")
	}
}

// customer and customer_creation are mutually exclusive at Stripe: sending both is a flat 400,
// "You may only specify one of these parameters: customer, customer_creation." An account only
// has a customer id after its first purchase, so sending the pair breaks top-ups for exactly the
// people who have paid before — the returning customer, never the first one.
func TestTopUpDoesNotPairCustomerWithCustomerCreation(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = r.PostForm
		json.NewEncoder(w).Encode(map[string]any{"id": "cs_1", "url": "https://pay.example/cs_1", "mode": "payment"})
	}))
	defer srv.Close()
	c := &stripeClient{key: "sk_test_x", whsec: testWhsec, currency: "usd", base: srv.URL, client: srv.Client()}
	base := CheckoutRequest{Mode: "payment", AmountMinor: 25000, Currency: "usd",
		SuccessURL: "https://app.example/ok", CancelURL: "https://app.example/no"}

	// A returning account: the customer travels, and Stripe must not also be asked to make one.
	returning := base
	returning.CustomerID, returning.CustomerEmail = "cus_existing", "her@example.com"
	if _, err := c.Checkout(context.Background(), returning); err != nil {
		t.Fatal(err)
	}
	if got.Get("customer") != "cus_existing" {
		t.Errorf("customer = %q, want cus_existing", got.Get("customer"))
	}
	if got.Has("customer_creation") {
		t.Error("customer_creation went with an existing customer: Stripe 400s the pair and the top-up never opens")
	}

	// A first purchase has nobody to send, so the customer Stripe makes has to be kept — else
	// the next top-up and the billing portal land on different customers.
	first := base
	first.CustomerEmail = "him@example.com"
	if _, err := c.Checkout(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if got.Get("customer_creation") != "always" {
		t.Error("a first top-up must keep its customer, or the account scatters across several")
	}
}

// stubSubscription is a Stripe that answers the three calls a size change makes: the read that
// finds the item, the write that moves it, and the read-back that confirms it. onPrice is the
// price it currently reports; applyWrite is whether the write actually lands, which is how a
// replayed idempotent request is imitated.
type stubSubscription struct {
	onPrice    string
	applyWrite bool
	invoice    map[string]any
	posted     url.Values
	idem       string
	posts      int
}

func (s *stubSubscription) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(map[string]any{"status": "active", "items": map[string]any{
				"data": []map[string]any{{"id": "si_1", "price": map[string]any{"id": s.onPrice}}}}})
			return
		}
		r.ParseForm()
		s.posted, s.idem, s.posts = r.PostForm, r.Header.Get("Idempotency-Key"), s.posts+1
		if s.applyWrite {
			s.onPrice = s.posted.Get("items[0][price]")
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "sub_1", "latest_invoice": s.invoice})
	}
}

// An upgrade is owed now, so it is invoiced and taken now.
func TestAnUpgradeIsInvoicedAndChargedOnTheSpot(t *testing.T) {
	stub := &stubSubscription{onPrice: "price_small", applyWrite: true,
		invoice: map[string]any{"amount_paid": 30000, "currency": "usd",
			"hosted_invoice_url": "https://pay.example/i/1", "status": "paid"}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	c := &stripeClient{key: "sk_test_x", currency: "usd", base: srv.URL, client: srv.Client()}

	up, err := c.ChangeSubscriptionPrice(context.Background(), PriceChange{
		SubscriptionID: "sub_1", PriceID: "price_mid", ChargeNow: true, IdempotencyKey: "size-key-1"})
	if err != nil {
		t.Fatal(err)
	}
	if stub.posted.Get("proration_behavior") != "always_invoice" {
		t.Errorf("an upgrade prorated as %q, want always_invoice", stub.posted.Get("proration_behavior"))
	}
	if stub.posted.Get("payment_behavior") != "error_if_incomplete" {
		t.Errorf("payment_behavior = %q: a card that cannot pay must leave the size where it was",
			stub.posted.Get("payment_behavior"))
	}
	if stub.posted.Get("items[0][id]") != "si_1" {
		t.Error("the item was not named, so Stripe would ADD a size rather than replace one")
	}
	if stub.idem != "size-key-1" {
		t.Errorf("idempotency key = %q: without it a double click buys the upgrade twice", stub.idem)
	}
	if !up.Charged || up.AmountMinor != 30000 || up.Currency != "usd" {
		t.Errorf("upgrade result = %+v, want a charge of 30000 usd", up)
	}
	if up.InvoiceURL != "https://pay.example/i/1" {
		t.Errorf("no invoice to show the customer: %q", up.InvoiceURL)
	}
	if up.Scheduled {
		t.Error("an upgrade was reported as waiting for the period to end")
	}
}

// An upgrade that lands on a renewal can invoice nothing at all. Charged has to follow the money
// rather than the intent, or the console tells somebody they paid when they did not.
func TestAnUpgradeThatCollectedNothingDoesNotClaimACharge(t *testing.T) {
	stub := &stubSubscription{onPrice: "price_small", applyWrite: true,
		invoice: map[string]any{"amount_paid": 0, "currency": "usd", "status": "paid"}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	c := &stripeClient{key: "sk_test_x", base: srv.URL, client: srv.Client()}
	res, err := c.ChangeSubscriptionPrice(context.Background(), PriceChange{
		SubscriptionID: "sub_1", PriceID: "price_mid", ChargeNow: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Charged {
		t.Error("an invoice that collected nothing was reported as a charge")
	}
}

// The failure a derived idempotency key buys: Stripe replays the first request's body, which
// describes a move that this time did not happen. Believing it would leave our record saying one
// size and Stripe billing another — so the subscription is read back, not taken on trust.
func TestAReplayedSizeChangeIsCaughtRatherThanRecorded(t *testing.T) {
	stub := &stubSubscription{onPrice: "price_small", applyWrite: false,
		invoice: map[string]any{"amount_paid": 30000, "currency": "usd", "status": "paid"}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	c := &stripeClient{key: "sk_test_x", base: srv.URL, client: srv.Client()}

	_, err := c.ChangeSubscriptionPrice(context.Background(), PriceChange{
		SubscriptionID: "sub_1", PriceID: "price_mid", ChargeNow: true, IdempotencyKey: "replayed"})
	if err == nil {
		t.Fatal("a subscription that never moved was reported as moved")
	}
	if !strings.Contains(err.Error(), "did not move") {
		t.Errorf("the error does not say what went wrong: %v", err)
	}
}

// A price that is already the one asked for is not an error and not a charge — a stale tab and a
// double click both land here.
func TestMovingToTheSizeAlreadyHeldChargesNothing(t *testing.T) {
	stub := &stubSubscription{onPrice: "price_mid", applyWrite: true}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	c := &stripeClient{key: "sk_test_x", base: srv.URL, client: srv.Client()}
	res, err := c.ChangeSubscriptionPrice(context.Background(), PriceChange{
		SubscriptionID: "sub_1", PriceID: "price_mid", ChargeNow: true})
	if err != nil || res.Charged || stub.posts != 0 {
		t.Fatalf("already on the size: res=%+v posts=%d err=%v, want no write at all", res, stub.posts, err)
	}
}

// Two refusals that need two different answers. A decline wants another card; a card asking for
// its owner cannot be answered by any API call at all, and telling somebody to retry it is
// telling them to press a button that will never work.
func TestACardAskingForItsOwnerIsNotJustAnotherRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, code       string
		status           int
		action, declined bool
	}{
		{"needs authentication", "subscription_payment_intent_requires_action", 402, true, false},
		{"declined", "card_declined", 402, false, true},
		{"something else", "resource_missing", 404, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"error":{"message":"nope","code":%q}}`, tc.code)
			}))
			defer srv.Close()
			c := &stripeClient{key: "sk_test_x", base: srv.URL, client: srv.Client()}
			err := c.CancelSubscription(context.Background(), "sub_1")
			if err == nil {
				t.Fatal("no error")
			}
			if needsCardAction(err) != tc.action || cardWasDeclined(err) != tc.declined {
				t.Errorf("%q classified as action=%v declined=%v, want %v/%v",
					tc.code, needsCardAction(err), cardWasDeclined(err), tc.action, tc.declined)
			}
			// Whatever the code, Stripe's own words still reach the customer.
			if !strings.Contains(err.Error(), "nope") {
				t.Errorf("Stripe's message was lost: %v", err)
			}
		})
	}
}

// ---- a downgrade waits for the boundary ----

// scheduleStub is a Stripe that knows about subscription schedules: creating one from a
// subscription, rewriting its phases, reading it back and releasing it.
type scheduleStub struct {
	subPrice     string
	scheduleID   string // attached schedule, empty when there is none
	pendingPrice string // what its second phase holds
	phaseStart   int64
	phaseEnd     int64
	subPosts     int
	phaseForm    url.Values
	createForm   url.Values
	released     int
	rollForward  bool // whether rewriting the phases actually takes
}

func (s *scheduleStub) schedJSON() map[string]any {
	phases := []map[string]any{{"start_date": s.phaseStart, "end_date": s.phaseEnd,
		"items": []map[string]any{{"price": s.subPrice, "quantity": 1}}}}
	if s.pendingPrice != "" {
		phases = append(phases, map[string]any{"start_date": s.phaseEnd,
			"items": []map[string]any{{"price": s.pendingPrice, "quantity": 1}}})
	}
	return map[string]any{"id": "sub_sched_1", "status": "active", "subscription": "sub_1",
		"current_phase": map[string]any{"start_date": s.phaseStart, "end_date": s.phaseEnd},
		"phases":        phases}
}

func (s *scheduleStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		r.ParseForm()
		switch {
		case r.URL.Path == "/v1/subscriptions/sub_1" && r.Method == "GET":
			enc.Encode(map[string]any{"id": "sub_1", "status": "active", "schedule": s.scheduleID,
				"metadata": map[string]any{"attesttag_org": "org_abc"},
				"items": map[string]any{"data": []map[string]any{
					{"id": "si_1", "quantity": 1, "price": map[string]any{"id": s.subPrice}}}}})
		case r.URL.Path == "/v1/subscriptions/sub_1":
			s.subPosts++
			s.subPrice = r.PostForm.Get("items[0][price]")
			enc.Encode(map[string]any{"id": "sub_1", "latest_invoice": map[string]any{
				"amount_paid": 30000, "currency": "usd", "status": "paid"}})
		case r.URL.Path == "/v1/subscription_schedules":
			s.createForm, s.scheduleID = r.PostForm, "sub_sched_1"
			enc.Encode(s.schedJSON())
		case strings.HasSuffix(r.URL.Path, "/release"):
			s.released++
			s.scheduleID, s.pendingPrice = "", ""
			enc.Encode(map[string]any{"id": "sub_sched_1", "status": "released"})
		case r.URL.Path == "/v1/subscription_schedules/sub_sched_1" && r.Method == "GET":
			enc.Encode(s.schedJSON())
		case r.URL.Path == "/v1/subscription_schedules/sub_sched_1":
			s.phaseForm = r.PostForm
			if s.rollForward {
				s.pendingPrice = r.PostForm.Get("phases[1][items][0][price]")
			}
			enc.Encode(s.schedJSON())
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 500)
		}
	}
}

func schedClient(t *testing.T, s *scheduleStub) (*stripeClient, func()) {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	return &stripeClient{key: "sk_test_x", currency: "usd", base: srv.URL, client: srv.Client()}, srv.Close
}

// The requirement in one test: they paid for this month, so they keep this month. The size drops
// at the boundary, nothing is charged, nothing is credited, and the subscription itself is not
// touched today.
func TestADowngradeWaitsForThePeriodToEndAndTakesNoMoney(t *testing.T) {
	stub := &scheduleStub{subPrice: "price_mid", phaseStart: 1790000000, phaseEnd: 1792612062, rollForward: true}
	c, done := schedClient(t, stub)
	defer done()

	res, err := c.ChangeSubscriptionPrice(context.Background(), PriceChange{
		SubscriptionID: "sub_1", PriceID: "price_small", ChargeNow: false, IdempotencyKey: "size-down"})
	if err != nil {
		t.Fatal(err)
	}
	if stub.subPosts != 0 {
		t.Error("the subscription itself was written: a downgrade must change nothing until the boundary")
	}
	if stub.createForm.Get("from_subscription") != "sub_1" {
		t.Errorf("the schedule was not built from the subscription: %v", stub.createForm)
	}
	if got := stub.phaseForm.Get("phases[1][items][0][price]"); got != "price_small" {
		t.Errorf("the phase after this one is on %q, want price_small", got)
	}
	if stub.phaseForm.Get("proration_behavior") != "none" {
		t.Errorf("proration_behavior = %q: nothing is owed either way on a downgrade",
			stub.phaseForm.Get("proration_behavior"))
	}
	if stub.phaseForm.Get("end_behavior") != "release" {
		t.Error("the schedule never releases, so it would own every later size change too")
	}
	if !res.Scheduled || res.Charged || res.AmountMinor != 0 {
		t.Errorf("result = %+v, want scheduled and no money", res)
	}
	if res.Pending.PriceID != "price_small" || res.Pending.AtUnix != 1792612062 {
		t.Errorf("pending = %+v, want price_small at the period end", res.Pending)
	}
}

// Phase 0 is the month already paid for. Rewriting it is how "you keep this size until the 5th"
// silently becomes a charge today, so it has to go back exactly as Stripe described it.
func TestAScheduledDowngradeLeavesTheMonthAlreadyPaidForAlone(t *testing.T) {
	stub := &scheduleStub{subPrice: "price_mid", phaseStart: 1790000000, phaseEnd: 1792612062, rollForward: true}
	c, done := schedClient(t, stub)
	defer done()
	if _, err := c.ChangeSubscriptionPrice(context.Background(), PriceChange{
		SubscriptionID: "sub_1", PriceID: "price_small"}); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"phases[0][start_date]":         "1790000000",
		"phases[0][end_date]":           "1792612062",
		"phases[0][items][0][price]":    "price_mid",
		"phases[0][items][0][quantity]": "1",
	} {
		if stub.phaseForm.Get(k) != want {
			t.Errorf("%s = %q, want %q — the month they bought was rewritten", k, stub.phaseForm.Get(k), want)
		}
	}
}

// Same paranoia as the upgrade, for the same reason: a replayed write answers with the first
// request's body. Believing it would tell somebody their size drops next month while nothing at
// Stripe intends to drop it, and they find out by being charged the larger fee.
func TestAScheduledDowngradeThatDidNotTakeIsRefusedRatherThanReported(t *testing.T) {
	stub := &scheduleStub{subPrice: "price_mid", phaseStart: 1790000000, phaseEnd: 1792612062, rollForward: false}
	c, done := schedClient(t, stub)
	defer done()
	_, err := c.ChangeSubscriptionPrice(context.Background(), PriceChange{
		SubscriptionID: "sub_1", PriceID: "price_small"})
	if err == nil {
		t.Fatal("a downgrade that was never recorded came back as scheduled")
	}
	if !strings.Contains(err.Error(), "did not record") {
		t.Errorf("the error does not say what went wrong: %v", err)
	}
}

// Changing your mind. Picking the size you are already on is the only way to call off a pending
// downgrade, so it cannot be answered from our side as a no-op.
func TestPickingTheSizeYouAreOnCallsOffAPendingDowngrade(t *testing.T) {
	stub := &scheduleStub{subPrice: "price_mid", scheduleID: "sub_sched_1", pendingPrice: "price_small",
		phaseStart: 1790000000, phaseEnd: 1792612062, rollForward: true}
	c, done := schedClient(t, stub)
	defer done()

	res, err := c.ChangeSubscriptionPrice(context.Background(), PriceChange{
		SubscriptionID: "sub_1", PriceID: "price_mid"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Released || stub.released != 1 {
		t.Errorf("the pending downgrade was not called off: released=%v calls=%d", res.Released, stub.released)
	}
	if res.Charged || res.Scheduled {
		t.Errorf("calling off a downgrade did something else as well: %+v", res)
	}
	// And with nothing pending, the same press is the no-op it looks like.
	res, err = c.ChangeSubscriptionPrice(context.Background(), PriceChange{
		SubscriptionID: "sub_1", PriceID: "price_mid"})
	if err != nil || res.Released || stub.released != 1 {
		t.Errorf("a second press released again: %+v %d %v", res, stub.released, err)
	}
}

// Upgrading with a downgrade pending. The schedule has to go first, or it would pull the price
// back down at the boundary and quietly undo the size that was just paid for.
func TestUpgradingCallsOffADowngradeThatWasWaiting(t *testing.T) {
	stub := &scheduleStub{subPrice: "price_small", scheduleID: "sub_sched_1", pendingPrice: "price_tiny",
		phaseStart: 1790000000, phaseEnd: 1792612062, rollForward: true}
	c, done := schedClient(t, stub)
	defer done()

	res, err := c.ChangeSubscriptionPrice(context.Background(), PriceChange{
		SubscriptionID: "sub_1", PriceID: "price_mid", ChargeNow: true})
	if err != nil {
		t.Fatal(err)
	}
	if stub.released != 1 {
		t.Error("the pending downgrade survived an upgrade: it would undo the new size at the boundary")
	}
	if !res.Released || !res.Charged {
		t.Errorf("result = %+v, want the upgrade charged and the pending change called off", res)
	}
}

func TestPendingPriceChangeReadsWhatIsWaitingAndWhen(t *testing.T) {
	stub := &scheduleStub{subPrice: "price_mid", scheduleID: "sub_sched_1", pendingPrice: "price_small",
		phaseStart: 1790000000, phaseEnd: 1792612062}
	c, done := schedClient(t, stub)
	defer done()

	p, err := c.PendingPriceChange(context.Background(), "sub_1")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Waiting() || p.PriceID != "price_small" || p.AtUnix != 1792612062 {
		t.Errorf("pending = %+v, want price_small at the period end", p)
	}

	// No schedule, nothing waiting — and no second call to find that out.
	stub.scheduleID, stub.pendingPrice = "", ""
	if p, err := c.PendingPriceChange(context.Background(), "sub_1"); err != nil || p.Waiting() {
		t.Errorf("pending = %+v err=%v, want nothing waiting", p, err)
	}
}

// A phase's metadata becomes the subscription's when that phase is entered, so phases that carry
// none would empty the subscription's at the boundary. Nothing breaks loudly — an event can
// still be traced through its customer — but the fallback that exists for Stripe's unordered
// delivery would vanish, months after the downgrade that did it.
func TestAScheduledDowngradeCarriesTheOrganisationOntoBothPhases(t *testing.T) {
	stub := &scheduleStub{subPrice: "price_mid", phaseStart: 1790000000, phaseEnd: 1792612062, rollForward: true}
	c, done := schedClient(t, stub)
	defer done()
	if _, err := c.ChangeSubscriptionPrice(context.Background(), PriceChange{
		SubscriptionID: "sub_1", PriceID: "price_small"}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"phases[0][metadata][attesttag_org]", "phases[1][metadata][attesttag_org]"} {
		if stub.phaseForm.Get(k) != "org_abc" {
			t.Errorf("%s = %q: the subscription loses its organisation at the boundary",
				k, stub.phaseForm.Get(k))
		}
	}
}
