package app

// `attesttag bucket-init`: make the bucket, then prove it works.
//
// It exists because an object store that is started beside the bot — MinIO in the compose file,
// MinIO in the Helm chart — comes up with no bucket in it, and the bot is not in the business of
// creating buckets at startup: on a real provider that would need a permission nobody should
// have to grant, and a typo'd bucket name would be created rather than reported.
//
// So it is a one-shot the operator (or a compose service, or an initContainer) runs, and it
// answers the question they actually have, which is not "does the bucket exist" but "will this
// deployment work against it": can these credentials list, write, read and delete, and does the
// store enforce the conditional writes the single-writer lease is built on.
//
// It reads the same three variables the bot does, so there is nothing new to configure:
//
//	DOCS_S3_URL=s3://bucket/prefix?endpoint=https://…&region=auto
//	DOCS_S3_KEY_ID, DOCS_S3_SECRET

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

func BucketInit(args []string) int {
	fs := flag.NewFlagSet("bucket-init", flag.ContinueOnError)
	wait := fs.Duration("wait", 0, "keep retrying this long while the store comes up; 0 tries once")
	create := fs.Bool("create", true, "create the bucket when it is not there (-create=false only checks)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	raw := os.Getenv("DOCS_S3_URL")
	if raw == "" {
		fmt.Fprintln(os.Stderr, "DOCS_S3_URL is not set. usage: DOCS_S3_URL=s3://bucket/prefix?endpoint=…&region=… attesttag bucket-init")
		return 2
	}
	loc, err := parseS3URL(raw, "DOCS_S3_URL")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	keyID, secret := os.Getenv("DOCS_S3_KEY_ID"), os.Getenv("DOCS_S3_SECRET")
	if keyID == "" || secret == "" {
		fmt.Fprintln(os.Stderr, "DOCS_S3_KEY_ID and DOCS_S3_SECRET are required")
		return 2
	}
	target := &replicaTarget{s3: true, bucket: loc.bucket, endpoint: loc.endpoint, region: loc.region,
		key: loc.prefix, keyID: keyID, secret: secret}

	ctx, cancel := context.WithTimeout(context.Background(), *wait+2*time.Minute)
	defer cancel()
	if err := bucketInit(ctx, target, loc.prefix, *wait, *create); err != nil {
		fmt.Fprintf(os.Stderr, "\n%s: %v\n", target.scheme()+loc.bucket, err)
		return 1
	}
	return 0
}

func bucketInit(ctx context.Context, target *replicaTarget, prefix string, wait time.Duration, create bool) error {
	s := target.signer()
	name := target.scheme() + target.bucket

	// Waiting is the whole reason this is not a shell one-liner: started by compose or as an
	// initContainer, it races the store's own boot, and "connection refused" for the first
	// second is normal rather than a failure.
	deadline := time.Now().Add(wait)
	for attempt := 1; ; attempt++ {
		code, err := s.bucketStatus(ctx)
		switch {
		case err == nil && code == http.StatusOK:
			fmt.Printf("bucket %s is there\n", name)
		case err == nil && code == http.StatusNotFound && create:
			if err := s.createBucket(ctx); err != nil {
				return err
			}
			fmt.Printf("created bucket %s\n", name)
		case err == nil && code == http.StatusNotFound:
			return errors.New("no such bucket, and -create=false")
		case err == nil && (code == http.StatusForbidden || code == http.StatusUnauthorized):
			return fmt.Errorf("the credentials were refused (%d): check DOCS_S3_KEY_ID, DOCS_S3_SECRET and the region", code)
		case err == nil:
			return fmt.Errorf("unexpected %d asking whether the bucket exists", code)
		case time.Now().After(deadline):
			return fmt.Errorf("cannot reach the store: %w", err)
		default:
			if attempt == 1 {
				fmt.Printf("waiting for %s at %s…\n", name, s.endpoint)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		break
	}

	// What the bot will actually do to it. A bucket that exists and cannot be written to is the
	// failure this command is for, and it is better to find it here than in an ingest an hour
	// from now.
	if err := s.proveReadWrite(ctx, prefix); err != nil {
		return err
	}
	fmt.Println("credentials can write, read and delete under the prefix")

	// And whether a write lease is possible here, which decides whether this deployment may
	// ever run more than one instance against one SQLite database.
	probe := &s3LeaseObject{s3Signer: s, key: target.leaseKey(), etags: map[int64]string{}}
	switch err := probe.verifyConditionalWrites(ctx); {
	case err == nil:
		fmt.Println("conditional writes are enforced, so the single-writer lease works here")
	case errors.Is(err, errNoConditionalWrites):
		fmt.Printf("conditional writes are NOT enforced (%v)\n"+
			"  the database can still be replicated here, but there is no write lease:\n"+
			"  run exactly one instance against it\n", err)
	default:
		return fmt.Errorf("checking conditional writes: %w", err)
	}
	return nil
}

// bucketStatus is HEAD on the bucket: 200 it is there, 404 it is not, 403 the credentials are
// not allowed to know.
func (s *s3Signer) bucketStatus(ctx context.Context) (int, error) {
	resp, err := s.do(ctx, http.MethodHead, "", nil, nil)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (s *s3Signer) createBucket(ctx context.Context) error {
	resp, err := s.do(ctx, http.MethodPut, "", nil, nil)
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusConflict: // somebody else created it between our two calls
		resp.Body.Close()
		return nil
	case resp.StatusCode >= 300:
		return fmt.Errorf("creating the bucket: %s: %s", resp.Status, drain(resp))
	}
	resp.Body.Close()
	return nil
}

// proveReadWrite does what an ingest does, to a key that is deleted again: PUT, GET, DELETE.
func (s *s3Signer) proveReadWrite(ctx context.Context, prefix string) error {
	key := prefix + "/.attesttag-bucket-init"
	if prefix == "" {
		key = ".attesttag-bucket-init"
	}
	body := []byte("attest_tag bucket-init " + time.Now().UTC().Format(time.RFC3339))
	put, err := s.do(ctx, http.MethodPut, key, body, map[string]string{"Content-Type": "text/plain"})
	if err != nil {
		return err
	}
	if put.StatusCode >= 300 {
		return fmt.Errorf("writing %s: %s: %s", key, put.Status, drain(put))
	}
	put.Body.Close()

	get, err := s.do(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return err
	}
	if get.StatusCode >= 300 {
		return fmt.Errorf("reading %s back: %s: %s", key, get.Status, drain(get))
	}
	if back := drain(get); back != string(body) {
		return fmt.Errorf("read back %q, wrote %q — this store is not storing what it is given", back, body)
	}

	del, err := s.do(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	if del.StatusCode >= 300 && del.StatusCode != http.StatusNotFound {
		return fmt.Errorf("deleting %s: %s: %s", key, del.Status, drain(del))
	}
	del.Body.Close()
	return nil
}
