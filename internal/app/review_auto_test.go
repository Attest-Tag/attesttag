package app

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"attesttag/internal/review"
)

// Automatic types: which reviews may gain one (planReview), which types may join as the organisation
// runs them (reviewAutoTypes), which a run adds and where they read (the engine), and how the run is
// recorded, cached and summarised (the lane). The totals fixture takes a lock away, which is what
// brings Concurrency and state in.

func autoSettings(t *testing.T, raw string) review.Effective {
	t.Helper()
	var s review.Settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	return review.Resolve([]review.LevelSettings{{Level: review.LevelConnection, Settings: s}})
}

func autoPull(base string, labels ...string) *githubPull {
	p := &githubPull{Number: 7, State: "open", Base: githubPullRef{Ref: base}, Head: githubPullRef{Ref: "feature"}}
	for _, l := range labels {
		p.Labels = append(p.Labels, githubLabel{Name: l})
	}
	return p
}

func specKeys(specs []reviewTypeSpec) []string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Key)
	}
	return out
}

// Only a review whose types the branch rule chose may gain one: not one a person named types for, not
// a release summary, and not with the setting off. A label's types added to the rule's leave it on.
func TestPlanReviewLetsTypesJoinOnlyARulesChoice(t *testing.T) {
	plan := func(eff review.Effective, pull *githubPull, named ...string) reviewPlan {
		t.Helper()
		p, s, err := planReview(eff, pull, named, "", false)
		if err != nil || s != nil {
			t.Fatalf("planReview: %v %v", s, err)
		}
		return p
	}
	eff := review.Resolve(nil)
	if !plan(eff, autoPull("main")).auto {
		t.Error("a review of the branch rule's types may gain no automatic type")
	}
	if plan(eff, autoPull("main"), "security").auto {
		t.Error("a review of the types a person named may gain automatic ones")
	}
	rules := autoSettings(t, `{"branch_rules":[{"base":"main","types":["release","security"]},{"labels":["perf"],"types":["performance"]},{}]}`)
	if plan(rules, autoPull("main")).auto {
		t.Error("a release summary may gain automatic types")
	}
	if p := plan(rules, autoPull("testing", "perf")); !p.auto || !slices.Equal(p.keys, []string{"general", "performance"}) {
		t.Errorf("a label's types on the rule's: auto %v, keys %v", p.auto, p.keys)
	}
	if plan(autoSettings(t, `{"auto_types":false}`), autoPull("main")).auto {
		t.Error("automatic types switched off still join")
	}
}

