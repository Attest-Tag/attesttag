package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

func testOrg(t *testing.T, st *Store, name string) *Org {
	t.Helper()
	org, err := st.CreateOrg(context.Background(), name, 0)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return org
}

// topUp is the shape every money test starts from: an account that has paid for something.
func topUp(t *testing.T, st *Store, orgID int64, usd float64, key string) int64 {
	t.Helper()
	bal, err := st.MoveCredit(context.Background(), CreditMovement{OrgID: orgID, Kind: creditTopUp,
		Micros: usdToMicros(usd), ExternalID: key, Note: "test", Actor: "stripe", Enforce: true})
	if err != nil {
		t.Fatalf("top up: %v", err)
	}
	return bal
}

// ---- the ceiling ----

// The regression test for the trap this whole feature walks into: budgetCeiling hands a pro
// account the deployment's own $25 ceiling when no operator granted it one, which would stop a
// customer who has just paid for $500 of credit at $25 a month.
func TestCreditFundedProHasNoPlatformCeiling(t *testing.T) {
	cfg := Config{PlatformMonthlyBudgetUSDPerOrg: 25, FreePlanBudgetUSD: 5}
	if got := budgetCeiling(PlanPro, 0, cfg, true); got != 0 {
		t.Errorf("a credit-funded pro account is capped at $%.2f a month; credit is what should stop it", got)
	}
	if got := budgetCeiling(PlanPro, 0, cfg, false); got != 25 {
		t.Errorf("a pro account with no credit should stay under the operator's ceiling, got %v", got)
	}
	// A free account is not affected by credit funding at all, whatever the flag says.
	if got := budgetCeiling(PlanFree, 0, cfg, true); got != 5 {
		t.Errorf("free plan ceiling moved to %v", got)
	}
	// And a grant an operator made by hand still wins for an account with no credit.
	if got := budgetCeiling(PlanPro, 50, cfg, false); got != 50 {
		t.Errorf("operator grant ignored, got %v", got)
	}
}

// ---- the billing row ----

// Every billing row is born with an empty customer_id: ensureBillingRow does not know the Stripe
// customer, and on the credit path nothing ever writes one. The empty sentinel is therefore the
// common case rather than the rare one, and a unique index that counts it allows exactly one such
// row per deployment — the second organisation ever credited fails its INSERT, the webhook answers
// 500, and Stripe retries for three days and gives up with the card already charged.
//
// Three organisations rather than two, because the failure is "only the first one works" and two
// cannot tell that apart from one unlucky pair.
func TestSeveralOrgsCanBeCreditedWithNoStripeCustomer(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	want := usdToMicros(25)
	for i, name := range []string{"First Ltd", "Second Ltd", "Third Ltd"} {
		org := testOrg(t, st, name)
		if _, err := st.MoveCredit(ctx, CreditMovement{OrgID: org.ID, Kind: creditTopUp,
			Micros: want, ExternalID: fmt.Sprintf("pi_%d", i), Actor: "stripe", Enforce: true}); err != nil {
			t.Fatalf("%s is organisation %d to be credited and it failed: %v", name, i+1, err)
		}
		acct, err := st.BillingAccountOf(ctx, org.ID)
		if err != nil {
			t.Fatal(err)
		}
		if acct.CustomerID != "" {
			t.Fatalf("%s was given customer_id %q by the credit path; the premise of this test is gone", name, acct.CustomerID)
		}
		if acct.CreditBalanceMicros != want {
			t.Errorf("%s holds %d micros, want %d", name, acct.CreditBalanceMicros, want)
		}
	}
}

