package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"attesttag/internal/app"
)

// The meter reports the difference across the run, not the key's running total.
func TestSpendMeterReportsTheDelta(t *testing.T) {
	reads, auth := 0, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openrouter.ai/key" {
			t.Errorf("asked for %s", r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		reads++
		if reads == 1 {
			w.Write([]byte(`{"data":{"label":"job","usage":1.5,"limit":10}}`))
			return
		}
		w.Write([]byte(`{"data":{"label":"job","usage":2.25,"limit":10}}`))
	}))
	defer srv.Close()

	m := newSpendMeter(app.JobLLMSecret{BaseURL: srv.URL + "/openrouter.ai", APIKey: "sk-or-test", PerJobKey: true})
	m.Start(context.Background())
	if got := m.Since(context.Background()); got != 0.75 {
		t.Errorf("Since = %v, want 0.75", got)
	}
	if auth != "Bearer sk-or-test" {
		t.Errorf("Authorization = %q", auth)
	}
}

// Anything but OpenRouter has no such route, so the meter stays dormant and never guesses.
func TestSpendMeterDormantOffOpenRouter(t *testing.T) {
	for _, sec := range []app.JobLLMSecret{
		{BaseURL: "https://api.openai.com/v1", APIKey: "sk-1", PerJobKey: true},
		{BaseURL: "https://openrouter.ai/api/v1", PerJobKey: true}, // no key
		// A shared key, or an organisation's own: its running total moves with everything else
		// that key is doing, so a difference across the job is not the job's spend.
		{BaseURL: "https://openrouter.ai/api/v1", APIKey: "sk-or-shared"},
	} {
		m := newSpendMeter(sec)
		m.Start(context.Background())
		if m.live || m.Since(context.Background()) != 0 {
			t.Errorf("%s: meter should be dormant", sec.BaseURL)
		}
	}
}

// A provider that will not answer must not fail the job or invent a figure.
func TestSpendMeterUnreadableKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	m := newSpendMeter(app.JobLLMSecret{BaseURL: srv.URL + "/openrouter.ai", APIKey: "sk-or-test", PerJobKey: true})
	m.Start(context.Background())
	if m.live {
		t.Error("an unreadable key should put the meter to sleep")
	}
	if got := m.Since(context.Background()); got != 0 {
		t.Errorf("Since = %v, want 0", got)
	}
}
