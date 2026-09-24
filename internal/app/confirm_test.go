package app

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/slack-go/slack"
)

func TestNeedsConfirmReadsGoStraightThrough(t *testing.T) {
	confirm := &Connection{Name: "ClickUp", Writes: "confirm"}
	auto := &Connection{Name: "ClickUp", Writes: "auto"}
	all := &Connection{Name: "Payroll", Writes: "all"}
	const (
		clickup  = "https://api.clickup.com/api/v2/task"
		freeBusy = "https://www.googleapis.com/calendar/v3/freeBusy"
		booking  = "https://www.googleapis.com/calendar/v3/calendars/primary/events?sendUpdates=all"
		gcpLogs  = "https://logging.googleapis.com/v2/entries:list"
		notionDB = "https://api.notion.com/v1/databases/9a1b/query"
		hsSearch = "https://api.hubapi.com/crm/v3/objects/contacts/search"
		hsRead   = "https://api.hubapi.com/crm/v3/objects/contacts/batch/read"
		esSearch = "https://logs.example.com/index-2026/_search"
		newList  = "https://api.clickup.com/api/v2/folder/901/list"
		markRead = "https://api.example.com/v1/notifications/read"
		bqQuery  = "https://bigquery.googleapis.com/bigquery/v2/projects/p/queries"
		graphQL  = "https://api.linear.app/graphql"
	)
	cases := []struct {
		conn   *Connection
		method string
		url    string
		want   bool
	}{
		{confirm, "GET", clickup, false},
		{confirm, "HEAD", clickup, false},
		{confirm, "OPTIONS", clickup, false},
		{confirm, "POST", clickup, true},
		{confirm, "delete", clickup, true},
		{auto, "POST", clickup, false},
		{all, "GET", clickup, true},
		{all, "POST", clickup, true},
		// A credential-less domain spends nothing of ours, but a write still changes something on
		// the far side, so it is held like any other write; a read still goes straight through.
		{nil, "GET", clickup, false},
		{nil, "POST", clickup, true},
		{nil, "DELETE", clickup, true},
		{nil, "POST", gcpLogs, false}, // a read that travels as a POST is still a read

		// A read that has to travel as a POST. Looking at a calendar goes straight through;
		// booking on the same host still waits, and "all" holds both.
		{confirm, "POST", freeBusy, false},
		{confirm, "POST", booking, true},
		{all, "POST", freeBusy, true},
		// The path is matched whole, so nothing under the read's name inherits its exemption.
		{confirm, "POST", freeBusy + "/../calendars/primary/events", true},
		// Only a POST is ever read-shaped: a DELETE is the verb of a change whatever it points at.
		{confirm, "DELETE", freeBusy, true},
		{confirm, "PATCH", hsSearch, true},
		{confirm, "DELETE", esSearch, true},

		// Reads recognised by the shape of the path, on hosts no table could name in advance.
		{confirm, "POST", gcpLogs, false},
		{confirm, "POST", notionDB, false},
		{confirm, "POST", hsSearch, false},
		{confirm, "POST", hsRead, false},
		{confirm, "POST", esSearch, false},
		{all, "POST", gcpLogs, true}, // "all" is about every call, not about writes

		// And the writes that wear a read's vocabulary. ClickUp creates a list by POSTing to
		// one, marking a notification read is a write, and a mutation on /graphql is a POST
		// like a query is.
		{confirm, "POST", newList, true},
		{confirm, "POST", markRead, true},
		{confirm, "POST", bqQuery, true}, // "queries" runs SQL that may be a DELETE
		{confirm, "POST", graphQL, true},
		{confirm, "POST", "https://api.example.com/v1/contacts:batchDelete", true},
	}
	for _, c := range cases {
		name := "domain"
		if c.conn != nil {
			name = c.conn.Writes
		}
		u, err := url.Parse(c.url)
		if err != nil {
			t.Fatalf("parse %s: %v", c.url, err)
		}
		if got := needsConfirm(c.conn, c.method, u); got != c.want {
			t.Errorf("needsConfirm(%s, %s, %s) = %v, want %v", name, c.method, c.url, got, c.want)
		}
	}
}

