package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// cardSlack answers conversations.replies with a thread whose root is an app's card: a coloured
// attachment carrying every field, and nothing at all in the message's own text. That is how a
// Acme document-erred alert arrives, and how most alerting apps post.
type cardSlack struct{ recordingSlack }

func (f *cardSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.TrimPrefix(r.URL.Path, "/api/") != "conversations.replies" {
		f.recordingSlack.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []map[string]any{
		{"bot_id": "B1", "username": "Acme", "ts": "1700000000.000100",
			"attachments": []map[string]any{{
				"color": "#e01e5a", "fallback": "Document - document erred",
				"title": "Document - document erred",
				"fields": []map[string]any{
					{"title": "Doc Id", "value": "0f12f9fe31fc4e1888a7a50c6f948e17"},
					{"title": "Status", "value": "erred"},
					{"title": "Environment", "value": "prod"},
					{"title": "Error", "value": "Error while processing"},
				},
			}}},
		{"user": "U1", "text": "why did this fail?", "ts": "1700000000.000200"},
	}})
}

// The bug this exists for: the card was the root of the thread the bot was answering in, and
// everything it said -- the doc id most of all -- was in an attachment. Read for its text alone
// the message came back empty, was dropped from the replay, and the turn asked the person to
// paste an id that had been in front of it the whole time.
func TestACardsFieldsReachTheModel(t *testing.T) {
	st := testStore(t)
	llm := &fakeLLM{answer: "The extractor API returned 500 on every attempt."}
	rec := &cardSlack{}
	slackSrv := httptest.NewServer(rec)
	llmSrv := httptest.NewServer(llm)
	t.Cleanup(slackSrv.Close)
	t.Cleanup(llmSrv.Close)

	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/api/"))}, BotUserID: "UBOT", TeamID: "T1", OrgID: 1}
	cfg := Config{Model: "test", MaxToolRounds: 3, TurnMaxMinutes: 12, HistoryLimit: 40}
	a := &Agent{
		cfg: cfg, store: st, tools: map[string]Tool{}, runs: map[int64]*runHandle{}, loc: time.UTC,
		slacks:   testRegistry(sl),
		settings: newSettingsCache(st, cfg),
		llm:      NewLLM(Config{LLMBaseURL: llmSrv.URL, LLMKey: "k", Model: "test"}),
	}

	ctx := context.Background()
	sess, err := st.EnsureSession(ctx, "T1", "C1", "1700000000.000100", "channel", "")
	if err != nil {
		t.Fatal(err)
	}
	c := &Call{TeamID: "T1", OrgID: 1, SL: sl, Channel: "C1", ThreadTS: "1700000000.000100", UserID: "U1",
		Text: "why did this fail?", Kind: "channel", Session: sess,
		Streamer: sl.NewStreamer("C1", "1700000000.000100", "U1")}
	if err := a.Run(ctx, c); err != nil {
		t.Fatalf("run: %v", err)
	}

	prompt := llm.prompt()
	for _, want := range []string{"0f12f9fe31fc4e1888a7a50c6f948e17", "Error while processing", "Document - document erred"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the model was never told %q; prompt: %s", want, prompt)
		}
	}
}

// An app that also puts the card's summary in the message text says each thing once, and a card
// no renderer understands still arrives as its fallback rather than as nothing.
func TestMessageTextSaysEachThingOnce(t *testing.T) {
	m := slack.Message{Msg: slack.Msg{
		Text: "Build failed",
		Attachments: []slack.Attachment{
			{Fallback: "Build failed", Title: "Build failed", Text: "step 3 of 7",
				Fields: []slack.AttachmentField{{Title: "Commit", Value: "79d8daa"}}},
			{Fallback: "Deploy skipped"},
		},
	}}
	got := messageText(m)
	want := "Build failed\nstep 3 of 7\nCommit: 79d8daa\nDeploy skipped"
	if got != want {
		t.Errorf("messageText =\n%q\nwant\n%q", got, want)
	}
}

// The footer under a reply is the bot's bookkeeping, and replaying it with every earlier answer
// taught the model to end its own answers with one — invented figures above the real footer. It
// is left out of a thread read back, by its block id and, on replies from before it had one, by
// its Configure link; another app's context line still arrives.
func TestTheReplyFooterIsNotReadBack(t *testing.T) {
	footer := "basic · 15k in · 101 out · $0.0019 · <https://bot.example.com/configure/C1?t=abc|Configure>"
	reply := func(blockID string) slack.Message {
		return slack.Message{Msg: slack.Msg{Text: "Three pull requests are open.", Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewMarkdownBlock("", "Three pull requests are open."),
			slack.NewContextBlock(blockID, slack.NewTextBlockObject(slack.MarkdownType, footer, false, false)),
		}}}}
	}
	for name, m := range map[string]slack.Message{"marked": reply(replyFooterBlockID), "from before the mark": reply("")} {
		if got := messageText(m); got != "Three pull requests are open." {
			t.Errorf("%s: messageText = %q", name, got)
		}
	}
	alert := slack.Message{Msg: slack.Msg{Text: "Deploy failed", Blocks: slack.Blocks{BlockSet: []slack.Block{
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, "env: prod · service: api", false, false)),
	}}}}
	if got := messageText(alert); got != "Deploy failed\nenv: prod · service: api" {
		t.Errorf("another app's context line = %q", got)
	}
}
