package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// A request on somebody's own connection is in the log as whose it was, what it called and how it
// went — which is what the reader of Activity needs to follow what the bot did. What was asked and
// what came back are not: the query string is the search, the body is the mail, and the response
// is their calendar, and Activity is open to every viewer.
func TestAPersonalConnectionCallIsLoggedWithoutItsContent(t *testing.T) {
	const (
		secretQuery = "interview-at-acme"
		secretBody  = "Offer call with Acme"
		secretEvent = "Final round, Acme Corp"
	)
	a, c, st := personalCallFixture(t, func(r *http.Request) (int, string, http.Header) {
		return 200, `{"items":[{"summary":"` + secretEvent + `"}]}`, nil
	})
	ctx := context.Background()

	read := `{"method":"GET","url":"https://www.googleapis.com/calendar/v3/calendars/primary/events?q=` + secretQuery + `&maxResults=5"}`
	if out, ok := a.runToolRaw(ctx, c, "http_request", read); !ok || !strings.Contains(out, secretEvent) {
		t.Fatalf("the read did not run: %s", out)
	}
	// A write stops for a Confirm, and the sentence the model is handed quotes the request.
	write := `{"method":"POST","url":"https://www.googleapis.com/calendar/v3/calendars/primary/events?sendUpdates=all&q=` + secretQuery + ` now","body":"{\"summary\":\"` + secretBody + `\"}"}`
	if out, _ := a.runToolRaw(ctx, c, "http_request", write); !strings.Contains(out, "needs a human OK") {
		t.Fatalf("the write was not held: %s", out)
	}

	rows := st.ToolCallsAfter(ctx, orgID, "1", 0)
	if len(rows) != 2 {
		t.Fatalf("logged %d calls, want 2", len(rows))
	}
	for _, r := range rows {
		for _, secret := range []string{secretQuery, secretBody, secretEvent} {
			if strings.Contains(r.Args, secret) || strings.Contains(r.Result, secret) {
				t.Errorf("%s logged %q, which is the person's own:\nargs %s\nresult %s", r.Name, secret, r.Args, r.Result)
			}
		}
		if !strings.HasPrefix(r.Args, privateMark+" ") || !strings.HasPrefix(r.Result, privateMark+" ") {
			t.Errorf("not marked private, so every reader would take it for the call itself:\nargs %s\nresult %s", r.Args, r.Result)
		}
	}

	type keptArgs struct{ Connection, Owner, Method, Endpoint string }
	type keptResult struct {
		Status  int
		Bytes   int
		Outcome string
	}
	var readArgs, writeArgs keptArgs
	var readResult, writeResult keptResult
	decodeKept(t, rows[0].Args, &readArgs)
	decodeKept(t, rows[0].Result, &readResult)
	if readArgs.Connection != "Google" || readArgs.Owner != "Priya Shah" || readArgs.Method != "GET" ||
		readArgs.Endpoint != "https://www.googleapis.com/calendar/v3/calendars/primary/events" {
		t.Errorf("the read kept %+v, want whose account, which connection and the endpoint", readArgs)
	}
	if readResult.Status != 200 || readResult.Bytes == 0 || readResult.Outcome != "" {
		t.Errorf("the read's outcome is %+v, want its status and size", readResult)
	}

	decodeKept(t, rows[1].Args, &writeArgs)
	decodeKept(t, rows[1].Result, &writeResult)
	if writeArgs.Method != "POST" || writeArgs.Endpoint != "https://www.googleapis.com/calendar/v3/calendars/primary/events" {
		t.Errorf("the write kept %+v", writeArgs)
	}
	if writeResult.Status != 0 || writeResult.Outcome != "This is a write request (POST https://www.googleapis.com/calendar/v3/calendars/primary/events) and needs a human OK." {
		t.Errorf("the write's outcome is %+v, want the sentence saying why nothing came back", writeResult)
	}
}

// The repeat guard logs the arguments of a call it refuses to run again, and those are the
// arguments of a call that did run — a personal one's included.
func TestARefusedRepeatOfAPersonalCallIsLoggedPrivately(t *testing.T) {
	a, c, st := personalCallFixture(t, nil)
	ctx := context.Background()
	a.refuseTool(ctx, c, "http_request", `{"method":"GET","url":"https://www.googleapis.com/calendar/v3/calendars/primary/events?q=interview-at-acme"}`)
	rows := st.ToolCallsAfter(ctx, orgID, "1", 0)
	if len(rows) != 1 {
		t.Fatalf("logged %d calls, want the refusal", len(rows))
	}
	if strings.Contains(rows[0].Args, "interview-at-acme") || !strings.HasPrefix(rows[0].Args, privateMark) {
		t.Errorf("the refusal logged %s", rows[0].Args)
	}
}

