package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The GitHub inbox is what stands between one pull request and two reviews of it. These run on
// both dialects; the race below is only a race on Postgres, where dispatchers really do claim at
// once — SQLite has one writer and serialises them, which is how the Slack inbox's version of
// this bug went unseen until production ran on Postgres.

func enqueueOK(t *testing.T, st *Store, id string, org int64) {
	t.Helper()
	fresh, err := st.enqueueGitHubDelivery(context.Background(), id, org, 42, "pull_request", "opened", []byte("sealed:"+id))
	if err != nil || !fresh {
		t.Fatalf("enqueue %s for org %d: fresh=%v err=%v", id, org, fresh, err)
	}
}

type githubDeliveryRow struct {
	leaseUntil, doneAt, deadAt int64
	attempts                   int
	payload                    []byte
	lastError                  string
}

func readGitHubDelivery(t *testing.T, st *Store, id string) githubDeliveryRow {
	t.Helper()
	var r githubDeliveryRow
	if err := st.db.QueryRowContext(context.Background(), `select lease_until, done_at, dead_at, attempts, payload_enc, last_error
		from github_deliveries where delivery_id=?`, id).
		Scan(&r.leaseUntil, &r.doneAt, &r.deadAt, &r.attempts, &r.payload, &r.lastError); err != nil {
		t.Fatalf("reading delivery %s: %v", id, err)
	}
	return r
}

// lapseGitHubDelivery ends a delivery's lease now, as a stalled dispatcher's would end on its own.
func lapseGitHubDelivery(t *testing.T, st *Store, id string) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(), `update github_deliveries set lease_until=? where delivery_id=?`,
		time.Now().Add(-time.Second).UnixNano(), id); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubDeliveryClaimRaceHasOneWinner(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	enqueueOK(t, st, "d-1", orgID)

	var wg sync.WaitGroup
	var won atomic.Int32
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := st.claimGitHubDelivery(ctx)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if d != nil {
				won.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := won.Load(); n != 1 {
		t.Fatalf("%d dispatchers claimed the same delivery; one pull request would be reviewed %d times", n, n)
	}
	if r := readGitHubDelivery(t, st, "d-1"); r.attempts != 1 {
		t.Errorf("attempts = %d after one claim, want 1: a losing claimer spent an attempt", r.attempts)
	}
}

// A dispatcher that stalls past its lease has lost the work to the next one. Everything it then
// tries to write must miss — extending a lease that is somebody else's, closing a delivery it no
// longer owns, or pushing back a retry the new holder is in the middle of.
func TestGitHubDeliveryLapsedLeaseFencesTheOldHolder(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	enqueueOK(t, st, "d-1", orgID)

	first, err := st.claimGitHubDelivery(ctx)
	if err != nil || first == nil {
		t.Fatalf("first claim: %v %v", first, err)
	}
	if first.OrgID != orgID || first.InstallationID != 42 || first.Event != "pull_request" || first.Action != "opened" ||
		string(first.Payload) != "sealed:d-1" {
		t.Errorf("claim returned %+v", first)
	}
	if again, _ := st.claimGitHubDelivery(ctx); again != nil {
		t.Fatal("a held delivery was claimed a second time")
	}
	lapseGitHubDelivery(t, st, "d-1")
	second, err := st.claimGitHubDelivery(ctx)
	if err != nil || second == nil {
		t.Fatalf("a lapsed lease was not reclaimable: %v %v", second, err)
	}
	if second.Attempts != 2 {
		t.Errorf("reclaim attempts = %d, want 2", second.Attempts)
	}

	stale := *first
	if err := st.touchGitHubDelivery(ctx, &stale); !errors.Is(err, errLeaseLost) {
		t.Errorf("old holder's touch: %v, want errLeaseLost", err)
	}
	if err := st.failGitHubDelivery(ctx, &stale, "old holder gave up"); !errors.Is(err, errLeaseLost) {
		t.Errorf("old holder's fail: %v, want errLeaseLost", err)
	}
	if err := st.finishGitHubDelivery(ctx, &stale); !errors.Is(err, errLeaseLost) {
		t.Errorf("old holder's finish: %v, want errLeaseLost", err)
	}
	r := readGitHubDelivery(t, st, "d-1")
	if r.leaseUntil != second.Lease || r.doneAt != 0 || r.deadAt != 0 || r.lastError != "" || r.payload == nil {
		t.Fatalf("the old holder's writes reached the row: %+v (new lease %d)", r, second.Lease)
	}

	// The new holder's writes land, and a finish drops the payload.
	before := second.Lease
	if err := st.touchGitHubDelivery(ctx, second); err != nil {
		t.Fatalf("new holder's touch: %v", err)
	}
	if second.Lease <= before {
		t.Errorf("touch did not move the lease forward: %d -> %d", before, second.Lease)
	}
	if err := st.finishGitHubDelivery(ctx, second); err != nil {
		t.Fatalf("new holder's finish: %v", err)
	}
	if r := readGitHubDelivery(t, st, "d-1"); r.doneAt == 0 || r.payload != nil {
		t.Errorf("finished delivery: done_at=%d payload=%q; want done and the payload gone", r.doneAt, r.payload)
	}
	if err := st.finishGitHubDelivery(ctx, second); !errors.Is(err, errLeaseLost) {
		t.Errorf("finishing twice: %v, want errLeaseLost", err)
	}
	if d, _ := st.claimGitHubDelivery(ctx); d != nil {
		t.Error("a finished delivery was claimed again")
	}
}

func TestGitHubDeliveryFailureBacksOffThenDeadLetters(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	enqueueOK(t, st, "d-1", orgID)

	for attempt := 1; attempt <= githubDeliveryMaxAttempts; attempt++ {
		d, err := st.claimGitHubDelivery(ctx)
		if err != nil || d == nil {
			t.Fatalf("attempt %d: claim %v %v", attempt, d, err)
		}
		if d.Attempts != attempt {
			t.Fatalf("attempt %d: claim says %d", attempt, d.Attempts)
		}
		if err := st.failGitHubDelivery(ctx, d, fmt.Sprintf("GitHub said 502 on attempt %d", attempt)); err != nil {
			t.Fatalf("attempt %d: fail: %v", attempt, err)
		}
		if attempt == githubDeliveryMaxAttempts {
			break
		}
		// Held back for the backoff, not handed straight to the next dispatcher.
		r := readGitHubDelivery(t, st, "d-1")
		if wait := time.Until(time.Unix(0, r.leaseUntil)); wait < githubDeliveryBackoff(attempt)-5*time.Second {
			t.Errorf("attempt %d: retry due in %v, want about %v", attempt, wait, githubDeliveryBackoff(attempt))
		}
		if again, _ := st.claimGitHubDelivery(ctx); again != nil {
			t.Fatalf("attempt %d: a backed-off delivery was claimed at once", attempt)
		}
		lapseGitHubDelivery(t, st, "d-1")
	}
	r := readGitHubDelivery(t, st, "d-1")
	if r.deadAt == 0 || r.payload != nil || !strings.Contains(r.lastError, "attempt 5") {
		t.Fatalf("after %d failures: %+v; want dead-lettered, payload dropped, the last reason kept", githubDeliveryMaxAttempts, r)
	}
	lapseGitHubDelivery(t, st, "d-1")
	if d, _ := st.claimGitHubDelivery(ctx); d != nil {
		t.Error("a dead-lettered delivery was claimed again")
	}
}

func TestGitHubDeliveryBackoffSchedule(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 15 * time.Second, 2: time.Minute, 3: 4 * time.Minute, 4: 16 * time.Minute, 9: 30 * time.Minute} {
		if got := githubDeliveryBackoff(n); got != want {
			t.Errorf("backoff after attempt %d = %v, want %v", n, got, want)
		}
	}
}

