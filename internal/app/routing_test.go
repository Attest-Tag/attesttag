package app

import (
	"context"
	"testing"
)

// The operator's ceilings hold whatever a tenant sets, and accounting that cannot be read is
// a reason to stop, not to spend.

func spendAgent(t *testing.T, cfg Config) (*Agent, *Store) {
	t.Helper()
	st := testStore(t)
	return &Agent{store: st, settings: newSettingsCache(st, cfg), cfg: cfg}, st
}

func TestPlatformCeilingCapsTenantBudget(t *testing.T) {
	a, st := spendAgent(t, Config{PlatformMonthlyBudgetUSDPerOrg: 1})
	ctx := context.Background()
	if _, err := st.db.ExecContext(ctx, `insert into usage (org_id, team_id, channel, cost_usd) values (7, '', '', 2.5)`); err != nil {
		t.Fatal(err)
	}
	// The tenant switches its own budget off — which used to switch the check off.
	if err := st.PutSettings(ctx, 7, map[string]string{"monthly_budget_usd": "0"}); err != nil {
		t.Fatal(err)
	}
	if got := a.settings.Get(ctx, 7).EffectiveBudget(); got != 1 {
		t.Fatalf("effective budget = %v, want the operator's 1", got)
	}
	if ok, why := a.budgetOK(ctx, 7, ""); ok {
		t.Fatalf("a tenant lifted the operator's ceiling: %q", why)
	}
	// A tenant budget above the ceiling is the ceiling; one below it is its own.
	st.PutSettings(ctx, 7, map[string]string{"monthly_budget_usd": "50"})
	a.settings.Invalidate(7)
	if got := a.settings.Get(ctx, 7).EffectiveBudget(); got != 1 {
		t.Errorf("effective budget = %v, want 1", got)
	}
	st.PutSettings(ctx, 7, map[string]string{"monthly_budget_usd": "0.5"})
	a.settings.Invalidate(7)
	if got := a.settings.Get(ctx, 7).EffectiveBudget(); got != 0.5 {
		t.Errorf("effective budget = %v, want 0.5", got)
	}
	// With no ceiling configured, zero still means what it said.
	b, _ := spendAgent(t, Config{})
	if got := b.settings.Get(ctx, 7).EffectiveBudget(); got != 0 {
		t.Errorf("effective budget without a ceiling = %v, want 0", got)
	}
}

func TestBudgetFailsClosedOnAccountingError(t *testing.T) {
	a, st := spendAgent(t, Config{PlatformMonthlyBudgetUSDPerOrg: 1})
	ctx := context.Background()
	if _, err := st.db.ExecContext(ctx, `drop table usage`); err != nil {
		t.Fatal(err)
	}
	if ok, why := a.budgetOK(ctx, 7, ""); ok || why == "" {
		t.Fatalf("spend that cannot be read was treated as zero: %v %q", ok, why)
	}
}

func TestInFlightCapIsPerOrganisation(t *testing.T) {
	a, _ := spendAgent(t, Config{PlatformMaxInFlightPerOrg: 2})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, end := a.beginRun(ctx, &Call{OrgID: 7, TeamID: "T1", Channel: "C1"})
		defer end()
	}
	if ok, why := a.allowed(ctx, &Call{OrgID: 7, TeamID: "T1", Channel: "C1", UserID: "U1"}); ok || why == "" {
		t.Fatalf("a third turn for a full organisation was allowed: %v %q", ok, why)
	}
	if ok, why := a.allowed(ctx, &Call{OrgID: 8, TeamID: "T2", Channel: "C1", UserID: "U1"}); !ok {
		t.Fatalf("another organisation was held by the first one's turns: %q", why)
	}
}

