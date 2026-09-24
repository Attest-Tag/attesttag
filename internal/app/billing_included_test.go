package app

// The monthly allowance a paid size carries: what it is worth, when it lapses, and the order a
// turn spends it in.
//
// The property under all of these is that an allowance is NOT a top-up. It belongs to the month
// it was billed for, it is spent before money the customer bought, and what is left of it on the
// last day of the period is gone rather than carried. The first version of this feature made it a
// credit_ledger movement, which got all three wrong at once.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// includedCfg sells the user ladder, with the allowance each size throws in.
func includedCfg() Config {
	cfg := billingCfg()
	cfg.StripeSizes = []Size{
		{Key: "upto_10", PriceID: "price_10", AmountMinor: 4900, IncludedMinor: 500, UserLimit: 10, Label: sizeLabels["upto_10"]},
		{Key: "upto_25", PriceID: "price_25", AmountMinor: 19900, IncludedMinor: 2000, UserLimit: 25, Label: sizeLabels["upto_25"]},
		{Key: "nothing_included", PriceID: "price_bare", AmountMinor: 9900, Label: "Bare"},
	}
	return cfg
}

// invoicePaid is a renewal as Stripe sends it: the invoice carries no metadata of ours, so the
// organisation is found through the customer already on the account, and the line names the price
// that was charged.
func invoicePaid(eventID, invoiceID, customer, price string) string {
	return fmt.Sprintf(`{"id":%q,"type":"invoice.paid","livemode":false,"data":{"object":{
		"id":%q,"object":"invoice","customer":%q,"subscription":"sub_inc","currency":"usd",
		"lines":{"data":[{"period":{"start":1790000000,"end":1792592000},"price":{"id":%q}}]}}}}`,
		eventID, invoiceID, customer, price)
}

func subscribedTo(t *testing.T, st *Store, orgID int64, customer, size string) {
	t.Helper()
	if err := st.PutSubscription(context.Background(), orgID, SubscriptionState{CustomerID: customer,
		SubscriptionID: "sub_inc", Status: "active", Size: size, Quantity: 1, Currency: "usd",
		PeriodEnd: time.Now().UTC().AddDate(0, 1, 0).Format(time.DateTime)}); err != nil {
		t.Fatalf("could not record the subscription: %v", err)
	}
}

func balanceOf(t *testing.T, st *Store, orgID int64) int64 {
	t.Helper()
	acct, err := st.BillingAccountOf(context.Background(), orgID)
	if err != nil {
		t.Fatalf("could not read the balance: %v", err)
	}
	return acct.CreditBalanceMicros
}

// allowanceOf is what the account may still spend of this period's included credit — zero once the
// period has passed, whatever the column says.
func allowanceOf(t *testing.T, st *Store, orgID int64) int64 {
	t.Helper()
	acct, err := st.BillingAccountOf(context.Background(), orgID)
	if err != nil {
		t.Fatalf("could not read the allowance: %v", err)
	}
	return acct.SpendableAllowanceMicros()
}

// ---- getting it ----

// The bug this feature was reported for. A size change fires customer.subscription.updated and
// produces no invoice at all — Stripe prorates onto the next one — so an allowance that hangs off
// invoice.paid leaves the customer looking at a plan promising $20 of credit beside a balance of
// $0.00, with nothing to do but wait a month.
func TestASizeChangeGrantsItsAllowanceWithNoInvoice(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	org := testOrg(t, st, "Upgraded Ltd")

	body := fmt.Sprintf(`{"id":"evt_sub_up","type":"customer.subscription.updated","livemode":false,"data":{"object":{
		"id":"sub_inc","object":"subscription","customer":"cus_up","status":"active",
		"current_period_start":1790000000,"current_period_end":1792592000,
		"metadata":{"attesttag_org":%q},
		"items":{"data":[{"quantity":1,"price":{"id":"price_25","currency":"usd","unit_amount":19900}}]}}}}`, org.PublicID)
	if w := postWebhook(t, b, body, true); w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}
	if got := allowanceOf(t, st, org.ID); got != usdToMicros(20) {
		t.Fatalf("a size change granted %s, want the $20.00 the size includes — no invoice is ever sent for one", creditAmount(got))
	}
}