// What may join is each built-in with a pattern that the review does not run already, as the
// organisation runs it: its copy, under the built-in's pattern. Turned off, or a type of the
// organisation's own under the same key, it joins nothing.
func TestReviewAutoTypesAsTheOrganisationRunsThem(t *testing.T) {
	ctx := context.Background()
	p, _ := reviewProxyFixture(t)
	st := p.store
	bt, _ := review.BuiltinType("concurrency")

	got, err := reviewAutoTypes(ctx, st, orgID, []string{"general", "security"})
	if err != nil || !slices.Equal(specKeys(got), []string{"concurrency"}) || got[0].Auto != bt.Auto || got[0].Version != 0 {
		t.Fatalf("the built-in: %v (%v)", specKeys(got), err)
	}
	if got, _ := reviewAutoTypes(ctx, st, orgID, []string{"general", "Concurrency"}); len(got) != 0 {
		t.Errorf("a type the review runs already joined it again: %v", specKeys(got))
	}

	row := reviewTypeOfBuiltin(bt)
	row.Name = "Races"
	cp, err := st.CopyBuiltinReviewType(ctx, orgID, row, "admin@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = reviewAutoTypes(ctx, st, orgID, []string{"general"})
	if len(got) != 1 || got[0].Name != "Races" || got[0].Auto != bt.Auto || got[0].Version != cp.Version {
		t.Errorf("the organisation's copy: %+v", got)
	}

	cp.Enabled = false
	if _, err := st.SaveReviewType(ctx, orgID, cp, "admin@acme.test"); err != nil {
		t.Fatal(err)
	}
	if got, _ := reviewAutoTypes(ctx, st, orgID, []string{"general"}); len(got) != 0 {
		t.Errorf("a type turned off joined: %v", specKeys(got))
	}

	// A type of the organisation's own that took the key before the built-in shipped.
	fresh, _ := reviewProxyFixture(t)
	if _, err := fresh.store.CreateReviewType(ctx, orgID, &ReviewType{Key: "concurrency", Name: "Our own", Purpose: "Ours.",
		Enabled: true, Rules: []ReviewTypeRule{{Text: "No globals.", Enabled: true}}}, "admin@acme.test"); err != nil {
		t.Fatal(err)
	}
	if got, _ := reviewAutoTypes(ctx, fresh.store, orgID, []string{"general"}); len(got) != 0 {
		t.Errorf("an organisation's own type under the built-in's key joined under the built-in's pattern: %+v", got)
	}
}

// The engine adds a type the diff matches, after the spec's own, and runs it; a diff it does not
// match, or a review already running it, adds nothing.
func TestReviewEngineAddsTheTypesTheDiffMatches(t *testing.T) {
	ctx := context.Background()
	rig := newReviewRig(t, totalsFixture())
	spec := rig.spec("general")
	auto, err := reviewAutoTypes(ctx, rig.st, orgID, []string{"general"})
	if err != nil {
		t.Fatal(err)
	}
	spec.Auto = auto
	out, err := rig.run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.AutoTypes) != 1 || out.AutoTypes[0].Key != "concurrency" || !out.AutoTypes[0].Automatic {
		t.Fatalf("added %+v", out.AutoTypes)
	}
	if len(out.TypeRuns) != 2 || out.TypeRuns[0].Key != "general" || out.TypeRuns[0].Auto ||
		out.TypeRuns[1].Key != "concurrency" || !out.TypeRuns[1].Auto || out.TypeRuns[1].Skipped != "" {
		t.Errorf("type runs = %+v", out.TypeRuns)
	}
	if got := finderTypes(rig.model.requests("finder")); !slices.Equal(got, []string{"general", "concurrency"}) {
		t.Errorf("finder passes for %v", got)
	}
	if len(spec.Types) != 1 {
		t.Errorf("the caller's types became %v", specKeys(spec.Types))
	}

	// Named already: it runs once, as chosen.
	again := newReviewRig(t, totalsFixture())
	both := again.spec("general", "concurrency")
	both.Auto = auto
	if out, err := again.run(both); err != nil || len(out.AutoTypes) != 0 || len(out.TypeRuns) != 2 || out.TypeRuns[1].Auto {
		t.Errorf("a type the spec runs already: added %v, runs %+v (%v)", specKeys(out.AutoTypes), out.TypeRuns, err)
	}

	// A change with nothing of the pattern in it.
	fx := reviewPRFixture{title: "Fix the sum", body: "Adds instead of subtracting.", head: map[string]string{}, base: map[string]string{}}
	fx.addFile("src/sum.go", "package sum\n\n// Sum adds a and b.\nfunc Sum(a, b int) int {\n\treturn a + b\n}\n")
	plain := newReviewRig(t, fx)
	ps := plain.spec("general")
	ps.Auto = auto
	if out, err := plain.run(ps); err != nil || len(out.AutoTypes) != 0 || len(out.TypeRuns) != 1 {
		t.Errorf("a diff the pattern does not match: added %v, runs %+v (%v)", specKeys(out.AutoTypes), out.TypeRuns, err)
	}
	if got := finderTypes(plain.model.requests("finder")); !slices.Equal(got, []string{"general"}) {
		t.Errorf("finder passes for %v", got)
	}
}

