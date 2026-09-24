package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

// TestLeaseAgainstGCSLive checks the one thing the in-memory fake cannot: that GCS's conditional
// writes actually map onto the precondition semantics the lease depends on. If a 412 ever stopped
// arriving as errLeaseTaken the lease would fail open — two containers would both believe they had
// it — and every other test here would still pass.
//
//	LEASE_LIVE_BUCKET=some-scratch-bucket go test ./internal/app/ -run Live
func TestLeaseAgainstGCSLive(t *testing.T) {
	bucket := os.Getenv("LEASE_LIVE_BUCKET")
	if bucket == "" {
		t.Skip("set LEASE_LIVE_BUCKET=… to run against real GCS")
	}
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		t.Fatalf("storage client: %v", err)
	}
	defer client.Close()

	object := "attesttag-test/lease-" + time.Now().UTC().Format("20060102T150405") + ".lock.json"
	obj := &gcsLeaseObject{client: client, bucket: bucket, object: object}
	t.Cleanup(func() {
		if err := client.Bucket(bucket).Object(object).Delete(context.Background()); err != nil &&
			!errors.Is(err, storage.ErrObjectNotExist) {
			t.Logf("could not clean up %s: %v", obj.name(), err)
		}
	})

	// Absent: a read says so rather than returning an empty lease.
	if _, _, _, err := obj.read(ctx); !errors.Is(err, errLeaseMissing) {
		t.Fatalf("read of an absent lease = %v, want errLeaseMissing", err)
	}

	gen, err := obj.create(ctx, []byte(`{"holder":"a"}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The race a deploy actually runs: a second container creating the same object must lose.
	if _, err := obj.create(ctx, []byte(`{"holder":"b"}`)); !errors.Is(err, errLeaseTaken) {
		t.Fatalf("a second create succeeded or failed wrongly: %v — the lease would fail open", err)
	}
	// And a write against a stale generation must lose too, which is what renewal relies on.
	if _, err := obj.replace(ctx, []byte(`{"holder":"b"}`), gen-1); !errors.Is(err, errLeaseTaken) {
		t.Fatalf("replace on a stale generation = %v, want errLeaseTaken", err)
	}

	next, err := obj.replace(ctx, []byte(`{"holder":"a"}`), gen)
	if err != nil {
		t.Fatalf("replace on our own generation: %v", err)
	}
	if next == gen {
		t.Fatalf("generation did not advance on write: %d", next)
	}
	body, readGen, modified, err := obj.read(ctx)
	if err != nil || readGen != next || string(body) != `{"holder":"a"}` {
		t.Fatalf("read back = %q gen=%d err=%v", body, readGen, err)
	}
	if time.Since(modified) > time.Hour {
		t.Fatalf("LastModified looks wrong (%v); the lease judges expiry by it", modified)
	}

	// Releasing on a stale generation must not delete someone else's lease.
	if err := obj.remove(ctx, next-1); !errors.Is(err, errLeaseTaken) {
		t.Fatalf("remove on a stale generation = %v, want errLeaseTaken", err)
	}
	if err := obj.remove(ctx, next); err != nil {
		t.Fatalf("remove on our own generation: %v", err)
	}
}
