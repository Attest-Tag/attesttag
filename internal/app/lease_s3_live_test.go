package app

// The two things the fake store cannot prove: that a real S3 implementation's conditional
// writes mean what lease_s3.go believes they mean, and that the config replica_target.go
// renders is one the actual Litestream binary can replicate and restore with.
//
// Both are gated, because a fresh clone must pass `go test ./...` with nothing running. MinIO in
// a container is the cheapest real store, and the same variables point at R2, B2 or AWS:
//
//	docker run -d --rm -p 9010:9000 -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
//	  quay.io/minio/minio server /data
//	LIVE_S3_URL="s3://attesttag-test/live?endpoint=http://127.0.0.1:9010&region=us-east-1" \
//	LIVE_S3_KEY_ID=minioadmin LIVE_S3_SECRET=minioadmin go test ./internal/app/ -run Live -v

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveS3Target resolves the store under test and gives this run a key of its own, so two runs —
// or a run against a bucket somebody else is using — cannot collide.
func liveS3Target(t *testing.T) *replicaTarget {
	t.Helper()
	raw := os.Getenv("LIVE_S3_URL")
	if raw == "" {
		t.Skip("set LIVE_S3_URL=… (with LIVE_S3_KEY_ID and LIVE_S3_SECRET) to run against a real S3-compatible store")
	}
	loc, err := parseS3URL(raw, "LIVE_S3_URL")
	if err != nil {
		t.Fatalf("LIVE_S3_URL: %v", err)
	}
	target := &replicaTarget{
		s3: true, bucket: loc.bucket, endpoint: loc.endpoint, region: loc.region,
		key:    filepath.Join(loc.prefix, fmt.Sprintf("run-%d", time.Now().UnixNano()), "attesttag.db"),
		keyID:  os.Getenv("LIVE_S3_KEY_ID"),
		secret: os.Getenv("LIVE_S3_SECRET"),
	}
	// The bucket may not exist yet on a store brought up for this test, and making it is what
	// `attesttag bucket-init` is for — so the live test runs the real thing rather than a
	// convenience of its own, and the command gets exercised against a real store too.
	if err := bucketInit(context.Background(), target, loc.prefix, 30*time.Second, true); err != nil {
		t.Fatalf("bucket-init against %s: %v", target, err)
	}
	obj := liveLeaseObject(target)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, key := range []string{target.key, target.leaseKey()} {
			if r, err := obj.do(ctx, http.MethodDelete, key, nil, nil); err == nil {
				r.Body.Close()
			}
		}
	})
	return target
}

func liveLeaseObject(t *replicaTarget) *s3LeaseObject {
	return &s3LeaseObject{s3Signer: t.signer(), key: t.leaseKey(), etags: map[int64]string{}}
}

// TestLeaseAgainstS3Live is the GCS live test's counterpart: if a refused precondition ever
// stopped arriving as errLeaseTaken the lease would fail open — two containers would both
// believe they held the database — and every fake-backed test here would still pass.
func TestLeaseAgainstS3Live(t *testing.T) {
	target := liveS3Target(t)
	ctx := context.Background()
	a, b := liveLeaseObject(target), liveLeaseObject(target)

	if err := a.verifyConditionalWrites(ctx); err != nil {
		t.Fatalf("this store cannot fence a writer: %v", err)
	}
	if _, _, _, err := a.read(ctx); !errors.Is(err, errLeaseMissing) {
		t.Fatalf("read of an absent lock = %v, want errLeaseMissing", err)
	}

	genA, err := a.create(ctx, []byte(`{"holder":"a"}`))
	if err != nil {
		t.Fatalf("a.create: %v", err)
	}
	if _, err := b.create(ctx, []byte(`{"holder":"b"}`)); !errors.Is(err, errLeaseTaken) {
		t.Fatalf("b.create against a held lock = %v, want errLeaseTaken", err)
	}

	_, genB, modified, err := b.read(ctx)
	if err != nil {
		t.Fatalf("b.read: %v", err)
	}
	if genB != genA {
		t.Errorf("the same object read as generation %d and written as %d", genB, genA)
	}
	if time.Since(modified) > time.Hour {
		t.Errorf("Last-Modified came back as %v; the steal timer is read off it", modified)
	}

	renewed, err := a.replace(ctx, []byte(`{"holder":"a","renewed":1}`), genA)
	if err != nil {
		t.Fatalf("a.replace on its own generation: %v", err)
	}
	if renewed == genA {
		t.Error("a renewal did not advance the generation, so a lost lease would look like a live one")
	}
	if _, err := b.replace(ctx, []byte(`{"holder":"b"}`), genB); !errors.Is(err, errLeaseTaken) {
		t.Fatalf("b.replace on a stale generation = %v, want errLeaseTaken", err)
	}
	if err := b.remove(ctx, genB); !errors.Is(err, errLeaseTaken) {
		t.Fatalf("b.remove on a stale generation = %v, want errLeaseTaken", err)
	}
	if err := a.remove(ctx, renewed); err != nil {
		t.Fatalf("a.remove of its own lock: %v", err)
	}
	if _, _, _, err := a.read(ctx); !errors.Is(err, errLeaseMissing) {
		t.Fatalf("after release the lock reads as %v, want errLeaseMissing", err)
	}
}

