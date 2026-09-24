package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// streamInfo is the streaminfo entity on a post the fake recorded, or nil.
func streamInfo(p fakePost) map[string]any {
	for _, e := range p.Activity.Entities {
		if m, ok := e.(map[string]any); ok && m["type"] == "streaminfo" {
			return m
		}
	}
	return nil
}

func streamTestTransport(t *testing.T) (*msteamsTransport, *fakeMicrosoft, *Store) {
	t.Helper()
	st := testStore(t)
	f := newFakeMicrosoft(t)
	tr := f.client(st).transport("msteams:"+teamsOrg, f.srv.URL)
	if err := st.SaveTeamsUser(context.Background(), "msteams:"+teamsOrg, msUser{UserID: anaID, TeamsID: "29:ana", Name: "Ana"}); err != nil {
		t.Fatal(err)
	}
	return tr, f, st
}

// A one-to-one answer is streamed the way Teams takes one: the tool call in flight said first, then
// the answer as it is written, every update carrying all of it so far and beginning with what the
// last one showed — so a mention or a code span cut in half is held back until it is whole — never
// faster than Teams takes them, and closed by a message with the whole answer and its footer.
func TestATeamsAnswerIsStreamedInAOneToOneChat(t *testing.T) {
	tr, f, st := streamTestTransport(t)
	ctx := context.Background()

	if _, err := tr.startStream(ctx, "19:eng@thread.tacv2", "1700000009500", "", ""); !errors.Is(err, errStreamUnsupported) {
		t.Fatalf("a channel opened a stream: %v", err)
	}

	key, err := tr.startStream(ctx, "a:chat-ana", msteamsChatThread, anaID, "")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := tr.stream(key)
	later := func() { s.last = time.Time{} } // as if a second and a half had passed

	tr.streamTask(ctx, "a:chat-ana", key, activityCard, "Searching docs: refunds", taskRunning, "")
	later()
	tr.appendStream(ctx, "a:chat-ana", key, "Refunds take five working days, <@"+anaID[:8])
	later()
	tr.appendStream(ctx, "a:chat-ana", key, anaID[8:]+"> asked before. Use `the refund")
	later()
	tr.appendStream(ctx, "a:chat-ana", key, " form` for the rest. And")
	tr.appendStream(ctx, "a:chat-ana", key, " one") // too soon after the last: folded into the next
	later()
	tr.streamTask(ctx, "a:chat-ana", key, activityCard, "Reading a thread", taskRunning, "") // the answer has begun
	id, err := tr.stopStream(ctx, "a:chat-ana", key, " more thing.", "test-model · Configure")
	if err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	posts := append([]fakePost(nil), f.posts...)
	f.mu.Unlock()
	want := []struct{ kind, text string }{
		{"informative", "Searching docs: refunds…"},
		{"streaming", "Refunds take five working days,"},
		{"streaming", "Refunds take five working days, <at>Ana</at> asked before. Use"},
		{"streaming", "Refunds take five working days, <at>Ana</at> asked before. Use `the refund form` for the rest."},
		{"final", "Refunds take five working days, <at>Ana</at> asked before. Use `the refund form` for the rest. And one more thing.\n\ntest-model · Configure"},
	}
	if len(posts) != len(want) {
		t.Fatalf("%d posts, want %d: %+v", len(posts), len(want), posts)
	}
	streamID := posts[0].ID
	for i, w := range want {
		p, info := posts[i], streamInfo(posts[i])
		if info == nil || info["streamType"] != w.kind || p.Activity.Text != w.text {
			t.Fatalf("post %d = %q %v, want %s %q", i, p.Activity.Text, info, w.kind, w.text)
		}
		if p.Conversation != "a:chat-ana" {
			t.Errorf("post %d went to %s", i, p.Conversation)
		}
		switch {
		case w.kind == "final":
			if p.Activity.Type != "message" || p.Activity.TextFormat != "markdown" || info["streamSequence"] != nil || info["streamId"] != streamID {
				t.Errorf("the closing message was %+v %v", p.Activity, info)
			}
		case p.Activity.Type != "typing" || info["streamSequence"] != float64(i+1):
			t.Errorf("update %d was a %s with sequence %v", i, p.Activity.Type, info["streamSequence"])
		case i == 0 && info["streamId"] != nil:
			t.Errorf("the opening update named a stream: %v", info)
		case i > 0 && info["streamId"] != streamID:
			t.Errorf("update %d is on stream %v, want %s", i, info["streamId"], streamID)
		}
		if i > 1 && !strings.HasPrefix(p.Activity.Text, posts[i-1].Activity.Text) {
			t.Errorf("update %d does not begin with what update %d showed; Teams refuses that", i, i-1)
		}
	}
	if id != posts[len(posts)-1].ID {
		t.Errorf("stopStream said the answer is %s, it is %s", id, posts[len(posts)-1].ID)
	}
	// The next turn reads the chat back from the log, so the streamed answer is in it.
	thread, _ := st.TeamsThread(ctx, "msteams:"+teamsOrg, "a:chat-ana", msteamsChatThread)
	if len(thread) != 1 || !thread[0].IsBot || !strings.Contains(thread[0].Text, "<@"+anaID+">") {
		t.Errorf("the log holds %+v", thread)
	}
}