// A renewal replaces the period's allowance; it does not add to it. The difference is the whole
// point: an account that underspends does not accumulate a year of unused allowance.
func TestARenewalReplacesTheAllowanceRatherThanAddingToIt(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	org := testOrg(t, st, "Renewing Ltd")
	subscribedTo(t, st, org.ID, "cus_inc", "upto_25")

	for i := 0; i < 3; i++ {
		if w := postWebhook(t, b, invoicePaid(fmt.Sprintf("evt_first_%d", i), "in_first", "cus_inc", "price_25"), true); w.Code != 200 {
			t.Fatalf("delivery %d answered %d: %s", i, w.Code, w.Body)
		}
	}
	if got := allowanceOf(t, st, org.ID); got != usdToMicros(20) {
		t.Fatalf("three deliveries of one invoice left %s of allowance, want $20.00", creditAmount(got))
	}

	// A second invoice with a different period. Still $20 — a month's allowance, not two.
	next := fmt.Sprintf(`{"id":"evt_second","type":"invoice.paid","livemode":false,"data":{"object":{
		"id":"in_second","object":"invoice","customer":"cus_inc","subscription":"sub_inc","currency":"usd",
		"lines":{"data":[{"period":{"start":1792592000,"end":1795184000},"price":{"id":"price_25"}}]}}}}`)
	if w := postWebhook(t, b, next, true); w.Code != 200 {
		t.Fatalf("the renewal answered %d: %s", w.Code, w.Body)
	}
	if got := allowanceOf(t, st, org.ID); got != usdToMicros(20) {
		t.Fatalf("after a renewal the allowance is %s, want $20.00 — it replaces, it does not accumulate", creditAmount(got))
	}
	assertLedgerMatchesBalance(t, st, org.ID)
}

// A size that includes nothing is the pre-existing spelling of STRIPE_SIZES and has to keep
// meaning what it meant: the fee buys the plan, and credit is bought separately.
func TestASizeThatIncludesNothingGrantsNothing(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	org := testOrg(t, st, "Bare Ltd")
	subscribedTo(t, st, org.ID, "cus_bare", "nothing_included")

	if w := postWebhook(t, b, invoicePaid("evt_bare", "in_bare", "cus_bare", "price_bare"), true); w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}
	if got := allowanceOf(t, st, org.ID); got != 0 {
		t.Fatalf("a size including nothing granted %s", creditAmount(got))
	}
	acct, _ := st.BillingAccountOf(context.Background(), org.ID)
	if acct.Metered() {
		t.Error("a size that includes nothing metered the account; there is nothing to meter it against")
	}
}

// The fee is not the allowance. $199 buys the plan and hands over $20 — crediting the fee would
// hand every subscriber their money straight back as model spend.
func TestTheFeeItselfIsNeverCredited(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	org := testOrg(t, st, "Fee Ltd")
	subscribedTo(t, st, org.ID, "cus_fee2", "upto_25")

	postWebhook(t, b, invoicePaid("evt_fee2", "in_fee2", "cus_fee2", "price_25"), true)
	if got := allowanceOf(t, st, org.ID); got != usdToMicros(20) {
		t.Fatalf("the allowance is %s; the $199 fee must not reach it", creditAmount(got))
	}
	if got := balanceOf(t, st, org.ID); got != 0 {
		t.Fatalf("prepaid credit moved by %s; an allowance is not a top-up", creditAmount(got))
	}
}

// Nothing about the allowance may reach credit_ledger. It is part of a fee rather than a payment,
// and a statement listing it would be telling the customer they had bought something they had not.
func TestTheAllowanceNeverTouchesTheLedger(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Clean Ltd")
	subscribedTo(t, st, org.ID, "cus_clean", "upto_25")
	postWebhook(t, b, invoicePaid("evt_clean", "in_clean", "cus_clean", "price_25"), true)

	rows, err := st.CreditLedger(ctx, org.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("the allowance wrote %d ledger row(s); it is not money that was paid", len(rows))
	}
	assertLedgerMatchesBalance(t, st, org.ID)
}

// ---- spending it ----

