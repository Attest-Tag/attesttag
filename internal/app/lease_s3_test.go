package app

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The fake store is the one in docs_s3_test.go: it signs-checks every request and enforces the
// two conditional headers, which is exactly the surface this lease is built on.

// Built the way the bot builds it — through the target — so the test cannot drift from what
// production does with the same bucket.
func s3LeaseFor(endpoint string) *s3LeaseObject {
	target := &replicaTarget{s3: true, bucket: "bucket", key: "docs/litestream/attesttag.db",
		endpoint: endpoint, region: "auto", keyID: "k", secret: "s"}
	return &s3LeaseObject{s3Signer: target.signer(), key: target.leaseKey(), etags: map[int64]string{}}
}

func TestAnAbsentLockReadsAsMissingRatherThanAnError(t *testing.T) {
	_, url := newFakeS3(t)
	if _, _, _, err := s3LeaseFor(url).read(context.Background()); !errors.Is(err, errLeaseMissing) {
		t.Fatalf("read of an absent lock = %v, want errLeaseMissing", err)
	}
}

// The property the whole mechanism rests on: two processes, one key, and only one of them may
// come away believing it holds the lease.
func TestTwoWritersCannotBothCreateTheLock(t *testing.T) {
	ctx := context.Background()
	_, url := newFakeS3(t)
	a, b := s3LeaseFor(url), s3LeaseFor(url)

	genA, err := a.create(ctx, []byte(`{"holder":"a"}`))
	if err != nil {
		t.Fatalf("a.create: %v", err)
	}
	if genA == 0 {
		t.Fatal("generation 0 means 'we do not hold this' to lease.go; create must never return it")
	}
	if _, err := b.create(ctx, []byte(`{"holder":"b"}`)); !errors.Is(err, errLeaseTaken) {
		t.Fatalf("b.create = %v, want errLeaseTaken", err)
	}

	// b reads what a wrote, and may not replace it once a has renewed: its generation is stale.
	_, genB, _, err := b.read(ctx)
	if err != nil {
		t.Fatalf("b.read: %v", err)
	}
	if genB != genA {
		t.Fatalf("b read generation %d, a wrote %d — the same object must have the same generation", genB, genA)
	}
	if _, err := a.replace(ctx, []byte(`{"holder":"a","renewed":1}`), genA); err != nil {
		t.Fatalf("a.replace on its own generation: %v", err)
	}
	if _, err := b.replace(ctx, []byte(`{"holder":"b"}`), genB); !errors.Is(err, errLeaseTaken) {
		t.Fatalf("b.replace on a stale generation = %v, want errLeaseTaken", err)
	}
}

func TestReleaseWillNotDeleteALockSomebodyElseNowHolds(t *testing.T) {
	ctx := context.Background()
	store, url := newFakeS3(t)
	a, b := s3LeaseFor(url), s3LeaseFor(url)

	genA, err := a.create(ctx, []byte(`{"holder":"a"}`))
	if err != nil {
		t.Fatalf("a.create: %v", err)
	}
	// b takes it over — an expired lease being stolen, from lease.go's point of view.
	_, gen, _, err := b.read(ctx)
	if err != nil {
		t.Fatalf("b.read: %v", err)
	}
	if _, err := b.replace(ctx, []byte(`{"holder":"b"}`), gen); err != nil {
		t.Fatalf("b.replace: %v", err)
	}
	if err := a.remove(ctx, genA); !errors.Is(err, errLeaseTaken) {
		t.Fatalf("a.remove after losing the lease = %v, want errLeaseTaken", err)
	}
	if len(store.keys()) != 1 {
		t.Fatalf("the lock was deleted out from under its holder: %v", store.keys())
	}
	if err := b.remove(ctx, gen+0); err != nil && !errors.Is(err, errLeaseTaken) {
		t.Fatalf("b.remove: %v", err)
	}
}