// A redelivery carries the delivery id it was first sent with, and must be answered as the
// duplicate it is: queued once, and never refused for want of room it does not need.
func TestGitHubDeliveryDedupesOnTheDeliveryID(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	enqueueOK(t, st, "d-1", orgID)
	fresh, err := st.enqueueGitHubDelivery(ctx, "d-1", orgID, 42, "pull_request", "opened", []byte("a different body"))
	if err != nil || fresh {
		t.Fatalf("redelivery: fresh=%v err=%v; want a duplicate, no error", fresh, err)
	}
	var n int
	st.db.QueryRowContext(ctx, `select count(*) from github_deliveries`).Scan(&n)
	if n != 1 {
		t.Errorf("%d rows for one delivery id", n)
	}
	if r := readGitHubDelivery(t, st, "d-1"); string(r.payload) != "sealed:d-1" {
		t.Errorf("the redelivery overwrote the stored body: %q", r.payload)
	}
	// Still a duplicate once done: the receipt outlives the payload.
	d, _ := st.claimGitHubDelivery(ctx)
	if err := st.finishGitHubDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	if fresh, err := st.enqueueGitHubDelivery(ctx, "d-1", orgID, 42, "pull_request", "opened", nil); err != nil || fresh {
		t.Errorf("redelivery of a finished delivery: fresh=%v err=%v", fresh, err)
	}

	at, err := st.LastGitHubDeliveryAt(ctx, orgID, 42)
	if err != nil || time.Since(at) > time.Minute {
		t.Errorf("last delivery at %v (%v), want just now", at, err)
	}
	if at, _ := st.LastGitHubDeliveryAt(ctx, orgID+1, 42); !at.IsZero() {
		t.Errorf("another organisation reads this one's last delivery: %v", at)
	}
}

