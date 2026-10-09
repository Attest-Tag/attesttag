package app

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"attesttag/internal/review"
)

// A file's history reaches a model as commits and nothing else about them: the hash, the date, the
// subject and the pull request it names — never who wrote or committed it. These pin that, which
// subjects count as fixes, the "Past fixes in these files" and documentation sections of the finder's
// prompt, and the file_history tool.

func TestReviewCommitOfKeepsNoPerson(t *testing.T) {
	sha := strings.Repeat("c", 40)
	for _, tc := range []struct {
		message, subject string
		pr               int
	}{
		{"Fix the retry loop dropping the last page (#123)\n\nCo-authored-by: Somebody <somebody@example.com>", "Fix the retry loop dropping the last page", 123},
		{"Merge pull request #45 from someone/fix-reset\n\nFix reset", "Merge pull request", 45},
		{"Revert \"Make Add lock-free (#40)\" (#41)", "Revert \"Make Add lock-free (#40)\"", 41},
		{"  Tidy the cache  ", "Tidy the cache", 0},
		{"", "", 0},
	} {
		c := reviewCommitOf(sha, tc.message, "2026-08-01T10:00:00Z")
		if c.Subject != tc.subject || c.PR != tc.pr || c.Date != "2026-08-01" || c.SHA != sha {
			t.Errorf("reviewCommitOf(%q) = %+v, want subject %q and PR %d", tc.message, c, tc.subject, tc.pr)
		}
	}
	long := reviewCommitOf(sha, strings.Repeat("é", 300), "")
	if n := len([]rune(long.Subject)); n > reviewSubjectChars || n == 0 {
		t.Errorf("a long subject was kept at %d characters", n)
	}
}

// A fix is a subject that says one, word for word: a fixture, a prefix or a debug line is not.
func TestReviewFixSubjects(t *testing.T) {
	for subject, want := range map[string]bool{
		"fix(api): close the body on error":    true,
		"Fixes the double charge":              true,
		"Hotfix: guard the nil session":        true,
		"Revert \"Cache the plan\"":            true,
		"Data race in the token cache":         true,
		"Plug a goroutine leak":                true,
		"Crash on an empty list":               true,
		"Roll back the index change":           true,
		"Security: escape the redirect target": true,
		"Bugfix for the pager":                 true,
		"Regression in the export":             true,
		"Add test fixtures":                    false,
		"Prefix the cache keys":                false,
		"Remove debug logging":                 false,
		"Trace the slow queries":               false,
		"Bump the dependencies":                false,
	} {
		if got := reviewFixSubject.MatchString(subject); got != want {
			t.Errorf("%q: fix = %v, want %v", subject, got, want)
		}
	}
}

// historyFixture is the totals pull request changing two files, with a history on the base branch:
// a fix to both files in one commit, a revert of the first, a commit that fixed nothing, and a merge
// whose subject names the person whose fork it came from. Every commit carries its author's name,
// email and login, which nothing may repeat.
func historyFixture(rig *reviewRig) *sync.Map {
	asked := &sync.Map{} // path → requests
	person := map[string]any{"name": "Example Author", "email": "author@example.com", "date": "2026-07-01T09:00:00Z"}
	commit := func(sha, message, date string) map[string]any {
		p := map[string]any{"name": "Example Author", "email": "author@example.com", "date": date}
		return map[string]any{"sha": sha, "commit": map[string]any{"message": message, "author": person, "committer": p},
			"author": map[string]any{"login": "example-author"}, "committer": map[string]any{"login": "example-author"}}
	}
	shared := commit(strings.Repeat("1", 40), "Fix the lost update in Add and reset (#12)", "2026-06-01T09:00:00Z")
	byPath := map[string][]map[string]any{
		"src/totals.go": {
			commit(strings.Repeat("2", 40), "Revert \"Make Add lock-free\" (#41)\n\nIt raced with Get.", "2026-08-01T09:00:00Z"),
			commit(strings.Repeat("3", 40), "Rename the totals package", "2026-07-15T09:00:00Z"),
			shared,
		},
		"src/other.go": {
			commit(strings.Repeat("4", 40), "Merge pull request #9 from example-author/tidy-reset", "2026-07-20T09:00:00Z"),
			shared,
		},
	}
	rig.gh.mux.HandleFunc("GET /repos/acme/web/commits", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := rig.gh.permsOf(r); got != readPerms {
			rig.t.Errorf("a history was read with a token for %q", got)
		}
		if q.Get("sha") != reviewBase || q.Get("per_page") != "10" {
			rig.t.Errorf("history asked as %s, want the base commit's last 10", r.URL.RawQuery)
		}
		n, _ := asked.LoadOrStore(q.Get("path"), new(int))
		*n.(*int)++
		list := byPath[q.Get("path")]
		if list == nil {
			list = []map[string]any{}
		}
		json.NewEncoder(w).Encode(list)
	})
	return asked
}