// An automatic type reads the units its pattern matches in, not the whole pull request again, and the
// finder's time is counted in the passes that leaves.
func TestReviewAutoTypeReadsOnlyTheUnitsItMatches(t *testing.T) {
	file := func(path, added string) *reviewFile {
		return &reviewFile{File: review.File{Path: path, Status: "modified", Additions: 1,
			Hunks: []review.Hunk{{NewStart: 1, NewLines: 1, Lines: []review.Line{{Kind: '+', New: 1, Text: added}}}}},
			tokens: reviewUnitTokens/2 + 1} // one file a unit
	}
	files := []*reviewFile{file("src/a.go", "return a"), file("src/b.ts", "await save(field)"), file("src/c.go", "return c"),
		file("src/d.go", "return d")}
	bt, _ := review.BuiltinType("concurrency")
	ts := &reviewTypeSpec{Type: bt, Automatic: true}
	units, _ := reviewSplit(files)
	if got := reviewAutoUnits(ts, units); len(got) != 1 || got[0].files[0].Path != "src/b.ts" {
		t.Errorf("the units an automatic type reads: %+v", got)
	}
	general, _ := review.BuiltinType(review.DefaultType)
	r := &reviewRun{reviewable: files, spec: reviewSpec{Types: []reviewTypeSpec{{Type: general}, *ts}}}
	if n := r.finderPasses(); n != 5 {
		t.Errorf("finder passes = %d, want General's four and Concurrency's one", n)
	}
}

// Through the lane: the run is recorded under the type the diff brought in, its summary says
// "(auto)", and another request for the same review is still answered from it. With the setting
// off, nothing joins.
func TestReviewLaneRecordsTheTypesTheDiffBroughtIn(t *testing.T) {
	ctx := context.Background()
	rig := newLaneRig(t, totalsFixture(), `{"mode":"live"}`)
	rig.confirmLock()
	rig.deliver("pull_request", prEvent("opened", 7, reviewHead))
	rig.drain()
	runs := rig.runs(7)
	if len(runs) != 1 || runs[0].Status != "posted" || !slices.Equal(runTypes(runs[0]), []string{"general", "concurrency"}) {
		t.Fatalf("runs = %+v", runs)
	}
	ck, _ := checkpointFrom(runs[0])
	if ck == nil || len(ck.Types) != 2 || ck.Types[0].Auto || !ck.Types[1].Auto {
		t.Errorf("checkpoint types = %+v", ck)
	}
	if summary := rig.lastSummary(); !strings.Contains(summary, "Reviewed as General and Concurrency and state (auto)") {
		t.Errorf("the summary does not say Concurrency came in by itself:\n%s", summary)
	}
	if got := reviewChosenTypes(runs[0]); !slices.Equal(got, []ReviewRunType{{Key: "general"}}) {
		t.Errorf("the types asked for = %v", got)
	}
	gh, err := rig.b.reviewClient(orgID, fakeInstallation, "acme/web", 7)
	if err != nil {
		t.Fatal(err)
	}
	pull, err := gh.Pull(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last, same := rig.b.reviewSameState(ctx, orgID, fakeInstallation, "acme/web", pull, nil, "", false); !same || last.ID != runs[0].ID {
		t.Error("the same review asked for again is not answered from the one that ran")
	}

	off := newLaneRig(t, totalsFixture(), `{"mode":"live","auto_types":false}`)
	off.confirmLock()
	off.deliver("pull_request", prEvent("opened", 7, reviewHead))
	off.drain()
	if runs := off.runs(7); len(runs) != 1 || !slices.Equal(runTypes(runs[0]), []string{"general"}) {
		t.Errorf("with automatic types off: %+v", runs)
	}
	if got := finderTypes(off.model.requests("finder")); !slices.Equal(got, []string{"general"}) {
		t.Errorf("with automatic types off, finder passes for %v", got)
	}
}

// The types the diff brought in, each at its version, are in the cache key; a run that brought in
// none keeps the key it had before there were any.
func TestReviewCacheKeyCountsAutomaticTypes(t *testing.T) {
	out := &reviewOutcome{HeadSHA: reviewHead, BaseSHA: reviewBase}
	none := reviewCacheKey(out, "cfg", nil, review.ModeLive, reviewScopeWhole, nil)
	out.AutoTypes = []reviewTypeSpec{{Type: review.Type{Key: "concurrency"}}}
	one := reviewCacheKey(out, "cfg", nil, review.ModeLive, reviewScopeWhole, nil)
	out.AutoTypes[0].Version = 2
	edited := reviewCacheKey(out, "cfg", nil, review.ModeLive, reviewScopeWhole, nil)
	if none == one || one == edited {
		t.Error("the cache key does not follow the types the diff brought in")
	}
}
