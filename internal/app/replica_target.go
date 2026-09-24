package app

// Where the SQLite replica goes.
//
// There are two answers and they are chosen the same way the documents are (bot.go): a folder
// unless you name a bucket, and the bucket you named for the documents is the one this uses
// unless you name another. Give the deployment no bucket at all and both halves stay local —
// which is the single-box answer, and the one `docker compose up` gives you.
//
// The GCS target is what the hosted service has always run on. The S3 one is what makes the
// same durability available anywhere else: R2, MinIO, Spaces, B2, Wasabi, Ceph, AWS itself.
// Both are Litestream underneath, and the difference between them is a stanza in a config file
// this renders (litestreamYAML) plus which lease implementation fences the writer — GCS object
// generations in lease_gcs.go, S3 conditional writes in lease_s3.go.

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// s3Location is the bucket half of an s3:// URL, shared by the documents store and this. Both
// read the same shape of URL and neither may disagree with the other about what it means.
type s3Location struct {
	endpoint string // https://…, derived from the region on AWS itself
	bucket   string
	prefix   string // no leading or trailing slash
	region   string
}

// parseS3URL reads s3://bucket/prefix?endpoint=https://…&region=auto. varName is the environment
// variable it came from, so the error names the thing the operator actually set.
func parseS3URL(raw, varName string) (*s3Location, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return nil, fmt.Errorf("%s must look like s3://bucket/prefix?endpoint=…&region=…, got %q", varName, raw)
	}
	q := u.Query()
	region := q.Get("region")
	if region == "" {
		region = "us-east-1"
	}
	endpoint := strings.TrimSuffix(q.Get("endpoint"), "/")
	if endpoint == "" {
		endpoint = "https://s3." + region + ".amazonaws.com"
	} else if !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		endpoint = "https://" + endpoint
	}
	return &s3Location{endpoint: endpoint, bucket: u.Host, prefix: strings.Trim(u.Path, "/"), region: region}, nil
}

// replicaTarget is one bucket and one key: where this database's replica lives, and — as the
// sibling key <path>.lock.json — where the single-writer lease lives with it.
type replicaTarget struct {
	s3     bool
	bucket string
	key    string // object key of the replica itself

	// S3 only. The endpoint is what a custom provider is reached at; the region is required by
	// the signature even where the provider ignores it.
	endpoint, region string
	keyID, secret    string

	// derived says the bucket was not named for the database — it is the documents bucket,
	// borrowed. Worth saying out loud in the startup log, because it is the line an operator
	// who only set DOCS_S3_URL will be surprised by.
	derived bool
}

func (t *replicaTarget) scheme() string {
	if t.s3 {
		return "s3://"
	}
	return "gs://"
}

func (t *replicaTarget) String() string { return t.scheme() + t.bucket + "/" + t.key }

// provenance is which variable chose this bucket. It is in the startup log because "the bucket
// you named for the documents" is the answer an operator who set one variable will not expect.
func (t *replicaTarget) provenance() string {
	switch {
	case t.derived:
		return "DOCS_S3_URL"
	case t.s3:
		return "LITESTREAM_S3_URL"
	default:
		return "LITESTREAM_BUCKET"
	}
}

// resolveReplica decides the target from the environment, in the order a reader would expect:
// something named for the database first, the documents bucket second, nothing third.
//
//	LITESTREAM_S3_URL   an S3-compatible bucket, named for this
//	LITESTREAM_BUCKET   a GCS bucket, named for this (what deploy/gcp/cloudrun.sh sets)
//	DOCS_S3_URL         the documents bucket, borrowed — the replica lands under the same
//	                    prefix, at LITESTREAM_PATH, which no document ever occupies because
//	                    documents live under <prefix>/org-<id>/
//
// A nil target with a nil error means no bucket was configured: local SQLite, no replication and
// no lease, which is correct for one box with a disk you back up yourself.
func resolveReplica(replicaPath string) (*replicaTarget, error) {
	if replicaPath == "" {
		replicaPath = "litestream/attesttag.db"
	}
	replicaPath = strings.Trim(replicaPath, "/")

	if raw := strings.TrimSpace(os.Getenv("LITESTREAM_S3_URL")); raw != "" {
		loc, err := parseS3URL(raw, "LITESTREAM_S3_URL")
		if err != nil {
			return nil, err
		}
		keyID := env("LITESTREAM_S3_KEY_ID", os.Getenv("DOCS_S3_KEY_ID"))
		secret := env("LITESTREAM_S3_SECRET", os.Getenv("DOCS_S3_SECRET"))
		if keyID == "" || secret == "" {
			return nil, fmt.Errorf("LITESTREAM_S3_URL is set but LITESTREAM_S3_KEY_ID and LITESTREAM_S3_SECRET are not (DOCS_S3_KEY_ID and DOCS_S3_SECRET are used when they are absent)")
		}
		// A URL named for the database carries its own key: its path is the replica, not a
		// prefix to put LITESTREAM_PATH under. An operator who points at a bucket root gets
		// the default key rather than a database at the top of somebody's bucket.
		key := loc.prefix
		if key == "" {
			key = replicaPath
		}
		return &replicaTarget{s3: true, bucket: loc.bucket, key: key,
			endpoint: loc.endpoint, region: loc.region, keyID: keyID, secret: secret}, nil
	}

	if bucket := strings.TrimSpace(os.Getenv("LITESTREAM_BUCKET")); bucket != "" {
		return &replicaTarget{bucket: bucket, key: replicaPath}, nil
	}

	if raw := strings.TrimSpace(os.Getenv("DOCS_S3_URL")); raw != "" {
		loc, err := parseS3URL(raw, "DOCS_S3_URL")
		if err != nil {
			return nil, err
		}
		keyID, secret := os.Getenv("DOCS_S3_KEY_ID"), os.Getenv("DOCS_S3_SECRET")
		if keyID == "" || secret == "" {
			return nil, fmt.Errorf("DOCS_S3_KEY_ID and DOCS_S3_SECRET are required with DOCS_S3_URL")
		}
		// Under the documents prefix rather than at the bucket root, so that two deployments
		// sharing one bucket — which is the reason a prefix exists — cannot land two different
		// databases on one key.
		return &replicaTarget{s3: true, bucket: loc.bucket, key: path.Join(loc.prefix, replicaPath),
			endpoint: loc.endpoint, region: loc.region, keyID: keyID, secret: secret, derived: true}, nil
	}

	return nil, nil
}

