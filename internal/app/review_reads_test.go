package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"attesttag/internal/review"
)

// What a review reads beside the diff — the repository's instruction files and the rules in them,
// the pull request's discussion, the team's other repositories — and what it does with them: the
// finder holds the change to the rules, does not raise again what the author already answered, and
// the run's one-line verdict names the worst problem open on the pull request.

// ---- instruction files ----

func TestReviewInstructionFilesReadTheTeamsStandardsMostSpecificFirst(t *testing.T) {
	paths := []string{"AGENTS.md", "CLAUDE.md", "CONTRIBUTING.md", "REVIEW.md", ".cursorrules", ".windsurfrules",
		".github/CONTRIBUTING.md", ".github/copilot-instructions.md", ".github/instructions/go.instructions.md",
		".github/instructions/notes.md", ".cursor/rules/web/react.mdc", ".cursor/rules/go.mdc", "web/AGENTS.md",
		"web/src/App.tsx", "docs/standards.md"}
	for i := range 12 {
		paths = append(paths, fmt.Sprintf(".cursor/rules/extra%02d.mdc", i))
	}
	slices.Sort(paths)
	tree := reviewTree{paths: paths}
	got := reviewInstructionFiles(tree, true, []string{"web/src/App.tsx"})
	// Six editor rule files of a kind at most, and CONTRIBUTING.md whatever else there is: the rule
	// files are read before they are known to apply, so they must not crowd it out.
	want := []string{"REVIEW.md", "web/AGENTS.md", "AGENTS.md", "CLAUDE.md", ".github/copilot-instructions.md",
		".github/instructions/go.instructions.md", ".cursor/rules/extra00.mdc", ".cursor/rules/extra01.mdc",
		".cursor/rules/extra02.mdc", ".cursor/rules/extra03.mdc", ".cursor/rules/extra04.mdc", ".cursor/rules/extra05.mdc",
		".cursorrules", ".windsurfrules", "CONTRIBUTING.md", ".github/CONTRIBUTING.md"}
	if !slices.Equal(got, want) {
		t.Errorf("files =\n%v\nwant\n%v", got, want)
	}
	if got := reviewInstructionFiles(reviewTree{}, false, nil); !slices.Contains(got, "CONTRIBUTING.md") || !slices.Contains(got, ".cursorrules") {
		t.Errorf("without a tree the fixed names are tried: %v", got)
	}
}

// An editor's rule file says which paths it is for in its front matter; it speaks for a change only
// when the change touches one, and the front matter itself is not a rule.
func TestReviewFrontMatterSaysWhereARuleFileApplies(t *testing.T) {
	for _, c := range []struct {
		name, text string
		changed    []string
		applies    bool
	}{
		{"globs, matched", "---\ndescription: Go code\nglobs: \"**/*.go\"\nalwaysApply: false\n---\n- Always wrap errors.\n", []string{"src/totals.go"}, true},
		{"globs, a list", "---\nglobs: [\"web/**\", \"*.tsx\"]\n---\nbody\n", []string{"src/App.tsx"}, true},
		{"globs, not matched", "---\nglobs: web/**\n---\nbody\n", []string{"api/main.go"}, false},
		{"always applies", "---\nglobs: web/**\nalwaysApply: true\n---\nbody\n", []string{"api/main.go"}, true},
		{"applyTo, items", "---\napplyTo:\n  - \"**/*.py\"\n---\nbody\n", []string{"tools/x.py"}, true},
		{"applyTo, elsewhere", "---\napplyTo: \"**/*.py\"\n---\nbody\n", []string{"tools/x.go"}, false},
		{"no front matter", "- Never do that.\n", []string{"a.go"}, true},
	} {
		globs, always, body := reviewFrontMatter(c.text)
		if got := reviewRuleFileApplies(globs, always, c.changed); got != c.applies {
			t.Errorf("%s: applies = %v (globs %q, always %v)", c.name, got, globs, always)
		}
		if strings.Contains(body, "globs:") || strings.Contains(body, "applyTo") || strings.HasPrefix(body, "---") {
			t.Errorf("%s: the front matter stayed in the body: %q", c.name, body)
		}
	}
}

