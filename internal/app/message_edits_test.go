package app

import (
	"context"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// Slack sends message_changed for things that are not edits at all: a thread parent gets one
// every time a reply lands under it, and the bot streams its own answer by editing the message
// it already posted. Recording those filled the transcript the model reads with notes saying
// somebody had edited a message that nobody had touched — and the ones quoting the bot's own
// reply taught it to write footers, step lines and signed Configure links of its own.
func TestMessageChangedRecordsOnlyRealHumanEdits(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := st.EnsureSession(ctx, "T1", "C1", "111.1", "channel", ""); err != nil {
		t.Fatalf("session: %v", err)
	}
	// SelfTest on: the bot's own edits must be ignored even here, which is where they used to
	// be recorded — and where every local run and every eval sees them.
	b := &Bot{store: st, cfg: Config{SelfTest: true}}
	sl := &Chat{TeamID: "T1", BotUserID: "UBOT"}
	sl.names.Store("U1", "alex")

	edit := func(user, before, after string) *slackevents.MessageEvent {
		return &slackevents.MessageEvent{
			SubType: "message_changed", Channel: "C1",
			Message:         &slack.Msg{User: user, Text: after, ThreadTimestamp: "111.1", Timestamp: "111.2"},
			PreviousMessage: &slack.Msg{User: user, Text: before, ThreadTimestamp: "111.1", Timestamp: "111.2"},
		}
	}
	notes := func() []Turn {
		t.Helper()
		n, err := st.Notes(ctx, "T1", "C1", "111.1")
		if err != nil {
			t.Fatalf("notes: %v", err)
		}
		return n
	}

	// A reply landing under the thread parent: Slack re-sends the parent, text unchanged.
	b.message(ctx, sl, edit("U1", "what is the wifi password?", "what is the wifi password?"))
	if n := notes(); len(n) != 0 {
		t.Errorf("an unchanged message wrote %d notes, want 0: %+v", len(n), n)
	}

	// The bot streaming its answer into the message it already posted, footer and all.
	b.message(ctx, sl, edit("UBOT", "", "Here is the answer.\nglm-5.3-flash · 3.3k in · 71 out · $0.0003 · Configure"))
	if n := notes(); len(n) != 0 {
		t.Errorf("the bot's own streamed edit wrote %d notes, want 0: %+v", len(n), n)
	}

	// Somebody actually changing what they said, which the model does need to know about.
	b.message(ctx, sl, edit("U1", "deploy on friday", "deploy on monday"))
	got := notes()
	if len(got) != 1 {
		t.Fatalf("a real edit wrote %d notes, want 1: %+v", len(got), got)
	}
	want := `alex edited a message: "deploy on friday" → "deploy on monday"`
	if got[0].Content != want {
		t.Errorf("note = %q, want %q", got[0].Content, want)
	}
}
