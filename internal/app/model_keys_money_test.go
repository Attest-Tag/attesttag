package app

import (
	"context"
	"strings"
	"testing"
)

// Spend on an organisation's own key is its provider's to bill: recorded, counted towards its own
// budget, and charged to nothing here. The single debit point stays single — the check is inside
// LogUsageBy, where every one of its callers passes.
func TestOwnKeySpendNeverTouchesCredit(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org := testOrg(t, st, "Own Key Ltd")
	topUp(t, st, org.ID, 10, "pi_own_key")
	before, err := st.BillingAccountOf(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}

	st.LogUsageBy(ctx, org.ID, "T1", "C1", "1.0", "U1", "gpt-5-mini", Usage{In: 1000, Out: 100, CostUSD: 2.5, KeyOwner: keyOwnerOrg})
	after, _ := st.BillingAccountOf(ctx, org.ID)
	if after.CreditBalanceMicros != before.CreditBalanceMicros || after.LifetimeDebitMicros != before.LifetimeDebitMicros {
		t.Errorf("own-key spend moved credit: balance %d → %d, debited %d → %d",
			before.CreditBalanceMicros, after.CreditBalanceMicros, before.LifetimeDebitMicros, after.LifetimeDebitMicros)
	}
	var owner string
	var cost float64
	if err := st.db.QueryRowContext(ctx, `select key_owner, cost_usd from usage where org_id=? order by id desc limit 1`, org.ID).Scan(&owner, &cost); err != nil {
		t.Fatal(err)
	}
	if owner != keyOwnerOrg || cost != 2.5 {
		t.Errorf("usage row = %s $%v; want it recorded as the organisation's, at what it cost", owner, cost)
	}
	if spent, _ := st.MonthSpend(ctx, org.ID, "", ""); spent != 2.5 {
		t.Errorf("month spend = %v; own-key spend still counts towards the organisation's own budget", spent)
	}

	// The deployment's key is charged exactly as before.
	st.LogUsageBy(ctx, org.ID, "T1", "C1", "1.0", "U1", "z-ai/glm-5.3-flash", Usage{In: 1000, Out: 100, CostUSD: 1})
	if acct, _ := st.BillingAccountOf(ctx, org.ID); acct.CreditBalanceMicros != before.CreditBalanceMicros-usdToMicros(1) {
		t.Errorf("platform spend debited %d, want $1", before.CreditBalanceMicros-acct.CreditBalanceMicros)
	}
	if err := st.db.QueryRowContext(ctx, `select key_owner from usage where org_id=? order by id desc limit 1`, org.ID).Scan(&owner); err != nil || owner != keyOwnerPlatform {
		t.Errorf("platform usage row owner = %q %v", owner, err)
	}
	assertLedgerMatchesBalance(t, st, org.ID)
}

// A turn made of several rounds keeps the mark.
func TestAddKeepsTheKeyOwner(t *testing.T) {
	var total Usage
	total.add(Usage{In: 1, KeyOwner: keyOwnerOrg})
	total.add(Usage{In: 1})
	if total.KeyOwner != keyOwnerOrg {
		t.Errorf("owner after adding = %q", total.KeyOwner)
	}
}