// Stop means stop: once the person has stopped a stream, nothing more is sent on it and the answer
// is not posted some other way instead.
func TestAStoppedTeamsStreamIsNotFinishedAnotherWay(t *testing.T) {
	tr, f, _ := streamTestTransport(t)
	ctx := context.Background()
	key, _ := tr.startStream(ctx, "a:chat-ana", msteamsChatThread, anaID, "")
	s, _ := tr.stream(key)
	tr.appendStream(ctx, "a:chat-ana", key, "The first part of a long answer ")
	f.mu.Lock()
	f.refuse = func(msOutgoing) (int, string) {
		return 403, `{"error":{"code":"ContentStreamNotAllowed","message":"Content stream was canceled by user"}}`
	}
	f.mu.Unlock()
	s.last = time.Time{}
	tr.appendStream(ctx, "a:chat-ana", key, "and the second part ")
	f.mu.Lock()
	f.refuse = nil
	f.mu.Unlock()
	s.last = time.Time{}
	tr.appendStream(ctx, "a:chat-ana", key, "and a third ")
	if _, err := tr.stopStream(ctx, "a:chat-ana", key, "and the end.", ""); err != nil {
		t.Fatal(err)
	}
	if got := f.messages(); len(got) != 0 {
		t.Errorf("a stopped answer was posted anyway: %+v", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.posts) != 1 {
		t.Errorf("%d updates were sent, want only the one before Stop", len(f.posts))
	}
}

// A stream that was never opened, or is too old to close — Teams ends every stream at two minutes
// — ends as a plain message with the whole answer, the one it would have posted had it never
// streamed at all.
func TestATeamsStreamThatCannotBeClosedEndsAsAPlainMessage(t *testing.T) {
	tr, f, _ := streamTestTransport(t)
	ctx := context.Background()

	key, _ := tr.startStream(ctx, "a:chat-ana", msteamsChatThread, anaID, "")
	if _, err := tr.stopStream(ctx, "a:chat-ana", key, "Short answer.", ""); err != nil {
		t.Fatal(err)
	}
	key, _ = tr.startStream(ctx, "a:chat-ana", msteamsChatThread, anaID, "")
	tr.streamTask(ctx, "a:chat-ana", key, activityCard, "Searching docs: everything", taskRunning, "")
	s, _ := tr.stream(key)
	s.opened = time.Now().Add(-msStreamClose - time.Second)
	s.last = time.Time{}
	tr.appendStream(ctx, "a:chat-ana", key, "An answer after a very long search ") // too late to begin on this stream
	if _, err := tr.stopStream(ctx, "a:chat-ana", key, "is still an answer.", ""); err != nil {
		t.Fatal(err)
	}

	got := f.messages()
	if len(got) != 2 {
		t.Fatalf("%d messages, want the two answers: %+v", len(got), got)
	}
	for i, text := range []string{"Short answer.", "An answer after a very long search is still an answer."} {
		if got[i].Activity.Text != text || streamInfo(got[i]) != nil {
			t.Errorf("answer %d was %q %v, want a plain %q", i, got[i].Activity.Text, streamInfo(got[i]), text)
		}
	}
}

// What can be shown for good is cut at a space, before a mention, a link or a code span that is not
// finished yet.
func TestOnlyWhatWillNotChangeIsStreamed(t *testing.T) {
	for raw, want := range map[string]string{
		"":                                "",
		"oneword":                         "",
		"two words":                       "two",
		"ends with a space ":              "ends with a space",
		"see <https://example.com|the":    "see",
		"ask <@8f3b1c2d-0000> now please": "ask <@8f3b1c2d-0000> now",
		"run `go test ./... now":          "run",
		"run `go test` now please":        "run `go test` now",
		"```\ncode block\nstill open":     "",
		"text\n```\ncode\n``` after it":   "text\n```\ncode\n``` after",
		"a `span with spaces` b":          "a `span with spaces`",
		"inside `a span":                  "inside",
		"x < y and more":                  "x",
	} {
		if got := msStreamable(raw); got != want {
			t.Errorf("msStreamable(%q) = %q, want %q", raw, got, want)
		}
	}
}