// The order, and the reason for it: the allowance expires and prepaid credit does not, so an
// account that spends its own balance first would watch an allowance it had already paid for
// inside the fee evaporate beside it.
func TestSpendDrawsTheAllowanceBeforePrepaidCredit(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Ordered Ltd")
	subscribedTo(t, st, org.ID, "cus_ord", "upto_25")
	postWebhook(t, b, invoicePaid("evt_ord", "in_ord", "cus_ord", "price_25"), true)
	topUp(t, st, org.ID, 100, "pi_ord")

	// $8 of a $20 allowance.
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 8})
	if got := allowanceOf(t, st, org.ID); got != usdToMicros(12) {
		t.Fatalf("allowance is %s after $8 of spend, want $12.00", creditAmount(got))
	}
	if got := balanceOf(t, st, org.ID); got != usdToMicros(100) {
		t.Fatalf("prepaid credit moved to %s while the allowance still had room", creditAmount(got))
	}

	// $20 more: the remaining $12 of allowance, then $8 off the balance.
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 20})
	if got := allowanceOf(t, st, org.ID); got != 0 {
		t.Fatalf("allowance is %s, want it spent", creditAmount(got))
	}
	if got := balanceOf(t, st, org.ID); got != usdToMicros(92) {
		t.Fatalf("prepaid credit is %s, want $92.00 — only the $8 the allowance could not cover", creditAmount(got))
	}
	// The invariant the whole ledger rests on: only the part that came out of prepaid credit is
	// counted as a debit, so the balance is still the ledger minus what is unrolled.
	assertLedgerMatchesBalance(t, st, org.ID)
}

// An allowance whose period has passed is not spendable, whatever the column still says, and
// whether or not the hourly sweep has run.
func TestALapsedAllowanceIsNotSpendable(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Lapsed Ltd")
	subscribedTo(t, st, org.ID, "cus_lapsed", "upto_25")
	postWebhook(t, b, invoicePaid("evt_lapsed", "in_lapsed", "cus_lapsed", "price_25"), true)
	topUp(t, st, org.ID, 50, "pi_lapsed")

	// Move the period into the past without touching anything else.
	if _, err := st.db.ExecContext(ctx,
		`update billing_accounts set allowance_period_end=? where org_id=?`,
		time.Now().UTC().AddDate(0, 0, -1).Format(time.DateTime), org.ID); err != nil {
		t.Fatal(err)
	}
	if got := allowanceOf(t, st, org.ID); got != 0 {
		t.Fatalf("a lapsed allowance still reads as %s of spendable credit", creditAmount(got))
	}

	// And a turn takes its cost from prepaid credit rather than from the expired allowance.
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 5})
	if got := balanceOf(t, st, org.ID); got != usdToMicros(45) {
		t.Fatalf("balance is %s, want $45.00 — an expired allowance must not pay for a turn", creditAmount(got))
	}
	assertLedgerMatchesBalance(t, st, org.ID)
}

// ---- moving size mid-period ----

func TestAMidPeriodUpgradeTopsTheAllowanceUp(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Growing Ltd")
	subscribedTo(t, st, org.ID, "cus_grow", "upto_10")
	b.syncAllowance(ctx, org.ID)
	if got := allowanceOf(t, st, org.ID); got != usdToMicros(5) {
		t.Fatalf("started at %s, want $5.00", creditAmount(got))
	}
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 2})

	// Same period, bigger size: $3 left plus the $15 difference.
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_grow", SubscriptionID: "sub_inc",
		Status: "active", Size: "upto_25", Quantity: 1, Currency: "usd", PeriodEnd: acct.PeriodEnd})
	b.syncAllowance(ctx, org.ID)

	if got := allowanceOf(t, st, org.ID); got != usdToMicros(18) {
		t.Fatalf("after upgrading mid-period the allowance is %s, want $18.00 — $3 left plus the $15 difference", creditAmount(got))
	}
}

// A downgrade must not take back what the customer was already told they had this month. The next
// period resets to the new size exactly, which is where the smaller figure takes effect.
func TestAMidPeriodDowngradeDoesNotClawBack(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Shrinking Ltd")
	subscribedTo(t, st, org.ID, "cus_shrink", "upto_25")
	b.syncAllowance(ctx, org.ID)

	acct, _ := st.BillingAccountOf(ctx, org.ID)
	st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_shrink", SubscriptionID: "sub_inc",
		Status: "active", Size: "upto_10", Quantity: 1, Currency: "usd", PeriodEnd: acct.PeriodEnd})
	b.syncAllowance(ctx, org.ID)

	if got := allowanceOf(t, st, org.ID); got != usdToMicros(20) {
		t.Fatalf("a downgrade left %s, want the $20.00 they already had this month", creditAmount(got))
	}
}