// An organisation that brings its own key stops being held by the limits the deployment puts on
// its own: not the free plan's cap, not the per-organisation ceiling, not the credit floor. Its
// own monthly budget is the one limit left, measured in what its calls cost.
func TestAnOrgOnItsOwnKeyAnswersOnlyToItsOwnBudget(t *testing.T) {
	st := testStore(t)
	sealer := testSealer(t)
	ctx := context.Background()
	cfg := Config{PlatformMonthlyBudgetUSDPerOrg: 25, FreePlanBudgetUSD: 5, MonthlyBudgetUSD: 20,
		StripeSecretKey: "sk_test_x", StripeWebhookSecret: "whsec_x", OrgModelKeys: OrgModelKeysAll}
	org := testOrg(t, st, "Free With A Key Ltd") // the free plan
	topUp(t, st, org.ID, 1, "pi_small")
	st.LogUsageBy(ctx, org.ID, "", "", "", "", "z-ai/glm-5.3-flash", Usage{CostUSD: 60}) // past the overdraft
	sc := newSettingsCache(st, cfg)
	a := &Agent{cfg: cfg, store: st, settings: sc}

	if ok, why := a.budgetOK(ctx, org.ID, ""); ok || !strings.Contains(why, "credit is spent") {
		t.Fatalf("before the key: budgetOK = %v %q; the premise is an account stopped by credit", ok, why)
	}

	if err := st.PutModelKey(ctx, org.ID, ModelKeyRef{BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-5-mini"},
		"sk-proj-own-key-abcdef", "a@x", sealer); err != nil {
		t.Fatal(err)
	}
	sc.Invalidate(org.ID)
	s := sc.Get(ctx, org.ID)
	if s.PlatformBudgetUSD != 0 || s.MonthlyBudgetUSD != 0 || s.EffectiveBudget() != 0 {
		t.Errorf("on its own key: ceiling $%v, own budget $%v, enforced $%v; want none of the deployment's",
			s.PlatformBudgetUSD, s.MonthlyBudgetUSD, s.EffectiveBudget())
	}
	if ok, why := a.budgetOK(ctx, org.ID, ""); !ok {
		t.Errorf("on its own key it is still stopped: %q", why)
	}
	acct, _ := st.BillingAccountOf(ctx, org.ID)
	if got := pausedBy(s, acct.Metered(), acct.SpendableMicros(), 60); got != pausedNone {
		t.Errorf("pausedBy = %q; nothing stops an organisation on its own key with no budget", got)
	}
	if got := warnedBy(s, acct, cfg, 0, 0); got != warnNone {
		t.Errorf("warnedBy = %q; there is no credit for it to run out of", got)
	}

	// Its own budget, once it sets one, stops it — on what its own key spent, which is what the
	// budget measures while the key is in use: the $60 spent on the deployment's key earlier in
	// the month is not this key's.
	if err := st.PutSetting(ctx, org.ID, "monthly_budget_usd", "10"); err != nil {
		t.Fatal(err)
	}
	sc.Invalidate(org.ID)
	st.LogUsageBy(ctx, org.ID, "", "", "", "", "gpt-5-mini", Usage{CostUSD: 15, KeyOwner: keyOwnerOrg})
	if ok, why := a.budgetOK(ctx, org.ID, ""); ok || !strings.Contains(why, "monthly budget of $10.00") || !strings.Contains(why, "$15.00 spent") {
		t.Errorf("over its own budget: budgetOK = %v %q", ok, why)
	}
	if spent, _ := budgetSpend(ctx, st, org.ID, sc.Get(ctx, org.ID)); spent != 15 {
		t.Errorf("budget spend on its own key = %v, want only what that key spent", spent)
	}

	// A key the plan does not allow lifts nothing: the organisation is blocked, not freed.
	blocked := newSettingsCache(st, Config{FreePlanBudgetUSD: 5, StripeSecretKey: "sk_test_x", StripeWebhookSecret: "whsec_x",
		OrgModelKeys: OrgModelKeysEnterprise}).Get(ctx, org.ID)
	if blocked.PlatformBudgetUSD != 5 {
		t.Errorf("a key the plan does not include lifted the free plan's cap to %v", blocked.PlatformBudgetUSD)
	}
}

// The footer says when the figure is an estimate.
func TestTheFooterMarksAnEstimate(t *testing.T) {
	st := testStore(t)
	a := &Agent{store: st, settings: newSettingsCache(st, Config{})}
	c := &Call{OrgID: 1}
	if got := a.footer(context.Background(), c, "gpt-5-mini", Usage{In: 1200, Out: 80, CostUSD: 0.0042, CostEstimated: true}); !strings.Contains(got, "~$") {
		t.Errorf("footer = %q; want the estimate marked", got)
	}
	if got := a.footer(context.Background(), c, "z-ai/glm-5.3-flash", Usage{In: 1200, Out: 80, CostUSD: 0.0042}); strings.Contains(got, "~$") {
		t.Errorf("footer = %q; a reported charge is not an estimate", got)
	}
}

// The deployment's ceilings protect the deployment's key, so what an organisation spent on its own
// key does not count towards them once it removes that key: it comes back to the included models
// with the month's allowance there untouched, not already over it.
func TestSpendOnAnOwnKeyDoesNotCountAgainstTheDeploymentsCeiling(t *testing.T) {
	st := testStore(t)
	sealer := testSealer(t)
	ctx := context.Background()
	cfg := Config{FreePlanBudgetUSD: 5, MonthlyBudgetUSD: 20, OrgModelKeys: OrgModelKeysAll}
	org := testOrg(t, st, "Came Back Ltd") // the free plan: a $5 ceiling on the deployment's key
	if err := st.PutModelKey(ctx, org.ID, ModelKeyRef{BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-5-mini"},
		"sk-proj-own-key-abcdef", "a@x", sealer); err != nil {
		t.Fatal(err)
	}
	st.LogUsageBy(ctx, org.ID, "", "", "", "", "gpt-5-mini", Usage{CostUSD: 12, KeyOwner: keyOwnerOrg})
	if _, err := st.DeleteModelKey(ctx, org.ID); err != nil {
		t.Fatal(err)
	}
	sc := newSettingsCache(st, cfg)
	a := &Agent{cfg: cfg, store: st, settings: sc}
	if ok, why := a.budgetOK(ctx, org.ID, ""); !ok {
		t.Errorf("$12 spent on its own key stops it on the deployment's: %q", why)
	}
	st.LogUsageBy(ctx, org.ID, "", "", "", "", "z-ai/glm-5.3-flash", Usage{CostUSD: 6})
	if ok, why := a.budgetOK(ctx, org.ID, ""); ok || !strings.Contains(why, "$5.00") {
		t.Errorf("$6 on the deployment's key under a $5 ceiling = %v %q", ok, why)
	}
}
