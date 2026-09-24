package app

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// A held write is its requester's to answer, or an approver's in that workspace — whichever way
// the answer arrives. Typing "cancel" used to drop everything held in the thread for whoever typed
// it, so anybody reading a thread could throw away the write a colleague was about to confirm;
// and the Cancel button, having checked the one card it was on, dropped everybody else's with it.
func TestCancellingIsTheRequestersOrAnApprovers(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	org, roleID := approvalOrg(t, st)
	if err := st.AddResolvedApprovalMember(ctx, org, roleID, "U_APP", "T1", "U_APP"); err != nil {
		t.Fatal(err)
	}
	rec := &recordingSlack{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1", OrgID: org}
	b := &Bot{store: st, settings: newSettingsCache(st, Config{}), agent: &Agent{store: st}}

	hold := func(thread, requester string) int64 {
		t.Helper()
		id, err := st.AddPendingWrite(ctx, org, "T1", "C1", thread, requester, `{"method":"POST","url":"https://api.example.com/tickets"}`)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	held := func(thread string) []string {
		t.Helper()
		hs, err := st.PendingWritesInThread(ctx, org, "T1", "C1", thread)
		if err != nil {
			t.Fatal(err)
		}
		var who []string
		for _, h := range hs {
			who = append(who, h.Requester)
		}
		return who
	}
	typed := func(thread, user, text string) bool {
		t.Helper()
		return b.confirmFlow(ctx, sl, "channel", "C1", thread, user, text, &Session{})
	}

	// Somebody who neither asked nor approves types it: nothing goes, and they are told whose it is
	// instead of the model being left to answer "cancel" as if it had worked.
	hold("1.1", "U_ALICE")
	if !typed("1.1", "U_BYSTANDER", "cancel") {
		t.Error("a refused cancel was handed to the model, which would say it had been done")
	}
	if got := held("1.1"); len(got) != 1 {
		t.Fatalf("a bystander's typed cancel dropped somebody else's write: %v left", got)
	}
	if posts := rec.posts(); len(posts) == 0 || !strings.Contains(posts[len(posts)-1], "<@U_ALICE> or an approver") {
		t.Errorf("the refusal should say whose it is: %v", posts)
	}

	// Two people's writes in one thread: each person's "no" is about their own.
	hold("1.1", "U_BOB")
	typed("1.1", "U_ALICE", "no")
	if got := held("1.1"); len(got) != 1 || got[0] != "U_BOB" {
		t.Fatalf("Alice's no should drop hers and leave Bob's, left %v", got)
	}
	// An approver here may answer anybody's.
	typed("1.1", "U_APP", "Cancel.")
	if got := held("1.1"); len(got) != 0 {
		t.Fatalf("an approver's cancel left %v", got)
	}
	// And a typed cancel is on the record, the way a pressed one is.
	events, err := st.AuditEvents(ctx, org, AuditFilter{Action: "write.cancelled"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Errorf("want Alice's and the approver's cancels recorded, got %d rows", len(events))
	}

	// The button: a press on your own card drops your own writes in the thread and nobody else's.
	mine := hold("2.2", "U_ALICE")
	hold("2.2", "U_ALICE")
	hold("2.2", "U_BOB")
	b.cancelPressed(ctx, sl, "C1", "2.2", "card", "U_ALICE", "summary", mine, false)
	if got := held("2.2"); len(got) != 1 || got[0] != "U_BOB" {
		t.Fatalf("Alice's Cancel should drop both of hers and leave Bob's, left %v", got)
	}
	// A press on somebody else's card is still refused whole.
	theirs := hold("2.2", "U_BOB")
	b.cancelPressed(ctx, sl, "C1", "2.2", "card", "U_BYSTANDER", "summary", theirs, false)
	if got := held("2.2"); len(got) != 2 {
		t.Fatalf("a bystander's press dropped something: %v left", got)
	}
}
