package app

// The account's own monthly budget, kept in step with what its plan includes.
//
// This is the one rule here that can overwrite a figure a person typed, so what it does and does
// not overwrite is worth pinning precisely.

import (
	"context"
	"testing"
)

func budgetOf(t *testing.T, st *Store, orgID int64) string {
	t.Helper()
	return st.Setting(context.Background(), orgID, "monthly_budget_usd")
}

// The case from the screenshot: a plan including $250 a month, stopped at the $20 the deployment
// happens to default to. The customer is paying for credit the guard rail will not let them reach.
func TestAnAccountThatNeverSetABudgetFollowsItsPlan(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Default Ltd")
	if budgetOf(t, st, org.ID) != "" {
		t.Fatal("this test needs an organisation that has never set a budget")
	}
	subscribedTo(t, st, org.ID, "cus_def", "upto_25")
	b.syncAllowance(ctx, org.ID)

	if got := budgetOf(t, st, org.ID); got != "20" {
		t.Fatalf("budget is %q, want the $20.00 the size includes", got)
	}
}

// A figure somebody typed is theirs, and a renewal is not a reason to touch it.
func TestARenewalLeavesAChosenBudgetAlone(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Chosen Ltd")
	subscribedTo(t, st, org.ID, "cus_chosen", "upto_25")
	b.syncAllowance(ctx, org.ID)

	st.PutSetting(ctx, org.ID, "monthly_budget_usd", "5")
	b.settings.Invalidate(org.ID)
	// Same size, same period: nothing about the plan moved.
	b.syncAllowance(ctx, org.ID)
	if got := budgetOf(t, st, org.ID); got != "5" {
		t.Fatalf("budget is %q after a sync that changed nothing, want the $5 they chose", got)
	}
}

// ...but a plan move is, because a guard rail chosen for one plan is not one for another.
func TestAPlanMoveResetsTheBudgetEvenOverAChosenFigure(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Moving Ltd")
	subscribedTo(t, st, org.ID, "cus_moving", "upto_10")
	b.syncAllowance(ctx, org.ID)
	st.PutSetting(ctx, org.ID, "monthly_budget_usd", "3")
	b.settings.Invalidate(org.ID)

	acct, _ := st.BillingAccountOf(ctx, org.ID)
	st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_moving", SubscriptionID: "sub_inc",
		Status: "active", Size: "upto_25", Quantity: 1, Currency: "usd", PeriodEnd: acct.PeriodEnd})
	b.syncAllowance(ctx, org.ID)

	if got := budgetOf(t, st, org.ID); got != "20" {
		t.Fatalf("budget is %q after moving to a size including $20.00, want 20", got)
	}
}

// Never above what the plan includes: the budget is a ceiling on spending the allowance, not a
// licence to spend past it into prepaid credit. Only a person puts it higher.
func TestTheBudgetIsNeverRaisedAboveWhatThePlanIncludes(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Ceiling Ltd")
	subscribedTo(t, st, org.ID, "cus_ceil", "upto_25")
	b.syncAllowance(ctx, org.ID)
	for i := 0; i < 3; i++ {
		b.syncAllowance(ctx, org.ID)
	}
	if got := budgetOf(t, st, org.ID); got != "20" {
		t.Fatalf("repeated syncs moved the budget to %q; it must settle at what the size includes", got)
	}
}

// A size that includes nothing, and an account with no plan at all, have no figure to impose.
// Overwriting the budget from them would be a change nobody asked for.
func TestNoPlanMeansNoOpinionAboutTheBudget(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()

	bare := testOrg(t, st, "Bare Ltd")
	subscribedTo(t, st, bare.ID, "cus_bare_b", "nothing_included")
	b.syncAllowance(ctx, bare.ID)
	if got := budgetOf(t, st, bare.ID); got != "" {
		t.Errorf("a size including nothing set the budget to %q", got)
	}

	free := testOrg(t, st, "Free Ltd")
	b.syncAllowance(ctx, free.ID)
	if got := budgetOf(t, st, free.ID); got != "" {
		t.Errorf("an account with no plan set the budget to %q", got)
	}
}

// A cancelled plan keeps whatever figure it had rather than being reset to nothing:
// EffectiveBudget already clamps it to the free plan's ceiling, so there is nothing to correct.
func TestALapsedPlanDoesNotWipeTheBudget(t *testing.T) {
	b, st := billingBot(t, includedCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Lapsing Ltd")
	subscribedTo(t, st, org.ID, "cus_lapsing", "upto_25")
	b.syncAllowance(ctx, org.ID)

	st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_lapsing", Status: "canceled"})
	b.syncAllowance(ctx, org.ID)
	if got := budgetOf(t, st, org.ID); got != "20" {
		t.Fatalf("a cancelled plan changed the budget to %q; it should keep what it had", got)
	}
}