// twoFileFixture is the totals pull request with src/other.go changed too, in the same unit.
func twoFileFixture() reviewPRFixture {
	fx := totalsFixture()
	fx.base["src/other.go"] = "package totals\n\nfunc reset(t *Totals) {\n\tt.value = -1\n}\n"
	fx.files = append(fx.files, map[string]any{"filename": "src/other.go", "status": "modified", "additions": 1, "deletions": 1,
		"patch": "@@ -2,4 +2,4 @@\n \n func reset(t *Totals) {\n-\tt.value = -1\n+\tt.value = 0\n }"})
	return fx
}

// The finder is shown the fixes in its files' history up front, one line a commit however many of
// its files the commit changed, newest first, and the documentation near them as paths; file_history
// answers from the same reads; and no model is told who wrote any of it.
func TestReviewEngineShowsPastFixesAndDocs(t *testing.T) {
	fx := twoFileFixture()
	for p, body := range map[string]string{"docs/totals.md": "# Totals\n", "src/NOTES.md": "Locking.\n", "CHANGELOG.md": "## 1.0\n",
		"LICENSE.md": "MIT\n", "docs/unrelated/billing.md": "# Billing\n"} {
		fx.base[p] = body
	}
	rig := newReviewRig(t, fx)
	asked := historyFixture(rig)
	rig.model.finder["general"] = func(n int, q reviewModelReq) reviewModelReply {
		if n == 0 {
			return reviewModelReply{Calls: []reviewCall{{"file_history", map[string]any{"path": "src/other.go"}}}}
		}
		return submitFindings()
	}
	if _, err := rig.run(rig.spec("general")); err != nil {
		t.Fatal(err)
	}
	finds := rig.model.requests("finder")
	prompt := finds[0].Users[0]
	i, j := strings.Index(prompt, "<past_fixes>"), strings.Index(prompt, "</past_fixes>")
	if i < 0 || j < i {
		t.Fatalf("the finder was not shown its files' past fixes:\n%s", prompt)
	}
	past := prompt[i:j]
	revert := "- 2222222 2026-08-01 src/totals.go: Revert \"Make Add lock-free\" (PR #41)"
	shared := "- 1111111 2026-06-01 src/other.go, src/totals.go: Fix the lost update in Add and reset (PR #12)"
	if !strings.Contains(past, revert) || !strings.Contains(past, shared) || strings.Index(past, revert) > strings.Index(past, shared) {
		t.Errorf("the past fixes are not the two fixes, newest first, one line each:\n%s", past)
	}
	if strings.Count(past, "1111111") != 1 || strings.Contains(past, "Rename the totals package") || strings.Contains(past, "Merge pull request") {
		t.Errorf("a commit was listed twice, or one that fixed nothing was listed:\n%s", past)
	}
	if !strings.Contains(past, "check that this change does not undo or repeat one") {
		t.Errorf("the section does not say what the fixes are for:\n%s", past)
	}

	i, j = strings.Index(prompt, "<repo_docs>"), strings.Index(prompt, "</repo_docs>")
	if i < 0 || j < i {
		t.Fatalf("the finder was not pointed at the repository's documentation:\n%s", prompt)
	}
	docs := prompt[i:j]
	for _, want := range []string{"- src/NOTES.md", "- docs/totals.md"} {
		if !strings.Contains(docs, want) {
			t.Errorf("the documentation does not list %s:\n%s", want, docs)
		}
	}
	if strings.Contains(docs, "CHANGELOG") || strings.Contains(docs, "LICENSE") || strings.Contains(docs, "# Totals") {
		t.Errorf("the documentation lists a changelog or licence, or more than paths:\n%s", docs)
	}

	res := strings.Join(finds[1].ToolResults, "\n")
	if !strings.Contains(res, "The last 2 commits that changed acme/web src/other.go") || !strings.Contains(res, "- 4444444 2026-07-20 Merge pull request (PR #9)") {
		t.Errorf("file_history answered:\n%s", res)
	}
	for _, q := range rig.model.requests("") {
		for _, who := range []string{"Example Author", "author@example.com", "example-author"} {
			if strings.Contains(q.Body, who) {
				t.Fatalf("a model was told %q, who wrote a commit", who)
			}
		}
	}
	for _, p := range []string{"src/totals.go", "src/other.go"} {
		if n, ok := asked.Load(p); !ok || *n.(*int) != 1 {
			t.Errorf("%s's history was read %v times, want once for the prompt and the tool together", p, n)
		}
	}
}

