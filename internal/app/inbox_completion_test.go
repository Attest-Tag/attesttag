package app

// The inbox promises that a delivery is only marked done once the work it started has finished.
// These tests hold a turn open — by blocking the Slack call it makes first — and then look at the
// inbox row underneath it.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// blockedSlack gives the bot a Slack API whose first users.info call hangs until released, so a
// turn can be parked at a known point and observed from the test. The email it eventually answers
// with is outside the allowed domain, so the turn finishes at the access check and never reaches
// the agent — these tests are about the inbox, not about what a turn does.
func blockedSlack(t *testing.T, b *Bot, team string, orgID int64) (started <-chan struct{}, release func(), calls func() int32) {
	t.Helper()
	begun := make(chan struct{})
	gate := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The reply is what counts a turn: it happens once per turn that runs to the end. The
		// email lookup cannot be counted instead — Slack user facts are cached per workspace, so
		// a second turn for the same person never calls users.info again.
		if !strings.HasSuffix(r.URL.Path, "/users.info") {
			n.Add(1)
			fmt.Fprint(w, `{"ok":true,"channel":"C_A","ts":"1.2"}`)
			return
		}
		startOnce.Do(func() { close(begun) })
		<-gate // only holds until released; later calls pass straight through
		fmt.Fprint(w, `{"ok":true,"user":{"id":"U_A","profile":{"email":"someone@outside.test"}}}`)
	}))
	release = func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(func() { release(); srv.Close() })
	b.slacks.Put(team, &Chat{t: &slackTransport{api: slack.New("fake", slack.OptionAPIURL(srv.URL+"/"))},
		TeamID: team, OrgID: orgID, BotUserID: "U_BOT"})
	// n counts turns that reached their reply; begun fires when the first turn starts.
	return begun, release, n.Load
}

// expireDeliveryLease simulates the container having died: the claim simply stops being renewed
// and lapses, which is what makes the row available again.
func expireDeliveryLease(t *testing.T, st *Store, key string) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`update slack_deliveries set lease_until=0 where delivery_key=?`, key); err != nil {
		t.Fatal(err)
	}
}

func dmEvent(id, team, ts string) string {
	return fmt.Sprintf(`{"type":"event_callback","event_id":%q,"team_id":%q,"event":{"type":"message","channel":"C_A","channel_type":"im","user":"U_A","ts":%q,"text":"hello"}}`,
		id, team, ts)
}

// deliveryRow reads the inbox row's completion state directly: done_at is the field that decides
// whether anything will ever retry this delivery.
func deliveryRow(t *testing.T, st *Store, key string) (doneAt, leaseUntil int64, attempts int) {
	t.Helper()
	err := st.db.QueryRowContext(context.Background(),
		`select done_at, lease_until, attempts from slack_deliveries where delivery_key=?`, key).
		Scan(&doneAt, &leaseUntil, &attempts)
	if err == sql.ErrNoRows {
		t.Fatalf("no delivery row for %s", key)
	}
	if err != nil {
		t.Fatal(err)
	}
	return
}

// A delivery must not be reported complete while the turn it started is still running. Otherwise a
// shutdown in that window loses the message outright: Slack has had its 200 so it will not retry,
// and the inbox row says done so the dispatcher will not either.
func TestDeliveryIsNotDoneWhileItsTurnIsStillRunning(t *testing.T) {
	t.Setenv("ALLOWED_EMAIL_DOMAINS", "example.com")
	b, mux := slackHTTPTestBot(t)
	b.settings.Invalidate(1) // the cache may have loaded before the domain was set
	started, release, _ := blockedSlack(t, b, "T_A", 1)

	key := "event:T_A:Ev_running"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", dmEvent("Ev_running", "T_A", "1.1"), time.Now()))
	if w.Code != http.StatusOK {
		t.Fatalf("accept = %d", w.Code)
	}
	b.dispatchNextSlackDelivery(context.Background())

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the turn never reached Slack; the test cannot observe it mid-flight")
	}

	doneAt, _, _ := deliveryRow(t, b.store, key)
	if doneAt != 0 {
		t.Fatalf("the delivery was marked done while its turn was still running: a shutdown here loses the message")
	}
	release()
}

