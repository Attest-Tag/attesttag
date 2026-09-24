package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/slack-go/slack"
)

// A card used to be built as Block Kit directly, so what Slack received was whatever each call site
// assembled. Now every card is a Card drawn by one renderer, and this pins that drawing to the
// blocks the call sites used to build by hand: a section per paragraph (cut at Slack's limit), an
// uncut context line for small print, one actions row, and the footer as a context line under it.
func TestSlackBlocksDrawsACardAsTheCallSitesUsedTo(t *testing.T) {
	card := Card{
		Parts: []CardPart{
			{Markdown: "*Alice is asking for access*"},
			{Markdown: "Asked by Alice in #ops", Small: true},
		},
		Buttons: []Button{
			{ActionID: "approve", Value: "7|", Label: "Approve", Primary: true},
			{ActionID: "deny", Value: "7|", Label: "Deny"},
			{ActionID: "open", Value: "open", Label: "Open", Primary: true, URL: "https://example.com/x"},
		},
		Footer: "Expires in an hour.",
	}
	want := []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "*Alice is asking for access*", false, false), nil, nil),
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, "Asked by Alice in #ops", false, false)),
		slack.NewActionBlock("",
			slack.NewButtonBlockElement("approve", "7|", slack.NewTextBlockObject(slack.PlainTextType, "Approve", true, false)).WithStyle(slack.StylePrimary),
			slack.NewButtonBlockElement("deny", "7|", slack.NewTextBlockObject(slack.PlainTextType, "Deny", true, false)),
			slack.NewButtonBlockElement("open", "open", slack.NewTextBlockObject(slack.PlainTextType, "Open", true, false)).WithStyle(slack.StylePrimary).WithURL("https://example.com/x"),
		),
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, "Expires in an hour.", false, false)),
	}
	got, _ := json.Marshal(slackBlocks(card))
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Fatalf("card drew differently from the hand-built blocks\n got: %s\nwant: %s", got, exp)
	}
}

// cardRecorder is a transport that only knows how to rewrite a card. Anything else a test did not
// expect to be called panics, through the nil transport it embeds.
type cardRecorder struct {
	transport
	updated []Card
}

func (r *cardRecorder) updateCard(_ context.Context, _, _ string, card Card, _ string) error {
	r.updated = append(r.updated, card)
	return nil
}

// Settling an access request used to rebuild its card and cut the last two blocks off, trusting
// that they were the buttons and the footer. An answered card is now the same card with its buttons
// gone and the outcome in the footer's place, whatever shape it had.
func TestAnAnsweredCardKeepsWhatItSaidAndLosesItsButtons(t *testing.T) {
	rec := &cardRecorder{}
	sl := &Chat{t: rec}
	r := &AccessRequest{ID: 7, Requester: "U_ALICE", What: "a seat", Channel: "C1",
		Why: "joining the team", ExpiresAt: "2026-09-23 12:00:00"}
	keep := accessCard(r, "", "", false)

	sl.ResolveCard(context.Background(), "D1", "111.1", keep, "Approved by <@U_BOB>.")

	if len(rec.updated) != 1 {
		t.Fatalf("want one card rewritten, got %d", len(rec.updated))
	}
	got := rec.updated[0]
	if len(got.Buttons) != 0 {
		t.Errorf("an answered card still has %d buttons", len(got.Buttons))
	}
	if got.Footer != "Approved by <@U_BOB>." {
		t.Errorf("footer = %q, want the outcome", got.Footer)
	}
	if len(got.Parts) != len(keep.Parts) {
		t.Fatalf("kept %d parts of %d", len(got.Parts), len(keep.Parts))
	}
	for i := range keep.Parts {
		if got.Parts[i] != keep.Parts[i] {
			t.Errorf("part %d changed: %+v, want %+v", i, got.Parts[i], keep.Parts[i])
		}
	}
}
