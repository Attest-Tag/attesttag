package app

// The lease's storage, on GCS. Everything here is one of four conditional operations; the
// decisions about when to make them live in lease.go, which imports no cloud library so its tests
// can run against a map.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

type gcsLeaseObject struct {
	client *storage.Client
	bucket string
	object string
}

func (g *gcsLeaseObject) name() string { return "gs://" + g.bucket + "/" + g.object }

func (g *gcsLeaseObject) handle() *storage.ObjectHandle {
	return g.client.Bucket(g.bucket).Object(g.object)
}

// classify keeps one rule in one place: a failed precondition means we lost a race, and nothing
// else does. A 429 is GCS rate-limiting the object, which is a retry, not a loss.
func classifyLeaseErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return errLeaseMissing
	}
	var ae *googleapi.Error
	if errors.As(err, &ae) && (ae.Code == http.StatusPreconditionFailed || ae.Code == http.StatusConflict) {
		return errLeaseTaken
	}
	return err
}

func (g *gcsLeaseObject) read(ctx context.Context) ([]byte, int64, time.Time, error) {
	r, err := g.handle().NewReader(ctx)
	if err != nil {
		return nil, 0, time.Time{}, classifyLeaseErr(err)
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	return body, r.Attrs.Generation, r.Attrs.LastModified, nil
}

func (g *gcsLeaseObject) write(ctx context.Context, body []byte, c storage.Conditions) (int64, error) {
	w := g.handle().If(c).NewWriter(ctx)
	w.ContentType = "application/json"
	w.CacheControl = "no-store"
	if _, err := w.Write(body); err != nil {
		w.Close()
		return 0, classifyLeaseErr(err)
	}
	if err := w.Close(); err != nil { // the precondition is reported here, not on Write
		return 0, classifyLeaseErr(err)
	}
	return w.Attrs().Generation, nil
}

func (g *gcsLeaseObject) create(ctx context.Context, body []byte) (int64, error) {
	return g.write(ctx, body, storage.Conditions{DoesNotExist: true})
}

func (g *gcsLeaseObject) replace(ctx context.Context, body []byte, gen int64) (int64, error) {
	return g.write(ctx, body, storage.Conditions{GenerationMatch: gen})
}

func (g *gcsLeaseObject) remove(ctx context.Context, gen int64) error {
	return classifyLeaseErr(g.handle().If(storage.Conditions{GenerationMatch: gen}).Delete(ctx))
}

// leaseObjectName sits beside the replica rather than inside it: a sibling key is never touched by
// Litestream's own retention sweep, and deriving it from LITESTREAM_PATH means it inherits the
// convention that pointing at a new replica prefix asks for a fresh database (deploy/gcp/cloudrun.sh).
func leaseObjectName(replicaPath string) string {
	if replicaPath == "" {
		return "attesttag.db.lock.json"
	}
	return strings.TrimSuffix(replicaPath, "/") + ".lock.json"
}

// newGCSLease builds the lease for this process. The holder id has to be unique per process and
// stable for its lifetime; the revision and commit are there so a human reading the lock object
// during a deploy can tell which container is holding it.
func newGCSLease(ctx context.Context, cfg Config, t *replicaTarget) (*Lease, error) {
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	obj := &gcsLeaseObject{client: c, bucket: t.bucket, object: t.leaseKey()}

	host, _ := os.Hostname()
	rev := os.Getenv("K_REVISION")
	holder := rev
	if holder == "" {
		holder = host
	}
	info := LeaseInfo{
		Holder:   holder + "/" + strconv.Itoa(os.Getpid()),
		Revision: rev,
		Commit:   os.Getenv("GIT_COMMIT"),
		DBPath:   cfg.DBPath,
		Replica:  t.String(),
		PID:      os.Getpid(),
	}
	return newLease(obj, info), nil
}

// newReplicaLease builds the lease for whichever bucket this deployment resolved. The two
// implementations differ only in how a precondition is spelled; everything above them — when to
// take, renew, steal or release — is lease.go, and is the same for both.
func newReplicaLease(ctx context.Context, cfg Config, t *replicaTarget) (*Lease, error) {
	if t.s3 {
		return newS3Lease(ctx, cfg, t)
	}
	return newGCSLease(ctx, cfg, t)
}
