package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A run_js script decides what to fetch while it runs, so nothing before the call says it will
// read somebody's own account. Its output is then whatever it made of their calendar — here the
// event itself — and Activity is open to every viewer. So a script that fetched through a
// personal connection is logged the way an http_request through it is: whose account and which
// connection, the size of what came back, and nothing of the script or its answer.
func TestAScriptThatReadSomebodysAccountIsLoggedPrivately(t *testing.T) {
	const (
		secretQuery = "interview-at-acme"
		secretEvent = "Final round, Acme Corp"
	)
	a, c, st := personalCallFixture(t, func(r *http.Request) (int, string, http.Header) {
		return 200, `{"items":[{"summary":"` + secretEvent + `"}]}`, nil
	})
	ctx := context.Background()

	script := `{"code":"const r = fetch('https://www.googleapis.com/calendar/v3/calendars/primary/events?q=` + secretQuery +
		`&maxResults=5'); r.json().items[0].summary"}`
	out, ok := a.runToolRaw(ctx, c, "run_js", script)
	if !ok || !strings.Contains(out, secretEvent) {
		t.Fatalf("the script did not read the calendar, so this proves nothing: %s", out)
	}

	rows := st.ToolCallsAfter(ctx, orgID, "1", 0)
	if len(rows) != 1 {
		t.Fatalf("logged %d calls, want 1", len(rows))
	}
	r := rows[0]
	for _, secret := range []string{secretQuery, secretEvent, "fetch("} {
		if strings.Contains(r.Args, secret) || strings.Contains(r.Result, secret) {
			t.Errorf("logged %q, which is the person's own:\nargs %s\nresult %s", secret, r.Args, r.Result)
		}
	}
	if !strings.HasPrefix(r.Args, privateMark+" ") || !strings.HasPrefix(r.Result, privateMark+" ") {
		t.Fatalf("not marked private:\nargs %s\nresult %s", r.Args, r.Result)
	}
	var args struct{ Connection, Owner, Method, Endpoint string }
	var result struct {
		Status  int
		Bytes   int
		Error   string
		Outcome string
	}
	decodeKept(t, r.Args, &args)
	decodeKept(t, r.Result, &result)
	if args.Connection != "Google" || args.Owner != "Priya Shah" || args.Method != "" || args.Endpoint != "" {
		t.Errorf("kept %+v, want whose account and which connection, and no request", args)
	}
	if result.Bytes == 0 || result.Status != 0 || result.Error != "" || result.Outcome != "" {
		t.Errorf("kept %+v, want the size and nothing a script printed", result)
	}
}

// The other half: a script that fetched nothing personal is an ordinary call, logged as it was,
// even in a channel that could have reached somebody's account. Hiding every script there would
// hide the arithmetic the tool is mostly for.
func TestAScriptThatReadNobodysAccountIsLoggedInFull(t *testing.T) {
	a, c, st := personalCallFixture(t, nil)
	ctx := context.Background()

	if out, ok := a.runToolRaw(ctx, c, "run_js", `{"code":"['north', 'south'].map(s => s.toUpperCase()).join(' and ')"}`); !ok || !strings.Contains(out, "NORTH and SOUTH") {
		t.Fatalf("the script did not run: %s", out)
	}
	rows := st.ToolCallsAfter(ctx, orgID, "1", 0)
	if len(rows) != 1 {
		t.Fatalf("logged %d calls, want 1", len(rows))
	}
	if strings.HasPrefix(rows[0].Args, privateMark) || !strings.Contains(rows[0].Args, "toUpperCase") || !strings.Contains(rows[0].Result, "NORTH and SOUTH") {
		t.Errorf("an ordinary script was not logged as it ran:\nargs %s\nresult %s", rows[0].Args, rows[0].Result)
	}
}

// The repeat guard logs the script of runs that already happened, with no note from them. Where
// the channel reaches somebody's own account the script may be what read it — its URL carries the
// search — so it is kept out; where it reaches none, there is nothing to keep out.
func TestARefusedRepeatOfAScriptIsPrivateWhereItCouldReadSomebodysAccount(t *testing.T) {
	a, c, st := personalCallFixture(t, nil)
	ctx := context.Background()
	script := `{"code":"fetch('https://www.googleapis.com/calendar/v3/calendars/primary/events?q=interview-at-acme').status"}`

	a.refuseTool(ctx, c, "run_js", script)
	elsewhere := *c
	elsewhere.Access = &Access{}
	elsewhere.tools = nil
	a.refuseTool(ctx, &elsewhere, "run_js", script)

	rows := st.ToolCallsAfter(ctx, orgID, "1", 0)
	if len(rows) != 2 {
		t.Fatalf("logged %d calls, want the two refusals", len(rows))
	}
	if strings.Contains(rows[0].Args, "interview-at-acme") || !strings.HasPrefix(rows[0].Args, privateMark) {
		t.Errorf("the refusal in a channel reaching Priya's account logged %s", rows[0].Args)
	}
	var args struct{ Connection, Owner string }
	decodeKept(t, rows[0].Args, &args)
	if args.Connection != "Google" || args.Owner != "Priya Shah" {
		t.Errorf("kept %+v, want whose account the script could have read", args)
	}
	if !strings.Contains(rows[1].Args, "interview-at-acme") {
		t.Errorf("a channel reaching nobody's account hid an ordinary script: %s", rows[1].Args)
	}
}
