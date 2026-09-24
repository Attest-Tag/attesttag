package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGateAnswersHealthBeforeTheDatabaseIsOpen(t *testing.T) {
	// This is the property the whole boot order rests on. If /health did not answer while the
	// container waits for the write lease, Cloud Run would never mark the revision Ready, never
	// shift traffic, and never tell the previous container to stop — so the two would wait on
	// each other and the deploy would hang.
	g := newGate()
	for _, p := range []string{"/health", "/healthz"} {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusOK || w.Body.String() != "ok\n" {
			t.Fatalf("%s before live = %d %q, want 200 \"ok\\n\"", p, w.Code, w.Body.String())
		}
	}
}

func TestGateHoldsEverythingElseAt503UntilLive(t *testing.T) {
	g := newGate()
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/slack/events", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	// Slack and the console both retry on Retry-After; without it a caller has no idea to come back.
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After on the 503")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/slack/events", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "routed") })
	g.live(mux, nil)

	w = httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/slack/events", nil))
	if w.Code != http.StatusOK || w.Body.String() != "routed" {
		t.Fatalf("after live = %d %q, want 200 \"routed\"", w.Code, w.Body.String())
	}
}

func TestGate503sStillCarrySecurityHeaders(t *testing.T) {
	// The gate is wrapped by secureHeaders, not the other way round, so responses served before
	// the database exists are not a hole in the headers every other response goes out with.
	h := secureHeaders(newGate(), nil, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("startup 503 went out without security headers: %v", w.Header())
	}
}

func TestGateStopsAcceptingWorkWhileDraining(t *testing.T) {
	g := newGate()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "routed") })
	g.live(mux, nil)
	g.drain()

	for _, p := range []string{"/health", "/admin/"} {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s while draining = %d, want 503", p, w.Code)
		}
	}
}

func TestGateReportsTheWriterOnlyToAnOperator(t *testing.T) {
	t.Setenv("HEALTH_SECRET", "s3cret")
	g := newGate()
	g.setWriter("waiting for rev-a")

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Body.String() != "ok\n" {
		t.Fatalf("public /health leaked deployment state: %q", w.Body.String())
	}

	w = httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health?secret=s3cret", nil))
	if !strings.Contains(w.Body.String(), "writer=waiting for rev-a") {
		t.Fatalf("operator /health did not report the writer: %q", w.Body.String())
	}
}
