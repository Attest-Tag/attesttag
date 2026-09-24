package app

import (
	"context"
	"strings"
	"testing"
	"time"
)

func bucketInitTarget(url string) *replicaTarget {
	return &replicaTarget{s3: true, bucket: "bucket", key: "docs", endpoint: url,
		region: "us-east-1", keyID: "k", secret: "s"}
}

func TestBucketInitMakesTheBucketAndLeavesItEmpty(t *testing.T) {
	store, url := newFakeS3(t)
	if err := bucketInit(context.Background(), bucketInitTarget(url), "docs", 0, true); err != nil {
		t.Fatalf("bucket-init: %v", err)
	}
	if !store.bucketExists {
		t.Error("the bucket was not created")
	}
	// Everything it wrote to prove the credentials — the read-write probe and the lease probe —
	// is its own and must be cleaned up: this runs against a bucket that already holds documents.
	if keys := store.keys(); len(keys) != 0 {
		t.Errorf("bucket-init left objects behind: %v", keys)
	}
	// Idempotent: it is an initContainer and will run again on every restart.
	if err := bucketInit(context.Background(), bucketInitTarget(url), "docs", 0, false); err != nil {
		t.Fatalf("second run against the bucket it made: %v", err)
	}
}

func TestBucketInitWillNotInventABucketWhenToldNotTo(t *testing.T) {
	_, url := newFakeS3(t)
	err := bucketInit(context.Background(), bucketInitTarget(url), "docs", 0, false)
	if err == nil || !strings.Contains(err.Error(), "-create=false") {
		t.Fatalf("err = %v, want it to say the bucket is missing and it was told not to create one", err)
	}
}

// A store without conditional writes is a deployment constraint, not a broken bucket: the bot
// runs there with one instance, so this reports it and succeeds.
func TestBucketInitReportsAStoreThatCannotFenceAWriter(t *testing.T) {
	store, url := newFakeS3(t)
	store.ignorePreconditions = true
	if err := bucketInit(context.Background(), bucketInitTarget(url), "docs", 0, true); err != nil {
		t.Fatalf("bucket-init: %v", err)
	}
	if keys := store.keys(); len(keys) != 0 {
		t.Errorf("bucket-init left objects behind: %v", keys)
	}
}

// Started beside a store that is still booting — compose, an initContainer — it waits rather
// than failing on the first refused connection.
func TestBucketInitWaitsForAStoreThatIsNotUpYet(t *testing.T) {
	target := bucketInitTarget("http://127.0.0.1:1") // nothing listens on port 1
	start := time.Now()
	err := bucketInit(context.Background(), target, "docs", 750*time.Millisecond, true)
	if err == nil || !strings.Contains(err.Error(), "cannot reach the store") {
		t.Fatalf("err = %v, want it to give up saying it cannot reach the store", err)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Errorf("gave up after %v without waiting", time.Since(start))
	}
}