// The other half of the same index. PutSubscription calls ensureBillingRow before it writes the
// customer id, so an organisation whose first billing write is a subscription collides in exactly
// the same place as one whose first write is a top-up — it just needs somebody else to be holding
// the empty slot first.
func TestAnOrgCanSubscribeWhileAnotherHoldsNoStripeCustomer(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	topUp(t, st, testOrg(t, st, "Credited Ltd").ID, 25, "pi_holds_the_slot")

	subscriber := testOrg(t, st, "Subscriber Ltd")
	if err := st.PutSubscription(ctx, subscriber.ID, SubscriptionState{
		CustomerID: "cus_second", SubscriptionID: "sub_second", Status: "active", Size: "under_25",
	}); err != nil {
		t.Fatalf("a first subscription failed while another organisation held a row with no customer: %v", err)
	}
}

// And the guard against over-correcting: excluding the empty string from the index must not cost
// what the index is for. A real Stripe customer still belongs to at most one organisation, which
// is what stops BillingAccountByCustomer resolving one payer onto two accounts.
func TestOneStripeCustomerStillCannotBeOnTwoOrgs(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	a, b := testOrg(t, st, "Alpha Ltd"), testOrg(t, st, "Beta Ltd")
	if err := st.PutSubscription(ctx, a.ID, SubscriptionState{CustomerID: "cus_shared", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSubscription(ctx, b.ID, SubscriptionState{CustomerID: "cus_shared", Status: "active"}); err == nil {
		t.Fatal("two organisations hold cus_shared; a webhook for it now resolves to whichever row the index happens to return")
	}
}

// ---- the balance and the ledger ----

// The invariant the materialised balance rests on. Stated as exact equality because the money is
// integer micro-dollars: with floats this would be a tolerance, and a tolerance is a test that
// passes on a drift small enough to compound.
func assertLedgerMatchesBalance(t *testing.T, st *Store, orgID int64) {
	t.Helper()
	ctx := context.Background()
	acct, err := st.BillingAccountOf(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := st.LedgerSum(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	unrolled := acct.LifetimeDebitMicros - acct.DebitRolledMicros
	if got, want := acct.CreditBalanceMicros, sum-unrolled; got != want {
		t.Fatalf("balance %d != ledger %d minus unrolled spend %d (= %d)", got, sum, unrolled, want)
	}
}

func TestTheBalanceEqualsTheLedger(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Ledger Ltd")

	topUp(t, st, org.ID, 100, "pi_1")
	assertLedgerMatchesBalance(t, st, org.ID)

	// Spend, which moves the balance without writing a ledger row until the roll-up.
	for i := 0; i < 40; i++ {
		st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 10, Out: 10, CostUSD: 0.37})
	}
	assertLedgerMatchesBalance(t, st, org.ID)

	if _, err := st.RollUpDebit(ctx, org.ID, org.PublicID, time.Now()); err != nil {
		t.Fatal(err)
	}
	assertLedgerMatchesBalance(t, st, org.ID)
	// After a roll-up there is nothing outstanding, so the balance is the plain sum.
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	sum, _ := st.LedgerSum(ctx, org.ID)
	if acct.CreditBalanceMicros != sum {
		t.Fatalf("after a roll-up the balance (%d) should be the ledger sum (%d)", acct.CreditBalanceMicros, sum)
	}

	topUp(t, st, org.ID, 50, "pi_2")
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 10, Out: 10, CostUSD: 1.25})
	if _, err := st.MoveCredit(ctx, CreditMovement{OrgID: org.ID, Kind: creditRefund, Micros: -usdToMicros(25),
		ExternalID: "refund:ch_1:2500", Actor: "stripe"}); err != nil {
		t.Fatal(err)
	}
	assertLedgerMatchesBalance(t, st, org.ID)
}

// Randomised, because the failure this guards against is an ordering one: a balance updated
// outside the transaction that writes its ledger row drifts only under interleaving.
func TestTheBalanceEqualsTheLedgerUnderConcurrency(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Race Ltd")
	topUp(t, st, org.ID, 500, "pi_seed")

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				st.MoveCredit(ctx, CreditMovement{OrgID: org.ID, Kind: creditTopUp, Micros: usdToMicros(10),
					ExternalID: fmt.Sprintf("pi_%d", i), Actor: "stripe", Enforce: true})
			case 1:
				st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: rand.Float64()})
			default:
				st.MoveCredit(ctx, CreditMovement{OrgID: org.ID, Kind: creditAdjustment, Micros: -usdToMicros(1),
					ExternalID: fmt.Sprintf("adj_%d", i), Actor: "operator"})
			}
		}(i)
	}
	wg.Wait()
	assertLedgerMatchesBalance(t, st, org.ID)
}

