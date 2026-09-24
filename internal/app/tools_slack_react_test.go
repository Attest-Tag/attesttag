package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// Until this tool existed, an emoji could only be placed by the "read every message" classifier
// (bot.go, watch) — and a channel taking forwarded mail never reaches that, because those turns
// are explicit. So a channel instruction saying "tick the message once the ticket is filed" was
// asking for something nothing in the process could do, silently.
func TestReactPutsTheEmojiOnThisChannelsMessage(t *testing.T) {
	var gotChannel, gotTS, gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/reactions.add") {
			http.NotFound(w, r)
			return
		}
		r.ParseForm()
		gotChannel, gotTS, gotName = r.Form.Get("channel"), r.Form.Get("timestamp"), r.Form.Get("name")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	a := &Agent{tools: map[string]Tool{}}
	a.registerSlackTools()
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, TeamID: "T1", BotUserID: "UBOT"}
	// An email turn: the thread root is the message Slack posted the mail as, which is the one
	// the channel's instructions mean by "the message".
	c := &Call{OrgID: 1, TeamID: "T1", SL: sl, Channel: "C1", ThreadTS: "1700000000.0001",
		UserID: emailRequester("C1")}

	// Models write the colons about as often as not, and Slack answers invalid_name to them.
	out, err := a.tools["react"].Run(context.Background(), c, []byte(`{"emoji":":White_Check_Mark:"}`))
	if err != nil {
		t.Fatalf("react: %v", err)
	}
	if gotChannel != "C1" || gotTS != "1700000000.0001" || gotName != "white_check_mark" {
		t.Errorf("sent channel=%q ts=%q name=%q", gotChannel, gotTS, gotName)
	}
	if !strings.Contains(out, "white_check_mark") {
		t.Errorf("the result should name what it added: %q", out)
	}

	// A channel the model names is not honoured: a reaction is visible to everyone where it
	// lands, so this tool does not travel out of the turn's own channel.
	gotChannel = ""
	if _, err := a.tools["react"].Run(context.Background(), c, []byte(`{"emoji":"x","ts":"1700000000.0002"}`)); err != nil {
		t.Fatalf("react with an explicit ts: %v", err)
	}
	if gotChannel != "C1" || gotTS != "1700000000.0002" {
		t.Errorf("an explicit ts should stay in this channel: channel=%q ts=%q", gotChannel, gotTS)
	}

	// A refusal the model can act on, rather than a round spent on Slack's invalid_name.
	if _, err := a.tools["react"].Run(context.Background(), c, []byte(`{"emoji":"a thumbs up please"}`)); err == nil {
		t.Error("a sentence is not an emoji name")
	}
}
