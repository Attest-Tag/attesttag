package app

import (
	"context"
	"testing"
	"time"
)

// A caller with a short window must not be able to shrink another limiter's window by sweeping.
// countThrottle deletes only its own key's expired rows, and a separate far-longer horizon
// collects across keys. The bug this guards: POST /api/auth/forgot runs a 15-minute window, and a
// global `delete ... where at < now-window` let one call every 15 minutes evict every other
// limiter's older rows — loosening signup, operator-secret and invite limits several-fold.
func TestThrottleSweepIsPerKey(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	// An hour-window limiter records one event.
	if ok, _, err := st.countThrottle(ctx, "hourly", 5, time.Hour); err != nil || !ok {
		t.Fatalf("first event: ok=%v err=%v", ok, err)
	}
	// Backdate it to 30 minutes ago: still inside its own hour window, but older than the
	// 15-minute window the next caller uses.
	if _, err := st.db.ExecContext(ctx, `update throttle_events set at=? where key=?`,
		time.Now().Add(-30*time.Minute).UnixNano(), "hourly"); err != nil {
		t.Fatal(err)
	}

	// A 15-minute-window caller records and sweeps.
	if _, _, err := st.countThrottle(ctx, "short", 5, 15*time.Minute); err != nil {
		t.Fatal(err)
	}

	var n int
	st.db.QueryRowContext(ctx, `select count(*) from throttle_events where key=?`, "hourly").Scan(&n)
	if n != 1 {
		t.Fatalf("a short-window caller's sweep dropped an hour-window key's row: %d left, want 1", n)
	}
}
