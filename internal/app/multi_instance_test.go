package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The leader lease is what stops two instances running the same loop. One holder at a time, a
// lease that lapses can be taken by somebody else, and the instance that lost it finds out when
// it tries to renew rather than carrying on beside its replacement.
func TestLeaderLeaseHasOneHolder(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	tokenA, gotA, err := st.TakeLeaderLease(ctx, "ingest", "A", time.Minute)
	if err != nil || !gotA {
		t.Fatalf("A could not take a free lease: %v %v", gotA, err)
	}
	// B is refused while A's lease is live.
	if _, gotB, err := st.TakeLeaderLease(ctx, "ingest", "B", time.Minute); err != nil || gotB {
		t.Fatalf("B took a lease A holds: %v %v", gotB, err)
	}
	// A renews its own.
	tokenA2, ok, err := st.RenewLeaderLease(ctx, "ingest", "A", tokenA, time.Minute)
	if err != nil || !ok {
		t.Fatalf("A could not renew its own lease: %v %v", ok, err)
	}
	// A renewal with a stale token fails — that is what catches an instance that paused.
	if _, ok, _ := st.RenewLeaderLease(ctx, "ingest", "A", tokenA, time.Minute); ok {
		t.Error("a renewal with a superseded token was accepted")
	}

	// Once it lapses, B takes it, and A's next renewal fails rather than stealing it back.
	if _, err := st.db.ExecContext(ctx, `update leader_leases set expires_at=1 where role='ingest'`); err != nil {
		t.Fatal(err)
	}
	if _, gotB, err := st.TakeLeaderLease(ctx, "ingest", "B", time.Minute); err != nil || !gotB {
		t.Fatalf("B could not take a lapsed lease: %v %v", gotB, err)
	}
	if _, ok, _ := st.RenewLeaderLease(ctx, "ingest", "A", tokenA2, time.Minute); ok {
		t.Error("A renewed a lease B now holds")
	}

	// Different roles are independent: one leader per loop, not one leader overall.
	if _, got, err := st.TakeLeaderLease(ctx, "drive-sync", "A", time.Minute); err != nil || !got {
		t.Errorf("a second role was blocked by the first: %v %v", got, err)
	}

	// Released early rather than leaving the next instance to wait out the TTL.
	tokenB, _, _ := st.TakeLeaderLease(ctx, "reconciler", "B", time.Minute)
	st.ReleaseLeaderLease(ctx, "reconciler", "B", tokenB)
	if _, got, err := st.TakeLeaderLease(ctx, "reconciler", "C", time.Minute); err != nil || !got {
		t.Errorf("a released lease was not free: %v %v", got, err)
	}
}