// The model only changes when someone changes it. Every automatic escalation is gone: the shape
// of the ask, a run's own budget and the forty tools this thread has already spent all leave it
// where it is, and the thread's length is not measured at all any more.
func TestModelDoesNotChangeUntilSomeoneChangesIt(t *testing.T) {
	a, st := spendAgent(t, Config{Model: "base/model", HeavyModel: "heavy/model"})
	ctx := context.Background()
	c := &Call{OrgID: 7, TeamID: "T1", Channel: "C1",
		Session: &Session{Channel: "C1", ThreadTS: "1.1", ToolCalls: 40}}
	for _, tc := range []struct {
		name   string
		text   string
		rounds int
	}{
		{"a plain ask", "who is on call today?", 0},
		{"a code ask", "fix the retry function in the ingest script", 0},
		{"a stack trace", "```\npanic: nil map\n```", 0},
		{"an investigation", "why did the ingest stall?", 30},
	} {
		c.Text, c.MaxRounds = tc.text, tc.rounds
		if m, why := a.chooseModel(ctx, c); m != "base/model" {
			t.Errorf("%s moved the thread to %q (%s), want the model it was already on", tc.name, m, why)
		}
	}
	// Asking is what moves it: a channel default, or !model in the thread.
	sc, err := st.UpsertChannelScope(ctx, 7, "T1", "C1", "general", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateScope(ctx, 7, sc.ID, "", "heavy", ""); err != nil {
		t.Fatal(err)
	}
	if m, why := a.chooseModel(ctx, c); m != "heavy/model" || why != "channel default" {
		t.Errorf("channel default = %q (%s), want the advanced model", m, why)
	}
	c.Session.Model = "picked/by/hand"
	if m, why := a.chooseModel(ctx, c); m != "picked/by/hand" || why != "thread override" {
		t.Errorf("thread override = %q (%s), want the model the thread asked for", m, why)
	}
}

// The per-turn spend ceiling. Rounds bound how long a turn digs; this bounds what the digging
// costs, which is a separate question, because the transcript is re-sent every round and so
// cost grows with the square of them rather than in step.
func TestOverSpentPrefersCostAndFallsBackToTokens(t *testing.T) {
	if overSpent(Usage{CostUSD: 9}, 0) {
		t.Error("a ceiling of zero is off, however much was spent")
	}
	if overSpent(Usage{CostUSD: 0.49}, 0.50) {
		t.Error("under the ceiling is not over it")
	}
	if !overSpent(Usage{CostUSD: 0.50}, 0.50) {
		t.Error("reaching the ceiling should land the turn")
	}
	// A provider that prices its own usage is trusted over any arithmetic here, in both
	// directions: a cheap turn with a great many tokens keeps going...
	if overSpent(Usage{CostUSD: 0.01, In: turnMaxPromptTokens * 2}, 0.50) {
		t.Error("a priced turn is judged on its price, not on its tokens")
	}
	// ...and one that reports no price at all is still bounded by something.
	if overSpent(Usage{In: turnMaxPromptTokens - 1}, 0.50) {
		t.Error("under the token fallback is not over it")
	}
	if !overSpent(Usage{In: turnMaxPromptTokens}, 0.50) {
		t.Error("a provider that reports no cost must still be bounded")
	}
}

// Sized against eight days of production: the runaway routine trips it and ordinary work does
// not. $0.31 is the honest near miss — a 160-round run that finished on its own — and it is
// left alone deliberately, because this ceiling is for the turns that do not.
func TestSpendCeilingSeparatesTheRunawayFromTheRest(t *testing.T) {
	ceiling := Config{TurnMaxUSD: 0.50}.turnSpendCeiling()
	for _, cost := range []float64{1.80, 1.41, 1.12} { // the same routine, three days running
		if !overSpent(Usage{CostUSD: cost}, ceiling) {
			t.Errorf("$%.2f should have been stopped", cost)
		}
	}
	for _, cost := range []float64{0.0024, 0.024, 0.072, 0.31} { // median, mean, largest reply, long routine
		if overSpent(Usage{CostUSD: cost}, ceiling) {
			t.Errorf("$%.4f is ordinary work and must not be stopped", cost)
		}
	}
	off := Config{}.turnSpendCeiling()
	if off != 0 {
		t.Errorf("an unset TURN_MAX_USD should leave the ceiling off, got %v", off)
	}
}
