package app

import (
	"context"
	"strings"
	"testing"
)

// The bug this pins: `!help` prints every command in backticks, Slack's composer keeps that
// formatting through a copy-paste, and the message that arrives is `!mute` rather than !mute.
// The bang was no longer the first character, so the command fell through to the model — which
// answered that it could not mute itself, in a thread where one word would have.
func TestCommandReadsThroughSlackCodeFormatting(t *testing.T) {
	for _, text := range []string{"!mute", "`!mute`", "```!mute```", "  `!mute` "} {
		a := &Agent{store: testStore(t)}
		c := &Call{TeamID: "T1", Channel: "C1", ThreadTS: "1788358509.639309", UserID: "U1", Kind: "channel"}
		a.store.EnsureSession(context.Background(), c.TeamID, c.Channel, c.ThreadTS, c.Kind, "")

		handled, reply := a.command(context.Background(), c, text)
		if !handled {
			t.Fatalf("%q was not handled as a command", text)
		}
		if !strings.Contains(reply, "this thread only") {
			t.Errorf("%q: the reply must say the mute is thread-scoped, got %q", text, reply)
		}
		se, err := a.store.GetSession(context.Background(), c.TeamID, c.Channel, c.ThreadTS)
		if err != nil || se == nil {
			t.Fatalf("%q: session: %v", text, err)
		}
		if !se.Muted {
			t.Errorf("%q: the thread was not actually muted", text)
		}
	}
}

// An argument survives the unwrapping, and a message that merely opens with a code span is
// left alone — "`foo` is broken" is a question for the model, not a command.
func TestUnwrapCodeLeavesOrdinaryMessagesAlone(t *testing.T) {
	cases := map[string]string{
		"`!note` buy milk": "!note  buy milk",
		"`foo` is broken":  "foo  is broken", // rewritten, but still not a command
		"what is `!mute`?": "what is `!mute`?",
		"!mute":            "!mute",
		"`unclosed !mute":  "`unclosed !mute",
	}
	for in, want := range cases {
		if got := unwrapCode(in); got != want {
			t.Errorf("unwrapCode(%q) = %q, want %q", in, got, want)
		}
	}

	a := &Agent{store: testStore(t)}
	c := &Call{TeamID: "T1", Channel: "C1", ThreadTS: "1", UserID: "U1", Kind: "channel"}
	if handled, _ := a.command(context.Background(), c, "`foo` is broken"); handled {
		t.Error("a sentence that opens with a code span must reach the model, not the command switch")
	}
}

// Mute is keyed by thread, so it must stop replies in the one thread and nowhere else — and
// `!unmute` has to work while muted, which is why commands run before the mute check.
func TestMuteCoversOneThreadOnly(t *testing.T) {
	ctx := context.Background()
	a := &Agent{store: testStore(t)}
	muted := &Call{TeamID: "T1", Channel: "C1", ThreadTS: "1788358509.639309", UserID: "U1", Kind: "channel"}
	sibling := &Call{TeamID: "T1", Channel: "C1", ThreadTS: "1788358600.111111", UserID: "U1", Kind: "channel"}
	elsewhere := &Call{TeamID: "T1", Channel: "C2", ThreadTS: "1788358509.639309", UserID: "U1", Kind: "channel"}
	for _, c := range []*Call{muted, sibling, elsewhere} {
		a.store.EnsureSession(ctx, c.TeamID, c.Channel, c.ThreadTS, c.Kind, "")
	}

	a.command(ctx, muted, "!mute")
	for _, c := range []*Call{sibling, elsewhere} {
		se, _ := a.store.GetSession(ctx, c.TeamID, c.Channel, c.ThreadTS)
		if se == nil || se.Muted {
			t.Errorf("muting one thread must not silence %s/%s", c.Channel, c.ThreadTS)
		}
	}

	handled, reply := a.command(ctx, muted, "`!unmute`")
	if !handled || reply == "" {
		t.Fatal("!unmute must be answered even in a muted thread")
	}
	if se, _ := a.store.GetSession(ctx, muted.TeamID, muted.Channel, muted.ThreadTS); se.Muted {
		t.Error("!unmute did not clear the mute")
	}
}
