package app

import (
	"context"
	"testing"
	"time"
)

// shift moves one of activitySince's bounds by d, keeping the format the column stores.
func shift(t *testing.T, bound string, d time.Duration) string {
	t.Helper()
	at, err := time.Parse("2006-01-02 15:04:05", bound)
	if err != nil {
		t.Fatalf("parse bound %q: %v", bound, err)
	}
	return at.Add(d).Format("2006-01-02 15:04:05")
}

func TestActivitySinceWindows(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct{ rng, want string }{
		{"today", now.Format("2006-01-02") + " 00:00:00"},
		{"month", now.Format("2006-01") + "-01 00:00:00"},
		{"", ""},
		{"all", ""},
		{"last-tuesday", ""},
	}
	for _, c := range cases {
		if got := activitySince(c.rng); got != c.want {
			t.Errorf("activitySince(%q) = %q, want %q", c.rng, got, c.want)
		}
	}
	// 7d is a moving bound, so it is checked as a distance rather than a literal.
	week, err := time.Parse("2006-01-02 15:04:05", activitySince("7d"))
	if err != nil {
		t.Fatalf("parse 7d bound: %v", err)
	}
	if d := now.Sub(week); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
		t.Errorf("7d bound is %v back, want 168h", d)
	}
}

// An overview tile is a promise: the number on it and the rows its link opens are the same set.
// The tile counts in SQLite (OverviewStats) and the listing is bounded in Go (activitySince), so
// the two have to agree on where each window starts.
func TestActivityRangeMatchesOverviewCounts(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	today, week, month := activitySince("today"), activitySince("7d"), activitySince("month")
	// A minute either side of every edge: close enough to catch an off-by-one window, far enough
	// that a bound computed in Go and one computed by SQLite still put the row on the same side.
	stamps := []string{
		shift(t, month, -time.Minute), shift(t, month, time.Minute),
		shift(t, week, -time.Minute), shift(t, week, time.Minute),
		shift(t, today, -time.Minute), shift(t, today, time.Minute),
	}
	for _, at := range stamps {
		if _, err := st.db.ExecContext(ctx, `insert into usage (org_id, team_id, channel, model, tokens_in, tokens_out, cost_usd, created_at)
			values (?, 'T1', 'C1', 'm', 10, 5, 1.0, ?)`, orgID, at); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
		if _, err := st.db.ExecContext(ctx, `insert into tool_calls (org_id, team_id, channel, name, args, result, ok, ms, created_at)
			values (?, 'T1', 'C1', 'http_request', '{}', 'boom', 0, 3, ?)`, orgID, at); err != nil {
			t.Fatalf("seed tool_calls: %v", err)
		}
		if _, err := st.db.ExecContext(ctx, `insert into proxy_audit (org_id, team_id, channel, method, host, path, status, ms, created_at)
			values (?, 'T1', 'C1', 'GET', 'api.example.com', '/v1/x', 500, 3, ?)`, orgID, at); err != nil {
			t.Fatalf("seed proxy_audit: %v", err)
		}
	}

	// created_at is stored as "YYYY-MM-DD HH:MM:SS", so a string compare orders it the way
	// SQLite's own compare does.
	want := func(bound string) int {
		n := 0
		for _, at := range stamps {
			if at >= bound {
				n++
			}
		}
		return n
	}

	for _, rng := range []string{"today", "7d", "month", "all"} {
		bound := activitySince(rng)
		turns, err := st.RecentTurns(ctx, orgID, "", 200, bound)
		if err != nil {
			t.Fatalf("turns %s: %v", rng, err)
		}
		if len(turns) != want(bound) {
			t.Errorf("range=%s returned %d turns, want %d", rng, len(turns), want(bound))
		}
		calls, err := st.RecentToolCalls(ctx, orgID, "", 200, false, bound)
		if err != nil {
			t.Fatalf("tool calls %s: %v", rng, err)
		}
		if len(calls) != want(bound) {
			t.Errorf("range=%s returned %d tool calls, want %d", rng, len(calls), want(bound))
		}
		// Every seeded call failed, so narrowing to failures must not narrow the window further.
		failed, err := st.RecentToolCalls(ctx, orgID, "", 200, true, bound)
		if err != nil {
			t.Fatalf("failed tool calls %s: %v", rng, err)
		}
		if len(failed) != want(bound) {
			t.Errorf("range=%s with errors=1 returned %d tool calls, want %d", rng, len(failed), want(bound))
		}
		audits, err := st.ProxyAudits(ctx, orgID, "", 200, false, bound)
		if err != nil {
			t.Fatalf("proxy %s: %v", rng, err)
		}
		if len(audits) != want(bound) {
			t.Errorf("range=%s returned %d proxied requests, want %d", rng, len(audits), want(bound))
		}
	}

	// The channel filter and the window narrow together rather than one replacing the other, and
	// every list narrows the same way: the console's tab counts are the rows underneath them.
	if rows, err := st.RecentTurns(ctx, orgID, "C2", 200, today); err != nil || len(rows) != 0 {
		t.Errorf("another channel inside the window returned %d turns (err %v), want 0", len(rows), err)
	}
	if rows, err := st.RecentToolCalls(ctx, orgID, "C2", 200, false, ""); err != nil || len(rows) != 0 {
		t.Errorf("another channel returned %d tool calls (err %v), want 0", len(rows), err)
	}
	if rows, err := st.ProxyAudits(ctx, orgID, "C2", 200, false, ""); err != nil || len(rows) != 0 {
		t.Errorf("another channel returned %d proxied requests (err %v), want 0", len(rows), err)
	}
	if rows, err := st.RecentToolCalls(ctx, orgID, "C1", 200, false, month); err != nil || len(rows) != want(month) {
		t.Errorf("the seeded channel returned %d tool calls this month (err %v), want %d", len(rows), err, want(month))
	}
	if rows, err := st.ProxyAudits(ctx, orgID, "C1", 200, false, month); err != nil || len(rows) != want(month) {
		t.Errorf("the seeded channel returned %d proxied requests this month (err %v), want %d", len(rows), err, want(month))
	}

	o := st.OverviewStats(ctx, orgID)
	if o.TurnsToday != want(today) {
		t.Errorf("tile says %d turns today, the range=today listing has %d", o.TurnsToday, want(today))
	}
	if o.Turns7d != want(week) {
		t.Errorf("tile says %d turns in 7 days, the range=7d listing has %d", o.Turns7d, want(week))
	}
	monthTurns, _ := st.RecentTurns(ctx, orgID, "", 200, month)
	var spend float64
	for _, turn := range monthTurns {
		spend += turn.Cost
	}
	if o.MonthSpend != spend {
		t.Errorf("tile says $%.2f spent this month, the range=month listing adds up to $%.2f", o.MonthSpend, spend)
	}
}