// The property that makes a redelivered webhook safe, stated on the store rather than through
// HTTP: one external id is one ledger row and one balance move, however many times it arrives.
func TestOnePaymentIsOnlyEverCreditedOnce(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Once Ltd")

	first := topUp(t, st, org.ID, 100, "pi_same")
	for i := 0; i < 5; i++ {
		_, err := st.MoveCredit(ctx, CreditMovement{OrgID: org.ID, Kind: creditTopUp, Micros: usdToMicros(100),
			ExternalID: "pi_same", Actor: "stripe", Enforce: true})
		if !errors.Is(err, ErrAlreadyCredited) {
			t.Fatalf("a redelivery returned %v, want ErrAlreadyCredited", err)
		}
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.CreditBalanceMicros != first {
		t.Fatalf("balance moved on a redelivery: %d, want %d", acct.CreditBalanceMicros, first)
	}
	assertLedgerMatchesBalance(t, st, org.ID)
}

func TestConcurrentDeliveriesOfOneEventCreditOnce(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Thunder Ltd")

	var wg sync.WaitGroup
	var ok int
	var mu sync.Mutex
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.MoveCredit(ctx, CreditMovement{OrgID: org.ID, Kind: creditTopUp,
				Micros: usdToMicros(250), ExternalID: "pi_thunder", Actor: "stripe", Enforce: true})
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d of 16 concurrent deliveries credited; exactly one may", ok)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.CreditBalanceMicros != usdToMicros(250) {
		t.Fatalf("balance is %d, want one credit of 250", acct.CreditBalanceMicros)
	}
}

// A refund of money already spent leaves the balance below zero. Clamping it at zero would make
// a chargeback a way of getting free model spend, which is the one outcome worth a test of its own.
func TestARefundLeavesTheBalanceNegativeRatherThanClamped(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Chargeback Ltd")
	topUp(t, st, org.ID, 100, "pi_cb")
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 90})

	bal, err := st.MoveCredit(ctx, CreditMovement{OrgID: org.ID, Kind: creditRefund,
		Micros: -usdToMicros(100), ExternalID: "refund:ch_cb:10000", Actor: "stripe"})
	if err != nil {
		t.Fatal(err)
	}
	if bal >= 0 {
		t.Fatalf("balance is %d after refunding money already spent; it must go negative, not clamp", bal)
	}
	assertLedgerMatchesBalance(t, st, org.ID)
}

// A refund must not switch the credit floor on for an account that never had one.
func TestARefundDoesNotTurnTheCreditFloorOn(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Never Paid Ltd")
	if _, err := st.MoveCredit(ctx, CreditMovement{OrgID: org.ID, Kind: creditRefund,
		Micros: -usdToMicros(5), ExternalID: "refund:ch_x:500", Actor: "stripe"}); err != nil {
		t.Fatal(err)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.CreditEnforced {
		t.Fatal("a refund enabled the credit floor on an account that never bought credit")
	}
}

// ---- the day-one test ----

// Every organisation in production on the day this ships has no billing row. If absence read as
// "balance zero", all of them would stop on their first turn.
func TestCreditIsNotDebitedForAnOrgWithNoBillingAccount(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Free Ltd")

	for i := 0; i < 5; i++ {
		st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 10, Out: 10, CostUSD: 2.50})
	}
	acct, err := st.BillingAccountOf(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if acct.Exists {
		t.Fatal("spending created a billing row: a billing row means a billing relationship, and this account has none")
	}
	if spend, _ := st.MonthSpend(ctx, org.ID, "", ""); spend != 12.50 {
		t.Fatalf("spend was not recorded: %v", spend)
	}
	if facts := st.BillingFacts(ctx, org.ID); facts.Active || facts.CreditEnforced {
		t.Fatalf("an account with no billing row reads as %+v", facts)
	}
}

