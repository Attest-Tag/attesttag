package app

// The lease's tests run against a map rather than GCS, which is only honest if the fake enforces
// exactly the preconditions GCS does: a create that fails when the object exists, and a
// replace/delete that fails unless the generation matches. Every test below turns on that.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

type memLeaseObject struct {
	mu       sync.Mutex
	body     []byte
	gen      int64
	present  bool
	modified time.Time
	err      error // when set, every operation fails with it (a network outage)
}

func (m *memLeaseObject) name() string { return "mem://lease" }

func (m *memLeaseObject) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// setModified forces the object's server-side timestamp, which is how the real expiry is judged.
func (m *memLeaseObject) setModified(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modified = t
}

func (m *memLeaseObject) read(context.Context) ([]byte, int64, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, 0, time.Time{}, m.err
	}
	if !m.present {
		return nil, 0, time.Time{}, errLeaseMissing
	}
	return m.body, m.gen, m.modified, nil
}

func (m *memLeaseObject) create(_ context.Context, body []byte) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, m.err
	}
	if m.present {
		return 0, errLeaseTaken
	}
	m.present, m.gen, m.body = true, m.gen+1, body
	return m.gen, nil
}

func (m *memLeaseObject) replace(_ context.Context, body []byte, gen int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, m.err
	}
	if !m.present || m.gen != gen {
		return 0, errLeaseTaken
	}
	m.gen, m.body = m.gen+1, body
	return m.gen, nil
}

func (m *memLeaseObject) remove(_ context.Context, gen int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if !m.present || m.gen != gen {
		return errLeaseTaken
	}
	m.present, m.body = false, nil
	return nil
}

// clock is a hand-wound wall clock; nothing in these tests sleeps.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTestLease wires a lease to the shared object and clock. Writes stamp the object's modified
// time from the same clock, as GCS would from its own.
func newTestLease(obj *memLeaseObject, c *clock, holder string) *Lease {
	l := newLease(obj, LeaseInfo{Holder: holder, Revision: holder})
	l.now = c.now
	return l
}

// stamp mimics GCS setting LastModified on a successful write.
func stamp(t *testing.T, l *Lease, obj *memLeaseObject, c *clock) {
	t.Helper()
	if held, _ := l.Held(); held {
		obj.setModified(c.now())
	}
}

// The whole safety argument in one assertion. A holder that cannot renew stops writing at
// leaseSelfFence and is gone leaseShutdown later; nobody may take its lease before
// leaseTTL+leaseStealAfter. If that inequality ever stops holding, two containers can write to one
// database and every other test here is beside the point.
func TestLeaseTimingsCannotOverlap(t *testing.T) {
	standDown := leaseSelfFence + leaseShutdown
	earliestSteal := leaseTTL + leaseStealAfter
	if standDown >= earliestSteal {
		t.Fatalf("a holder stands down after %s but its lease can be taken at %s: two writers overlap", standDown, earliestSteal)
	}
	// And a container must not give up before a hard-killed predecessor's lease can be taken, or
	// every SIGKILL turns into a crash loop instead of a handover.
	if leaseGiveUp <= earliestSteal {
		t.Fatalf("leaseGiveUp %s is not past the earliest steal at %s", leaseGiveUp, earliestSteal)
	}
}

func TestLeaseAcquiresWhenAbsent(t *testing.T) {
	obj := &memLeaseObject{}
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l := newTestLease(obj, c, "rev-a")

	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	held, at := l.Held()
	if !held || !at.Equal(c.now()) {
		t.Fatalf("expected the lease held at %v, got held=%v at=%v", c.now(), held, at)
	}
	if l.State() != "held" {
		t.Fatalf("state = %q, want held", l.State())
	}
	var got LeaseInfo
	if err := json.Unmarshal(obj.body, &got); err != nil {
		t.Fatalf("lock object is not readable JSON: %v", err)
	}
	if got.Holder != "rev-a" || !got.Expires.Equal(c.now().Add(leaseTTL)) {
		t.Fatalf("lock object = %+v", got)
	}
}

func TestSecondContenderIsRefused(t *testing.T) {
	obj := &memLeaseObject{}
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	a, b := newTestLease(obj, c, "rev-a"), newTestLease(obj, c, "rev-b")

	if err := a.Acquire(context.Background()); err != nil {
		t.Fatalf("a acquire: %v", err)
	}
	stamp(t, a, obj, c)

	// b must never succeed while a's lease is live: it gives up only when its context does.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := b.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("b acquired the lease, or failed for the wrong reason: %v", err)
	}
	if held, _ := b.Held(); held {
		t.Fatal("two containers hold the write lease")
	}
}

