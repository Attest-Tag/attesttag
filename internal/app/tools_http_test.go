package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDescribeCall(t *testing.T) {
	clickup := &Connection{Name: "ClickUp"}
	repo := &Connection{Name: "app", Repo: "acme/app"}
	for _, tc := range []struct {
		method, url string
		conn        *Connection
		want        string
	}{
		// The connection name, or the repository, in place of the API hostname.
		{"POST", "https://api.clickup.com/api/v2/list/901/task", clickup, "ClickUp: POST /api/v2/list/901/task"},
		{"", "https://api.github.com/repos/o/r/pulls?state=open", repo, "acme/app: GET /repos/o/r/pulls"},
		// Unmatched: the host stands in for the connection name, same shape.
		{"get", "https://api.github.com/user/repos/", nil, "api.github.com: GET /user/repos"},
		{"POST", "", nil, "http request"},
	} {
		if got := describeCall(tc.method, tc.url, tc.conn); got != tc.want {
			t.Errorf("describeCall(%q, %q) = %q, want %q", tc.method, tc.url, got, tc.want)
		}
	}
}

func TestHumanTitleNamesTheTarget(t *testing.T) {
	args := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	got := humanTitle("http_request", args(map[string]any{"method": "POST", "url": "https://api.clickup.com/api/v2/list/901/task"}), &Connection{Name: "ClickUp"})
	if got != "ClickUp: POST /api/v2/list/901/task" {
		t.Errorf("http_request title = %q", got)
	}
	if got := humanTitle("github_search_issues", args(map[string]any{"query": "repo:o/r is:open"}), nil); got != "github search issues: repo:o/r is:open" {
		t.Errorf("pack tool title = %q", got)
	}
	if got := humanTitle("read_thread", args(map[string]any{}), nil); got != "Reading a thread" {
		t.Errorf("named tool title = %q", got)
	}
}

func TestViewableLink(t *testing.T) {
	cases := map[string]string{
		// ClickUp: the task carries the page people open.
		`{"id":"86abc","url":"https://app.clickup.com/t/86abc","list":{"id":"901"}}`: "https://app.clickup.com/t/86abc",
		// GitHub: html_url is the page, url is the API self-link.
		`{"number":7,"url":"https://api.github.com/repos/o/r/issues/7","html_url":"https://github.com/o/r/issues/7"}`: "https://github.com/o/r/issues/7",
		// Nested one level down, as a wrapped create response.
		`{"ok":true,"message":{"permalink":"https://acme.slack.com/archives/C1/p1"}}`: "https://acme.slack.com/archives/C1/p1",
		// Nothing viewable: an API self-link alone, plain text, or no link at all.
		`{"url":"https://api.clickup.com/api/v2/task/86abc"}`: "",
		`{"id":"86abc"}`: "",
		`not json`:       "",
	}
	for body, want := range cases {
		if got := viewableLink(body); got != want {
			t.Errorf("viewableLink(%s) = %q, want %q", body, got, want)
		}
	}
	if viewLine(`{"html_url":"https://github.com/o/r/issues/7"}`) != " view: https://github.com/o/r/issues/7" {
		t.Error("viewLine should state the link plainly for the model")
	}
}

// A write that is held for a human runs later, out of a stored payload, and what it made is
// reported from the note that run left behind. Until the note carried the link, a confirmed
// ticket was reported as an id and an HTTP status — a thing nobody could open.
func TestViewedLinkReadsTheNoteAWriteLeftBehind(t *testing.T) {
	task := "https://app.clickup.com/t/86abc"
	notes := "Alice confirmed the write POST https://api.clickup.com/api/v2/list/901/task; " +
		"the service returned HTTP 200. view: " + task + ` Response: {"id":"86abc","url":"` + task + `"}`
	if got := viewedLink(notes); got != task {
		t.Errorf("viewedLink = %q, want %q", got, task)
	}
	// Several steps, and only the later one made something: the link must not be missed because
	// step one had none.
	first := "Alice confirmed the write GET https://api.clickup.com/api/v2/list/901; the service returned HTTP 200. Response: {}"
	if got := viewedLink(first + "\n" + notes); got != task {
		t.Errorf("viewedLink over two steps = %q, want %q", got, task)
	}
	// The response body is the service's text, not ours. A "view:" inside it is not a link this
	// bot vouches for, and must not become the one a card points at.
	spoof := `Alice confirmed the write POST https://api.example.com/x; the service returned HTTP 200. Response: {"note":" view: https://evil.example/pwn "}`
	if got := viewedLink(spoof); got != "" {
		t.Errorf("a link from inside the response body was used: %q", got)
	}
	if viewedLink("nothing here") != "" {
		t.Error("a note with no link should yield none")
	}
}

// clickupAgent returns an agent whose proxy answers ClickUp from fn, plus a Call holding a
// clickup connection, and counts how many upstream requests the walk makes.
func clickupAgent(t *testing.T, fn fakeAPI) (*Agent, *Call, *int) {
	t.Helper()
	b := repoTestBot(t, fn)
	calls := 0
	b.proxy.client.Transport = fakeAPI(func(r *http.Request) (int, string) {
		calls++
		return fn(r)
	})
	c, sec, err := b.buildConnection(&connectionInput{Name: "ClickUp", Preset: "clickup", CredType: "bearer",
		Secret: &Secret{Token: "tok"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := b.sealSecret(sec)
	if err != nil {
		t.Fatal(err)
	}
	c.secretEnc, c.Status = enc, "active"
	a := &Agent{proxy: b.proxy, store: b.store}
	return a, &Call{Channel: "C1", Access: &Access{Rules: []Rule{{Conn: c, Rank: 2}}}}, &calls
}

func TestClickupListsResolvesInOneToolCall(t *testing.T) {
	body := func(r *http.Request) (int, string) {
		switch p := r.URL.Path; {
		case p == "/api/v2/team":
			return 200, `{"teams":[{"id":"9","name":"Acme HQ"}]}`
		case strings.HasSuffix(p, "/space"):
			return 200, `{"spaces":[{"id":"90","name":"Engineering"}]}`
		case strings.HasSuffix(p, "/list"):
			return 200, `{"lists":[{"id":"700","name":"inbox"}]}`
		case strings.HasSuffix(p, "/folder"):
			return 200, `{"folders":[{"id":"600","name":"API Sprints","lists":[{"id":"900000000002","name":"backlog"}]}]}`
		}
		return 404, `{}`
	}
	a, call, upstream := clickupAgent(t, body)
	ctx := context.Background()

	out, err := a.clickupFindLists(ctx, call, "https://api.clickup.com", "backlog")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "900000000002") || !strings.Contains(out, "API Sprints › backlog") {
		t.Fatalf("lookup = %q, want the backlog list id and its path", out)
	}
	first := *upstream

	// The second lookup — the one a follow-up task would make — costs nothing.
	if _, err := a.clickupFindLists(ctx, call, "https://api.clickup.com", "inbox"); err != nil {
		t.Fatal(err)
	}
	if *upstream != first {
		t.Errorf("second lookup made %d more upstream calls, want it served from the cache", *upstream-first)
	}

	// A miss names the way out instead of leaving the model to guess.
	out, err = a.clickupFindLists(ctx, call, "https://api.clickup.com", "no-such-list")
	if err != nil || !strings.Contains(out, "clickup_lists without a query") {
		t.Errorf("miss = %q, %v", out, err)
	}
}