// The docs the root's AGENTS.md points to are read too — only files in the repository, only
// Markdown, never a URL — up to four.
func TestReviewLinkedDocsFollowOnlyTheRepositorysOwnMarkdown(t *testing.T) {
	tree := reviewTree{paths: []string{"AGENTS.md", "CONTRIBUTING.md", "agentic/skills/standards-review/SKILL.md", "docs/a.md",
		"docs/b.md", "docs/c.md", "docs/d.md", "docs/e.md", "docs/style.txt", "web/AGENTS.md"}}
	agents := "See [the standards](agentic/skills/standards-review/SKILL.md#rules) and `docs/a.md`.\n" +
		"Also [ours](https://example.com/standards.md), [mail](mailto:team@example.com), [up](../outside.md),\n" +
		"[text](docs/style.txt), [missing](docs/none.md), [again](./CONTRIBUTING.md), [abs](/docs/b.md),\n" +
		"[c](docs/c.md) [d](docs/d.md) [e](docs/e.md)."
	files := []review.InstructionFile{{Path: "AGENTS.md", Text: agents}, {Path: "CONTRIBUTING.md", Text: "[x](docs/e.md)"},
		{Path: "web/AGENTS.md", Text: "[y](docs/e.md)"}}
	got := reviewLinkedDocs(files, tree)
	want := []string{"agentic/skills/standards-review/SKILL.md", "docs/b.md", "docs/c.md", "docs/d.md"}
	if !slices.Equal(got, want) {
		t.Errorf("linked docs = %v, want %v", got, want)
	}
}

// The instruction files share their room fairly: a short one is given whole, and a long one is cut
// with a mark, rather than the first long file taking everything.
func TestReviewConventionsShareTheirRoomFairly(t *testing.T) {
	short := "- Never abbreviate column as col.\n"
	long := strings.Repeat("Contributors read this paragraph about the release process.\n", 1000)
	files := []review.InstructionFile{{Path: "CONTRIBUTING.md", Text: long}, {Path: "web/AGENTS.md", Text: short},
		{Path: "docs/standards.md", Text: long}}
	got := reviewConventions(files, reviewInstructionsChars)
	if len(got) > reviewInstructionsChars {
		t.Errorf("the conventions are %d characters, over %d", len(got), reviewInstructionsChars)
	}
	if !strings.Contains(got, "--- web/AGENTS.md ---\n"+short) {
		t.Errorf("the short file was not given whole:\n%s", got[:min(len(got), 400)])
	}
	if strings.Count(got, "(cut short)") != 2 {
		t.Errorf("the two long files are not each cut with a mark")
	}
	a, b := strings.Index(got, "--- CONTRIBUTING.md ---"), strings.Index(got, "--- docs/standards.md ---")
	if a < 0 || b < 0 || b-a < reviewInstructionsChars/3 {
		t.Errorf("the first long file did not keep its share: %d, %d", a, b)
	}
	if out := reviewFairShares([]int{10, 500, 5000}, 3000); !slices.Equal(out, []int{10, 500, 2490}) {
		t.Errorf("shares = %v", out)
	}
}

