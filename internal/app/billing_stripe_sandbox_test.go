package app

// A verification against real Stripe, in test mode, on a test clock.
//
// Every other test of the size-change path answers "does this code do what I think Stripe wants".
// Only this one answers "does Stripe want that". It is skipped unless STRIPE_SANDBOX=1, because it
// creates objects at Stripe and takes a couple of minutes.
//
//	STRIPE_SANDBOX=1 go test ./internal/app/ -run Sandbox -v -timeout 15m
//
// The test clock is the point. Everything before the period boundary is a schedule sitting there
// looking correct; the only evidence a scheduled downgrade works is a period actually rolling
// over, and the only evidence our record follows is customer.subscription.updated arriving with
// the new price. A clock answers both in minutes rather than a month.

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sandboxEnv reads .env.testing itself rather than being handed values on a command line, so the
// secret key never appears in a shell history or a process list.
func sandboxEnv(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("../../.env.testing")
	if err != nil {
		t.Skipf("no .env.testing to read: %v", err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out
}

func sandboxClient(t *testing.T) (*stripeClient, []Size) {
	t.Helper()
	if os.Getenv("STRIPE_SANDBOX") != "1" {
		t.Skip("set STRIPE_SANDBOX=1 to run the real-Stripe check")
	}
	env := sandboxEnv(t)
	key := env["STRIPE_SECRET_KEY"]
	// The one check that must never be skipped. Everything below creates and cancels
	// subscriptions and advances clocks; against a live key that is somebody's real money.
	if !strings.HasPrefix(key, "sk_test_") {
		t.Fatal("refusing to run: STRIPE_SECRET_KEY in .env.testing is not a test key")
	}
	sizes := parseSizes(env["STRIPE_SIZES"])
	// Only what is for sale: the "Talk to us" rung is listed with no Price and cannot be
	// subscribed to, and every test here moves a subscription between real Prices.
	sizes = slices.DeleteFunc(sizes, func(s Size) bool { return s.PriceID == "" })
	if len(sizes) < 2 {
		t.Fatalf("need two sizes to move between, got %d", len(sizes))
	}
	return &stripeClient{key: key, currency: "usd", base: "https://api.stripe.com",
		client: &http.Client{Timeout: 30 * time.Second}}, sizes
}

// sandboxRig is a customer on a test clock with a working card and a live subscription.
type sandboxRig struct {
	c         *stripeClient
	clock     string
	customer  string
	sub       string
	periodEnd int64
	org       string
}

func newSandboxRig(t *testing.T, c *stripeClient, priceID string) *sandboxRig {
	t.Helper()
	ctx := context.Background()
	// Deliberately NOT a 32-hex public id, and deliberately self-describing. orgForStripe reads
	// metadata BEFORE it checks that the customer belongs to the organisation, so a real id here
	// would let these events be applied to that account for real — and since adcb3df a
	// subscription event also rewrites the org's monthly budget. A value that cannot resolve is
	// the whole safety margin.
	r := &sandboxRig{c: c, org: "NOT-A-REAL-ORG-sandbox-check"}

	// Frozen a little in the past, so the first period has somewhere to run to.
	var clock struct {
		ID string `json:"id"`
	}
	f := url.Values{}
	f.Set("frozen_time", strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10))
	f.Set("name", "attest_tag size-change check")
	must(t, c.post(ctx, "/v1/test_helpers/test_clocks", f, "", &clock), "create test clock")
	r.clock = clock.ID
	t.Cleanup(func() {
		// Deleting the clock takes its customers and subscriptions with it, so this is the whole
		// cleanup even when the test fails half way through.
		req, err := http.NewRequest("DELETE", c.base+"/v1/test_helpers/test_clocks/"+r.clock, nil)
		if err != nil {
			t.Logf("could not build the cleanup request for clock %s: %v", r.clock, err)
			return
		}
		c.auth(req)
		resp, err := c.client.Do(req)
		if err != nil {
			t.Logf("clock %s was left behind at Stripe: %v", r.clock, err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Logf("clock %s was left behind at Stripe: HTTP %d", r.clock, resp.StatusCode)
		}
	})

	var cust struct {
		ID string `json:"id"`
	}
	f = url.Values{}
	f.Set("test_clock", r.clock)
	f.Set("email", "size-change-check@example.com")
	must(t, c.post(ctx, "/v1/customers", f, "", &cust), "create customer")
	r.customer = cust.ID

	// A card that always works, so error_if_incomplete has something to succeed against.
	// Attaching pm_card_visa CLONES it onto the customer under a new id, so the default has to be
	// the id that comes back and not the one that was asked for.
	var card struct {
		ID string `json:"id"`
	}
	must(t, c.post(ctx, "/v1/payment_methods/pm_card_visa/attach",
		url.Values{"customer": {r.customer}}, "", &card), "attach card")
	must(t, c.post(ctx, "/v1/customers/"+r.customer,
		url.Values{"invoice_settings[default_payment_method]": {card.ID}}, "", nil), "default card")

	var sub struct {
		ID               string `json:"id"`
		Status           string `json:"status"`
		CurrentPeriodEnd int64  `json:"current_period_end"`
	}
	f = url.Values{}
	f.Set("customer", r.customer)
	f.Set("items[0][price]", priceID)
	f.Set("payment_behavior", "error_if_incomplete")
	f.Set("metadata["+metaOrg+"]", r.org)
	must(t, c.post(ctx, "/v1/subscriptions", f, "", &sub), "create subscription")
	if sub.Status != "active" {
		t.Fatalf("the subscription came up %q, not active", sub.Status)
	}
	r.sub, r.periodEnd = sub.ID, sub.CurrentPeriodEnd
	return r
}

// advanceTo moves the clock and waits for Stripe to finish reacting to it.
func (r *sandboxRig) advanceTo(t *testing.T, at int64) {
	t.Helper()
	ctx := context.Background()
	must(t, r.c.post(ctx, "/v1/test_helpers/test_clocks/"+r.clock+"/advance",
		url.Values{"frozen_time": {strconv.FormatInt(at, 10)}}, "", nil), "advance clock")
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var clock struct {
			Status string `json:"status"`
		}
		must(t, r.c.get(ctx, "/v1/test_helpers/test_clocks/"+r.clock, &clock), "read clock")
		if clock.Status == "ready" {
			return
		}
		if clock.Status == "internal_failure" {
			t.Fatal("the test clock failed internally")
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatal("the test clock never became ready")
}

func (r *sandboxRig) subscriptionNow(t *testing.T) (priceID string, metaOrgValue string, schedule string) {
	t.Helper()
	var sub struct {
		Schedule string            `json:"schedule"`
		Metadata map[string]string `json:"metadata"`
		Items    struct {
			Data []struct {
				Price struct {
					ID string `json:"id"`
				} `json:"price"`
			} `json:"data"`
		} `json:"items"`
	}
	must(t, r.c.get(context.Background(), "/v1/subscriptions/"+r.sub, &sub), "read subscription")
	if len(sub.Items.Data) == 0 {
		t.Fatal("the subscription has no items")
	}
	return sub.Items.Data[0].Price.ID, sub.Metadata[metaOrg], sub.Schedule
}

func (r *sandboxRig) invoiceCount(t *testing.T) int {
	t.Helper()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	must(t, r.c.get(context.Background(), "/v1/invoices?limit=100&customer="+r.customer, &out), "list invoices")
	return len(out.Data)
}

func must(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// The whole downgrade story against real Stripe: scheduled, nothing taken, and the boundary
// actually rolling the subscription over.
func TestSandboxADowngradeIsScheduledAndRollsOverAtTheBoundary(t *testing.T) {
	c, sizes := sandboxClient(t)
	big, small := sizes[len(sizes)-1], sizes[0]
	rig := newSandboxRig(t, c, big.PriceID)
	ctx := context.Background()

	invoicesBefore := rig.invoiceCount(t)

	res, err := c.ChangeSubscriptionPrice(ctx, PriceChange{
		SubscriptionID: rig.sub, PriceID: small.PriceID, ChargeNow: false,
		IdempotencyKey: "sandbox-down-" + rig.sub})
	if err != nil {
		t.Fatalf("scheduling the downgrade: %v", err)
	}
	if !res.Scheduled || res.Charged || res.AmountMinor != 0 {
		t.Errorf("result = %+v, want scheduled and no money", res)
	}
	if res.Pending.PriceID != small.PriceID {
		t.Errorf("pending price = %q, want %q", res.Pending.PriceID, small.PriceID)
	}
	if res.Pending.AtUnix != rig.periodEnd {
		t.Errorf("pending at %d, want the period end %d", res.Pending.AtUnix, rig.periodEnd)
	}

	// Nothing may have been billed. This is the assertion the whole requirement rests on.
	if after := rig.invoiceCount(t); after != invoicesBefore {
		t.Errorf("scheduling a downgrade raised %d invoice(s); it must take nothing", after-invoicesBefore)
	}
	price, _, schedule := rig.subscriptionNow(t)
	if price != big.PriceID {
		t.Errorf("the subscription moved to %q today; they keep %q until the boundary", price, big.PriceID)
	}
	if schedule == "" {
		t.Fatal("no schedule is attached, so nothing will happen at the boundary")
	}

	// The two assumptions I could not test against a stub.
	var sched stripeSchedule
	must(t, c.get(ctx, "/v1/subscription_schedules/"+schedule, &sched), "read schedule")
	if len(sched.Phases) != 2 {
		t.Fatalf("the schedule has %d phases, want exactly 2", len(sched.Phases))
	}
	if got := sched.Phases[0].Items[0].Price; got != big.PriceID {
		t.Errorf("phase 0 is on %q, want the size they are paying for (%q)", got, big.PriceID)
	}
	if got := sched.Phases[1].Items[0].Price; got != small.PriceID {
		t.Errorf("phase 1 is on %q, want %q", got, small.PriceID)
	}
	if sched.Phases[0].EndDate != rig.periodEnd {
		t.Errorf("phase 0 ends at %d, want the real period end %d", sched.Phases[0].EndDate, rig.periodEnd)
	}

	if p, err := c.PendingPriceChange(ctx, rig.sub); err != nil || p.PriceID != small.PriceID || p.AtUnix != rig.periodEnd {
		t.Errorf("PendingPriceChange read back %+v (err %v), want %q at %d", p, err, small.PriceID, rig.periodEnd)
	}

	// The boundary. Everything above is a schedule that merely looks right.
	rig.advanceTo(t, rig.periodEnd+3600)

	price, org, _ := rig.subscriptionNow(t)
	if price != small.PriceID {
		t.Fatalf("after the boundary the subscription is on %q, want %q — the downgrade never happened",
			price, small.PriceID)
	}
	// The reason phase metadata is set at all: without it this comes back empty and every later
	// event loses the organisation it belongs to.
	if org != rig.org {
		t.Errorf("the organisation on the subscription is %q after rollover, want %q", org, rig.org)
	}

	// A phase boundary is exactly where the top-level period and the item's period can disagree,
	// and subscriptionPeriod() prefers the item. If they diverge, the allowance expiry is pinned
	// to the wrong date — a month out, silently, in the customer's favour, so nobody reports it.
	var after struct {
		CurrentPeriodStart int64 `json:"current_period_start"`
		CurrentPeriodEnd   int64 `json:"current_period_end"`
		Items              struct {
			Data []struct {
				CurrentPeriodStart int64 `json:"current_period_start"`
				CurrentPeriodEnd   int64 `json:"current_period_end"`
			} `json:"data"`
		} `json:"items"`
	}
	must(t, c.get(ctx, "/v1/subscriptions/"+rig.sub, &after), "read subscription period")
	t.Logf("after rollover: top-level period %d..%d, item period %d..%d",
		after.CurrentPeriodStart, after.CurrentPeriodEnd,
		after.Items.Data[0].CurrentPeriodStart, after.Items.Data[0].CurrentPeriodEnd)
	if it := after.Items.Data[0]; it.CurrentPeriodEnd != 0 && after.CurrentPeriodEnd != 0 &&
		it.CurrentPeriodEnd != after.CurrentPeriodEnd {
		t.Errorf("the item's period ends %d but the subscription's ends %d; subscriptionPeriod() prefers the item, so the allowance would expire on the wrong date",
			it.CurrentPeriodEnd, after.CurrentPeriodEnd)
	}
	if after.CurrentPeriodEnd != 0 && after.CurrentPeriodEnd <= rig.periodEnd {
		t.Errorf("the period did not move past the boundary: ends %d, boundary was %d",
			after.CurrentPeriodEnd, rig.periodEnd)
	}
}

// Upgrading while a downgrade waits: the schedule has to go, or it pulls the price back down at
// the boundary and undoes the band that was just paid for.
func TestSandboxUpgradingCallsOffAWaitingDowngrade(t *testing.T) {
	c, sizes := sandboxClient(t)
	mid, small := sizes[1], sizes[0]
	rig := newSandboxRig(t, c, mid.PriceID)
	ctx := context.Background()

	if _, err := c.ChangeSubscriptionPrice(ctx, PriceChange{
		SubscriptionID: rig.sub, PriceID: small.PriceID, ChargeNow: false,
		IdempotencyKey: "sandbox-down2-" + rig.sub}); err != nil {
		t.Fatalf("scheduling the downgrade: %v", err)
	}
	big := sizes[len(sizes)-1]
	res, err := c.ChangeSubscriptionPrice(ctx, PriceChange{
		SubscriptionID: rig.sub, PriceID: big.PriceID, ChargeNow: true,
		IdempotencyKey: "sandbox-up-" + rig.sub})
	if err != nil {
		t.Fatalf("upgrading over a pending downgrade: %v", err)
	}
	if !res.Released {
		t.Error("the pending downgrade was not reported as called off")
	}
	if !res.Charged || res.AmountMinor <= 0 {
		t.Errorf("the upgrade took nothing: %+v", res)
	}
	price, _, schedule := rig.subscriptionNow(t)
	if price != big.PriceID {
		t.Errorf("the subscription is on %q, want %q", price, big.PriceID)
	}
	if schedule != "" {
		t.Error("a schedule survived the upgrade; it would pull the price back down at the boundary")
	}
	if p, err := c.PendingPriceChange(ctx, rig.sub); err != nil || p.Waiting() {
		t.Errorf("something is still waiting after the upgrade: %+v (err %v)", p, err)
	}
}

// The enterprise half, and the only read-only check in this file: an enterprise deal's Price is
// read back from Stripe when the operator names it, and this is the one place the parser meets
// Stripe's real Price rather than a fake's. The ladder's own Prices stand in for one made for a
// customer — they are recurring, flat and monthly, which is exactly what a deal needs. Nothing is
// created, so there is nothing to clean up.
func TestSandboxAPriceReadsBackTheWayADealNeedsIt(t *testing.T) {
	c, sizes := sandboxClient(t)
	ctx := context.Background()
	for _, size := range sizes {
		p, err := c.Price(ctx, size.PriceID)
		if err != nil {
			t.Fatalf("reading %s: %v", size.PriceID, err)
		}
		if p.ID != size.PriceID || !p.Recurring || p.Interval != "month" || p.IntervalN != 1 || p.Tiered {
			t.Errorf("%s read back as %+v", size.PriceID, p)
		}
		if p.UnitAmount != size.AmountMinor {
			t.Errorf("%s charges %d at Stripe and STRIPE_SIZES says %d", size.PriceID, p.UnitAmount, size.AmountMinor)
		}
		if err := enterprisePriceOK(p, "usd"); err != nil {
			t.Errorf("%s would be refused for a deal: %v", size.PriceID, err)
		}
	}
	if _, err := c.Price(ctx, "price_does_not_exist_attesttag"); err == nil {
		t.Error("a Price that does not exist read back without an error")
	}
}