// ---- being metered by it ----

// The decision that makes the allowance mean anything: an account with one is stopped when it runs
// out, exactly as a paying one is, even though it has never bought credit.
func TestAnAllowanceMetersAnAccountThatNeverBoughtCredit(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Allowance Only Ltd")
	subscribedTo(t, st, org.ID, "cus_only", "upto_25")
	postWebhook(t, b, invoicePaid("evt_only", "in_only", "cus_only", "price_25"), true)

	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.CreditEnforced {
		t.Error("an allowance set credit_enforced; that flag never goes back off and would brick the account when the plan ends")
	}
	if !acct.Metered() {
		t.Fatal("an account with a live allowance is not metered, so nothing would ever hold it to the figure on its plan")
	}

	// Spend the allowance and then the whole overdraft.
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 20 + microsToUSD(maxOverdraftMicros) + 1})
	spendable, metered, err := st.SpendableCredit(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !metered || spendable > -maxOverdraftMicros {
		t.Fatalf("spendable %s, metered %v — past the overdraft this account must be refused", creditAmount(spendable), metered)
	}
}

// ...and the other half of that decision: when the plan ends, the metering ends with it rather
// than leaving the account stopped at a zero balance it never agreed to.
func TestALapsedPlanStopsMeteringRatherThanBricking(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Lapsed Plan Ltd")
	subscribedTo(t, st, org.ID, "cus_end", "upto_25")
	postWebhook(t, b, invoicePaid("evt_end", "in_end", "cus_end", "price_25"), true)

	// The subscription ends. syncSubscription runs on the deletion like any other event.
	body := fmt.Sprintf(`{"id":"evt_gone","type":"customer.subscription.deleted","livemode":false,"data":{"object":{
		"id":"sub_inc","object":"subscription","customer":"cus_end","status":"canceled",
		"metadata":{"attesttag_org":%q},
		"items":{"data":[{"quantity":1,"price":{"id":"price_25","currency":"usd","unit_amount":19900}}]}}}}`, org.PublicID)
	if w := postWebhook(t, b, body, true); w.Code != 200 {
		t.Fatalf("answered %d: %s", w.Code, w.Body)
	}

	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.Metered() {
		t.Fatal("a cancelled plan left the account metered against an allowance it no longer has")
	}
	if acct.SpendableAllowanceMicros() != 0 {
		t.Errorf("a cancelled plan left %s of allowance", creditAmount(acct.SpendableAllowanceMicros()))
	}
}

// The sweep is housekeeping, not policy — but a row nobody has swept must not read as credit.
func TestExpiringAllowancesIsHousekeepingAndNotThePolicy(t *testing.T) {
	_, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Sweep Ltd")
	subscribedTo(t, st, org.ID, "cus_sweep", "upto_25")
	st.SyncAllowance(ctx, org.ID, "upto_25",
		time.Now().UTC().AddDate(0, 0, -1).Format(time.DateTime), usdToMicros(20))

	// Before the sweep: stored, but not spendable.
	if got := allowanceOf(t, st, org.ID); got != 0 {
		t.Fatalf("a lapsed allowance read as %s before the sweep ran", creditAmount(got))
	}
	st.ExpireAllowances(ctx)
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if acct.AllowanceMicros != 0 || acct.AllowancePeriodEnd != "" {
		t.Fatalf("after the sweep the row still holds %s expiring %q", creditAmount(acct.AllowanceMicros), acct.AllowancePeriodEnd)
	}
}