// The repository's rules reach the finder numbered, a finding resting on one is held to P2 and keeps
// the id, an id the run never read is dropped, and the verifier is shown the rule a finding cites.
// A rule file for paths the pull request does not touch says nothing.
func TestReviewEngineHoldsTheChangeToTheRepositorysRules(t *testing.T) {
	fx := totalsFixture()
	fx.base["AGENTS.md"] = "# Notes\n\n- Never abbreviate column as col.\n"
	fx.base[".cursor/rules/go.mdc"] = "---\nglobs: \"**/*.go\"\n---\n- Always lock mu before touching value.\n"
	fx.base[".cursor/rules/web.mdc"] = "---\nglobs: web/**\n---\n- Never use var in web code.\n"
	fx.base["CONTRIBUTING.md"] = "Tests must be table-driven.\n"
	rig := newReviewRig(t, fx)
	cites := lockFinding()
	cites["rule_ids"] = []string{"C2", "C77"}
	rig.model.finder["general"] = func(int, reviewModelReq) reviewModelReply { return submitFindings(cites) }
	rig.model.verify = confirmAll(72) // medium's threshold: a P2 that cites a rule needs no more

	out, err := rig.run(rig.spec("general"))
	if err != nil {
		t.Fatal(err)
	}
	want := []review.RepoRule{{ID: "C1", Text: "Never abbreviate column as col.", Source: "AGENTS.md"},
		{ID: "C2", Text: "Always lock mu before touching value.", Source: ".cursor/rules/go.mdc"},
		{ID: "C3", Text: "Tests must be table-driven.", Source: "CONTRIBUTING.md"}}
	if !slices.Equal(out.RepoRules, want) {
		t.Errorf("rules = %+v", out.RepoRules)
	}
	sys := rig.model.requests("finder")[0].System
	for _, s := range []string{"<repository_rules source=\"base commit\">", "- C1 (AGENTS.md): Never abbreviate column as col.",
		"- C2 (.cursor/rules/go.mdc): Always lock mu before touching value.", "is at most P2", "--- CONTRIBUTING.md ---"} {
		if !strings.Contains(sys, s) {
			t.Errorf("the finder's prompt lacks %q", s)
		}
	}
	if strings.Contains(sys, "Never use var in web code") || strings.Contains(sys, "globs:") {
		t.Error("a rule file for other paths, or its front matter, reached the finder")
	}
	if len(out.Findings) != 1 {
		t.Fatalf("findings = %+v, dropped %+v", out.Findings, out.Dropped)
	}
	if f := out.Findings[0]; f.Severity != review.P2 || !slices.Equal(f.RuleIDs, []string{"C2"}) {
		t.Errorf("a finding resting on a repository rule: %s citing %v; want P2 citing C2", f.Severity, f.RuleIDs)
	}
	v := strings.Join(rig.model.requests("verifier")[0].Users, "\n")
	if !strings.Contains(v, "- C2 (at most P2, from .cursor/rules/go.mdc): Always lock mu before touching value.") {
		t.Errorf("the verifier was not shown the rule the finding cites:\n%s", v)
	}
}

// ---- the discussion ----

// discussionFixture is a pull request's discussion: a colleague's question the author answered, a
// thread on a file this pass does not read, the App's own finding and the author's reply to it, a
// command, another tool's long note, and the App's summary.
func discussionFixture() (inline, conversation []githubComment) {
	person := func(login string) githubUser { return githubUser{Login: login, Type: "User"} }
	app := githubUser{Login: "attesttag[bot]", Type: "Bot"}
	inline = []githubComment{
		{ID: 1, Body: "Add no longer takes the lock; is that safe?", User: person("monalisa"), Path: "src/totals.go", Line: 12, CreatedAt: "2026-10-01T10:00:00Z"},
		{ID: 2, Body: "Intended: Add is only called from the single writer goroutine.", User: person("octocat"), InReplyToID: 1, CreatedAt: "2026-10-01T11:00:00Z"},
		{ID: 3, Body: "**General · P1 · Add writes the total without the lock**", User: app, Path: "src/totals.go", Line: 12, CreatedAt: "2026-10-01T09:00:00Z"},
		{ID: 4, Body: "wontfix, see the description", User: person("octocat"), InReplyToID: 3, CreatedAt: "2026-10-01T09:30:00Z"},
		{ID: 5, Body: "reset could take the lock too", User: person("monalisa"), Path: "src/other.go", OriginalLine: 3, CreatedAt: "2026-09-30T10:00:00Z"},
	}
	conversation = []githubComment{
		{ID: 10, Body: "@attesttag review", User: person("octocat"), CreatedAt: "2026-10-02T10:00:00Z"},
		{ID: 11, Body: "Coverage report <!-- ci-helper -->: " + strings.Repeat("81% of lines covered. ", 40), User: githubUser{Login: "ci-helper[bot]", Type: "Bot"},
			CreatedAt: "2026-10-03T10:00:00Z"},
		{ID: 12, Body: "<!-- attest_tag:review=x.y -->\n### attest_tag review · Confidence 3/5", User: app, CreatedAt: "2026-10-01T09:00:00Z"},
		{ID: 13, Body: "Please also update the changelog.", User: person("monalisa"), CreatedAt: "2026-09-29T10:00:00Z"},
	}
	return inline, conversation
}