func TestMCPNeedsConfirm(t *testing.T) {
	read := &mcp.Tool{Name: "fetch_customer", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}
	write := &mcp.Tool{Name: "create_invoice"}
	// A name nothing in either verb list places: treated as a write.
	unknown := &mcp.Tool{Name: "reticulate_splines"}

	confirm := &Connection{Name: "Books", Writes: "confirm"}
	if mcpNeedsConfirm(confirm, read) {
		t.Error("a tool whose name reads should not wait for a human")
	}
	if !mcpNeedsConfirm(confirm, write) {
		t.Error("create_invoice should wait")
	}
	if !mcpNeedsConfirm(confirm, unknown) {
		t.Error("an unrecognised name should wait")
	}
	if mcpNeedsConfirm(&Connection{Writes: "auto"}, write) {
		t.Error("auto runs writes without asking")
	}
	if !mcpNeedsConfirm(&Connection{Writes: "all"}, read) {
		t.Error(`"all" holds reads too`)
	}
}

func TestMCPReadOnlyIgnoresServerHint(t *testing.T) {
	// readOnlyHint is the remote server's claim about itself. A server that wanted its writes
	// to skip the Confirm card would only have to set it, so the name decides instead.
	if mcpReadOnly(&mcp.Tool{Name: "delete_everything", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}) {
		t.Error("a write verb in the name must beat the server's readOnlyHint")
	}
	if mcpReadOnly(&mcp.Tool{Name: "reticulate_splines", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}) {
		t.Error("an unrecognised name must stay gated even when the server calls it read-only")
	}
	if !mcpReadOnly(&mcp.Tool{Name: "get_task"}) {
		t.Error("a read verb with no annotations at all should still read")
	}
	// The hint is honoured in the direction that adds caution.
	yes := true
	if mcpReadOnly(&mcp.Tool{Name: "get_task", Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes}}) {
		t.Error("a server calling its own tool destructive should be believed")
	}
	if mcpReadOnly(&mcp.Tool{Name: "delete_task", Annotations: &mcp.ToolAnnotations{Title: "Delete task"}}) {
		t.Error("annotations without a hint must not make a write look like a read")
	}
}

