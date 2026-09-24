package app

// The lease's storage, on any S3-compatible bucket. The decisions live in lease.go, which knows
// nothing about either cloud; this is the same four conditional operations as lease_gcs.go,
// expressed in the two headers S3 has for them.
//
// GCS gives every object a generation number and preconditions on it. S3 gives an ETag and two
// conditional headers — `If-None-Match: *` on a PUT means create-only, and `If-Match: "<etag>"`
// means replace-this-exact-version — which are the same two preconditions under other names.
// The generation lease.go passes around is therefore a hash of the ETag, mapped back to the
// literal ETag here, because an ETag is a string and the interface speaks int64.
//
// The part worth being suspicious of is that conditional writes were late to S3 (AWS shipped
// If-Match on PUT at the end of 2024) and some compatible stores still do not implement them —
// and a store that *ignores* the header rather than refusing it would hand the same lease to two
// writers while telling both they hold it. So this does not take the store's word for it:
// verifyConditionalWrites proves the precondition on a scratch key at startup, and a store that
// fails the proof gets replication without a lease rather than a lease that is a lie.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errNoConditionalWrites is not a failure of this bucket — it is a fact about it. bot.go turns it
// into a warning and one instance, not an exit.
var errNoConditionalWrites = errors.New("lease: this object store does not honour conditional writes")

// s3Signer is one bucket and a key to sign with: the whole of what talking to an S3-compatible
// store takes once signAWSv4 exists. The lease is built on it, and so is `attesttag bucket-init`.
type s3Signer struct {
	client           *http.Client
	endpoint, bucket string
	cred             *Secret
}

type s3LeaseObject struct {
	s3Signer
	key string

	// The ETag each generation stands for. Bounded because only the most recent few are ever
	// asked for: lease.go conditions on what it just read or just wrote.
	mu    sync.Mutex
	etags map[int64]string
}

func (s *s3LeaseObject) name() string { return "s3://" + s.bucket + "/" + s.key }

// s3Gen turns an ETag into the generation number lease.go compares. It must never return 0:
// gen == 0 is how lease.go says "we do not hold this".
func s3Gen(etag string) int64 {
	h := fnv.New64a()
	io.WriteString(h, strings.Trim(etag, `"`))
	return int64(h.Sum64()>>1) | 1
}

func (s *s3LeaseObject) remember(etag string) int64 {
	gen := s3Gen(etag)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.etags) > 8 { // the old ones can no longer be the subject of a precondition
		s.etags = map[int64]string{}
	}
	s.etags[gen] = strings.Trim(etag, `"`)
	return gen
}

func (s *s3LeaseObject) etag(gen int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.etags[gen]
	return e, ok
}

// do signs and runs one request against a key, or against the bucket itself when key is empty.
// It is deliberately the same shape as s3Docs.do — path-style addressing, one signer, no SDK.
func (s *s3Signer) do(ctx context.Context, method, key string, body []byte, hdr map[string]string) (*http.Response, error) {
	u := s.endpoint + "/" + s.bucket
	if key != "" {
		// Each segment is escaped, but the separators are not: a key is a path, not one name.
		parts := strings.Split(key, "/")
		for i, p := range parts {
			parts[i] = url.PathEscape(p)
		}
		u += "/" + strings.Join(parts, "/")
	}
	req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	if err := signAWSv4(req, body, s.cred, time.Now()); err != nil {
		return nil, err
	}
	return s.client.Do(req)
}

// classifyS3Lease is the one place that decides what a status code means, and it keeps the same
// rule lease_gcs.go keeps: only a refused precondition is a lost race. 404 is a missing object,
// 429 and 5xx are the network having a bad minute, and everything else is reported as itself.
func classifyS3Lease(resp *http.Response, what string) error {
	switch resp.StatusCode {
	case http.StatusNotFound:
		resp.Body.Close()
		return errLeaseMissing
	case http.StatusPreconditionFailed, http.StatusConflict:
		resp.Body.Close()
		return errLeaseTaken
	}
	return fmt.Errorf("lease %s: %s: %s", what, resp.Status, drain(resp))
}

func ok2xx(code int) bool { return code >= 200 && code < 300 }

func (s *s3LeaseObject) read(ctx context.Context) ([]byte, int64, time.Time, error) {
	resp, err := s.do(ctx, http.MethodGet, s.key, nil, nil)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	if !ok2xx(resp.StatusCode) {
		return nil, 0, time.Time{}, classifyS3Lease(resp, "read")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		// Without an ETag there is no precondition to make, and a lease that cannot condition
		// its writes is not a lease. Fail rather than proceed unfenced.
		return nil, 0, time.Time{}, errors.New("lease read: the store returned no ETag")
	}
	modified, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	return body, s.remember(etag), modified, nil
}

func (s *s3LeaseObject) put(ctx context.Context, key string, body []byte, cond map[string]string) (int64, error) {
	hdr := map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"}
	for k, v := range cond {
		hdr[k] = v
	}
	resp, err := s.do(ctx, http.MethodPut, key, body, hdr)
	if err != nil {
		return 0, err
	}
	if !ok2xx(resp.StatusCode) {
		return 0, classifyS3Lease(resp, "write")
	}
	etag := resp.Header.Get("ETag")
	resp.Body.Close()
	if etag == "" {
		// Rare, and recoverable: ask for the version we have just written rather than guess.
		head, err := s.do(ctx, http.MethodHead, key, nil, nil)
		if err != nil {
			return 0, err
		}
		etag = head.Header.Get("ETag")
		head.Body.Close()
		if etag == "" {
			return 0, errors.New("lease write: the store returned no ETag")
		}
	}
	return s.remember(etag), nil
}

