package app

// The single-writer lease.
//
// All state lives in one SQLite file on the container's local disk, streamed to GCS by Litestream.
// Cloud Run's --max-instances 1 is not a writer lock: a new revision's container boots, and
// restores its own copy of the database, while the old one is still serving and writing. For that
// window two containers stream two different databases into one replica prefix and writes are
// lost. This lease is what makes "one writer" actually true, and nothing may touch the database
// without holding it — the restore least of all, since restoring before the lease is held is how
// a stale snapshot gets promoted over live data.
//
// It is a lease, not a fence: see the note on Release.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// The timings, and the one inequality that makes them safe (asserted in lease_test.go):
//
//	leaseSelfFence + leaseShutdown  <  leaseTTL + leaseStealAfter
//	          30s  +          9s    <      60s  +            15s
//
// A holder that can no longer renew stops writing leaseSelfFence after its last success and has
// finished shutting down leaseShutdown after that. Nobody may take its lease until leaseStealAfter
// past expiry. The 36s between those two is the margin that keeps two processes off one database,
// and it is why the TTL cannot simply be lowered to make failover faster.
//
// leaseSelfFence is measured against wall-clock time since the last successful renewal rather than
// counted in failures, so a wedged HTTP call or a stalled ticker cannot outrun it.
const (
	leaseTTL        = 60 * time.Second
	leaseRenew      = 10 * time.Second // three attempts per TTL: two consecutive GCS blips are survivable
	leaseSelfFence  = 30 * time.Second
	leaseShutdown   = 9 * time.Second // Cloud Run's ~10s SIGTERM grace, minus signal delivery
	leaseOpTimeout  = 5 * time.Second
	leasePoll       = 5 * time.Second
	leaseStealAfter = 15 * time.Second

	// Bounded, so a container that can never get the lease dies visibly instead of serving 503
	// forever. It must exceed leaseTTL+leaseStealAfter, or a predecessor that was SIGKILLed
	// without releasing would guarantee a crash loop rather than a handover.
	leaseGiveUp = 90 * time.Second
)

var (
	// errLeaseTaken is the only error that means we lost a race. Everything else is a network
	// problem, and conflating the two would restart the service on every GCS hiccup.
	errLeaseTaken   = errors.New("lease: held by another writer")
	errLeaseMissing = errors.New("lease: no such object")
)

// leaseObject is all the lease needs from object storage: reads that report a generation, and
// writes conditional on one. GCS in production (lease_gcs.go), a map in the tests — both must
// have identical precondition semantics or the tests prove nothing.
type leaseObject interface {
	read(ctx context.Context) (body []byte, gen int64, modified time.Time, err error)
	create(ctx context.Context, body []byte) (int64, error)             // must fail if it exists
	replace(ctx context.Context, body []byte, gen int64) (int64, error) // must fail unless generation matches
	remove(ctx context.Context, gen int64) error
	name() string
}

// LeaseInfo is what the lock object says. It is written for a human reading it during an incident,
// so it carries the revision and commit as well as the machine-readable timestamps.
type LeaseInfo struct {
	Holder   string    `json:"holder"`
	Revision string    `json:"revision"` // K_REVISION on Cloud Run
	Commit   string    `json:"commit"`
	DBPath   string    `json:"db_path"`
	Replica  string    `json:"replica"`
	PID      int       `json:"pid"`
	Acquired time.Time `json:"acquired_at"`
	Renewed  time.Time `json:"renewed_at"`
	Expires  time.Time `json:"expires_at"`
	TTLSec   int       `json:"ttl_seconds"`
}

type Lease struct {
	obj leaseObject
	now func() time.Time // test seam

	mu     sync.Mutex
	info   LeaseInfo
	gen    int64 // the generation we own; advances on every renew
	lastOK time.Time

	// Steal bookkeeping. An expired lease has to be seen twice at the same generation,
	// leaseStealAfter apart, before it counts as abandoned: a generation that moved between the
	// two reads is a live holder whose clock or payload we misread, not a corpse.
	seenGen int64
	seenAt  time.Time

	state atomic.Pointer[string]
	lost  chan struct{}
	once  sync.Once

	// onState mirrors the state onto /health, so an operator watching a deploy can see which
	// container owns the database. Set once, before Acquire.
	onState func(string)
}