// The one replica setting, which is not the operator's business: stream continuously.
//
// `retention` and `snapshot-interval` are deliberately absent. They were in the config file this
// replaced, and Litestream 0.5 ignores both — it accepts unknown keys in silence, and its own
// compaction levels decide how much history a replica keeps. A key that reads as a policy and
// does nothing is worse than no key: it is a promise in a file that nothing is keeping.
const replicaSyncInterval = "1s"

// litestreamYAML renders the config Litestream is started with. It is generated rather than
// shipped as a file because the two targets differ only here, and a file in the image cannot
// know which one this deployment chose.
//
// No credential is written into it: Litestream reads LITESTREAM_ACCESS_KEY_ID and
// LITESTREAM_SECRET_ACCESS_KEY from its environment (childEnv), so the file stays something an
// operator can read out of a running container without finding a secret in it.
func (t *replicaTarget) litestreamYAML(dbPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by attest_tag at startup. Point LITESTREAM_CONFIG at your own file to replace it.\n")
	fmt.Fprintf(&b, "dbs:\n  - path: %q\n    replicas:\n", dbPath)
	if t.s3 {
		fmt.Fprintf(&b, "      - type: s3\n        bucket: %q\n        path: %q\n        region: %q\n", t.bucket, t.key, t.region)
		if t.endpoint != "" && !strings.HasSuffix(t.endpoint, ".amazonaws.com") {
			// Path-style addressing for everyone but AWS: MinIO and Ceph require it, R2 and
			// the rest accept it, and AWS itself is left to Litestream's own virtual-host
			// addressing because path-style is deprecated there.
			fmt.Fprintf(&b, "        endpoint: %q\n        force-path-style: true\n", t.endpoint)
		}
	} else {
		fmt.Fprintf(&b, "      - type: gs\n        bucket: %q\n        path: %q\n", t.bucket, t.key)
	}
	fmt.Fprintf(&b, "        sync-interval: %s\n", replicaSyncInterval)
	return b.String()
}

// childEnv is what the Litestream child needs in its environment beyond what it inherits.
func (t *replicaTarget) childEnv() []string {
	if !t.s3 {
		return nil // GCS uses the ambient service account, as everything else on Cloud Run does
	}
	return []string{
		"LITESTREAM_ACCESS_KEY_ID=" + t.keyID,
		"LITESTREAM_SECRET_ACCESS_KEY=" + t.secret,
	}
}

// signer is how this target is talked to. The service is named rather than guessed from the
// hostname, for the same reason s3Docs names it: a custom endpoint — R2, MinIO — is a host
// signAWSv4 has never heard of, and it would refuse to guess rather than sign something wrong.
func (t *replicaTarget) signer() s3Signer {
	return s3Signer{
		client:   &http.Client{Timeout: 30 * time.Second},
		endpoint: t.endpoint,
		bucket:   t.bucket,
		cred:     &Secret{AWSKeyID: t.keyID, AWSSecret: t.secret, AWSRegion: t.region, AWSService: "s3"},
	}
}

// leaseKey is the lock object, a sibling of the replica: a key beside it is never touched by
// Litestream's own retention sweep, and deriving it from the replica key means that pointing at
// a new replica asks for a fresh database and a fresh lease together.
func (t *replicaTarget) leaseKey() string { return leaseObjectName(t.key) }