// A subscriber who has not topped up must not be refused at a balance of zero they never agreed
// to. The floor turns on with the first payment, and only then.
func TestASubscriptionAloneDoesNotTurnTheCreditFloorOn(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Subscribed Ltd")
	if err := st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_1",
		SubscriptionID: "sub_1", Status: "active", Size: "25_100", Quantity: 1}); err != nil {
		t.Fatal(err)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if !acct.Exists || !acct.Active() {
		t.Fatal("the subscription was not recorded")
	}
	if acct.CreditEnforced {
		t.Fatal("subscribing alone switched the credit floor on: the account would stop at zero having paid for a plan")
	}
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 5})
	acct, _ = st.BillingAccountOf(ctx, org.ID)
	if acct.CreditBalanceMicros != 0 || acct.LifetimeDebitMicros != 0 {
		t.Fatal("spend was debited from an account with no credit")
	}
}

// ---- cost at the boundary ----

// The second half of the NaN fix. validCost guards the budget; this guards the balance, because
// the worker's costs arrive over the wire from another container and never pass through it.
func TestANaNCostNeverReachesTheLedgerOrTheBudget(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "NaN Ltd")
	topUp(t, st, org.ID, 100, "pi_nan")

	nan := func() float64 { var z float64; return z / z }()
	inf := func() float64 { var z float64; return 1 / z }()
	for _, bad := range []float64{nan, inf, -inf, -5} {
		st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: bad})
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.CreditBalanceMicros != usdToMicros(100) {
		t.Fatalf("a cost that is not money moved the balance to %d", acct.CreditBalanceMicros)
	}
	spend, err := st.MonthSpend(ctx, org.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// The property that actually matters: whatever was stored, the budget comparison still works.
	// Against NaN every comparison is false, which is how a budget silently stops binding.
	if !(spend >= 0) {
		t.Fatalf("MonthSpend is %v, which no budget comparison can be true against", spend)
	}
	assertLedgerMatchesBalance(t, st, org.ID)
}

func TestUsdToMicrosRoundsRatherThanTruncates(t *testing.T) {
	for _, tc := range []struct {
		usd  float64
		want int64
	}{
		{1, 1_000_000},
		{0.0003, 300},
		{0.0000004, 0}, // below a micro-dollar; there is nowhere for it to go
		{0.0000006, 1}, // and rounding is to nearest, not toward zero
		{-1, 0},
		{0, 0},
	} {
		if got := usdToMicros(tc.usd); got != tc.want {
			t.Errorf("usdToMicros(%v) = %d, want %d", tc.usd, got, tc.want)
		}
	}
}

// ---- the roll-up ----

func TestRollUpWritesOneLineAnHourAndAdvancesTheWatermark(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Rollup Ltd")
	topUp(t, st, org.ID, 100, "pi_roll")

	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 3})
	at := time.Date(2026, 9, 17, 14, 30, 0, 0, time.UTC)
	written, err := st.RollUpDebit(ctx, org.ID, org.PublicID, at)
	if err != nil || written != usdToMicros(3) {
		t.Fatalf("first roll-up wrote %d (%v), want %d", written, err, usdToMicros(3))
	}
	// A retry inside the same hour writes nothing: the ledger key carries the hour.
	again, err := st.RollUpDebit(ctx, org.ID, org.PublicID, at.Add(20*time.Minute))
	if err != nil || again != 0 {
		t.Fatalf("a retry inside the hour wrote %d (%v)", again, err)
	}
	// And with nothing spent since, the next hour writes nothing either.
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 1.5})
	next, err := st.RollUpDebit(ctx, org.ID, org.PublicID, at.Add(time.Hour))
	if err != nil || next != usdToMicros(1.5) {
		t.Fatalf("the next hour wrote %d (%v), want %d", next, err, usdToMicros(1.5))
	}
	assertLedgerMatchesBalance(t, st, org.ID)

	ledger, _ := st.CreditLedger(ctx, org.ID, 50)
	debits := 0
	for _, e := range ledger {
		if e.Kind == creditDebit {
			debits++
		}
	}
	if debits != 2 {
		t.Fatalf("%d debit lines for two hours of spending; the statement is per hour, not per turn", debits)
	}
}

