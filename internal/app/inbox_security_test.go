package app

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The Slack inbox is shared by every tenant; these are the bounds that keep it fair.

func TestInboxCapsPerOrganisation(t *testing.T) {
	b, mux := slackHTTPTestBot(t)
	ctx := context.Background()
	// Organisation 1 holds two workspaces; between them it may fill its own share and no more.
	if err := b.store.SaveTeam(ctx, &Team{TeamID: "T_A2", OrgID: 1, Name: "A2", Status: "active"}, []byte("unused")); err != nil {
		t.Fatal(err)
	}
	send := func(team, id string) int {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, signedSlackRequest("/slack/events", eventBody(id, team, "message"), time.Now()))
		return w.Code
	}
	for i := 0; i < slackOrgPendingLimit; i++ {
		team := "T_A"
		if i%2 == 1 {
			team = "T_A2"
		}
		if code := send(team, fmt.Sprintf("Ev%03d", i)); code != 200 {
			t.Fatalf("delivery %d for %s = %d", i+1, team, code)
		}
	}
	if code := send("T_A", "Ev-over"); code != 503 {
		t.Fatalf("organisation 1 exceeded its share: %d", code)
	}
	// Organisation 2 is unaffected by organisation 1's burst.
	if code := send("T_B", "Ev-b"); code != 200 {
		t.Fatalf("organisation 2 was refused because of organisation 1: %d", code)
	}
}

func TestInboxDeadLettersAfterRepeatedFailures(t *testing.T) {
	b, mux := slackHTTPTestBot(t)
	ctx := context.Background()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", eventBody("Ev1", "T_A", "message"), time.Now()))
	if w.Code != 200 {
		t.Fatalf("accept = %d", w.Code)
	}
	// The payload becomes unreadable — what a MASTER_KEY rotation does to everything queued.
	if _, err := b.store.db.ExecContext(ctx, `update slack_deliveries set payload_enc=?`, []byte{0}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < slackDeliveryMaxAttempts; i++ {
		if b.dispatchNextSlackDelivery(ctx) {
			t.Fatalf("attempt %d succeeded on an unreadable payload", i+1)
		}
		b.store.db.ExecContext(ctx, `update slack_deliveries set lease_until=0`) // the lease expires
	}
	var dead, pending int
	b.store.db.QueryRowContext(ctx, `select count(*) from slack_deliveries where dead_at>0 and payload_enc is null`).Scan(&dead)
	b.store.db.QueryRowContext(ctx, `select count(*) from slack_deliveries where done_at=0 and dead_at=0`).Scan(&pending)
	if dead != 1 || pending != 0 {
		t.Fatalf("after %d failures: dead=%d pending=%d, want the row retired", slackDeliveryMaxAttempts, dead, pending)
	}
	if d, _ := b.store.claimSlackDelivery(ctx); d != nil {
		t.Fatal("a dead-lettered delivery was claimed again")
	}
	// The slot is free: a new delivery for the same workspace is accepted.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", eventBody("Ev2", "T_A", "message"), time.Now()))
	if w.Code != 200 {
		t.Fatalf("after dead-lettering: %d", w.Code)
	}
}

func TestRevokedWorkspaceDeliveriesAreDropped(t *testing.T) {
	b, mux := slackHTTPTestBot(t)
	ctx := context.Background()
	b.store.RevokeTeam(ctx, "T_A", "uninstalled in the test")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", eventBody("Ev1", "T_A", "message"), time.Now()))
	var queued int
	b.store.db.QueryRowContext(ctx, `select count(*) from slack_deliveries`).Scan(&queued)
	if w.Code != 200 || queued != 0 {
		t.Fatalf("a revoked workspace's message was queued: %d, %d row(s)", w.Code, queued)
	}
	// The uninstall notice itself still gets through, since it is what revokes.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", eventBody("Ev2", "T_A", "app_uninstalled"), time.Now()))
	b.store.db.QueryRowContext(ctx, `select count(*) from slack_deliveries`).Scan(&queued)
	if w.Code != 200 || queued != 1 {
		t.Fatalf("the uninstall notice was dropped: %d, %d row(s)", w.Code, queued)
	}
}

// The cutover freeze must ACKNOWLEDGE Slack events, not refuse them.
//
// This is the single most consequential line in the runbook. Slack disables event delivery to
// an app that fails more than about 95% of deliveries, and the only way back is a reinstall by
// every connected workspace — so a freeze that answered 503 would turn ten quiet minutes into
// an outage that needs other people to fix. It must also still answer url_verification, or the
// Request URL cannot be re-verified while the freeze is on.
func TestMaintenanceAcknowledgesSlackRatherThanRefusing(t *testing.T) {
	b, mux := slackHTTPTestBot(t)
	b.cfg.Maintenance = true
	ctx := context.Background()

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", eventBody("Ev_freeze", "T_A", "message"), time.Now()))
	if w.Code != 200 {
		t.Fatalf("a Slack event during the freeze got %d; anything but 200 counts against the "+
			"failure rate that disables the app", w.Code)
	}
	// Acknowledged, and dropped: nothing queued for a database about to be replaced.
	var queued int
	if err := b.store.db.QueryRowContext(ctx, `select count(*) from slack_deliveries`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Errorf("%d delivery(s) were queued during the freeze", queued)
	}

	// And url_verification still works, so the Request URL can be re-verified mid-freeze.
	w = httptest.NewRecorder()
	body := `{"type":"url_verification","challenge":"c0ffee"}`
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", body, time.Now()))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "c0ffee") {
		t.Errorf("url_verification during the freeze: %d %s", w.Code, w.Body.String())
	}

	// With the freeze off, the same event is accepted as normal.
	b.cfg.Maintenance = false
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, signedSlackRequest("/slack/events", eventBody("Ev_after", "T_A", "message"), time.Now()))
	if w.Code != 200 {
		t.Fatalf("after the freeze: %d", w.Code)
	}
	b.store.db.QueryRowContext(ctx, `select count(*) from slack_deliveries`).Scan(&queued)
	if queued != 1 {
		t.Errorf("after the freeze %d deliveries queued, want 1", queued)
	}
}
