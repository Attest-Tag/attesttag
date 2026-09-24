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

// assistant.search.context is the only search a bot token can do, and Slack will not run it
// without the action token from the message that asked. Sending the call without one earned
// invalid_action_token on every attempt the product ever made — five for five — which the
// model reported to the person asking as "the search tool is erroring out".
func TestSlackSearchSendsTheTurnsActionToken(t *testing.T) {
	var gotToken, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/assistant.search.context") {
			http.NotFound(w, r)
			return
		}
		r.ParseForm()
		gotToken, gotQuery = r.Form.Get("action_token"), r.Form.Get("query")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"results": map[string]any{"messages": []map[string]any{{
				"author_name": "alex", "channel_name": "compliance",
				"permalink": "https://x/p/1", "content": "the SOC 2 report is in Drive",
			}}},
		})
	}))
	defer srv.Close()

	a := &Agent{tools: map[string]Tool{}}
	a.registerSlackTools()
	sl := &Chat{t: &slackTransport{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/api/"))}, TeamID: "T1", BotUserID: "UBOT"}
	c := &Call{OrgID: 1, TeamID: "T1", SL: sl, Channel: "C1", UserID: "U1", ActionToken: "1234567.abcdefg"}

	out, err := a.tools["slack_search"].Run(context.Background(), c, []byte(`{"query":"SOC 2 report"}`))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotToken != "1234567.abcdefg" {
		t.Errorf("action_token sent = %q, want the turn's token", gotToken)
	}
	if gotQuery != "SOC 2 report" {
		t.Errorf("query sent = %q", gotQuery)
	}
	if !strings.Contains(out, "compliance") || !strings.Contains(out, "SOC 2 report is in Drive") {
		t.Errorf("result not rendered: %q", out)
	}
}

// A routine, an investigation or a confirmed-write follow-up has no message behind it and so
// no token to search with, and none can be asked for after the fact. That is a sentence the
// model can act on, not a failure: a raw Slack error taught it to tell people search is broken.
func TestSlackSearchWithoutATokenSaysSoInsteadOfFailing(t *testing.T) {
	a := &Agent{tools: map[string]Tool{}}
	a.registerSlackTools()
	c := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", UserID: "U1"} // no ActionToken: not a person typing

	out, err := a.tools["slack_search"].Run(context.Background(), c, []byte(`{"query":"SOC 2"}`))
	if err != nil {
		t.Fatalf("a turn that cannot search is not an error: %v", err)
	}
	if !strings.Contains(out, "not available") {
		t.Errorf("slack_search said %q, want an explanation of why it cannot search", out)
	}
}
