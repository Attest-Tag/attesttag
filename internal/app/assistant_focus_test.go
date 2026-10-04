package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// The page focus: what the console had open, beyond its route, re-read inside the organisation
// before anything is said about it or offered because of it.

// reviewTreeFix is a review tree in the assistant fixture's organisation: the octo-org installation
// as a connection with two branch rules of its own and a group, and acme/web, one of its
// repositories with no row of its own.
type reviewTreeFix struct {
	conn, group *ReviewSetting
}

func seedReviewTree(f *assistFix) reviewTreeFix {
	f.t.Helper()
	ctx := context.Background()
	seedInstall(f.t, f.st, f.org, 5151, "octo-org")
	conn, _, err := f.st.AddReviewConnection(ctx, f.org, 5151, json.RawMessage(`{}`), "admin@example.com")
	if err != nil {
		f.t.Fatal(err)
	}
	rules := `{"branch_rules":[{"base":"main","types":["general","security"]},{"types":["general"]}]}`
	if err := f.st.UpdateReviewSettings(ctx, f.org, conn.ID, json.RawMessage(rules), "admin@example.com"); err != nil {
		f.t.Fatal(err)
	}
	group, err := f.st.AddReviewGroup(ctx, f.org, conn.ID, "Frontend", "admin@example.com")
	if err != nil {
		f.t.Fatal(err)
	}
	bd, err := f.st.CreateBundle(ctx, f.org, repoBundleName, "")
	if err != nil {
		f.t.Fatal(err)
	}
	c, sec, err := f.b.buildConnection(&connectionInput{BundleID: bd.ID, Name: "acme-web", Preset: "github",
		CredType: "github_app", Secret: &Secret{InstallationID: 5151}}, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	c.Repo, c.Status, c.GitHubInstallationID = "acme/web", "active", 5151
	enc, err := f.b.sealSecret(sec)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.st.InsertConnection(ctx, f.org, c, enc); err != nil {
		f.t.Fatal(err)
	}
	return reviewTreeFix{conn: conn, group: group}
}

// reviewCall is a console call asked from the Reviews page by somebody holding perms.
func (f *assistFix) reviewCall(perms ...string) *consoleCall {
	c := f.consoleCallFor(perms...)
	c.Path = "/reviews/"
	return c
}

func focusOf(kind string, params map[string]string) *assistantFocus {
	return &assistantFocus{Kind: kind, Params: params}
}

// What the page has open is named from the row it resolves to inside the caller's organisation, and
// only for somebody who may read it, on the page it belongs to, where code review is on. Another
// organisation's type or node names nothing here: the focus falls back to the page itself, so
// nothing about it reaches the prompt and nothing is offered on its account.
func TestReviewFocusIsResolvedInsideTheOrg(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	ctx := context.Background()
	tree := seedReviewTree(f)
	if _, err := f.st.CreateReviewType(ctx, f.org, &ReviewType{Key: "ours", Name: "Ours", Purpose: "Our own checks.",
		Enabled: true, Rules: []ReviewTypeRule{{Text: "No raw SQL.", Enabled: true}}}, "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	other, _ := secondOrg(t, f.st)
	theirs, err := f.st.CreateReviewType(ctx, other, &ReviewType{Key: "theirs", Name: "Theirs", Purpose: "Not yours.",
		Enabled: true}, "other@example.com")
	if err != nil {
		t.Fatal(err)
	}
	seedInstall(t, f.st, other, 6161, "other-org")
	otherConn, _, err := f.st.AddReviewConnection(ctx, other, 6161, json.RawMessage(`{}`), "other@example.com")
	if err != nil {
		t.Fatal(err)
	}

	view := f.reviewCall(PermReviewsView, PermReviewsManage)
	for _, tc := range []struct {
		name  string
		focus *assistantFocus
		kind  string
		ref   map[string]string
		line  []string
	}{
		{"a built-in nobody edited", focusOf("review_type", map[string]string{"type": "general"}), "review_type",
			map[string]string{"type": "general"}, []string{"on the Types tab", "key general", "built-in, unedited, v0", "do not ask which"}},
		{"the organisation's own type", focusOf("review_type", map[string]string{"type": "ours"}), "review_type",
			map[string]string{"type": "ours"}, []string{`review type "Ours"`, "custom, v1; 1 rule"}},
		{"a repository by name, inheriting", focusOf("review_node", map[string]string{"node": "Acme/Web"}), "review_node",
			map[string]string{"node": "acme/web", "conn": tree.conn.PublicID},
			[]string{"repository acme/web under connection octo-org", "no branch rules of its own and inherits 2 branch rules from connection octo-org", "this is the level they mean"}},
		{"a connection by id", focusOf("review_node", map[string]string{"node": tree.conn.PublicID}), "review_node",
			map[string]string{"node": tree.conn.PublicID, "conn": tree.conn.PublicID},
			[]string{"connection octo-org (id " + tree.conn.PublicID + ")", "it has 2 branch rules of its own"}},
		{"a group by id", focusOf("review_node", map[string]string{"node": tree.group.PublicID}), "review_node",
			map[string]string{"node": tree.group.PublicID, "conn": tree.conn.PublicID},
			[]string{`group "Frontend" (id ` + tree.group.PublicID + ") under connection octo-org", "inherits 2 branch rules from connection octo-org"}},
		{"the tab alone", focusOf("reviews", map[string]string{"tab": "history"}), "reviews", nil, []string{"on the History tab"}},
		{"another organisation's custom key", focusOf("review_type", map[string]string{"type": "theirs"}), "reviews", nil,
			[]string{"on the Reviews page"}},
		{"another organisation's type by id", focusOf("review_type", map[string]string{"type": theirs.PublicID}), "reviews", nil,
			[]string{"on the Reviews page"}},
		{"another organisation's connection", focusOf("review_node", map[string]string{"node": otherConn.PublicID}), "reviews", nil,
			[]string{"on the Reviews page"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.b.resolveFocus(ctx, view, tc.focus)
			if got == nil {
				t.Fatal("no focus")
			}
			t.Log(got.Line)
			if got.Kind != tc.kind {
				t.Errorf("kind %q, want %q (%s)", got.Kind, tc.kind, got.Line)
			}
			if len(got.Ref) != len(tc.ref) {
				t.Errorf("ref %v, want %v", got.Ref, tc.ref)
			}
			for k, v := range tc.ref {
				if got.Ref[k] != v {
					t.Errorf("ref %v, want %v", got.Ref, tc.ref)
				}
			}
			for _, want := range tc.line {
				if !strings.Contains(got.Line, want) {
					t.Errorf("the line does not say %q:\n%s", want, got.Line)
				}
			}
			for _, never := range []string{"theirs", "Theirs", "other-org", theirs.PublicID, otherConn.PublicID} {
				if strings.Contains(got.Line, never) {
					t.Errorf("another organisation's %q reached the line:\n%s", never, got.Line)
				}
			}
		})
	}

	// A repository with a row of its own, under the group, found by its name as the page names it.
	row, err := f.st.EnsureReviewRepo(ctx, f.org, tree.group.ID, "acme/web", "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpdateReviewSettings(ctx, f.org, row.ID, json.RawMessage(`{"branch_rules":[{"types":["security"]}]}`), "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	got := f.b.resolveFocus(ctx, view, focusOf("review_node", map[string]string{"node": "acme/web"}))
	if want := "repository acme/web (id " + row.PublicID + `) in group "Frontend" under connection octo-org; it has 1 branch rule of its own`; got == nil ||
		!strings.Contains(got.Line, want) || got.Ref["node"] != row.PublicID || got.Ref["conn"] != tree.conn.PublicID {
		t.Errorf("a repository with its own row = %+v, want a line with %q", got, want)
	}

	// Nothing at all where the focus does not belong: a caller who may not read code review, a page
	// that is not Reviews, a kind nobody registered, a deployment without code review.
	general := focusOf("review_type", map[string]string{"type": "general"})
	if got := f.b.resolveFocus(ctx, f.reviewCall(PermReviewsManage, PermScopesManage), general); got != nil {
		t.Errorf("a caller without reviews.view was told %q", got.Line)
	}
	for _, path := range []string{"/workspaces/", "/", "", "/reviews/history"} {
		c := f.reviewCall(PermReviewsView)
		c.Path = path
		if got := f.b.resolveFocus(ctx, c, general); got != nil {
			t.Errorf("a review focus sent from %q was resolved: %q", path, got.Line)
		}
	}
	noSlash := f.reviewCall(PermReviewsView)
	noSlash.Path = "/reviews"
	if got := f.b.resolveFocus(ctx, noSlash, general); got == nil || got.Kind != "review_type" {
		t.Errorf("the route without its trailing slash did not resolve: %+v", got)
	}
	if got := f.b.resolveFocus(ctx, view, focusOf("channel_rules", map[string]string{"type": "general"})); got != nil {
		t.Errorf("a kind nobody registered was resolved: %q", got.Line)
	}
	if got := f.b.resolveFocus(ctx, view, nil); got != nil {
		t.Errorf("no focus sent, and still one: %q", got.Line)
	}
	f.b.cfg.CodeReview = CodeReviewOff
	if got := f.b.resolveFocus(ctx, view, general); got != nil {
		t.Errorf("with code review off the focus was still resolved: %q", got.Line)
	}
}

// [R9] A focus that names something not found — a type deleted since the tab was opened, a link to
// a repository nothing reviews — falls back to the page rather than to nothing, so what the page
// offers does not go away with one stale parameter in the address bar.
func TestAStaleReviewFocusFallsBackToThePage(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	ctx := context.Background()
	seedReviewTree(f)
	for _, focus := range []*assistantFocus{
		focusOf("review_type", map[string]string{"type": "nope"}),
		focusOf("review_type", nil),
		focusOf("review_node", map[string]string{"node": "acme/nope"}),
		focusOf("review_node", map[string]string{"node": "0123456789abcdef0123456789abcdef"}),
		focusOf("review_node", map[string]string{"node": "acme/web", "conn": "0123456789abcdef0123456789abcdef"}),
	} {
		c := f.reviewCall(PermReviewsView, PermReviewsManage)
		c.Focus = f.b.resolveFocus(ctx, c, focus)
		if c.Focus == nil || c.Focus.Kind != "reviews" || !strings.Contains(c.Focus.Line, "on the Reviews page") {
			t.Errorf("%+v did not fall back to the page: %+v", focus, c.Focus)
			continue
		}
		if c.Focus.Ref != nil || c.Focus.Dirty {
			t.Errorf("%+v fell back with something still selected: %+v", focus, c.Focus)
		}
		names := f.toolNames(c)
		if !c.onPage(f.b, reviewsOn, reviewKinds) || !names["propose_review_type"] || !names["propose_branch_rules"] {
			t.Errorf("%+v took the Reviews page's tools away: %v", focus, names)
		}
	}
}

// [R8] The editor's unsaved edits are reported with the focus, and said in its line, so a card on the
// same thing can warn that its Confirm will conflict with them.
func TestADirtyEditorIsSaidWithTheFocus(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	c := f.reviewCall(PermReviewsView)
	got := f.b.resolveFocus(context.Background(), c, focusOf("review_type", map[string]string{"type": "general", "dirty": "1"}))
	if got == nil || !got.Dirty || !strings.Contains(got.Line, "unsaved edits") {
		t.Fatalf("the dirty editor was not reported: %+v", got)
	}
	got = f.b.resolveFocus(context.Background(), c, focusOf("review_type", map[string]string{"type": "general"}))
	if got == nil || got.Dirty || strings.Contains(got.Line, "unsaved") {
		t.Errorf("a clean editor was reported dirty: %+v", got)
	}
	// A tab with nothing open, or a selection that fell back to the page, has no editor to be dirty.
	for _, focus := range []*assistantFocus{
		focusOf("reviews", map[string]string{"tab": "types", "dirty": "1"}),
		focusOf("review_type", map[string]string{"type": "nope", "dirty": "1"}),
	} {
		if got := f.b.resolveFocus(context.Background(), c, focus); got == nil || got.Dirty || strings.Contains(got.Line, "unsaved") {
			t.Errorf("%+v was reported dirty: %+v", focus, got)
		}
	}
}

// A focus is bounded, and one out of its bounds or not the console's shape is dropped — never a 400,
// and never cut to a part that would name something else. Both shapes of request carry it.
func TestAFocusOutOfBoundsIsDropped(t *testing.T) {
	long := strings.Repeat("é", 201)
	many := map[string]string{}
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		many[k] = "x"
	}
	for name, f := range map[string]*assistantFocus{
		"kind too long":    focusOf(strings.Repeat("k", 31), nil),
		"no kind":          focusOf("  ", map[string]string{"type": "general"}),
		"seven params":     focusOf("review_type", many),
		"a key too long":   focusOf("review_type", map[string]string{strings.Repeat("k", 21): "x"}),
		"a value too long": focusOf("review_type", map[string]string{"type": long}),
	} {
		if f.bounded() != nil {
			t.Errorf("%s was kept", name)
		}
	}
	if f := focusOf("review_type", map[string]string{"type": strings.Repeat("é", 200)}); f.bounded() == nil {
		t.Error("a value at the bound, in runes, was dropped")
	}

	for name, body := range map[string]string{
		"a number where an id goes": `{"question":"hi","focus":{"kind":"review_type","params":{"type":5}}}`,
		"a string for the focus":    `{"question":"hi","focus":"review_type"}`,
	} {
		req, _, err := decodeAssistant(httptest.NewRequest("POST", "/api/assistant", strings.NewReader(body)))
		if err != nil {
			t.Errorf("%s failed the question: %v", name, err)
			continue
		}
		if req.Question != "hi" || (req.Focus != nil && req.Focus.bounded() != nil) {
			t.Errorf("%s: %+v", name, req)
		}
	}

	req, _, err := decodeAssistant(httptest.NewRequest("POST", "/api/assistant",
		strings.NewReader(`{"question":"hi","focus":{"kind":"review_type","params":{"type":"general"}}}`)))
	if err != nil || req.Focus == nil || req.Focus.Kind != "review_type" || req.Focus.Params["type"] != "general" {
		t.Errorf("the JSON shape lost its focus: %+v, %v", req.Focus, err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("question", "hi")
	mw.WriteField("focus", `{"kind":"review_node","params":{"node":"acme/web"}}`)
	mw.Close()
	r := httptest.NewRequest("POST", "/api/assistant", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	req, _, err = decodeAssistant(r)
	if err != nil || req.Focus == nil || req.Focus.Kind != "review_node" || req.Focus.Params["node"] != "acme/web" {
		t.Errorf("the multipart shape lost its focus: %+v, %v", req.Focus, err)
	}
}

// What a screen's tools and readers belong to is decided in one place: held permissions, the
// feature on, and the page's resolved focus among the kinds they name. One that names no kinds is
// every page's, as everything was before there were any.
func TestWhatIsOfferedFollowsThePageFocus(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	on := func(*Bot) bool { return true }
	off := func(*Bot) bool { return false }
	onReviews := f.reviewCall(PermReviewsView)
	onReviews.Focus = &consoleFocus{Kind: "review_type"}
	elsewhere := f.consoleCallFor(PermReviewsView)
	elsewhere.Path = "/workspaces/"

	for _, tc := range []struct {
		name  string
		c     *consoleCall
		on    func(*Bot) bool
		kinds []string
		want  bool
	}{
		{"every page's, anywhere", elsewhere, nil, nil, true},
		{"a review tool on a review focus", onReviews, nil, reviewKinds, true},
		{"a review tool with no focus", elsewhere, nil, reviewKinds, false},
		{"a review tool on another screen's focus", &consoleCall{Focus: &consoleFocus{Kind: "channel"}}, nil, reviewKinds, false},
		{"its feature on", onReviews, on, reviewKinds, true},
		{"its feature off", onReviews, off, reviewKinds, false},
		{"its feature off, every page's", elsewhere, off, nil, false},
	} {
		if got := tc.c.onPage(f.b, tc.on, tc.kinds); got != tc.want {
			t.Errorf("%s: offered = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !holdsAll(map[string]bool{PermReviewsView: true, PermReviewsManage: true}, []Permission{PermReviewsView, PermReviewsManage}) ||
		holdsAll(map[string]bool{PermReviewsManage: true}, []Permission{PermReviewsView, PermReviewsManage}) || !holdsAll(nil, nil) {
		t.Error("holdsAll does not mean every one of them")
	}
}

// [R4] A card that freezes what it was read from replaces the one already staged on the same thing,
// in its place and under its id, so a card that requires it still names it. A second card on one
// type that stood beside the first could never be confirmed after it. Cards with no key — a
// channel's fields, a tier's members — still stand side by side.
func TestASecondCardOnOneTargetReplacesTheFirst(t *testing.T) {
	c := &consoleCall{}
	first, replaces := c.proposalIDFor("review_type", "general")
	if replaces {
		t.Fatal("nothing was staged, and still something would be replaced")
	}
	if err := c.stage(proposal{ID: first, Kind: "review_type", key: "general", Target: "General", Note: "first"}); err != nil {
		t.Fatal(err)
	}
	other, _ := c.proposalIDFor("review_type", "security")
	if err := c.stage(proposal{ID: other, Kind: "review_type", key: "security", Target: "Security"}); err != nil {
		t.Fatal(err)
	}
	again, replaces := c.proposalIDFor("review_type", "general")
	if !replaces || again != first {
		t.Fatalf("the second card on General is staged as %s (replaces=%v), not under the first's id %s", again, replaces, first)
	}
	if err := c.stage(proposal{ID: again, Kind: "review_type", key: "general", Target: "General", Note: "second"}); err != nil {
		t.Fatal(err)
	}
	if len(c.proposals) != 2 || c.proposals[0].Note != "second" || c.proposals[0].ID != first || c.proposals[1].Target != "Security" {
		t.Fatalf("the second card did not take the first's place: %+v", c.proposals)
	}
	// One under a fresh id would leave a card that requires the first naming nothing.
	if err := c.stage(proposal{ID: newProposalID(), Kind: "review_type", key: "general", Note: "third"}); err == nil {
		t.Error("a replacement under a new id was staged")
	}
	if c.proposals[0].Note != "second" {
		t.Error("a refused replacement was kept anyway")
	}
	// The same key on another kind is another thing.
	if err := c.stage(proposal{ID: newProposalID(), Kind: "review_settings", key: "general"}); err != nil || len(c.proposals) != 3 {
		t.Errorf("a card of another kind replaced one: %v, %d cards", err, len(c.proposals))
	}

	keyless := &consoleCall{}
	for range 2 {
		if err := keyless.stage(proposal{ID: newProposalID(), Kind: "channel", Target: "#ops"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(keyless.proposals) != 2 {
		t.Errorf("two channel cards became %d", len(keyless.proposals))
	}
}

// [R6] A result cut to fit says so at its end, inside its cap, counted in bytes; a reader's own cap
// and its own way to ask for the rest last for its one result.
func TestACutResultSaysSo(t *testing.T) {
	f := newAssist(t, RoleAdmin, &fakeLLM{answer: "ok"})
	ctx := context.Background()
	c := f.consoleCallFor()
	long := strings.Repeat("R1 · a rule — ", 400) // multi-byte, so bytes and runes differ
	byName := map[string]consoleTool{
		"capped": {Name: "capped", Run: func(ctx context.Context, c *consoleCall, args json.RawMessage) (string, error) {
			c.resultCap, c.resultMore = 1000, "ask for one rule by its R-number"
			return long, nil
		}},
		"plain": {Name: "plain", Run: func(ctx context.Context, c *consoleCall, args json.RawMessage) (string, error) {
			return long, nil
		}},
		"short": {Name: "short", Run: func(ctx context.Context, c *consoleCall, args json.RawMessage) (string, error) {
			return "R1 · a rule", nil
		}},
	}

	capped := f.b.runConsoleTool(ctx, c, byName, "capped", "{}")
	if len(capped) > 1000 || !utf8.ValidString(capped) || !strings.HasSuffix(capped, "\n…[cut — ask for one rule by its R-number]") {
		t.Errorf("a capped result is %d bytes, valid=%v, ending %q", len(capped), utf8.ValidString(capped), capped[max(len(capped)-60, 0):])
	}
	plain := f.b.runConsoleTool(ctx, c, byName, "plain", "{}")
	if len(plain) > assistantResultC || len(plain) < 1000 || !strings.HasSuffix(plain, "…[cut — "+consoleCutMore+"]") {
		t.Errorf("the next result kept the last one's cap, or was cut in silence: %d bytes, ending %q", len(plain), plain[max(len(plain)-60, 0):])
	}
	if c.resultCap != 0 || c.resultMore != "" {
		t.Error("a reader's cap outlived its result")
	}
	if short := f.b.runConsoleTool(ctx, c, byName, "short", "{}"); short != "R1 · a rule" {
		t.Errorf("a result that fits was changed: %q", short)
	}
}

// The assistant.proposed row records a list change by its summary and carries what the tool added
// for the log, without letting that stand in for the row's own keys.
func TestTheProposalAuditKeepsItsOwnKeys(t *testing.T) {
	p := proposal{ID: "prop_1", Kind: "review_type", Target: "General",
		Changes: []proposalChange{
			{Key: "rules", Label: "Rules", To: "16 rules (1 added)", Format: "list",
				Items: []proposalItem{{Mark: "+", Text: "R16 [P1] no SQL by concatenation"}, {Mark: "=", Text: "15 rules unchanged"}}},
			{Key: "strictness", Label: "Strictness", From: "medium", To: "high"},
		},
		Steps:       []proposalStep{{Method: "PUT", Path: "/api/review-types/general"}},
		auditDetail: map[string]any{"before": []string{"1. main ← any · General"}, "proposal_id": "forged", "kind": "forged"},
	}
	got := proposalAudit(p)
	changed, _ := got["changed"].(map[string]any)
	if changed["rules"] != "16 rules (1 added)" || changed["strictness"] != "high" {
		t.Errorf("changed = %v", changed)
	}
	if got["proposal_id"] != "prop_1" || got["kind"] != "review_type" || got["steps"] != 1 {
		t.Errorf("the tool's detail overwrote the row's own keys: %v", got)
	}
	if _, ok := got["before"]; !ok {
		t.Errorf("the tool's detail was dropped: %v", got)
	}
}

// Decision 1, kept strictly: the system prompt is the same bytes on every page, and what the model needs
// to know about the Reviews page's tools — rule text is data, one call a target, a new type and the rule
// that runs it, what stays on the page — rides with the Reviews page's line and nowhere else. Asked from
// any other page, or with a review focus claimed on one, not a line of it is sent.
func TestReviewGuidanceIsSentOnlyOnTheReviewsPage(t *testing.T) {
	for _, w := range []string{"review", "branch rule", "learned", "Try on a PR"} {
		if strings.Contains(strings.ToLower(assistantPrompt), strings.ToLower(w)) {
			t.Errorf("the system prompt, sent on every page, says %q", w)
		}
	}
	// As the request carries a string: the fake model sees each message's content as JSON.
	inJSON := func(s string) string {
		raw, _ := json.Marshal(s)
		return string(raw[1 : len(raw)-1])
	}
	llm := &fakeLLM{answer: "ok"}
	f := newAssist(t, RoleAdmin, llm)
	seedReviewTree(f)
	for _, tc := range []struct {
		name  string
		path  string
		focus *assistantFocus
		want  bool
	}{
		{"the channels page", "/channels/", nil, false},
		{"the overview", "/", nil, false},
		{"a review focus claimed on another page", "/channels/", focusOf("review_type", map[string]string{"type": "general"}), false},
		{"the History tab", "/reviews/", focusOf("reviews", map[string]string{"tab": "history"}), true},
		{"a type open", "/reviews/", focusOf("review_type", map[string]string{"type": "general"}), true},
		{"a level open", "/reviews/", focusOf("review_node", map[string]string{"node": "acme/web"}), true},
		{"a type that is not there, standing in for the page", "/reviews/", focusOf("review_type", map[string]string{"type": "nope"}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(assistantRequest{Question: "what can I change here?", Path: tc.path, Page: "Somewhere", Focus: tc.focus})
			if w := f.send(httptest.NewRequest("POST", "/api/assistant", strings.NewReader(string(body)))); w.Code != 200 {
				t.Fatalf("turn failed: %d %s", w.Code, w.Body.String())
			}
			sent := llm.prompt()
			if !strings.Contains(sent, inJSON(assistantPrompt)) {
				t.Error("the system prompt was not sent as it is written")
			}
			for _, line := range strings.Split(reviewPagePrompt, "\n") {
				if got := strings.Contains(sent, inJSON(line)); got != tc.want {
					t.Errorf("sent %q: %v, want %v", truncate(line, 60), got, tc.want)
				}
			}
		})
	}
}

// The console's system prompt is the one breakpoint its first round has, and it is marked whatever its
// length. Taking the Reviews page's lines out of it left it under withCache's floor, where nothing is
// marked: on a model that caches only as far as a marker, every round one — every answer that needs no
// tool — then bought the tool definitions and the prompt again. Sent to OpenRouter (the fake stands
// for one), the system message is the prompt as written, with cache_control on it, and the clock after.
func TestTheConsolesSystemPromptIsACacheBreakpoint(t *testing.T) {
	var mu sync.Mutex
	var system json.RawMessage
	fake := &fakeLLM{answer: "ok"}
	f := newAssist(t, RoleAdmin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(raw, &body) == nil && len(body.Messages) > 0 && body.Messages[0].Role == "system" {
			mu.Lock()
			system = body.Messages[0].Content
			mu.Unlock()
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		fake.ServeHTTP(w, r)
	}))
	body, _ := json.Marshal(assistantRequest{Question: "what is on this page?", Path: "/channels/", Page: "Channels"})
	if w := f.send(httptest.NewRequest("POST", "/api/assistant", bytes.NewReader(body))); w.Code != 200 {
		t.Fatalf("turn failed: %d %s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	var parts []struct {
		Text         string         `json:"text"`
		CacheControl map[string]any `json:"cache_control"`
	}
	if err := json.Unmarshal(system, &parts); err != nil || len(parts) < 2 {
		t.Fatalf("the system message is not sent as marked parts: %s", system)
	}
	if parts[0].Text != assistantPrompt || parts[0].CacheControl["type"] != "ephemeral" {
		t.Errorf("the first part is not the prompt as written with its breakpoint: %+v", parts[0].CacheControl)
	}
	if !strings.HasPrefix(parts[len(parts)-1].Text, clockPrefix) || parts[len(parts)-1].CacheControl != nil {
		t.Errorf("the clock is not after the breakpoint, unmarked: %+v", parts[len(parts)-1])
	}
}