// What stands in for a response keeps its sense and loses the request's content, including the
// part of a query an URL pattern would miss: the model writes spaces into queries, and the
// sentence quotes the URL exactly as it was written.
func TestPrivateLineCutsTheQueryOutOfTheSentence(t *testing.T) {
	for _, tc := range []struct{ in, url, want string }{
		{
			"Held for approval: GET https://www.googleapis.com/drive/v3/files?q=fullText contains 'SOC 2' needs a named approver's OK, and it is not the requester's to confirm. Say plainly what you have asked for.",
			"https://www.googleapis.com/drive/v3/files?q=fullText contains 'SOC 2'",
			"Held for approval: GET https://www.googleapis.com/drive/v3/files needs a named approver's OK, and it is not the requester's to confirm.",
		},
		{
			// An error from the transport quotes the URL as it went on the wire, encoded.
			`Get "https://gmail.googleapis.com/gmail/v1/users/me/messages?q=from%3Aboss%20salary": context deadline exceeded`,
			"https://gmail.googleapis.com/gmail/v1/users/me/messages?q=from:boss salary",
			`Get "https://gmail.googleapis.com/gmail/v1/users/me/messages": context deadline exceeded`,
		},
		{
			// A held write's card puts the body on the lines after the first.
			"*Waiting for your OK* on Google\n`POST https://www.googleapis.com/gmail/v1/users/me/messages/send`\n```{\"raw\":\"secret\"}``` was NOT sent.",
			"https://www.googleapis.com/gmail/v1/users/me/messages/send",
			"*Waiting for your OK* on Google",
		},
	} {
		if got := privateLine(tc.in, tc.url); got != tc.want {
			t.Errorf("privateLine(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

// The console takes the mark off and says the row is private; a row logged before the log kept
// anything reads as private with nothing to show, and every other row is left alone.
func TestUnmarkPrivate(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		in                   ToolCallRow
		wantArgs, wantResult string
		wantPrivate          bool
	}{
		{"kept", ToolCallRow{Args: `(private) {"owner":"Priya Shah"}`, Result: `(private) {"bytes":12}`},
			`{"owner":"Priya Shah"}`, `{"bytes":12}`, true},
		{"logged before anything was kept", ToolCallRow{Args: "(private)", Result: "(private)"}, "", "", true},
		{"refused, whose result is the refusal", ToolCallRow{Args: `(private) {"owner":"Priya Shah"}`, Result: "error: refused"},
			`{"owner":"Priya Shah"}`, "error: refused", true},
		{"ordinary, quoting the word", ToolCallRow{Args: `{"query":"(private)"}`, Result: "(private) channel"},
			`{"query":"(private)"}`, "(private) channel", false},
	} {
		row := tc.in
		row.unmarkPrivate()
		if row.Args != tc.wantArgs || row.Result != tc.wantResult || row.Private != tc.wantPrivate {
			t.Errorf("%s: got %q / %q / %v", tc.name, row.Args, row.Result, row.Private)
		}
	}
}

// The wiring: both of the console's reads hand the page a private row it can lay out.
func TestTheConsoleReadsAPrivateCallAsPrivate(t *testing.T) {
	st := testStore(t)
	token := seedAdmin(t, st)
	ctx := context.Background()
	sl := &Chat{TeamID: "T1"}
	sl.chans.Store("C1", "general") // the Activity list names each row's channel
	b := &Bot{store: st, settings: newSettingsCache(st, Config{}), slacks: testRegistry(sl)}
	mux := http.NewServeMux()
	b.routes(mux, fstest.MapFS{})
	st.LogToolCall(ctx, orgID, "T1", "C1", "1", "http_request",
		`(private) {"connection":"Google","owner":"Priya Shah"}`, `(private) {"status":200,"bytes":42}`, true, 5)

	get := func(path string, out any) {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	var list struct {
		ToolCalls []ToolCallRow `json:"tool_calls"`
	}
	get("/api/activity", &list)
	if len(list.ToolCalls) != 1 {
		t.Fatalf("%d calls, want 1", len(list.ToolCalls))
	}
	var one ToolCallRow
	get(fmt.Sprintf("/api/tool-calls/%d", list.ToolCalls[0].ID), &one)
	for _, row := range []ToolCallRow{list.ToolCalls[0], one} {
		if !row.Private || row.Args != `{"connection":"Google","owner":"Priya Shah"}` || row.Result != `{"status":200,"bytes":42}` {
			t.Errorf("the console got %+v", row)
		}
	}
}

// personalCallFixture is a turn by Priya in a channel that reaches her own Google connection,
// with Google answered by api.
func personalCallFixture(t *testing.T, api headerAPI) (*Agent, *Call, *Store) {
	t.Helper()
	st, p, conn := userConnFixture(t)
	grant(t, st, p, conn, "T1", "UPRIYA", "priya-token", time.Now().Add(time.Hour))
	if api != nil {
		p.client.Transport = api
	}
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, p, newSettingsCache(st, Config{}))
	sl := &Chat{TeamID: "T1"}
	sl.names.Store("UPRIYA", "Priya Shah")
	c := &Call{OrgID: orgID, TeamID: "T1", SL: sl, Channel: "C1", ThreadTS: "1", UserID: "UPRIYA", Kind: "channel",
		HumanTurn: true, Session: &Session{}, Streamer: &Streamer{silent: true},
		Access: &Access{Rules: []Rule{{Conn: conn}}}}
	return a, c, st
}

func decodeKept(t *testing.T, logged string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(strings.TrimPrefix(logged, privateMark)), into); err != nil {
		t.Fatalf("%q: %v", logged, err)
	}
}