// ---- what retention may not touch ----

// usage is swept by data_retention_days, so spend can never be recomputed. That makes the ledger
// the only surviving record of what was drawn down, and a sweep that took it would be
// unrecoverable rather than inconvenient.
func TestRetentionSweepsUsageAndNeverTheLedger(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Retained Ltd")
	topUp(t, st, org.ID, 100, "pi_ret")
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 4})
	if _, err := st.RollUpDebit(ctx, org.ID, org.PublicID, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Backdate everything a year and sweep with the shortest policy the console allows.
	old := time.Now().AddDate(-1, 0, 0).UTC().Format(time.DateTime)
	for _, tbl := range []string{"usage", "credit_ledger"} {
		if _, err := st.db.ExecContext(ctx, `update `+tbl+` set created_at=? where org_id=?`, old, org.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.PurgeOrgData(ctx, org.ID, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}

	var usageRows int
	st.db.QueryRowContext(ctx, `select count(*) from usage where org_id=?`, org.ID).Scan(&usageRows)
	if usageRows != 0 {
		t.Fatalf("retention left %d usage rows; this test's premise is that it does not", usageRows)
	}
	ledger, _ := st.CreditLedger(ctx, org.ID, 50)
	if len(ledger) != 2 {
		t.Fatalf("retention took the statement: %d rows left, want the top-up and the debit", len(ledger))
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.CreditBalanceMicros != usdToMicros(96) {
		t.Fatalf("balance is %d after a sweep, want %d", acct.CreditBalanceMicros, usdToMicros(96))
	}
}

// ---- deletion ----

func TestDeletingAnOrgRemovesItsBillingRows(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Gone Ltd")
	topUp(t, st, org.ID, 100, "pi_gone")
	if _, err := st.DeleteOrg(ctx, org.ID); err != nil {
		t.Fatal(err)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Exists {
		t.Fatal("the billing row outlived the organisation")
	}
	ledger, _ := st.CreditLedger(ctx, org.ID, 50)
	if len(ledger) != 0 {
		t.Fatalf("%d ledger rows outlived the organisation", len(ledger))
	}
}

// ---- the floor, as the bot applies it ----

// creditAgent is spendAgent with a real organisation and a deployment that sells plans, because
// the credit floor is gated on both the configuration and a billing row.
func creditAgent(t *testing.T, cfg Config) (*Agent, *Store, *Org) {
	t.Helper()
	st := testStore(t)
	org := testOrg(t, st, "Gated Ltd")
	return &Agent{store: st, settings: newSettingsCache(st, cfg), cfg: cfg}, st, org
}

// The bound the terms name. A turn is gated before it runs and priced after, so some overshoot is
// arithmetic; this asserts where the arithmetic stops being allowed, and it cites the constant by
// name so that moving the constant is a deliberate act rather than a silent one.
func TestSpendStopsWithinTheDocumentedOverdraft(t *testing.T) {
	cfg := billingCfg()
	a, st, org := creditAgent(t, cfg)
	ctx := context.Background()
	topUp(t, st, org.ID, 25, "pi_floor")

	if ok, why := a.budgetOK(ctx, org.ID, ""); !ok {
		t.Fatalf("a funded account was refused: %q", why)
	}

	// Spend it all, and then some: this is the work that was already in flight.
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 25})
	a.settings.Invalidate(org.ID)
	if ok, _ := a.budgetOK(ctx, org.ID, ""); !ok {
		t.Fatal("an account at exactly zero was refused; the overdraft is what it may still spend")
	}

	// One dollar inside the bound is still running.
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: microsToUSD(maxOverdraftMicros) - 1})
	if ok, _ := a.budgetOK(ctx, org.ID, ""); !ok {
		t.Fatalf("refused $1 inside the documented $%.2f overdraft", microsToUSD(maxOverdraftMicros))
	}

	// And past it, it stops — naming credit, and saying where to fix it.
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 2})
	ok, why := a.budgetOK(ctx, org.ID, "")
	if ok {
		bal, _ := st.CreditBalance(ctx, org.ID)
		t.Fatalf("still spending at %s, past the $%.2f bound", creditAmount(bal), microsToUSD(maxOverdraftMicros))
	}
	if !strings.Contains(why, "credit") {
		t.Errorf("the refusal does not name credit, so somebody will go and raise a budget instead: %q", why)
	}
	if strings.Contains(why, "monthly budget") {
		t.Errorf("the refusal blames the monthly budget for something credit stopped: %q", why)
	}
}