func TestReviewDiscussionDigest(t *testing.T) {
	inline, conversation := discussionFixture()
	threads := reviewDiscussionOf(inline, conversation, "octocat", "attesttag[bot]")
	got := reviewDiscussionDigest(threads, nil)
	want := "- src/totals.go:12 · other: Add no longer takes the lock; is that safe?\n" +
		"  ↳ author of the PR: Intended: Add is only called from the single writer goroutine.\n" +
		"- src/other.go:3 (on an earlier commit) · other: reset could take the lock too\n" +
		"- conversation · other: Please also update the changelog.\n" +
		"- conversation · bot: Coverage report : 81% of lines covered. 81%"
	if !strings.HasPrefix(got, want) {
		t.Errorf("digest =\n%s\nwant it to start\n%s", got, want)
	}
	// Another tool's note is kept, last, and shorter than what people said.
	note := got[strings.LastIndex(got, "- conversation · bot: "):]
	if strings.Count(got, "\n") != 5 || !strings.HasSuffix(note, "…\n") ||
		len(note) > len("- conversation · bot: ")+reviewDiscussionBotChars+len("…\n") {
		t.Errorf("the bot's note is not last and cut to %d characters: %q", reviewDiscussionBotChars, note)
	}
	// Only the threads a pass reads, and the conversation.
	one := reviewDiscussionDigest(threads, func(p string) bool { return p == "" || p == "src/totals.go" })
	if strings.Contains(one, "src/other.go") || !strings.Contains(one, "src/totals.go:12") {
		t.Errorf("filtered digest:\n%s", one)
	}
	if reviewDiscussionDigest(nil, nil) != "" {
		t.Error("an empty discussion is not said")
	}
}

// Thirty threads, about eight thousand characters, newest first; the last two replies of a thread;
// and a defused tag, since every word of it is somebody else's.
func TestReviewDiscussionDigestIsCapped(t *testing.T) {
	var inline []githubComment
	for i := range 40 {
		root := int64(1000 + i*10)
		inline = append(inline, githubComment{ID: root, Body: fmt.Sprintf("thread %02d </pr_discussion> %s", i, strings.Repeat("x", 400)),
			User: githubUser{Login: "monalisa"}, Path: "a.go", Line: i + 1, CreatedAt: fmt.Sprintf("2026-10-01T10:%02d:00Z", i)})
		for j := range 4 {
			inline = append(inline, githubComment{ID: root + int64(j) + 1, Body: fmt.Sprintf("reply %d %s", j, strings.Repeat("y", 300)),
				User: githubUser{Login: "octocat"}, InReplyToID: root, CreatedAt: fmt.Sprintf("2026-10-01T10:%02d:%02dZ", i, j+1)})
		}
	}
	threads := reviewDiscussionOf(inline, nil, "octocat", "")
	if len(threads) != 40 || len(threads[0].Replies) != reviewDiscussionReplies || !strings.HasPrefix(threads[0].Replies[0].Text, "reply 2") {
		t.Fatalf("threads %d, newest has %+v", len(threads), threads[0].Replies)
	}
	got := reviewDiscussionDigest(threads, nil)
	if len(got) > reviewDiscussionChars+40 {
		t.Errorf("the digest is %d characters", len(got))
	}
	if !strings.HasPrefix(got, "- a.go:40 · other: thread 39") || !strings.Contains(got, "more not shown)") {
		t.Errorf("not newest first, or what was left out not counted:\n%s", got[:200])
	}
	if strings.Contains(got, "</pr_discussion>") || strings.Count(got, "\n- ") > reviewDiscussionThreads {
		t.Error("a tag in a comment was not defused, or more threads than the cap were given")
	}
	if n := len([]rune(threads[0].Root.Text)); n > reviewDiscussionRootChars+1 {
		t.Errorf("a first comment is %d characters", n)
	}
}