// TestReplicationAgainstS3Live is the round trip: the rendered config, the real Litestream, a
// real bucket, and a restore that has to come back with every row. It is the whole promise of
// "give it a bucket and the database is safe", tested rather than asserted.
func TestReplicationAgainstS3Live(t *testing.T) {
	target := liveS3Target(t)
	bin, err := exec.LookPath(env("LITESTREAM_BIN", "litestream"))
	if err != nil {
		t.Skip("no litestream on PATH")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "attesttag.db")
	db, err := sql.Open(parseDSNForTest(dbPath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`create table notes (id integer primary key, body text)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	cfg := Config{DBPath: dbPath, LitestreamBin: bin, Replica: target}
	rep, err := newReplicator(cfg)
	if err != nil {
		t.Fatalf("newReplicator: %v", err)
	}
	if err := rep.Start(); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	for i := range 200 {
		if _, err := db.Exec(`insert into notes (body) values (?)`, fmt.Sprintf("row %d", i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// Wait for the replica to actually hold something. Litestream syncs on its own interval, and
	// stopping it half a second after it started would test that a process can be spawned rather
	// than that a database is replicated.
	waitForReplica(t, bin, rep.conf, dbPath, target)

	// The database is closed before the final sync, exactly as bot.go shuts down, so what the
	// replica holds is everything this process ever wrote.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := rep.Stop(30 * time.Second); err != nil {
		t.Fatalf("final sync: %v", err)
	}

	// Restore somewhere else and count. A config of our own, because Stop removed the generated
	// one — this is also what an operator does by hand during an incident.
	conf := filepath.Join(dir, "litestream.yml")
	if err := os.WriteFile(conf, []byte(target.litestreamYAML(dbPath)), 0o600); err != nil {
		t.Fatalf("writing the restore config: %v", err)
	}
	restored := filepath.Join(dir, "restored.db")
	cmd := exec.Command(bin, "restore", "-config", conf, "-o", restored, dbPath)
	cmd.Env = append(os.Environ(), target.childEnv()...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("litestream restore: %v\n%s", err, out)
	}

	back, err := sql.Open(parseDSNForTest(restored))
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer back.Close()
	var n int
	if err := back.QueryRow(`select count(*) from notes`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 200 {
		t.Fatalf("the restored database has %d rows, want 200", n)
	}
}

// waitForReplica blocks until the replica lists at least one LTX file. `litestream ltx` prints a
// header line and nothing else when the replica is empty, so two lines is the condition.
func waitForReplica(t *testing.T, bin, conf, dbPath string, target *replicaTarget) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		cmd := exec.Command(bin, "ltx", "-config", conf, dbPath)
		cmd.Env = append(os.Environ(), target.childEnv()...)
		out, err := cmd.CombinedOutput()
		if err == nil && len(strings.Split(strings.TrimSpace(string(out)), "\n")) > 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing reached %s within 30s: %v\n%s", target, err, out)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// parseDSNForTest opens a SQLite file the way the store does, WAL included — Litestream
// replicates the write-ahead log and has nothing to read without it.
func parseDSNForTest(path string) (string, string) {
	driver, conn, _ := parseDSN(path)
	return driver, conn
}
