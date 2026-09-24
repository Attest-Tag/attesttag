package app

import (
	"context"
	"reflect"
	"testing"
)

// `!restart` promised "I'll only look at messages from here on" and did two other things: it
// archived the session, which nothing ever set back, so every follow-up that did not mention the
// bot was ignored from then on; and it forgot nothing, because a turn reads the thread from Slack,
// whole. Now the session stays active and remembers the message the restart was said in.
func TestRestartKeepsTheThreadAndCutsItAtTheCommand(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, nil, newSettingsCache(st, Config{}))
	if _, err := st.EnsureSession(ctx, "T1", "C1", "100.000100", "channel", ""); err != nil {
		t.Fatal(err)
	}
	st.SetSessionSummary(ctx, "T1", "C1", "100.000100", "they agreed on Tuesday", "100.000150")

	c := &Call{OrgID: orgID, TeamID: "T1", Channel: "C1", ThreadTS: "100.000100", MessageTS: "100.000200",
		UserID: "U1", Kind: "channel", Session: &Session{}, HumanTurn: true}
	if _, reply := a.command(ctx, c, "!restart"); reply != "Fresh start. I'll only look at messages from here on." {
		t.Fatalf("!restart answered %q", reply)
	}
	sess, err := st.GetSession(ctx, "T1", "C1", "100.000100")
	if err != nil || sess == nil {
		t.Fatalf("session: %v", err)
	}
	if sess.Status != "active" {
		t.Errorf("status %q after !restart: follow-ups that do not mention the bot would be ignored", sess.Status)
	}
	if sess.RestartTS != "100.000200" {
		t.Errorf("restart point %q, want the !restart message's own ts", sess.RestartTS)
	}
	if summary, upto := st.SessionSummary(ctx, "T1", "C1", "100.000100"); summary != "" || upto != "" {
		t.Errorf("the summary of the forgotten part survived: %q up to %q", summary, upto)
	}

	// A command with no message to cut at must not claim a fresh start it cannot deliver.
	c.MessageTS = ""
	if _, reply := a.command(ctx, c, "!restart"); reply == "Fresh start. I'll only look at messages from here on." {
		t.Error("!restart claimed a fresh start with nowhere to cut the thread")
	}
}

// Threads archived before the fix — and any archived for another reason — come back when the
// bot is talked to there again, instead of staying deaf to every unmentioned follow-up for good.
func TestEnsureSessionReactivatesAnArchivedThread(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	st.EnsureSession(ctx, "T1", "C1", "100.000100", "channel", "")
	if err := st.ArchiveSession(ctx, "T1", "C1", "100.000100"); err != nil {
		t.Fatal(err)
	}
	sess, err := st.EnsureSession(ctx, "T1", "C1", "100.000100", "channel", "")
	if err != nil || sess == nil || sess.Status != "active" {
		t.Fatalf("EnsureSession left the thread %+v (err %v), want it active", sess, err)
	}
}

func TestAfterRestart(t *testing.T) {
	thread := []ThreadMsg{{TS: "100.000100", Text: "root"}, {TS: "100.000200", Text: "!restart"},
		{TS: "100.000300", Text: "after"}, {TS: "100.000400", Text: "later"}}
	texts := func(ms []ThreadMsg) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Text)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		sess *Session
		want []string
	}{
		{"no restart", &Session{}, []string{"root", "!restart", "after", "later"}},
		{"no session", nil, []string{"root", "!restart", "after", "later"}},
		{"cut at the command, which goes too", &Session{RestartTS: "100.000200"}, []string{"after", "later"}},
		{"the command was deleted", &Session{RestartTS: "100.000250"}, []string{"after", "later"}},
		{"said as the last message", &Session{RestartTS: "100.000400"}, nil},
	} {
		if got := texts(afterRestart(thread, tc.sess)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