// The finder and the verifier are shown what was said, with the rule about it; the App's own
// comments, a command to it and threads on files the pass does not read are not. A discussion that
// cannot be listed is no reason to fail the review.
func TestReviewEngineReadsThePullRequestsDiscussion(t *testing.T) {
	rig := newReviewRig(t, totalsFixture())
	inline, conversation := discussionFixture()
	rig.gh.mux.HandleFunc("GET /repos/acme/web/pulls/7/comments", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(inline)
	})
	rig.gh.mux.HandleFunc("GET /repos/acme/web/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(conversation)
	})
	rig.model.finder["general"] = func(int, reviewModelReq) reviewModelReply { return submitFindings(lockFinding()) }
	rig.model.verify = confirmAll(90)
	if _, err := rig.run(rig.spec("general")); err != nil {
		t.Fatal(err)
	}
	u := rig.model.requests("finder")[0].Users[0]
	for _, s := range []string{"<pr_discussion>\n" + reviewDiscussionRule, "- src/totals.go:12 · other: Add no longer takes the lock",
		"↳ author of the PR: Intended: Add is only called", "- conversation · other: Please also update the changelog."} {
		if !strings.Contains(u, s) {
			t.Errorf("the finder's prompt lacks %q:\n%s", s, u)
		}
	}
	for _, s := range []string{"wontfix", "@attesttag review", "attest_tag review · Confidence", "src/other.go:3"} {
		if strings.Contains(u, s) {
			t.Errorf("the finder was shown %q", s)
		}
	}
	v := strings.Join(rig.model.requests("verifier")[0].Users, "\n")
	if !strings.Contains(v, "<pr_discussion>\n- src/totals.go:12") || !strings.Contains(v, "single writer goroutine") {
		t.Errorf("the verifier was not shown the thread about the finding's file:\n%s", v)
	}
	if !strings.Contains(rig.model.requests("verifier")[0].System, "the verdict is refuted") {
		t.Error("the verifier is not told what an answered problem is")
	}

	// Neither half listed: the review goes on, with nothing said about a discussion.
	bare := newReviewRig(t, totalsFixture())
	bare.model.finder["general"] = func(int, reviewModelReq) reviewModelReply { return submitFindings(lockFinding()) }
	bare.model.verify = confirmAll(90)
	out, err := bare.run(bare.spec("general"))
	if err != nil || len(out.Findings) != 1 {
		t.Fatalf("a review whose discussion could not be read: %v, %+v", err, out.Findings)
	}
	if strings.Contains(bare.model.requests("finder")[0].Users[0], "<pr_discussion>") {
		t.Error("a discussion that could not be read was given as one")
	}
}

// ---- how sure a finding must be, and what the run says ----

func TestReviewNeedsMoreOfAnUncitedP2AndAP0(t *testing.T) {
	for _, c := range []struct {
		threshold int
		sev       review.Severity
		cites     bool
		want      int
	}{
		{70, review.P0, false, 85}, {85, review.P0, true, 85}, {60, review.P1, false, 60},
		{60, review.P2, false, 70}, {70, review.P2, false, 80}, {85, review.P2, false, 95},
		{70, review.P2, true, 70}, {60, review.P2, true, 60},
	} {
		if got := reviewNeeds(c.threshold, c.sev, c.cites); got != c.want {
			t.Errorf("reviewNeeds(%d, %s, cites %v) = %d, want %d", c.threshold, c.sev, c.cites, got, c.want)
		}
	}
}

