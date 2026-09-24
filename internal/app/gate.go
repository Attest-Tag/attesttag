package app

// The gate is the HTTP handler for the whole process lifetime, including the part of it before
// there is a database to serve from.
//
// The ordering it exists for is load-bearing. A container that blocked on the write lease before
// listening would never be marked Ready, so Cloud Run would never shift traffic to it, so the old
// container would never be told to stop — and the two would wait on each other until the deploy
// timed out. So: bind and answer health immediately, hold everything else at 503, and swap the
// real handler in once the database is open.

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
)

type gate struct {
	handler  atomic.Pointer[http.Handler]
	details  atomic.Pointer[func(io.Writer, *http.Request)] // the HEALTH_SECRET body; nil until the store is open
	writer   atomic.Pointer[string]                         // lease state, for operators watching a deploy
	draining atomic.Bool
}

func newGate() *gate {
	g := &gate{}
	g.setWriter("starting")
	return g
}

func (g *gate) setWriter(s string) { g.writer.Store(&s) }

func (g *gate) writerState() string {
	if s := g.writer.Load(); s != nil {
		return *s
	}
	return "unknown"
}

// live swaps in the real routes. Everything served before this point is health or 503.
func (g *gate) live(h http.Handler, details func(io.Writer, *http.Request)) {
	g.details.Store(&details)
	g.handler.Store(&h)
}

// draining stops accepting work without closing the listener, so the shutdown that follows is
// not racing new requests into a database that is about to close.
func (g *gate) drain() { g.draining.Store(true) }

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// /healthz is intercepted by Google's front end on Cloud Run, so /health is the public one.
	if r.URL.Path == "/health" || r.URL.Path == "/healthz" {
		g.health(w, r)
		return
	}
	if g.draining.Load() {
		g.unavailable(w, "shutting down")
		return
	}
	h := g.handler.Load()
	if h == nil {
		g.unavailable(w, "starting up")
		return
	}
	(*h).ServeHTTP(w, r)
}

func (g *gate) unavailable(w http.ResponseWriter, why string) {
	w.Header().Set("Retry-After", "5")
	http.Error(w, why, http.StatusServiceUnavailable)
}

// health is a liveness check for the process. The figures behind it — how many workspaces the
// deployment holds, what has been spent — are the operator's, so they go out only to a caller
// holding HEALTH_SECRET; everyone else gets "ok".
func (g *gate) health(w http.ResponseWriter, r *http.Request) {
	if g.draining.Load() {
		// Say so honestly: a load balancer should stop sending here, and an operator watching a
		// deploy should see the handover rather than a flat "ok".
		g.unavailable(w, "draining")
		return
	}
	secret := os.Getenv("HEALTH_SECRET")
	// A header is preferred over the query string: the query string lands in access logs and any
	// proxy's records, so the secret should not have to travel there. The query form is still
	// accepted, because a browser check and some uptime probes can only send a URL.
	offered := r.Header.Get("X-Health-Secret")
	if offered == "" {
		offered = r.URL.Query().Get("secret")
	}
	if secret == "" || subtle.ConstantTimeCompare([]byte(offered), []byte(secret)) != 1 {
		fmt.Fprint(w, "ok\n")
		return
	}
	fmt.Fprint(w, "ok\n")
	// The writer state is the readout that matters during a deploy: it says whether this container
	// owns the database or is still waiting for the previous one to let go.
	if s := g.writer.Load(); s != nil {
		fmt.Fprintf(w, "writer=%s\n", *s)
	}
	if d := g.details.Load(); d != nil && *d != nil {
		(*d)(w, r)
	}
}