func TestParseSizesReadsTheIncludedAllowance(t *testing.T) {
	sizes := parseSizes("upto_10=price_a:2500:500,upto_25=price_b:19900,broken=price_c:9900:notanumber")
	if len(sizes) != 3 {
		t.Fatalf("parsed %d sizes, want 3", len(sizes))
	}
	if sizes[0].AmountMinor != 2500 || sizes[0].IncludedMinor != 500 {
		t.Errorf("three-field entry read as %d/%d, want 2500/500", sizes[0].AmountMinor, sizes[0].IncludedMinor)
	}
	// The spelling every deployment already has: a fee and no allowance.
	if sizes[1].AmountMinor != 19900 || sizes[1].IncludedMinor != 0 {
		t.Errorf("two-field entry read as %d/%d, want 19900/0", sizes[1].AmountMinor, sizes[1].IncludedMinor)
	}
	// A typo in the allowance must not take the size off sale.
	if sizes[2].AmountMinor != 9900 || sizes[2].IncludedMinor != 0 {
		t.Errorf("entry with a bad allowance read as %d/%d, want 9900/0", sizes[2].AmountMinor, sizes[2].IncludedMinor)
	}
	if sizes[0].Label != sizeLabels["upto_10"] {
		t.Errorf("label is %q, want the one from sizeLabels", sizes[0].Label)
	}
}

// An unpaid renewal hands over nothing. Stripe moves a past_due subscription's period on while its
// dunning retries the card; reading that as a renewal granted the new month's allowance to an
// account that had not paid for it. The next allowance arrives with invoice.paid.
func TestAnUnpaidRenewalGrantsNoNewAllowance(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Dunning Ltd")
	sub := func(id, status string, start, end time.Time) string {
		return fmt.Sprintf(`{"id":%q,"type":"customer.subscription.updated","livemode":false,"data":{"object":{
			"id":"sub_D","object":"subscription","customer":"cus_D","status":%q,"metadata":{"attesttag_org":%q},
			"items":{"data":[{"quantity":1,"price":{"id":"price_25","currency":"usd","unit_amount":19900},
			"current_period_start":%d,"current_period_end":%d}]}}}}`, id, status, org.PublicID, start.Unix(), end.Unix())
	}
	now := time.Now()
	postWebhook(t, b, sub("evt_paid", "active", now.Add(-29*24*time.Hour), now.Add(time.Hour)), true)
	st.LogUsage(ctx, org.ID, "T1", "C1", "", "m", Usage{In: 1, Out: 1, CostUSD: 20})
	if got := allowanceOf(t, st, org.ID); got != 0 {
		t.Fatalf("spending the month's $20 left %s", creditAmount(got))
	}
	postWebhook(t, b, sub("evt_unpaid", "past_due", now.Add(time.Hour), now.Add(31*24*time.Hour)), true)
	if got := allowanceOf(t, st, org.ID); got != 0 {
		t.Errorf("an unpaid renewal handed over %s of allowance", creditAmount(got))
	}
}

// A move to a bigger size is charged at Stripe for the part of the period that is left, so it
// brings that part of the difference. Handing over all of it let an account upgrade on the last
// day, pay a thirtieth of the fee and schedule the downgrade back for nothing. The hourly sync must
// not then keep topping the share up towards the full figure.
func TestALateUpgradeBringsTheShareOfItsAllowanceThatIsLeft(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Last Minute Ltd")
	start := time.Now().UTC().Add(-27 * 24 * time.Hour) // nine-tenths of a thirty-day period gone
	end := start.Add(30 * 24 * time.Hour)
	on := func(size string) {
		t.Helper()
		if err := st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_late", SubscriptionID: "sub_late",
			Status: "active", Size: size, Quantity: 1, Currency: "usd",
			PeriodStart: start.Format(time.DateTime), PeriodEnd: end.Format(time.DateTime)}); err != nil {
			t.Fatal(err)
		}
		b.syncAllowance(ctx, org.ID)
	}
	on("upto_10")
	on("upto_25")
	got := allowanceOf(t, st, org.ID)
	want := usdToMicros(5 + 15*0.1) // the $5 it had, and a tenth of the $15 difference
	if d := got - want; d < -usdToMicros(0.05) || d > usdToMicros(0.05) {
		t.Errorf("a late upgrade brought the allowance to %s, want about %s", creditAmount(got), creditAmount(want))
	}
	b.syncAllowance(ctx, org.ID)
	if again := allowanceOf(t, st, org.ID); again != got {
		t.Errorf("a later sync in the same period moved the allowance from %s to %s", creditAmount(got), creditAmount(again))
	}
}