// Two schedulers finding the same routine due must produce one run, not two. Before this the
// claim was a map in the agent, so both would claim it and the channel would get the same
// answer twice from what its members were told was one scheduled job.
func TestOnlyOneSchedulerClaimsARoutine(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	a := &Agent{store: st}
	id, err := st.AddRoutine(ctx, Routine{OrgID: 1, TeamID: "T1", Channel: "C1", Cron: "* * * * *",
		TZ: "UTC", Prompt: "p", NextRun: now()})
	if err != nil {
		t.Fatal(err)
	}

	// Ten goroutines race for it; exactly one may win.
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := a.claimRoutine(ctx, 1, id); ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d of 10 racing schedulers claimed the same routine, want 1", won)
	}

	// The scheduler skips it without another query, off the row it already read.
	rs, err := st.Routines(ctx, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || !a.routineBusy(rs[0]) {
		t.Fatalf("the claimed routine does not read as busy: %+v", rs)
	}

	// A holder that dies leaves a lease that lapses, rather than a routine blocked forever —
	// which is what the in-process map did when a process was killed mid-run.
	if _, err := st.db.ExecContext(ctx, `update routines set run_lease=1 where id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.claimRoutine(ctx, 1, id); !ok {
		t.Error("a lapsed claim was not reclaimable")
	}
}

// The limiters are security controls, so they have to count what every instance has seen. Two
// limiters sharing one store stand in for two instances sharing one database.
func TestSharedLimitersCountAcrossInstances(t *testing.T) {
	st := testStore(t)
	a, b := newRateLimiter(), newRateLimiter()
	a.shareAcross(st)
	b.shareAcross(st)

	key := "signup:" + t.Name()
	// Five allowed in total, alternating between the two "instances".
	for i := 0; i < 5; i++ {
		l := a
		if i%2 == 1 {
			l = b
		}
		if ok, _ := l.allow(key, 5, time.Hour); !ok {
			t.Fatalf("attempt %d of 5 was refused", i+1)
		}
	}
	// The sixth is refused whichever one sees it — which is the whole point. Counted per
	// process, each would have had five of its own.
	if ok, retry := b.allow(key, 5, time.Hour); ok {
		t.Error("the sixth attempt was allowed: the two instances are not sharing a count")
	} else if retry <= 0 || retry > time.Hour {
		t.Errorf("retry-after = %v, want something inside the window", retry)
	}
	// A different key is unaffected.
	if ok, _ := a.allow(key+":other", 5, time.Hour); !ok {
		t.Error("an unrelated key was caught by another key's window")
	}
}

// A sign-in started on one instance must be completable on the other — the provider redirects
// the browser to whichever container answers, and it is a coin toss which. The state is
// redeemed exactly once, and the PKCE verifier is not stored in the clear.
func TestOAuthPendingSurvivesTheInstanceThatStartedIt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	fixedMasterKey(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	p := oauthPending{userID: 7, orgID: 3, connID: 11, verifier: "the-pkce-verifier"}
	if err := st.PutOAuthPending(ctx, sealer, "state-1", p); err != nil {
		t.Fatal(err)
	}

	// Stored sealed: the verifier completes somebody else's sign-in if it leaks.
	var enc []byte
	st.db.QueryRowContext(ctx, `select verifier_enc from oauth_pendings where state='state-1'`).Scan(&enc)
	if len(enc) == 0 || string(enc) == p.verifier {
		t.Errorf("the PKCE verifier is not sealed: %q", enc)
	}

	got, err := st.TakeOAuthPending(ctx, sealer, "state-1")
	if err != nil {
		t.Fatalf("redeeming on another instance: %v", err)
	}
	if got.orgID != 3 || got.connID != 11 || got.userID != 7 || got.verifier != p.verifier {
		t.Errorf("round trip lost something: %+v", got)
	}

	// Single use: a replayed callback must not exchange the code twice.
	if _, err := st.TakeOAuthPending(ctx, sealer, "state-1"); err == nil {
		t.Error("the same state was redeemed twice")
	}
	// And an unknown state says so rather than panicking.
	if _, err := st.TakeOAuthPending(ctx, sealer, "never-issued"); err == nil {
		t.Error("an unknown state was accepted")
	}

	// Expired is refused too, with its own reason.
	st.PutOAuthPending(ctx, sealer, "state-old", p)
	if _, err := st.db.ExecContext(ctx, `update oauth_pendings set expires_at=? where state='state-old'`,
		nowMinus(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TakeOAuthPending(ctx, sealer, "state-old"); err == nil {
		t.Error("an expired sign-in was completed")
	}
}

// "stop" reaches the instance running the turn, which need not be the one that took the
// message: a Slack event goes to whichever container answered the webhook.
func TestStopRequestCrossesInstances(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := st.db.ExecContext(ctx,
		`insert into sessions (team_id, channel, thread_ts, kind) values ('T1','C1','1.1','channel')`); err != nil {
		t.Fatal(err)
	}
	if at := st.StopRequestedAt(ctx, "T1", "C1", "1.1"); at != "" {
		t.Fatalf("a fresh thread already has a stop request: %q", at)
	}
	if err := st.RequestStop(ctx, "T1", "C1", "1.1"); err != nil {
		t.Fatal(err)
	}
	if at := st.StopRequestedAt(ctx, "T1", "C1", "1.1"); at == "" {
		t.Error("the running instance would not see the stop")
	}
	// Another thread is unaffected.
	if at := st.StopRequestedAt(ctx, "T1", "C1", "9.9"); at != "" {
		t.Errorf("the stop leaked to another thread: %q", at)
	}
	// Cleared when the next turn begins, or an hour-old stop kills the next question.
	if err := st.ClearStopRequest(ctx, "T1", "C1", "1.1"); err != nil {
		t.Fatal(err)
	}
	if at := st.StopRequestedAt(ctx, "T1", "C1", "1.1"); at != "" {
		t.Errorf("the stop survived the next turn starting: %q", at)
	}
}

// instanceID is what a human reads out of leader_leases during an incident, so it has to say
// something. Two processes on one machine must not look like the same holder.
func TestInstanceIDIdentifiesTheProcess(t *testing.T) {
	id := instanceID()
	if id == "" || id == "/0" {
		t.Fatalf("instanceID() = %q", id)
	}
	if want := fmt.Sprintf("/%d", os.Getpid()); !strings.HasSuffix(id, want) {
		t.Errorf("instanceID() = %q, want it to end in %q", id, want)
	}
}