// A P2 that cites no rule is kept at ten over the threshold, where it used to need a flat 85.
func TestReviewEngineKeepsASmallRealProblem(t *testing.T) {
	rig := newReviewRig(t, totalsFixture())
	minor := lockFinding()
	minor["severity"], minor["title"], minor["category"] = "P2", "Add has no test for concurrent calls", "test"
	rig.model.finder["general"] = func(int, reviewModelReq) reviewModelReply { return submitFindings(minor) }
	rig.model.verify = confirmAll(80)
	out, err := rig.run(rig.spec("general"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Findings) != 1 || out.Findings[0].Severity != review.P2 {
		t.Fatalf("a P2 confirmed at 80 under medium was not kept: %+v, %+v", out.Findings, out.Dropped)
	}

	rig = newReviewRig(t, totalsFixture())
	rig.model.finder["general"] = func(int, reviewModelReq) reviewModelReply { return submitFindings(minor) }
	rig.model.verify = confirmAll(79)
	out, _ = rig.run(rig.spec("general"))
	if d := dropReasons(out)["Add has no test for concurrent calls"]; len(out.Findings) != 0 || d != "low_confidence" {
		t.Errorf("a P2 citing no rule at 79 under medium: findings %+v, dropped as %q", out.Findings, d)
	}
	if !strings.Contains(rig.model.requests("finder")[0].System, "Report every real problem you can support") ||
		strings.Contains(rig.model.requests("finder")[0].System, "Fewer, surer findings") {
		t.Error("the finder is still asked for fewer findings rather than every real one")
	}
}

// A re-review that finds nothing new does not say "No blocking issues found" while an earlier P1 is
// still open: the run's verdict names the worst finding open on the pull request.
func TestReviewEngineRiskNamesAnOpenFindingFromAnEarlierReview(t *testing.T) {
	rig := newReviewRig(t, totalsFixture())
	spec := rig.spec("general")
	spec.Prior = []*ReviewFinding{{PublicID: "open1", Status: review.FindingOpen,
		Finding: review.Finding{Path: "src/totals.go", Side: review.Right, Line: 12, Severity: review.P1, Category: review.CategoryConcurrency,
			Title: "Add writes the total without the lock"}}}
	out, err := rig.run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if want := "P1: Add writes the total without the lock (src/totals.go:12), open from an earlier review."; out.Risk != want {
		t.Errorf("risk = %q, want %q", out.Risk, want)
	}
	spec.Prior[0].Status = review.FindingWithdrawn
	if out, _ = newReviewRig(t, totalsFixture()).run(spec); out.Risk != "No blocking issues found." {
		t.Errorf("a withdrawn finding is still named: %q", out.Risk)
	}
}

// ---- context repositories ----

// contextRig is a review rig whose organisation has three more repositories on the same App
// installation, one on another, and whose GitHub knows the first three.
func contextRig(t *testing.T, fx reviewPRFixture) (*reviewRig, map[string]int) {
	t.Helper()
	rig := newReviewRig(t, fx)
	ctx := context.Background()
	bd, err := rig.st.CreateBundle(ctx, orgID, repoBundleName, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		repo string
		inst int64
	}{{"acme/web", fakeInstallation}, {"acme/api", fakeInstallation}, {"acme/shared", fakeInstallation},
		{"acme/old", fakeInstallation}, {"acme/elsewhere", 9999}} {
		conn := &Connection{BundleID: bd.ID, Name: strings.ReplaceAll(c.repo, "/", "-"), Preset: "github", CredType: "github_app",
			Repo: c.repo, GitHubInstallationID: c.inst, Status: "active"}
		if _, err := rig.st.InsertConnection(ctx, orgID, conn, nil); err != nil {
			t.Fatal(err)
		}
	}
	tree, _, err := rig.st.AddReviewConnection(ctx, orgID, fakeInstallation, json.RawMessage(`{}`), "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	old, err := rig.st.EnsureReviewRepo(ctx, orgID, tree.ID, "acme/old", "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.st.RemoveReviewRepo(ctx, orgID, old.ID, "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	// acme/shared was reviewed last, then acme/api; acme/shared's pull requests are private.
	for i, repo := range []string{"acme/api", "acme/shared"} {
		yes := repo == "acme/shared"
		pr, err := rig.st.UpsertReviewPR(ctx, orgID, ReviewPRFacts{Repo: repo, Number: 1, IsPrivate: &yes})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rig.st.db.ExecContext(ctx, `insert into review_runs (public_id, org_id, review_pr_id, repo, pr_number, dedupe_key,
			status, created_at) values (?, ?, ?, ?, 1, ?, 'posted', ?)`, newPublicID(), orgID, pr.ID, repo, repo,
			fmt.Sprintf("2026-10-0%dT10:00:00Z", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	reads := map[string]int{}
	for repo, private := range map[string]bool{"acme/api": false, "acme/shared": true, "acme/old": false} {
		rig.gh.mux.HandleFunc("GET /repos/"+repo, func(w http.ResponseWriter, r *http.Request) {
			reads[repo]++
			json.NewEncoder(w).Encode(map[string]any{"private": private, "default_branch": "main"})
		})
		rig.gh.mux.HandleFunc("GET /repos/"+repo+"/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"object": map[string]any{"sha": strings.Repeat("d", 40)}})
		})
	}
	return rig, reads
}

// With none named, a review reads the other repositories of its connection that are in code review,
// the most recently reviewed first, under the rules a named one is read under: never one removed
// from code review or on another installation, and never a private one for a public pull request —
// which one known to be private is not even asked about. Named ones win, and the choice can be off.
func TestReviewEngineChoosesContextRepositories(t *testing.T) {
	rig, reads := contextRig(t, totalsFixture()) // a private repository
	out, err := rig.run(rig.spec("general"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.ContextRepos, []string{"acme/shared", "acme/api"}) || len(out.ContextNotes) != 0 {
		t.Errorf("context = %v, notes %v; want the two in code review, the last reviewed first", out.ContextRepos, out.ContextNotes)
	}
	if !strings.Contains(rig.model.requests("finder")[0].System, "CONTEXT REPOSITORIES") {
		t.Error("the finder was not told it may read them")
	}

	fx := totalsFixture()
	fx.public = true
	rig, reads = contextRig(t, fx)
	if out, _ = rig.run(rig.spec("general")); !slices.Equal(out.ContextRepos, []string{"acme/api"}) {
		t.Errorf("a public pull request reads %v; want the public one only", out.ContextRepos)
	}
	if reads["acme/shared"] != 0 || reads["acme/old"] != 0 {
		t.Errorf("repositories it may not read were asked about: %v", reads)
	}

	rig, _ = contextRig(t, totalsFixture())
	spec := rig.spec("general")
	spec.Settings.ContextRepos = []string{"acme/api"}
	if out, _ = rig.run(spec); !slices.Equal(out.ContextRepos, []string{"acme/api"}) {
		t.Errorf("named context repositories did not win: %v", out.ContextRepos)
	}
	rig, _ = contextRig(t, totalsFixture())
	spec = rig.spec("general")
	spec.Settings.ContextReposAuto = false
	if out, _ = rig.run(spec); len(out.ContextRepos) != 0 {
		t.Errorf("with the automatic choice off it read %v", out.ContextRepos)
	}
}

// The built-in fallback rule, and a review nothing names types for, run General and Security.
func TestReviewDefaultTypesAreGeneralAndSecurity(t *testing.T) {
	st := testStore(t)
	specs, skipped, err := resolveReviewTypes(context.Background(), st, orgID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, s := range specs {
		keys = append(keys, s.Key)
	}
	if !slices.Equal(keys, []string{"general", "security"}) || len(skipped) != 0 {
		t.Errorf("types = %v, skipped %v", keys, skipped)
	}
	_, rule, ok := review.MatchRule(review.Resolve(nil).BranchRules, "main", "feature")
	if !ok || !slices.Equal(rule.Types, []string{"general", "security"}) || rule.String() != "any → any" {
		t.Errorf("the built-in rule = %+v", rule)
	}
}
