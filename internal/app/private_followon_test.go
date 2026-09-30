package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Once a turn has spent somebody's own connection, the content it read must not reach /activity —
// not through the call that read it, and not through the next call the model hands it to. The model
// routinely carries a personal result forward (a run_js input, an artifact, a DM), and that
// following call spends nothing of its own, so before the latch it was logged in full. This drives
// a personal read and then a plain tool holding the same content, and asserts neither row carries it.
func TestPersonalContentDoesNotLeakThroughAFollowOnCall(t *testing.T) {
	const secret = "Final round interview, Acme Corp"
	a, c, st := personalCallFixture(t, func(r *http.Request) (int, string, http.Header) {
		return 200, `{"items":[{"summary":"` + secret + `"}]}`, nil
	})
	ctx := context.Background()

	read := `{"method":"GET","url":"https://www.googleapis.com/calendar/v3/calendars/primary/events?maxResults=5"}`
	if out, ok := a.runToolRaw(ctx, c, "http_request", read); !ok || !strings.Contains(out, secret) {
		t.Fatalf("the personal read did not run: %s", out)
	}

	// A plain tool the model hands the calendar content to next — standing in for run_js input,
	// create_artifact content, send_dm text, post_to_thread.
	a.ensureTools(ctx, c)
	c.tools["create_artifact"] = Tool{Name: "create_artifact", Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
		return "created artifact #7", nil
	}}
	if _, ok := a.runToolRaw(ctx, c, "create_artifact", `{"title":"My week","content":"`+secret+`"}`); !ok {
		t.Fatal("the follow-on call did not run")
	}

	rows := st.ToolCallsAfter(ctx, orgID, "1", 0)
	if len(rows) != 2 {
		t.Fatalf("logged %d calls, want 2", len(rows))
	}
	for _, r := range rows {
		if strings.Contains(r.Args+r.Result, secret) {
			t.Fatalf("%s leaked the personal content to the activity log: args=%q result=%q", r.Name, r.Args, r.Result)
		}
		if !strings.HasPrefix(r.Args, privateMark) {
			t.Errorf("%s was not marked private: args=%q", r.Name, r.Args)
		}
	}
}
