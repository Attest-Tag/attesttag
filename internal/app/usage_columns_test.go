package app

import (
	"context"
	"testing"
)

// A usage row keeps the two numbers that say whether the arrangement producing its cost is worth
// keeping: how much of the prompt a provider served from cache, and how much of the answer went
// on reasoning nobody reads. Both were coming back on every response and both were written to a
// log line and dropped, which left the question unanswerable past the log retention.
func TestUsageRowKeepsCachedAndReasoningTokens(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	st.LogUsageBy(ctx, 1, "T1", "C1", "1.1", "U1", "m", Usage{In: 1000, Out: 200, CachedIn: 850, Reasoning: 120, CostUSD: 0.004})

	var in, out, cached, reasoning int
	var cost float64
	err := st.db.QueryRowContext(ctx,
		`select tokens_in, tokens_out, cached_in, tokens_reasoning, cost_usd from usage where org_id=?`, 1).
		Scan(&in, &out, &cached, &reasoning, &cost)
	if err != nil {
		t.Fatalf("reading the row back: %v", err)
	}
	if in != 1000 || out != 200 || cost != 0.004 {
		t.Errorf("in/out/cost = %d/%d/%v, want 1000/200/0.004", in, out, cost)
	}
	if cached != 850 {
		t.Errorf("cached_in = %d, want 850", cached)
	}
	if reasoning != 120 {
		t.Errorf("tokens_reasoning = %d, want 120", reasoning)
	}

	// A worker reports three numbers over the wire and has neither of the other two. Zero is the
	// truthful answer there, not a missing one.
	u := JobUsage{In: 10, Out: 5, CostUSD: 0.1}.usage()
	if u.CachedIn != 0 || u.Reasoning != 0 || u.In != 10 || u.Out != 5 || u.CostUSD != 0.1 {
		t.Errorf("JobUsage.usage() = %+v", u)
	}
}