func TestRenewAdvancesGenerationAndIsNotMistakenForATheft(t *testing.T) {
	obj := &memLeaseObject{}
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l := newTestLease(obj, c, "rev-a")
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	first := obj.gen
	for i := 0; i < 3; i++ {
		c.add(leaseRenew)
		if err := l.renew(context.Background()); err != nil {
			t.Fatalf("renew %d: %v", i, err)
		}
	}
	if obj.gen <= first {
		t.Fatalf("generation did not advance: %d -> %d", first, obj.gen)
	}
	if _, at := l.Held(); !at.Equal(c.now()) {
		t.Fatalf("last renewal not recorded: %v", at)
	}
}

func TestExpiredLeaseIsTakenOverAndTheOldHolderFindsOut(t *testing.T) {
	obj := &memLeaseObject{}
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	a, b := newTestLease(obj, c, "rev-a"), newTestLease(obj, c, "rev-b")
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatalf("a acquire: %v", err)
	}
	stamp(t, a, obj, c)

	// a is killed without releasing. b sees an expired lease, but must confirm it is not moving
	// before taking it.
	c.add(leaseTTL + time.Second)
	if ok, _, err := b.try(context.Background()); ok || err != nil {
		t.Fatalf("b took an expired lease on first sight: ok=%v err=%v", ok, err)
	}
	c.add(leaseStealAfter + time.Second)
	ok, _, err := b.try(context.Background())
	if err != nil || !ok {
		t.Fatalf("b did not take the abandoned lease: ok=%v err=%v", ok, err)
	}

	// And a, were it somehow still alive, is told it no longer owns the database.
	if err := a.renew(context.Background()); !errors.Is(err, errLeaseTaken) {
		t.Fatalf("the evicted holder renewed anyway: %v", err)
	}
}

func TestALeaseThatIsStillMovingIsNotStolen(t *testing.T) {
	obj := &memLeaseObject{}
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	a, b := newTestLease(obj, c, "rev-a"), newTestLease(obj, c, "rev-b")
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatalf("a acquire: %v", err)
	}

	// An old modification time with a live holder behind it: b sees it as expired.
	obj.setModified(c.now().Add(-2 * leaseTTL))
	if ok, _, _ := b.try(context.Background()); ok {
		t.Fatal("b took the lease on first sight")
	}
	// a renews — the generation moves, even though the timestamp still looks stale.
	if err := a.renew(context.Background()); err != nil {
		t.Fatalf("a renew: %v", err)
	}
	obj.setModified(c.now().Add(-2 * leaseTTL))
	c.add(leaseStealAfter + time.Second)
	if ok, _, _ := b.try(context.Background()); ok {
		t.Fatal("b took a lease whose generation was still advancing: a live writer would have been evicted")
	}
}

func TestReleaseWillNotEvictWhoeverOwnsItNow(t *testing.T) {
	obj := &memLeaseObject{}
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	a, b := newTestLease(obj, c, "rev-a"), newTestLease(obj, c, "rev-b")
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatalf("a acquire: %v", err)
	}
	c.add(leaseTTL + time.Second)
	b.try(context.Background()) // first sighting: expired, but not yet confirmed abandoned
	c.add(leaseStealAfter + time.Second)
	if ok, _, _ := b.try(context.Background()); !ok {
		t.Fatal("b could not take the abandoned lease")
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("a release: %v", err)
	}
	if !obj.present {
		t.Fatal("the old holder deleted the new holder's lease")
	}
	if held, _ := b.Held(); !held {
		t.Fatal("b lost the lease it owns")
	}
}

func TestRenewalOutageIsNotTreatedAsLossUntilTheFence(t *testing.T) {
	obj := &memLeaseObject{}
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	l := newTestLease(obj, c, "rev-a")
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	obj.fail(errors.New("dial tcp: connection refused"))

	// A network error is not a theft: renew reports it, and it must not be errLeaseTaken, or the
	// service would restart on every GCS hiccup.
	err := l.renew(context.Background())
	if err == nil || errors.Is(err, errLeaseTaken) {
		t.Fatalf("an outage was reported as a lost lease: %v", err)
	}
	select {
	case <-l.Lost():
		t.Fatal("the lease was given up on the first failed renewal")
	default:
	}
}