func TestConfirmValueRoundTrip(t *testing.T) {
	id, thread, ok := parseConfirmValue(confirmValue(42, "1788320047.708289"))
	if !ok || id != 42 || thread != "1788320047.708289" {
		t.Fatalf("round trip: %d %q %v", id, thread, ok)
	}
	for _, bad := range []string{"", "42", "abc|1", "0|1788320047.708289"} {
		if _, _, ok := parseConfirmValue(bad); ok {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
}

func TestConfirmSummaries(t *testing.T) {
	s := httpConfirmSummary(&Connection{Name: "ClickUp"}, ProxyRequest{Method: "post", URL: "https://api.clickup.com/api/v2/list/9/task", Body: `{"name":"Ship it"}`})
	for _, want := range []string{"ClickUp", "POST", "api.clickup.com", "Ship it"} {
		if !strings.Contains(s, want) {
			t.Errorf("http summary missing %q: %s", want, s)
		}
	}
	m := mcpConfirmSummary(&Connection{Name: "Books"}, "create_invoice", map[string]any{"amount": 12})
	if !strings.Contains(m, "create_invoice") || !strings.Contains(m, "Books") || !strings.Contains(m, "amount") {
		t.Errorf("mcp summary: %s", m)
	}
}

// The card is posted inside an attachment, so resolving one has to read its summary back out
// of there, not out of the message's own blocks.
func TestSummaryFromMessage(t *testing.T) {
	att := brandCard(slackBlocks(confirmCard("*Waiting for your OK*\n`POST /task`", "111.1", 42)), "fallback")
	if att.Color != brandColor {
		t.Errorf("card should carry the brand colour, got %q", att.Color)
	}
	var m slack.Message
	m.Attachments = []slack.Attachment{att}
	if got := summaryFromMessage(m); !strings.Contains(got, "Waiting for your OK") {
		t.Errorf("summaryFromMessage = %q", got)
	}
	if got := summaryFromMessage(slack.Message{}); got != "" {
		t.Errorf("empty message should give an empty summary, got %q", got)
	}
}

func TestHoldForConfirm(t *testing.T) {
	c := &Call{}
	c.holdForConfirm(0, "ignored")
	if c.pendingID != 0 || c.pendingSummary != "" {
		t.Error("a write that was never stored must not raise a card")
	}
	c.holdForConfirm(7, "summary")
	if c.pendingID != 7 || c.pendingSummary != "summary" {
		t.Errorf("holdForConfirm: %d %q", c.pendingID, c.pendingSummary)
	}
}

// A Confirm button names one specific held write, so a stale card cannot approve a newer
// request and no card can run the same one twice.
func TestTakePendingWriteByID(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	first, err := st.AddPendingWrite(ctx, orgID, "", "C1", "111.1", "U1", `{"method":"POST","url":"https://api.example.com/a"}`)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.AddPendingWrite(ctx, orgID, "", "C1", "111.1", "U1", `{"method":"POST","url":"https://api.example.com/b"}`)
	if err != nil {
		t.Fatal(err)
	}
	// A press from another channel, or another workspace, does not reach the write.
	if raw, _ := st.TakePendingWriteByID(ctx, orgID, "", "C2", first); raw != "" {
		t.Error("a press from another channel answered the card")
	}
	if raw, _ := st.TakePendingWriteByID(ctx, orgID, "T9", "C1", first); raw != "" {
		t.Error("a press from another workspace answered the card")
	}
	raw, err := st.TakePendingWriteByID(ctx, orgID, "", "C1", first)
	if err != nil || !strings.Contains(raw, "/a") {
		t.Fatalf("the older card must run its own request: %q %v", raw, err)
	}
	if raw, _ := st.TakePendingWriteByID(ctx, orgID, "", "C1", first); raw != "" {
		t.Error("a card can only be answered once")
	}
	if who, _ := st.PendingWriteRequester(ctx, orgID, "", "C1", second); who != "U1" {
		t.Errorf("requester = %q, want U1", who)
	}
	st.DiscardPendingWrite(ctx, orgID, "", "C1", second)
	if raw, _ := st.TakePendingWriteByID(ctx, orgID, "", "C1", second); raw != "" {
		t.Error("a cancelled request must not run")
	}
	if raw, _ := st.TakePendingWriteByID(ctx, orgID, "", "C1", 9999); raw != "" {
		t.Error("an unknown id must come back empty")
	}
}

// The card has to be valid Slack: one action per answer, each carrying the write it belongs to,
// and no text over Slack's block limits.
func TestConfirmBlocks(t *testing.T) {
	blocks := slackBlocks(confirmCard(strings.Repeat("x", 5000), "111.1", 42))
	raw, err := json.Marshal(slack.Blocks{BlockSet: blocks})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, want := range []string{actConfirm, actCancel, actOther, `"value":"42|111.1"`, `"style":"primary"`} {
		if !strings.Contains(body, want) {
			t.Errorf("card missing %s: %s", want, truncate(body, 400))
		}
	}
	if strings.Contains(body, `"style":"danger"`) {
		t.Error("Cancel is neutral: dropping a request is not a dangerous act")
	}
	section, ok := blocks[0].(*slack.SectionBlock)
	if !ok || len(section.Text.Text) > 3000 {
		t.Errorf("summary must fit Slack's 3000-character section limit")
	}
	if got := slackBlocks(confirmCard("", "111.1", 42))[0].(*slack.SectionBlock).Text.Text; got == "" {
		t.Error("a card with no summary still needs a line of text")
	}
}
