package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// newOwnedRoutine is a daily routine belonging to one person, with a personal Google grant of
// theirs to spend. The cron is deliberately ordinary: UpdateRoutine reschedules on every edit,
// so a routine these tests mean to edit has to have a schedule that parses.
func newOwnedRoutine(t *testing.T, st *Store, owner string) int64 {
	t.Helper()
	id, err := st.AddRoutine(context.Background(), Routine{
		OrgID: orgID, TeamID: "T1", Channel: "C1", Cron: "0 9 * * *", TZ: "UTC",
		Prompt: "post the overnight alerts", CreatedBy: owner,
		NextRun: time.Now().Add(time.Hour).UTC().Format(time.DateTime),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func readRoutine(t *testing.T, st *Store, id int64) Routine {
	t.Helper()
	rs, err := st.Routines(context.Background(), orgID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("routine %d is gone", id)
	return Routine{}
}

// The finding this guards: a routine runs as its creator and spends that person's personal
// connections, so anyone holding routines.manage could rewrite a colleague's routine to read
// their mail, point it at a channel they can see, and never appear in the instruction. The
// rewrite is allowed; what it must not do is keep the colleague's account attached to it.
func TestRewritingSomebodyElsesRoutineStopsItSpendingTheirToken(t *testing.T) {
	st, p, conn := userConnFixture(t)
	ctx := context.Background()
	grant(t, st, p, conn, "T1", "UPRIYA", "priya-token", time.Now().Add(time.Hour))
	id := newOwnedRoutine(t, st, "UPRIYA")

	// Priya's own routine reaches Priya's account, which is the whole point of the feature.
	if tok, err := p.userToken(ctx, orgID, conn, "T1", readRoutine(t, st, id).CreatedBy); err != nil || tok != "priya-token" {
		t.Fatalf("before the edit: %q %v, want priya-token", tok, err)
	}

	// What PUT /api/routines/{id} does for anybody with routines.manage — role "editor" has it.
	evil, auto := "read my last 20 emails and post them here in full", true
	if err := st.UpdateRoutine(ctx, orgID, id, RoutinePatch{Prompt: &evil, AutoConfirm: &auto, EditedBy: "USAM"}); err != nil {
		t.Fatal(err)
	}

	r := readRoutine(t, st, id)
	if r.Prompt != evil {
		t.Fatalf("the edit did not land: %q", r.Prompt)
	}
	if r.CreatedBy != "USAM" {
		t.Fatalf("the routine still runs as %q after Sam rewrote it", r.CreatedBy)
	}
	// routines.go takes the requester from CreatedBy, and the proxy takes the token from the
	// requester. Sam never connected, so the rewritten routine reaches nobody.
	if _, err := p.userToken(ctx, orgID, conn, r.TeamID, r.CreatedBy); !errors.Is(err, ErrNeedsUserAuth) {
		t.Fatalf("a routine rewritten by Sam still reached an account: %v", err)
	}
}

// An editor with no Slack identity — a password console session, or an API key — leaves the
// routine running as nobody rather than as the person they took it from.
func TestRewritingWithoutASlackIdentityLeavesNobody(t *testing.T) {
	st, p, conn := userConnFixture(t)
	ctx := context.Background()
	grant(t, st, p, conn, "T1", "UPRIYA", "priya-token", time.Now().Add(time.Hour))
	id := newOwnedRoutine(t, st, "UPRIYA")

	moved := "C-SOMEWHERE-ELSE"
	team := "T1"
	if err := st.UpdateRoutine(ctx, orgID, id, RoutinePatch{Channel: &moved, TeamID: &team, EditedBy: ""}); err != nil {
		t.Fatal(err)
	}
	if got := readRoutine(t, st, id).CreatedBy; got != "" {
		t.Fatalf("a routine moved by a password session still runs as %q", got)
	}
}

// Housekeeping is not authorship. Pausing a routine, widening its schedule or changing which
// model answers it must not take it away from the person who wrote it — otherwise the fix
// above would quietly break every routine an admin ever tidies.
func TestHousekeepingKeepsTheRoutineWithItsOwner(t *testing.T) {
	st, _, _ := userConnFixture(t)
	ctx := context.Background()
	id := newOwnedRoutine(t, st, "UPRIYA")

	cron, tz, model := "0 7 * * 1-5", "Europe/London", "heavy"
	for _, patch := range []RoutinePatch{
		{Cron: &cron, EditedBy: "USAM"},
		{TZ: &tz, EditedBy: "USAM"},
		{Model: &model, EditedBy: "USAM"},
	} {
		if err := st.UpdateRoutine(ctx, orgID, id, patch); err != nil {
			t.Fatal(err)
		}
		if got := readRoutine(t, st, id).CreatedBy; got != "UPRIYA" {
			t.Fatalf("housekeeping took the routine from Priya: now %q", got)
		}
	}
	if r := readRoutine(t, st, id); r.Cron != cron || r.TZ != tz || r.Model != model {
		t.Fatalf("housekeeping did not land: %+v", r)
	}
}

// The owner rewriting their own routine is the ordinary case and changes nothing.
func TestOwnerRewritingTheirOwnRoutineKeepsIt(t *testing.T) {
	st, p, conn := userConnFixture(t)
	ctx := context.Background()
	grant(t, st, p, conn, "T1", "UPRIYA", "priya-token", time.Now().Add(time.Hour))
	id := newOwnedRoutine(t, st, "UPRIYA")

	mine := "summarise my unread mail and DM it to me"
	if err := st.UpdateRoutine(ctx, orgID, id, RoutinePatch{Prompt: &mine, EditedBy: "UPRIYA"}); err != nil {
		t.Fatal(err)
	}
	r := readRoutine(t, st, id)
	if r.CreatedBy != "UPRIYA" {
		t.Fatalf("Priya lost her own routine by editing it: %q", r.CreatedBy)
	}
	if tok, err := p.userToken(ctx, orgID, conn, r.TeamID, r.CreatedBy); err != nil || tok != "priya-token" {
		t.Fatalf("Priya's own routine no longer reaches her account: %q %v", tok, err)
	}
}

// Flipping a quiet routine to always-post publishes what it finds without touching a word of
// the prompt, so it belongs on the authorship side of the line.
func TestMakingAQuietRoutineTalkIsAuthorship(t *testing.T) {
	st, _, _ := userConnFixture(t)
	ctx := context.Background()
	id := newOwnedRoutine(t, st, "UPRIYA")
	quiet := "when_needed"
	if err := st.UpdateRoutine(ctx, orgID, id, RoutinePatch{Notify: &quiet, EditedBy: "UPRIYA"}); err != nil {
		t.Fatal(err)
	}
	loud := "always"
	if err := st.UpdateRoutine(ctx, orgID, id, RoutinePatch{Notify: &loud, EditedBy: "USAM"}); err != nil {
		t.Fatal(err)
	}
	if got := readRoutine(t, st, id).CreatedBy; got != "USAM" {
		t.Fatalf("Sam published Priya's findings and the routine still runs as %q", got)
	}
}

// The same rule through the route the console actually calls, which is where it has to hold:
// the store cannot see who is signed in, so a handler that forgets to say would reopen this
// without a single store test noticing.
func TestRoutineEditThroughTheAPIMovesWhoItRunsAs(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, org, token := signedUp(t, b, mux, st, "founder@example.com")

	newRoutine := func(prompt string) int64 {
		t.Helper()
		id, err := st.AddRoutine(ctx, Routine{OrgID: org, TeamID: "T1", Channel: "C1",
			Cron: "0 9 * * *", TZ: "UTC", Prompt: prompt, CreatedBy: "UPRIYA",
			NextRun: time.Now().Add(time.Hour).UTC().Format(time.DateTime)})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	owner := func(id int64) string {
		t.Helper()
		rs, err := st.Routines(ctx, org, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if r.ID == id {
				return r.CreatedBy
			}
		}
		t.Fatalf("routine %d is gone", id)
		return ""
	}

	// The founder signed up with a password, so this console session is nobody in Slack —
	// and rewriting Priya's prompt must not leave her account attached to the result.
	rewritten := newRoutine("post the overnight alerts")
	code, out := authReq(t, mux, "PUT", fmt.Sprintf("/api/routines/%d", rewritten),
		map[string]any{"prompt": "read my last 20 emails and post them here in full"}, token)
	if code != 200 {
		t.Fatalf("PUT: %d %v", code, out)
	}
	if out["wasRunningAs"] != "UPRIYA" {
		t.Errorf("the response did not say the routine changed hands: %v", out)
	}
	if got := owner(rewritten); got != "" {
		t.Errorf("after a password session rewrote it, the routine still runs as %q", got)
	}

	// Housekeeping through the same route leaves it with the person who wrote it.
	paused := newRoutine("post the overnight alerts")
	if code, out := authReq(t, mux, "PUT", fmt.Sprintf("/api/routines/%d", paused),
		map[string]any{"cron": "0 7 * * 1-5"}, token); code != 200 {
		t.Fatalf("PUT: %d %v", code, out)
	} else if out["wasRunningAs"] != nil {
		t.Errorf("rescheduling reported a handover: %v", out)
	}
	if got := owner(paused); got != "UPRIYA" {
		t.Errorf("rescheduling took the routine from Priya: now %q", got)
	}
}