func newLease(obj leaseObject, info LeaseInfo) *Lease {
	l := &Lease{obj: obj, now: time.Now, info: info, lost: make(chan struct{})}
	l.setState("starting")
	return l
}

// Lost closes when the lease is gone and this process has no business writing any more.
func (l *Lease) Lost() <-chan struct{} { return l.lost }

// State is a short phrase for /health and the startup logs.
func (l *Lease) State() string {
	if s := l.state.Load(); s != nil {
		return *s
	}
	return "unknown"
}

func (l *Lease) setState(s string) {
	l.state.Store(&s)
	if l.onState != nil {
		l.onState(s)
	}
}

func (l *Lease) markLost() {
	l.once.Do(func() {
		l.setState("lost")
		close(l.lost)
	})
}

// Held reports whether we currently hold the lease, and when it was last confirmed.
func (l *Lease) Held() (bool, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gen != 0, l.lastOK
}

// Acquire blocks until this process owns the database, ctx is done, or leaseGiveUp passes.
func (l *Lease) Acquire(ctx context.Context) error {
	deadline := l.now().Add(leaseGiveUp)
	var lastWarn time.Time
	for {
		ok, holder, err := l.try(ctx)
		switch {
		case err != nil && !errors.Is(err, errLeaseTaken):
			// A config error (no such bucket, no permission) will never come right by waiting,
			// but we cannot tell it apart from an outage here; the deadline covers both.
			if l.now().Sub(lastWarn) > 30*time.Second {
				slog.Warn("cannot reach the write lease", "object", l.obj.name(), "err", err)
				lastWarn = l.now()
			}
		case ok:
			slog.Info("acquired the write lease", "object", l.obj.name(), "holder", l.info.Holder, "ttl", leaseTTL)
			return nil
		default:
			l.setState("waiting for " + holder)
			if l.now().Sub(lastWarn) > 30*time.Second {
				slog.Warn("waiting for the write lease; another container still owns the database",
					"holder", holder, "object", l.obj.name())
				lastWarn = l.now()
			}
		}
		if l.now().After(deadline) {
			return fmt.Errorf("no write lease after %s (held by %s)", leaseGiveUp, holder)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(leasePoll + rand.N(2*time.Second)):
		}
	}
}

// try makes one attempt. It returns the current holder's description when it fails, for the logs.
func (l *Lease) try(ctx context.Context) (bool, string, error) {
	octx, cancel := context.WithTimeout(ctx, leaseOpTimeout)
	defer cancel()

	body, gen, modified, err := l.obj.read(octx)
	if errors.Is(err, errLeaseMissing) {
		return l.take(octx, 0)
	}
	if err != nil {
		return false, "unknown", err
	}

	var cur LeaseInfo
	if jsonErr := json.Unmarshal(body, &cur); jsonErr != nil {
		// An unreadable lock is still a lock: only its expiry can be trusted, and that comes
		// from GCS's own clock below. Never treat a parse failure as "vacant".
		slog.Warn("write lease object is not readable JSON; using its modification time only", "err", jsonErr)
	}
	who := cur.Revision
	if who == "" {
		who = cur.Holder
	}
	if who == "" {
		who = "an unidentified writer"
	}

	// Expiry off GCS's LastModified, which both containers see on one server clock; the payload's
	// own claim is honoured too when it is later, so a longer TTL can never be shortened by a
	// reader. Only "now" comes from the local clock.
	expires := modified.Add(leaseTTL)
	if cur.Expires.After(expires) {
		expires = cur.Expires
	}
	if expires.After(l.now()) {
		l.mu.Lock()
		if gen != l.seenGen {
			l.seenGen, l.seenAt = gen, l.now()
		}
		l.mu.Unlock()
		return false, who, nil
	}

	// Expired. Confirm it is not moving before taking it.
	l.mu.Lock()
	if gen != l.seenGen {
		l.seenGen, l.seenAt = gen, l.now()
		l.mu.Unlock()
		return false, who + " (expired; confirming)", nil
	}
	stable := l.now().Sub(l.seenAt)
	l.mu.Unlock()
	if stable < leaseStealAfter {
		return false, who + " (expired; confirming)", nil
	}
	slog.Warn("taking over an expired write lease", "previous", who, "expired", l.now().Sub(expires).Truncate(time.Second))
	return l.take(octx, gen)
}

