package app

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestChunkKeepsHeadings(t *testing.T) {
	text := "# Title\n\nintro paragraph that is long enough to be kept as a chunk by the splitter.\n\n## Section A\n\n" +
		strings.Repeat("Sentence about A. ", 200) + "\n\n## Section B\n\nshort but long enough to count as a chunk here."
	cs := chunk(text)
	if len(cs) < 3 {
		t.Fatalf("expected >=3 chunks, got %d", len(cs))
	}
	found := false
	for _, c := range cs {
		if c.Heading == "Section A" {
			found = true
		}
		if len(c.Text) > 2700 {
			t.Errorf("chunk too long: %d", len(c.Text))
		}
	}
	if !found {
		t.Errorf("no chunk carried heading 'Section A'")
	}
}

func TestNormalizeTS(t *testing.T) {
	cases := map[string]string{
		"1788320047.708289": "1788320047.708289",
		"p1788320047708289": "1788320047.708289",
		"https://x.slack.com/archives/C1/p1788320047708289":             "1788320047.708289",
		"https://x.slack.com/archives/C1/p1788320047708289?thread_ts=1": "1788320047.708289",
		// A Teams message id is milliseconds, and the id a thread is read by as it stands.
		"1790112022272": "1790112022272",
	}
	for in, want := range cases {
		if got := normalizeTS(in); got != want {
			t.Errorf("normalizeTS(%q)=%q want %q", in, got, want)
		}
	}
}

// Found in a real Teams channel: history read back fifty thousand years in the future, because a
// Teams message id is the time in milliseconds and was read as Slack's seconds.
func TestSlackTimeReadsATeamsIDAsMilliseconds(t *testing.T) {
	want := time.UnixMilli(1790112022272).Format("2006-01-02 15:04")
	if got := slackTime("1790112022272"); got != want {
		t.Errorf("slackTime(teams id) = %q, want %q", got, want)
	}
	if got, want := slackTime("1788320047.708289"), time.Unix(1788320047, 0).Format("2006-01-02 15:04"); got != want {
		t.Errorf("slackTime(slack ts) = %q, want %q", got, want)
	}
}

func TestRedact(t *testing.T) {
	// The AWS key is built in two halves so the file never holds one: GitHub push protection
	// refuses a push that contains an AWS access key id, fake or not.
	in := "token xoxb-3601234567-abcdefghij and key sk-or-v1-abcdefghijklmnopqrstuvwxyz and " + "AKIA" + "ABCDEFGHIJKLMNOP"
	out := redact(in)
	for _, bad := range []string{"xoxb-", "sk-or", "AKIA"} {
		if strings.Contains(out, bad) {
			t.Errorf("secret leaked: %s in %q", bad, out)
		}
	}
	if !strings.Contains(out, "[redacted-secret]") {
		t.Errorf("no redaction marker in %q", out)
	}
}

func TestStripThinking(t *testing.T) {
	if got := stripThinking("<think>hmm</think>\n\nanswer"); got != "answer" {
		t.Errorf("got %q", got)
	}
}

func TestCleanHTML(t *testing.T) {
	got := cleanHTML("<html><head><style>x{}</style><script>bad()</script></head><body><h1>Hi</h1><p>one &amp; two</p></body></html>")
	if strings.Contains(got, "bad()") || !strings.Contains(got, "one & two") || !strings.Contains(got, "Hi") {
		t.Errorf("got %q", got)
	}
}

func TestNextRun(t *testing.T) {
	if _, err := nextRun("0 9 * * 1-5", "Asia/Kathmandu", nowT()); err != nil {
		t.Fatal(err)
	}
	if _, err := nextRun("bad", "Asia/Kathmandu", nowT()); err == nil {
		t.Fatal("expected error for bad cron")
	}
}

func nowT() time.Time { return time.Now() }

func TestStripFakeToolResult(t *testing.T) {
	got := stripThinking("Good morning! <tool_result> {\"ok\":true} </tool_result>")
	if got != "Good morning!" {
		t.Errorf("got %q", got)
	}
}

func TestForcedTool(t *testing.T) {
	cases := map[string]string{
		"Above what amount does a refund need approval? Check our docs.": "search_docs",
		"Search the web: capital of Australia?":                          "web_search",
		"Summarize what happened in this channel today.":                 "read_channel_history",
		"what's 2+2": "",
	}
	for in, want := range cases {
		if got := forcedTool(in); got != want {
			t.Errorf("forcedTool(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSplitLongAnswer(t *testing.T) {
	short := "hello"
	if l, _, long := splitLongAnswer(short, 100); long || l != short {
		t.Fatal("short answer must not split")
	}
	text := strings.Repeat("Para one is here. ", 10) + "\n\n" + strings.Repeat("Para two. ", 60) + "\n\n" + strings.Repeat("Para three. ", 60)
	lead, full, long := splitLongAnswer(text, 500)
	if !long || full != text || !strings.Contains(lead, "attached as a file") || len(lead) > 900 {
		t.Fatalf("bad split: long=%v lead=%q", long, lead)
	}
}

// preview cuts a lead out of a long text. The two things that would show up in a channel if it
// were wrong: a broken character where a multi-byte rune was sliced, and a word chopped in half.
func TestPreviewCutsCleanly(t *testing.T) {
	if got := preview("short enough", 40); got != "short enough" {
		t.Errorf("preview shortened a text that already fit: %q", got)
	}
	long := strings.Repeat("work the new-lead queue ", 20)
	got := preview(long, 60)
	if !strings.HasSuffix(got, "…") || len(got) > 63 {
		t.Errorf("preview(%d) = %q", 60, got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") {
		t.Errorf("preview left the space before its ellipsis: %q", got)
	}
	if !utf8.ValidString(preview(strings.Repeat("→ étape ", 30), 25)) {
		t.Error("preview sliced through a rune")
	}
	if strings.Contains(preview(long, 60), "truncated") {
		t.Error("preview should say nothing about what it left out")
	}
}

func TestHostMatch(t *testing.T) {
	cases := []struct {
		pat, host string
		want      bool
	}{
		{"api.example.com", "api.example.com", true}, {"api.example.com", "API.example.com", true},
		{"*.example.com", "a.b.example.com", true}, {"*.example.com", "example.com", false}, {"api.example.com", "evil-api.example.com", false},
	}
	for _, c := range cases {
		if got := hostMatch(c.pat, c.host); got != c.want {
			t.Errorf("hostMatch(%q,%q)=%v", c.pat, c.host, got)
		}
	}
}

func TestPKCE(t *testing.T) {
	v, c := pkcePair()
	if len(v) < 40 || len(c) < 40 || v == c {
		t.Fatal("bad pkce pair")
	}
}

func TestMCPReadOnlyHeuristic(t *testing.T) {
	for _, n := range []string{"ask_question", "list_collections", "find", "get_user", "search-docs",
		"clickup_filter_tasks", "download_file_content", "discover_hubspot_schema", "whoami",
		"get_task_time_in_status", "read_terminal"} {
		if !mcpLooksReadOnly(n) {
			t.Errorf("expected read-only: %s", n)
		}
	}
	for _, n := range []string{"insert_document", "delete_many", "create_issue", "update_task",
		"send_chat_message", "trash_file", "merge_document", "add_task_to_list",
		"search_and_delete", "get_or_create_list"} { // a write verb anywhere wins over a read one
		if mcpLooksReadOnly(n) {
			t.Errorf("expected write: %s", n)
		}
	}
}