func TestTheProbeAcceptsAStoreThatEnforcesPreconditionsAndLeavesNothingBehind(t *testing.T) {
	store, url := newFakeS3(t)
	obj := s3LeaseFor(url)
	if err := obj.verifyConditionalWrites(context.Background()); err != nil {
		t.Fatalf("verifyConditionalWrites: %v", err)
	}
	if keys := store.keys(); len(keys) != 0 {
		t.Errorf("the probe left objects behind: %v", keys)
	}
}

// The case this exists for: a store that takes the conditional header and writes anyway would
// hand the same lease to two containers and tell both they hold it.
func TestTheProbeRefusesAStoreThatIgnoresPreconditions(t *testing.T) {
	store, url := newFakeS3(t)
	store.ignorePreconditions = true
	obj := s3LeaseFor(url)
	err := obj.verifyConditionalWrites(context.Background())
	if !errors.Is(err, errNoConditionalWrites) {
		t.Fatalf("verifyConditionalWrites = %v, want errNoConditionalWrites", err)
	}
	if keys := store.keys(); len(keys) != 0 {
		t.Errorf("the probe left objects behind: %v", keys)
	}
}

// Half-implemented is its own hazard: taking the lease would be fenced and every renewal after
// it would not, so a stale holder could overwrite a live one.
func TestTheProbeRefusesAStoreThatEnforcesOnlyHalfOfIt(t *testing.T) {
	store, url := newFakeS3(t)
	store.ignoreIfMatch = true
	err := s3LeaseFor(url).verifyConditionalWrites(context.Background())
	if !errors.Is(err, errNoConditionalWrites) {
		t.Fatalf("verifyConditionalWrites = %v, want errNoConditionalWrites", err)
	}
	if !strings.Contains(err.Error(), "If-Match") {
		t.Errorf("the error does not say which half is missing: %v", err)
	}
	if keys := store.keys(); len(keys) != 0 {
		t.Errorf("the probe left objects behind: %v", keys)
	}
}

// Two containers starting at the same second must not read each other's probe as proof of a
// precondition neither store made.
func TestConcurrentProbesDoNotCollide(t *testing.T) {
	_, url := newFakeS3(t)
	errs := make(chan error, 4)
	for range cap(errs) {
		go func() { errs <- s3LeaseFor(url).verifyConditionalWrites(context.Background()) }()
	}
	for range cap(errs) {
		if err := <-errs; err != nil {
			t.Fatalf("a concurrent probe failed: %v", err)
		}
	}
}

// And the whole state machine over it: lease.go, unchanged, driving the S3 object. This is the
// same shape as TestSecondContenderIsRefused over the in-memory one.
func TestTheLeaseStateMachineRunsOnS3(t *testing.T) {
	ctx := context.Background()
	_, url := newFakeS3(t)
	first := newLease(s3LeaseFor(url), LeaseInfo{Holder: "first", TTLSec: int(leaseTTL.Seconds())})
	second := newLease(s3LeaseFor(url), LeaseInfo{Holder: "second", TTLSec: int(leaseTTL.Seconds())})

	if ok, _, err := first.try(ctx); err != nil || !ok {
		t.Fatalf("first.try = %v, %v; want it to take the lease", ok, err)
	}
	ok, holder, err := second.try(ctx)
	if err != nil || ok {
		t.Fatalf("second.try = %v, %v; want it refused", ok, err)
	}
	if holder != "first" {
		t.Errorf("the refusal names %q as the holder, want %q", holder, "first")
	}
	if held, _ := second.Held(); held {
		t.Error("the second lease believes it holds the database")
	}

	// A renewal is a conditional replace on our own generation, and must not read as a theft.
	if err := first.renew(ctx); err != nil {
		t.Fatalf("first.renew: %v", err)
	}
	if held, _ := first.Held(); !held {
		t.Error("the holder lost its own lease by renewing it")
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("first.Release: %v", err)
	}
	if ok, _, err := second.try(ctx); err != nil || !ok {
		t.Fatalf("second.try after release = %v, %v; want the handover", ok, err)
	}
}