func (s *s3LeaseObject) create(ctx context.Context, body []byte) (int64, error) {
	return s.put(ctx, s.key, body, map[string]string{"If-None-Match": "*"})
}

func (s *s3LeaseObject) replace(ctx context.Context, body []byte, gen int64) (int64, error) {
	etag, ok := s.etag(gen)
	if !ok {
		// We are being asked to condition on a version we never saw. Refusing is the safe
		// answer: an unconditional write here is precisely the overwrite the lease exists to
		// prevent.
		return 0, fmt.Errorf("lease write: no ETag for generation %d", gen)
	}
	return s.put(ctx, s.key, body, map[string]string{"If-Match": `"` + etag + `"`})
}

// remove deletes the lock on a clean release. S3's conditional delete is the one precondition
// that is not portable — AWS ignores If-Match on DELETE, R2 honours it — so the generation is
// checked with a read first and the header sent anyway, for the stores that enforce it. The
// unclosable gap between that read and the delete is milliseconds wide at a clean shutdown, and
// it is why Release is conditional rather than trusted: a successor that waits out the TTL is
// the correct behaviour if this ever loses the race.
func (s *s3LeaseObject) remove(ctx context.Context, gen int64) error {
	_, cur, _, err := s.read(ctx)
	if err != nil {
		return err
	}
	if cur != gen {
		return errLeaseTaken
	}
	etag, _ := s.etag(gen)
	resp, err := s.do(ctx, http.MethodDelete, s.key, nil, map[string]string{"If-Match": `"` + etag + `"`})
	if err != nil {
		return err
	}
	if !ok2xx(resp.StatusCode) && resp.StatusCode != http.StatusNoContent {
		return classifyS3Lease(resp, "remove")
	}
	resp.Body.Close()
	return nil
}

// verifyConditionalWrites proves on a scratch key that `If-None-Match: *` is enforced rather
// than ignored. It doubles as the permission check — PUT and DELETE on this prefix — so a
// misconfigured key fails here, at startup, with the bucket named, instead of an hour later.
func (s *s3LeaseObject) verifyConditionalWrites(ctx context.Context) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	// Unique per process: two containers starting together must not probe the same key, or one
	// would read the other's probe as proof of a precondition it never made.
	probe := s.key + ".probe-" + hex.EncodeToString(b[:])
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseOpTimeout)
		defer cancel()
		if resp, err := s.do(dctx, http.MethodDelete, probe, nil, nil); err == nil {
			resp.Body.Close()
		}
	}()

	// Both preconditions, because they fence different moments and a store can have one
	// without the other: If-None-Match is how the lease is taken, If-Match is how every
	// renewal keeps it. A store that enforced only the first would let a stale holder
	// overwrite a live one on its next renewal and never notice.
	gen, err := s.put(ctx, probe, []byte(`{"probe":1}`), map[string]string{"If-None-Match": "*"})
	if err != nil {
		return fmt.Errorf("writing to %s: %w", s.name(), err)
	}
	switch _, err := s.put(ctx, probe, []byte(`{"probe":2}`), map[string]string{"If-None-Match": "*"}); {
	case errors.Is(err, errLeaseTaken): // enforced
	case err != nil:
		return err
	default:
		return fmt.Errorf("%w (If-None-Match is ignored)", errNoConditionalWrites)
	}

	etag, ok := s.etag(gen)
	if !ok {
		return errors.New("lease probe: the store returned no usable ETag")
	}
	switch _, err := s.put(ctx, probe, []byte(`{"probe":3}`), map[string]string{"If-Match": `"` + etag + `-stale"`}); {
	case errors.Is(err, errLeaseTaken):
		return nil // both enforced, which is everything the lease depends on
	case err != nil:
		return err
	default:
		return fmt.Errorf("%w (If-Match is ignored)", errNoConditionalWrites)
	}
}

// newS3Lease builds the lease for this process against an S3-compatible bucket. It returns
// errNoConditionalWrites when the store cannot fence a writer at all; the caller decides what to
// do about that, because the answer differs between one container and several.
func newS3Lease(ctx context.Context, cfg Config, t *replicaTarget) (*Lease, error) {
	obj := &s3LeaseObject{s3Signer: t.signer(), key: t.leaseKey(), etags: map[int64]string{}}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := obj.verifyConditionalWrites(vctx); err != nil {
		return nil, err
	}

	host, _ := os.Hostname()
	holder := os.Getenv("K_REVISION")
	if holder == "" {
		holder = host
	}
	info := LeaseInfo{
		Holder:   holder + "/" + strconv.Itoa(os.Getpid()),
		Revision: os.Getenv("K_REVISION"),
		Commit:   os.Getenv("GIT_COMMIT"),
		DBPath:   cfg.DBPath,
		Replica:  t.String(),
		PID:      os.Getpid(),
	}
	return newLease(obj, info), nil
}
