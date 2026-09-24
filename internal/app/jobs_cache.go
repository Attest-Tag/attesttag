package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"cloud.google.com/go/storage"
)

// The dependency cache, from the bot's side: two short-lived signed URLs handed out with the
// claim, one to read the previous run's package-manager stores and one to write this run's.
//
// Signed URLs rather than a bucket grant: the worker's service account is deliberately given
// nothing at all (deploy/gcp/worker.sh), and a job that runs a repository's own code is the last
// place to put a storage credential. They expire with the job, and each one names exactly one
// object. Everything here is best-effort — a bot that cannot sign hands out no URLs, and the
// worker installs from cold, which is slower and never wrong.

const (
	cacheURLTTL   = 90 * time.Minute
	cachePrefix   = "jobcache/v1"
	cacheDisabled = "" // no bucket, no signer, or signing refused
)

// cacheKey is per organisation, repository and base branch. Coarse on purpose: what is inside is
// content-addressed, so a stale entry is dead weight rather than a wrong answer, and a key that
// is coarse is a key that is warm.
func cacheKey(orgID int64, repo, base string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(repo + "\x00" + base)))
	return fmt.Sprintf("%s/%d/%s.tgz", cachePrefix, orgID, hex.EncodeToString(sum[:8]))
}

// cacheURLs signs a GET and a PUT for one job's cache object.
func (r *JobRunner) cacheURLs(ctx context.Context, j *Job) JobCache {
	bucket := strings.TrimSpace(r.cfg.WorkerCacheBucket)
	if bucket == "" || r.cfg.WorkerCacheOff {
		return JobCache{}
	}
	key := cacheKey(j.OrgID, j.Repo, j.BaseBranch)
	cl, err := r.storage(ctx)
	if err != nil {
		slog.Debug("job cache: no storage client", "err", err)
		return JobCache{}
	}
	sign := func(method string) string {
		u, err := cl.Bucket(bucket).SignedURL(key, &storage.SignedURLOptions{
			Method:  method,
			Expires: time.Now().Add(cacheURLTTL),
			Scheme:  storage.SigningSchemeV4,
		})
		if err != nil {
			// The usual cause is that the bot's service account cannot sign for itself
			// (roles/iam.serviceAccountTokenCreator on itself). Said once, at debug, because
			// the consequence is only a cold install.
			slog.Debug("job cache: signing", "method", method, "err", err)
			return cacheDisabled
		}
		return u
	}
	get, put := sign("GET"), sign("PUT")
	if get == cacheDisabled || put == cacheDisabled {
		return JobCache{}
	}
	return JobCache{Key: key, GetURL: get, PutURL: put, MaxBytes: JobCacheMaxBytes}
}

// storage is one client for the process, made on first use.
func (r *JobRunner) storage(ctx context.Context) (*storage.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gcs != nil {
		return r.gcs, nil
	}
	c, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	r.gcs = c
	return c, nil
}