// take writes the lock object: created when gen is 0, replaced conditionally otherwise.
func (l *Lease) take(ctx context.Context, gen int64) (bool, string, error) {
	now := l.now()
	l.mu.Lock()
	l.info.Acquired, l.info.Renewed = now, now
	l.info.Expires = now.Add(leaseTTL)
	l.info.TTLSec = int(leaseTTL / time.Second)
	body, err := json.Marshal(l.info)
	l.mu.Unlock()
	if err != nil {
		return false, "unknown", err
	}

	var newGen int64
	if gen == 0 {
		newGen, err = l.obj.create(ctx, body)
	} else {
		newGen, err = l.obj.replace(ctx, body, gen)
	}
	if errors.Is(err, errLeaseTaken) {
		return false, "another container", nil // lost the race; the next poll will report who won
	}
	if err != nil {
		return false, "unknown", err
	}
	l.mu.Lock()
	l.gen, l.lastOK = newGen, now
	l.mu.Unlock()
	l.setState("held")
	return true, "", nil
}

// Run renews the lease until ctx is done or the lease is gone. Losing it closes Lost().
func (l *Lease) Run(ctx context.Context) {
	t := time.NewTicker(leaseRenew)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := l.renew(ctx)
		switch {
		case err == nil:
		case errors.Is(err, errLeaseTaken):
			slog.Error("the write lease was taken by another container; this process no longer owns the database")
			l.markLost()
			return
		case errors.Is(err, context.Canceled):
			return
		default:
			_, last := l.Held()
			idle := l.now().Sub(last)
			if idle > leaseSelfFence {
				slog.Error("could not renew the write lease; standing down before anyone can take it",
					"since_last_renewal", idle.Truncate(time.Second), "err", err)
				l.markLost()
				return
			}
			slog.Warn("write lease renewal failed; retrying", "since_last_renewal", idle.Truncate(time.Second), "err", err)
		}
	}
}

func (l *Lease) renew(ctx context.Context) error {
	octx, cancel := context.WithTimeout(ctx, leaseOpTimeout)
	defer cancel()

	now := l.now()
	l.mu.Lock()
	gen := l.gen
	l.info.Renewed = now
	l.info.Expires = now.Add(leaseTTL)
	body, err := json.Marshal(l.info)
	l.mu.Unlock()
	if err != nil {
		return err
	}
	if gen == 0 {
		return errLeaseTaken // never held, or already stood down
	}
	newGen, err := l.obj.replace(octx, body, gen)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.gen, l.lastOK = newGen, now
	l.mu.Unlock()
	l.setState("held")
	return nil
}

// Release drops the lease on a clean shutdown, so a successor starts in one poll instead of
// waiting out the TTL. It is conditional on our own generation: after we have lost the lease the
// object belongs to someone else, and deleting it would evict a healthy writer.
//
// This is also the honest limit of the whole mechanism. The lease is advisory — GCS will accept
// writes from anyone holding the service account, so a process paused past its own self-fence
// could in principle write once more before noticing. The margin above makes that improbable, not
// impossible; only a shared transactional database removes it.
func (l *Lease) Release(ctx context.Context) error {
	l.mu.Lock()
	gen := l.gen
	l.gen = 0
	l.mu.Unlock()
	if gen == 0 {
		return nil
	}
	octx, cancel := context.WithTimeout(ctx, leaseOpTimeout)
	defer cancel()
	err := l.obj.remove(octx, gen)
	if errors.Is(err, errLeaseTaken) || errors.Is(err, errLeaseMissing) {
		return nil // someone else already owns it; leave theirs alone
	}
	return err
}