// And when the turn does finish, the delivery must be marked done — or every message would be
// redelivered until it dead-lettered.
func TestDeliveryIsDoneOnceItsTurnFinishes(t *testing.T) {
	t.Setenv("ALLOWED_EMAIL_DOMAINS", "example.com")
	b, mux := slackHTTPTestBot(t)
	b.settings.Invalidate(1) // the cache may have loaded before the domain was set
	started, release, _ := blockedSlack(t, b, "T_A", 1)

	key := "event:T_A:Ev_finish"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", dmEvent("Ev_finish", "T_A", "2.1"), time.Now()))
	b.dispatchNextSlackDelivery(context.Background())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the turn never started")
	}
	release()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if doneAt, _, _ := deliveryRow(t, b.store, key); doneAt != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn finished but the delivery was never marked done: it would be retried until it dead-lettered")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The point of leaving the delivery open: when the process stops with a turn still running, the
// row must still be claimable. This is the case the whole change exists for.
func TestShutdownLeavesAnUnfinishedTurnsDeliveryRetryable(t *testing.T) {
	t.Setenv("ALLOWED_EMAIL_DOMAINS", "example.com")
	b, mux := slackHTTPTestBot(t)
	b.settings.Invalidate(1)
	started, _, _ := blockedSlack(t, b, "T_A", 1)

	key := "event:T_A:Ev_shutdown"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", dmEvent("Ev_shutdown", "T_A", "3.1"), time.Now()))
	b.dispatchNextSlackDelivery(context.Background())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the turn never started")
	}

	// The real shutdown path, with a turn it cannot wait out. store is nil so the test keeps its
	// database; replication and the lease are not involved here.
	g := newGate()
	_, cancel := context.WithCancel(context.Background())
	closed := make(chan struct{})
	close(closed)
	shutdown(shutdownArgs{gate: g, srv: &http.Server{Handler: g}, cancel: cancel, stopSignals: func() {},
		deliveries: closed, turns: &b.turns})

	doneAt, _, _ := deliveryRow(t, b.store, key)
	if doneAt != 0 {
		t.Fatal("shutdown marked a delivery done whose turn never finished: the message is lost")
	}
	expireDeliveryLease(t, b.store, key)
	d, err := b.store.claimSlackDelivery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d == nil || d.Key != key {
		t.Fatalf("the delivery was not redelivered after its lease lapsed: %+v", d)
	}
}

// And the redelivery has to actually do the work. The per-message dedup would otherwise swallow it
// — the key was already recorded on the first attempt — leaving a delivery that is retried, marked
// done, and never answered. That would look fixed and lose the message just the same.
func TestARedeliveredEventIsNotSwallowedByTheDedup(t *testing.T) {
	t.Setenv("ALLOWED_EMAIL_DOMAINS", "example.com")
	b, mux := slackHTTPTestBot(t)
	b.settings.Invalidate(1)
	started, release, calls := blockedSlack(t, b, "T_A", 1)

	key := "event:T_A:Ev_redeliver"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", dmEvent("Ev_redeliver", "T_A", "4.1"), time.Now()))
	b.dispatchNextSlackDelivery(context.Background())
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the first turn never started")
	}
	release()

	// The container died mid-turn; the lease lapses and the next one picks the delivery up.
	expireDeliveryLease(t, b.store, key)
	if !b.dispatchNextSlackDelivery(context.Background()) {
		t.Fatal("the delivery was not redispatched")
	}
	deadline := time.Now().Add(5 * time.Second)
	for calls() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the redelivered event never ran: the dedup swallowed the retry (%d turns completed, want 2)", calls())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A turn can run for minutes; the claim lasts a minute. The lease has to be held for as long as
// the work, or a second dispatcher starts the same turn again while the first is still going.
func TestALongTurnKeepsItsDeliveryLeased(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.enqueueSlackDelivery(ctx, "k1", 1, "T_A", "event", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	d, err := st.claimSlackDelivery(ctx)
	if err != nil || d == nil {
		t.Fatalf("claim: %v %v", d, err)
	}
	first := d.Lease
	if err := st.touchSlackDelivery(ctx, d); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if d.Lease <= first {
		t.Fatalf("the lease did not move: %d -> %d", first, d.Lease)
	}
	// Renewing keeps the row out of the queue...
	if next, _ := st.claimSlackDelivery(ctx); next != nil {
		t.Fatal("a renewed delivery was claimed by a second dispatcher")
	}
	// ...and completion still matches, which it would not if the renewal had not updated d.
	if err := st.finishSlackDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	if doneAt, _, _ := deliveryRow(t, st, "k1"); doneAt == 0 {
		t.Fatal("a renewed delivery could not be completed by its owner")
	}
	// A holder that has lost the row is told so, rather than silently finishing someone else's.
	stale := *d
	stale.Lease = first
	if err := st.touchSlackDelivery(ctx, &stale); err == nil {
		t.Fatal("renewing a lease we no longer hold was allowed")
	}
}
