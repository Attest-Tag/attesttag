package worker

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"attesttag/internal/app"
)

// The dependency cache. Every job used to pay a cold install: Cloud Run gives each execution a
// fresh, memory-backed filesystem, so the same repository downloaded the same packages every
// time, and on a large one that was most of the job's wall clock — and the ten-minute install
// cap it kept walking into.
//
// What is cached is the package managers' own stores (npm, uv, Go modules, the Cargo registry,
// Gradle, mise's toolchains), never the resolved tree inside the repository. Those stores are
// content-addressed, so a stale entry is dead weight rather than a wrong answer: the cache can
// be keyed coarsely, per repository and base branch, and still be correct. That is what makes
// it warm often enough to matter.
//
// The bytes go straight to storage over two short-lived signed URLs handed out with the claim.
// The container gets no storage credential and the bot proxies no traffic.

const (
	cacheRestoreTimeout = 3 * time.Minute
	cacheSaveTimeout    = 4 * time.Minute
	// Below this a save is not worth its upload: an empty or barely-used cache.
	cacheMinSaveBytes = 4 << 20
)

// cacheSubdirs are what is worth carrying between jobs. Anything else under the cache directory
// is left behind.
var cacheSubdirs = []string{"uv", "npm", "go", "cargo", "mise", "gradle", "maven", "composer", "bundle", "nuget", "pub"}

// restoreCache unpacks the previous run's cache over the work directory. Every failure is a
// miss: a job with a cold cache is slower, never wrong.
func (r *Runner) restoreCache(ctx context.Context, claim *app.JobClaim, cacheDir string) {
	if claim.Cache.GetURL == "" {
		return
	}
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, cacheRestoreTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, claim.Cache.GetURL, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("cache restore", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		slog.Info("cache miss", "key", claim.Cache.Key)
		return
	}
	if resp.StatusCode != http.StatusOK {
		slog.Debug("cache restore", "status", resp.StatusCode)
		return
	}
	n, err := untarInto(io.LimitReader(resp.Body, app.JobCacheMaxBytes), cacheDir)
	if err != nil {
		slog.Warn("cache restore failed; continuing cold", "err", err)
		return
	}
	slog.Info("cache restored", "key", claim.Cache.Key, "bytes", n, "took", time.Since(start).Round(time.Second))
}

// saveCache uploads what the job downloaded, for the next one. Best-effort and never fatal: it
// runs after the pull request exists, so a failure here costs nothing anyone is waiting for.
func (r *Runner) saveCache(ctx context.Context, claim *app.JobClaim, cacheDir string) {
	if claim.Cache.PutURL == "" {
		return
	}
	max := claim.Cache.MaxBytes
	if max <= 0 || max > app.JobCacheMaxBytes {
		max = app.JobCacheMaxBytes
	}
	tmp, err := os.CreateTemp(filepath.Dir(cacheDir), "cache-*.tgz")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := tarFrom(tmp, cacheDir, max)
	if err != nil {
		slog.Debug("cache pack", "err", err)
		return
	}
	if n < cacheMinSaveBytes {
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return
	}
	// On a context that outlives the job's: the job is done, and a cancelled context here would
	// throw away the one thing that makes the next run fast.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheSaveTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPut, claim.Cache.PutURL, tmp)
	if err != nil {
		return
	}
	req.ContentLength = n
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("cache save", "err", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		slog.Debug("cache save", "status", resp.StatusCode)
		return
	}
	slog.Info("cache saved", "key", claim.Cache.Key, "bytes", n)
}

// tarFrom packs the cache subdirectories into gzipped tar, stopping at max.
func tarFrom(w io.Writer, root string, max int64) (int64, error) {
	counter := &countWriter{w: w}
	gz := gzip.NewWriter(counter)
	tw := tar.NewWriter(gz)
	var written int64
	for _, sub := range cacheSubdirs {
		base := filepath.Join(root, sub)
		if st, err := os.Stat(base); err != nil || !st.IsDir() {
			continue
		}
		err := filepath.Walk(base, func(path string, fi os.FileInfo, err error) error {
			if err != nil || written > max {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			if fi.Mode()&os.ModeSymlink != 0 || (!fi.Mode().IsRegular() && !fi.IsDir()) {
				return nil // a cache is files; links out of it are not worth carrying
			}
			h, err := tar.FileInfoHeader(fi, "")
			if err != nil {
				return nil
			}
			h.Name = filepath.ToSlash(rel)
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if fi.IsDir() {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			defer f.Close()
			n, err := io.Copy(tw, f)
			written += n
			return err
		})
		if err != nil {
			break
		}
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	return counter.n, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// untarInto unpacks under root, refusing any entry that would land outside it.
func untarInto(r io.Reader, root string) (int64, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			continue
		}
		dest := filepath.Join(root, name)
		if rel, err := filepath.Rel(root, dest); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(dest, 0o755)
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				continue
			}
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0o777)
			if err != nil {
				continue
			}
			n, err := io.Copy(f, io.LimitReader(tr, app.JobCacheMaxBytes))
			f.Close()
			total += n
			if err != nil {
				return total, err
			}
		}
	}
}
