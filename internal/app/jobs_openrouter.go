package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Per-job model keys. OpenRouter's provisioning API mints API keys with a spend limit and reports
// exact usage per key, so each job gets its own capped key and the bill is read back afterwards,
// whatever the engine reports. Needs OPENROUTER_PROVISIONING_KEY (a management key); without it
// the worker gets the shared key, uncapped (llmSecret in jobs.go), which is the weaker choice
// because a worker runs the repository's own code.
// https://openrouter.ai/docs/features/provisioning-api-keys

const openRouterBaseURL = "https://openrouter.ai/api/v1"

// openRouterKeysURL is overridable for tests.
var openRouterKeysURL = openRouterBaseURL + "/keys"

type openRouterKeys struct {
	provisioning string
	client       *http.Client
}

func newOpenRouterKeys(provisioning string) *openRouterKeys {
	return &openRouterKeys{provisioning: provisioning, client: &http.Client{Timeout: 20 * time.Second}}
}

func (k *openRouterKeys) enabled() bool { return k != nil && k.provisioning != "" }

func (k *openRouterKeys) do(ctx context.Context, method, url string, body any) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k.provisioning)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("openrouter %s %s: %d %s", method, url, resp.StatusCode, truncate(redact(string(raw)), 200))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// mint creates a key capped at limitUSD and returns it with the hash that names it afterwards.
func (k *openRouterKeys) mint(ctx context.Context, name string, limitUSD float64) (key, hash string, err error) {
	// A per-job key exists to cap the job. Minting one without a finite, positive limit — from a
	// zero, a negative, a NaN or an infinity that slipped through a budget setting — would hand a
	// job an uncapped key, so refuse rather than omit the cap and fail open.
	if !(limitUSD > 0 && limitUSD < 1e12) {
		return "", "", fmt.Errorf("refusing to mint a key without a spend cap (limit=%v)", limitUSD)
	}
	body := map[string]any{"name": name, "limit": limitUSD}
	out, err := k.do(ctx, "POST", openRouterKeysURL, body)
	if err != nil {
		return "", "", err
	}
	key, _ = out["key"].(string)
	data, _ := out["data"].(map[string]any)
	if data != nil {
		if key == "" {
			key, _ = data["key"].(string)
		}
		hash, _ = data["hash"].(string)
	}
	if key == "" || hash == "" {
		return "", "", errors.New("openrouter returned no key")
	}
	return key, hash, nil
}

// usage is the dollars charged to a key so far.
func (k *openRouterKeys) usage(ctx context.Context, hash string) (float64, error) {
	out, err := k.do(ctx, "GET", openRouterKeysURL+"/"+hash, nil)
	if err != nil {
		return 0, err
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		return 0, errors.New("openrouter returned no key data")
	}
	u, _ := data["usage"].(float64)
	return u, nil
}

func (k *openRouterKeys) delete(ctx context.Context, hash string) error {
	_, err := k.do(ctx, "DELETE", openRouterKeysURL+"/"+hash, nil)
	return err
}