// A file this pull request renames is looked up under the name the base branch knows; a file with no
// history says so; and a review reads twenty histories beyond the ones read up front.
func TestReviewFileHistoryTool(t *testing.T) {
	r := &reviewRun{repo: "acme/web", base: reviewBase, byPath: map[string]*reviewFile{
		"src/new.go": {File: review.File{Path: "src/new.go", PrevPath: "src/old.go"}}}}
	r.history.byPath = map[string][]reviewCommit{
		"src/old.go":   {{SHA: strings.Repeat("5", 40), Date: "2026-05-05", Subject: "Fix the parser", PR: 7}},
		"src/fresh.go": {},
	}
	ctx := context.Background()
	out, err := r.fileHistory(ctx, "/src/new.go")
	if err != nil || !strings.Contains(out, "acme/web src/old.go on the base branch") || !strings.Contains(out, "- 5555555 2026-05-05 Fix the parser (PR #7)") {
		t.Errorf("a renamed file's history = %q, %v", out, err)
	}
	if out, err := r.fileHistory(ctx, "src/fresh.go"); err != nil || !strings.HasPrefix(out, "No commit changed") {
		t.Errorf("a file with no history = %q, %v", out, err)
	}
	for _, p := range []string{"", "../etc/passwd", strings.Repeat("a/", 300)} {
		if _, err := r.fileHistory(ctx, p); err == nil {
			t.Errorf("file_history(%.30q) was read", p)
		}
	}
	r.history.reads = reviewHistoryCalls
	if _, err := r.fileHistory(ctx, "src/elsewhere.go"); err == nil || !strings.Contains(err.Error(), "20 file histories") {
		t.Errorf("err = %v, want the cap said", err)
	}
	if _, err := r.fileHistory(ctx, "src/fresh.go"); err != nil {
		t.Errorf("a history already read was refused past the cap: %v", err)
	}
}

// The documentation a unit is pointed at: the Markdown beside each of its files and above them to the
// root, nearest first, then documentation folders' files that share a word with its paths. A small
// folder is listed whole, a large one only where it shares a word; changelogs, licences, instruction
// files and files the pull request changes are left out, and never more than twenty.
func TestReviewDocPointers(t *testing.T) {
	tree := []string{"AGENTS.md", "CHANGELOG.md", "LICENSE.md", "README.md", "docs/adr/0001-session-tokens.md", "docs/api.md",
		"docs/auth/sessions.md", "docs/billing.md", "src/README.md", "src/auth/NOTES.mdx", "src/auth/README.md", "src/auth/session.go",
		"src/billing/README.md"}
	got := reviewDocPointers(tree, []string{"src/auth/session.go"}, []string{"src/auth/session.go", "docs/api.md"})
	want := []string{"src/auth/NOTES.mdx", "src/auth/README.md", "src/README.md", "README.md", "docs/adr/0001-session-tokens.md",
		"docs/auth/sessions.md", "docs/billing.md"}
	if !slices.Equal(got, want) {
		t.Errorf("docs = %v\nwant   %v", got, want)
	}

	var big []string
	for _, w := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo",
		"lima", "mike", "november", "oscar", "papa", "quebec", "romeo", "sierra", "tango", "uniform", "victor"} {
		big = append(big, "docs/"+w+".md", "docs/"+w+"-session.md")
	}
	slices.Sort(big)
	got = reviewDocPointers(append(big, "src/auth/session.go"), []string{"src/auth/session.go"}, nil)
	if len(got) != reviewDocsMax {
		t.Errorf("listed %d documents, want the cap of %d", len(got), reviewDocsMax)
	}
	for _, p := range got {
		if !strings.Contains(p, "-session") {
			t.Errorf("a large folder's %s was listed though it shares no word with the change", p)
		}
	}
}
