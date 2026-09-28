package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"attesttag/internal/app"
)

// Client is the worker's side of the bot protocol (internal/app/jobs_proto.go). Transient
// failures are retried with backoff; 401, 409 and 410 are final — the bot has decided.
type Client struct {
	base    string
	jobID   int64
	token   string
	version string
	http    *http.Client
	oidc    *identityToken // set on Cloud Run
}

func newClient(o Options) *Client {
	c := &Client{base: o.BotURL, jobID: o.JobID, token: o.Token, version: o.Version, http: &http.Client{Timeout: 30 * time.Second}}
	if os.Getenv("CLOUD_RUN_JOB") != "" || os.Getenv("K_SERVICE") != "" {
		c.oidc = &identityToken{audience: o.BotURL}
	}
	return c
}

// fatalError is a refusal the worker must not retry (and, for claim, must not report on).
type fatalError struct {
	Status int
	Body   string
}

func (e *fatalError) Error() string { return fmt.Sprintf("bot answered %d: %s", e.Status, e.Body) }

func isFatal(err error) bool {
	var fe *fatalError
	return errors.As(err, &fe)
}

func (c *Client) url(path string) string {
	return fmt.Sprintf("%s/api/worker/jobs/%d%s", c.base, c.jobID, path)
}

// do sends one request with retries. attempts counts tries; backoff is 500 ms doubling to 8 s
// with full jitter.
func (c *Client) do(ctx context.Context, method, path string, body []byte, contentType string, attempts int) (int, []byte, error) {
	var lastErr error
	delay := 500 * time.Millisecond
	for i := 0; i < attempts; i++ {
		if i > 0 {
			d := time.Duration(rand.Int63n(int64(delay)))
			select {
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			case <-time.After(d):
			}
			if delay < 8*time.Second {
				delay *= 2
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.url(path), bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("User-Agent", "attesttag-worker/"+c.version)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if c.oidc != nil {
			if tok, err := c.oidc.get(ctx); err == nil {
				req.Header.Set("X-Worker-OIDC", tok)
			} else {
				slog.Warn("identity token", "err", err)
			}
		}
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 || resp.StatusCode == 409 || resp.StatusCode == 410 || resp.StatusCode == 413 || resp.StatusCode == 400:
			return resp.StatusCode, raw, &fatalError{Status: resp.StatusCode, Body: cut(string(raw), 300)}
		case resp.StatusCode == 408 || resp.StatusCode == 425 || resp.StatusCode == 429 || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("bot answered %d: %s", resp.StatusCode, cut(string(raw), 200))
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if d, err := time.ParseDuration(ra + "s"); err == nil && d < time.Minute {
					delay = d
				}
			}
			continue
		}
		return resp.StatusCode, raw, nil
	}
	return 0, nil, lastErr
}

func (c *Client) Claim(ctx context.Context, info app.JobWorkerInfo) (*app.JobClaim, error) {
	body, _ := json.Marshal(app.JobClaimRequest{Worker: info})
	_, raw, err := c.do(ctx, "POST", "/claim", body, "application/json", 5)
	if err != nil {
		return nil, err
	}
	var claim app.JobClaim
	if err := json.Unmarshal(raw, &claim); err != nil {
		return nil, fmt.Errorf("claim body: %w", err)
	}
	if claim.Job.ID != c.jobID {
		return nil, fmt.Errorf("claim is for job %d, not %d", claim.Job.ID, c.jobID)
	}
	return &claim, nil
}

func (c *Client) Events(ctx context.Context, evs []app.JobEvent) (*app.JobEventsResponse, error) {
	body, _ := json.Marshal(app.JobEventsRequest{Events: evs})
	_, raw, err := c.do(ctx, "POST", "/events", body, "application/json", 3)
	if err != nil {
		return nil, err
	}
	var out app.JobEventsResponse
	json.Unmarshal(raw, &out)
	return &out, nil
}

func (c *Client) PutDiff(ctx context.Context, diff string) error {
	_, _, err := c.do(ctx, "PUT", "/diff", []byte(diff), "text/x-diff", 3)
	return err
}

func (c *Client) Result(ctx context.Context, res app.JobResult) error {
	body, _ := app.EncodeJobResult(&res) // the form Fit measured
	_, _, err := c.do(ctx, "POST", "/result", body, "application/json", 20)
	return err
}

// identityToken fetches a Google ID token for the bot's URL from the metadata server, which on
// Cloud Run proves the request comes from the job's service account. Cached for 50 minutes.
type identityToken struct {
	audience string
	mu       sync.Mutex
	token    string
	at       time.Time
}

func (t *identityToken) get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Since(t.at) < 50*time.Minute {
		return t.token, nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience="+t.audience+"&format=full", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	tok := strings.TrimSpace(string(raw))
	if resp.StatusCode != 200 || tok == "" {
		return "", fmt.Errorf("metadata server answered %d", resp.StatusCode)
	}
	t.token, t.at = tok, time.Now()
	return tok, nil
}