// Reading the balance must fail closed, the way reading the spend already does: an accounting
// error is not permission to keep spending.
func TestTheCreditFloorFailsClosedWhenTheBalanceCannotBeRead(t *testing.T) {
	a, st, org := creditAgent(t, billingCfg())
	ctx := context.Background()
	topUp(t, st, org.ID, 100, "pi_gone")
	a.settings.Invalidate(org.ID)
	if _, err := st.db.ExecContext(ctx, `drop table billing_accounts`); err != nil {
		t.Fatal(err)
	}
	if ok, why := a.budgetOK(ctx, org.ID, ""); ok || why == "" {
		t.Fatalf("a balance that cannot be read was treated as plenty: %v %q", ok, why)
	}
}

// The day-one property, through the gate rather than the store: an account with no billing row
// behaves exactly as it did before this feature existed.
func TestTheCreditFloorDoesNotApplyToAnOrgWithNoBillingAccount(t *testing.T) {
	a, st, org := creditAgent(t, billingCfg())
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 100})
	}
	if ok, why := a.budgetOK(ctx, org.ID, ""); !ok {
		t.Fatalf("an account with no billing row was stopped by a credit floor it has no part in: %q", why)
	}
}

// The balance is read live, not off the settings cache. A cached balance would be a window,
// fifteen seconds wide, of spending money that is not there — at eight concurrent turns per
// container.
func TestTheBalanceIsNotServedFromTheSettingsCache(t *testing.T) {
	a, st, org := creditAgent(t, billingCfg())
	ctx := context.Background()
	topUp(t, st, org.ID, 100, "pi_cache")
	if ok, _ := a.budgetOK(ctx, org.ID, ""); !ok {
		t.Fatal("a funded account was refused")
	}
	// Drain it without touching the cache at all.
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 200})
	if ok, why := a.budgetOK(ctx, org.ID, ""); ok {
		t.Fatalf("the next turn was allowed on a cached balance: %q", why)
	}
}

// A fix job reserves its whole budget up front, because it runs in another container for minutes
// and is the one workload big enough to matter.
func TestAJobIsRefusedWhenTheCreditWouldNotCoverIt(t *testing.T) {
	cfg := billingCfg()
	st := testStore(t)
	org := testOrg(t, st, "Jobs Ltd")
	topUp(t, st, org.ID, 2, "pi_job")
	r := &JobRunner{store: st, settings: newSettingsCache(st, cfg)}
	err := r.budgetRoom(context.Background(), org.ID, "T1", "C1", 3)
	if err == nil {
		t.Fatal("a $3 job was started against $2 of credit")
	}
	if !strings.Contains(err.Error(), "credit") {
		t.Errorf("the refusal does not name credit: %v", err)
	}
	if err := r.budgetRoom(context.Background(), org.ID, "T1", "C1", 1); err != nil {
		t.Fatalf("a $1 job was refused against $2 of credit: %v", err)
	}
}
