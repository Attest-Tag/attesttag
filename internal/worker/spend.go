package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"attesttag/internal/app"
)

// spendMeter reads what a job actually cost. The engines report tokens and no price — qwen's
// stream-json carries counts only — so a job's cost reached the bot as $0.00 unless a per-job
// OpenRouter key had been minted, which needs OPENROUTER_PROVISIONING_KEY on the bot. OpenRouter
// answers GET /key with the spend on the key doing the asking, so the worker reads it either
// side of the run and reports the difference in the result.
//
// Only on a key minted for this job (JobLLMSecret.PerJobKey), which starts at zero and only ever
// sees its own spend. It used to read any OpenRouter key, the shared one included, on the reasoning
// that a difference across the run was close enough. It was not close at all: the shared worker key
// is, by default, the bot's own key, so the difference was every organisation's chat spend while
// the job ran — and all of it was charged to the organisation whose job it was. An organisation's
// own OpenRouter key has the same problem with its own turns. On those keys the worker reports
// tokens, and the bot prices them.
type spendMeter struct {
	url    string
	key    string
	client *http.Client
	base   float64
	live   bool
}

// newSpendMeter returns a meter for a per-job OpenRouter key and a dormant one for anything else:
// no other provider answers that route, a shared key's total is not this job's, and a dormant
// meter reports nothing rather than guessing.
func newSpendMeter(llm app.JobLLMSecret) *spendMeter {
	base := strings.TrimRight(llm.BaseURL, "/")
	if llm.APIKey == "" || !llm.PerJobKey || !strings.Contains(base, "openrouter.ai") {
		return &spendMeter{}
	}
	return &spendMeter{url: base + "/key", key: llm.APIKey, live: true,
		client: &http.Client{Timeout: 15 * time.Second}}
}

func (m *spendMeter) read(ctx context.Context) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", m.url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+m.key)
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode >= 300 {
		return 0, errors.New("openrouter GET /key: " + resp.Status)
	}
	var out struct {
		Data struct {
			Usage float64 `json:"usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, err
	}
	return out.Data.Usage, nil
}

// Start records the spend the key carried before the job. A key that cannot be read is not worth
// failing a job over: the meter goes quiet and the bot keeps whatever figure it has.
func (m *spendMeter) Start(ctx context.Context) {
	if !m.live {
		return
	}
	v, err := m.read(ctx)
	if err != nil {
		slog.Warn("openrouter key usage unreadable; the job's cost will not include a metered figure", "err", err)
		m.live = false
		return
	}
	m.base = v
}

// Since is what the job spent. OpenRouter settles a generation shortly after it streams, so a
// reading still at the baseline is retried a couple of times before the meter gives up.
func (m *spendMeter) Since(ctx context.Context) float64 {
	if !m.live {
		return 0
	}
	for i := 0; ; i++ {
		v, err := m.read(ctx)
		if err != nil {
			slog.Warn("openrouter key usage unreadable", "err", err)
			return 0
		}
		if v > m.base {
			return v - m.base
		}
		if i == 2 {
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(1500 * time.Millisecond):
		}
	}
}