func TestGitHubDeliveryInboxCaps(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	// One organisation's share.
	for i := range githubOrgPendingLimit {
		enqueueOK(t, st, fmt.Sprintf("org1-%d", i), 1)
	}
	if _, err := st.enqueueGitHubDelivery(ctx, "org1-over", 1, 42, "pull_request", "opened", nil); !errors.Is(err, errGitHubInboxFull) {
		t.Fatalf("delivery %d for one organisation: %v, want errGitHubInboxFull", githubOrgPendingLimit+1, err)
	}
	if fresh, err := st.enqueueGitHubDelivery(ctx, "org1-0", 1, 42, "pull_request", "opened", nil); err != nil || fresh {
		t.Errorf("a redelivery while the organisation is full: fresh=%v err=%v; want a duplicate", fresh, err)
	}
	enqueueOK(t, st, "org2-0", 2) // a full neighbour is not this organisation's problem

	// Finishing one frees its slot.
	d, err := st.claimGitHubDelivery(ctx)
	if err != nil || d == nil || d.OrgID != 1 {
		t.Fatalf("claim: %+v %v", d, err)
	}
	if err := st.finishGitHubDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	enqueueOK(t, st, "org1-over", 1)

	// The deployment's ceiling: fill the rest from many organisations, none of them at its own cap.
	var total int
	st.db.QueryRowContext(ctx, `select count(*) from github_deliveries where done_at=0 and dead_at=0`).Scan(&total)
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; total < githubPendingLimit; i, total = i+1, total+1 {
		if _, err := tx.ExecContext(ctx, `insert into github_deliveries (delivery_id, org_id, installation_id, event, accepted_at)
			values (?, ?, 7, 'pull_request', ?)`, fmt.Sprintf("fill-%d", i), 100+i/50, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.enqueueGitHubDelivery(ctx, "org3-0", 3, 42, "pull_request", "opened", nil); !errors.Is(err, errGitHubInboxFull) {
		t.Errorf("a delivery past the deployment's ceiling: %v, want errGitHubInboxFull", err)
	}
	if fresh, err := st.enqueueGitHubDelivery(ctx, "fill-0", 3, 42, "pull_request", "opened", nil); err != nil || fresh {
		t.Errorf("a redelivery while the inbox is full: fresh=%v err=%v; want a duplicate", fresh, err)
	}
}

func TestGitHubDeliveryPurge(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := time.Now()
	old, recent := now.Add(-8*24*time.Hour).UnixNano(), now.Add(-time.Hour).UnixNano()
	held := now.Add(time.Minute).UnixNano()
	for _, r := range []struct {
		id                                    string
		accepted, lease, done, dead, attempts int64
	}{
		{"done-old", old, 0, old, 0, 1},
		{"done-recent", recent, 0, recent, 0, 1},
		{"dead-old", old, 0, 0, old, 5},
		{"dead-recent", recent, 0, 0, recent, 5},
		// The last attempt's dispatcher died: nobody will fail it and the claim will not take it.
		{"abandoned", recent, recent, 0, 0, githubDeliveryMaxAttempts},
		// The last attempt is still running.
		{"last-attempt", recent, held, 0, 0, githubDeliveryMaxAttempts},
		// Old but still pending is still work.
		{"pending-old", old, 0, 0, 0, 1},
	} {
		if _, err := st.db.ExecContext(ctx, `insert into github_deliveries
			(delivery_id, org_id, installation_id, event, payload_enc, accepted_at, lease_until, done_at, dead_at, attempts)
			values (?, 1, 42, 'pull_request', ?, ?, ?, ?, ?, ?)`,
			r.id, []byte("body"), r.accepted, r.lease, r.done, r.dead, r.attempts); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.purgeGitHubDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	rows, err := st.db.QueryContext(ctx, `select delivery_id from github_deliveries`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		left[id] = true
	}
	rows.Close()
	for id, want := range map[string]bool{"done-old": false, "dead-old": false, "done-recent": true, "dead-recent": true,
		"abandoned": true, "last-attempt": true, "pending-old": true} {
		if left[id] != want {
			t.Errorf("%s kept=%v, want %v", id, left[id], want)
		}
	}
	if r := readGitHubDelivery(t, st, "abandoned"); r.deadAt == 0 || r.payload != nil || r.lastError == "" {
		t.Errorf("an abandoned last attempt was not retired: %+v", r)
	}
	if r := readGitHubDelivery(t, st, "last-attempt"); r.deadAt != 0 || r.payload == nil {
		t.Errorf("a last attempt still under lease was retired: %+v", r)
	}
}
