package review

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
)

func TestResolveWithNothingSetIsTheBuiltInDefaults(t *testing.T) {
	e := Resolve(nil)
	if e.Mode != ModeShadow || e.Trigger != TriggerOpen || e.Drafts || e.Forks != ForksCommand ||
		e.Strictness != StrictnessMedium || e.MaxComments != 8 || e.CommentHeader != "" ||
		e.Model != "heavy" || e.MaxUSD != 1.00 {
		t.Errorf("defaults resolved to %+v", e)
	}
	if len(e.BranchRules) != 1 || !e.BranchRules[0].Fallback() || !slices.Equal(e.BranchRules[0].Types, []string{DefaultType, "security"}) {
		t.Errorf("default branch rules = %+v, want one fallback running %s and security", e.BranchRules, DefaultType)
	}
	if e.ContextReposAuto {
		t.Error("the automatic choice of context repositories is on by default")
	}
	for _, field := range SettingFields() {
		if e.Source[field] != LevelDefault {
			t.Errorf("source[%s] = %q, want default", field, e.Source[field])
		}
	}
	// The console renders the lists, so they are [] rather than null.
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "null") {
		t.Errorf("effective settings should have no null field: %s", raw)
	}
}

// SettingFields, Settings, Effective and Source must agree on the field names, or the console
// shows a field with no "from" and the permission check misses one.
func TestEveryFieldHasOneNameEverywhere(t *testing.T) {
	var set map[string]json.RawMessage
	raw, _ := json.Marshal(Defaults())
	json.Unmarshal(raw, &set)
	full := Defaults()
	full.Instructions, full.ExcludeAuthors, full.IgnorePaths, full.ContextRepos = []string{"x"}, []string{"x"}, []string{"x"}, []string{"x/y"}
	full.ReviewBots = []string{"x"}
	raw, _ = json.Marshal(full)
	json.Unmarshal(raw, &set)

	var eff map[string]json.RawMessage
	// With a channel: a zero one is left out of the JSON (Effective.Notify).
	// With a channel, and the automatic context repositories on: a zero one of either is left out of
	// the JSON (Effective.Notify, Effective.ContextReposAuto).
	raw, _ = json.Marshal(Resolve([]LevelSettings{{LevelConnection, Settings{Notify: &NotifyChannel{Team: "T1", Channel: "C1"},
		ContextReposAuto: ptr(true)}}}))
	json.Unmarshal(raw, &eff)
	delete(eff, "source")

	fields := SettingFields()
	if len(set) != len(fields) || len(eff) != len(fields) {
		t.Fatalf("Settings has %d fields, Effective %d, SettingFields %d", len(set), len(eff), len(fields))
	}
	for _, f := range fields {
		if _, ok := set[f]; !ok {
			t.Errorf("Settings has no %q", f)
		}
		if _, ok := eff[f]; !ok {
			t.Errorf("Effective has no %q", f)
		}
	}
}

func TestResolveTakesEachSingleValueFromTheNearestLevelThatSetsIt(t *testing.T) {
	chain := []LevelSettings{
		{LevelConnection, Settings{Strictness: ptr(StrictnessHigh), Mode: ptr(ModeLive), Drafts: ptr(true), CommentHeader: ptr("Bot review")}},
		{LevelGroup, Settings{Strictness: ptr(StrictnessLow), MaxUSD: ptr(2.5)}},
		{LevelRepo, Settings{Drafts: ptr(false), CommentHeader: ptr("")}},
	}
	e := Resolve(chain)
	checks := []struct {
		field string
		got   any
		want  any
		from  Level
	}{
		{"strictness", e.Strictness, StrictnessLow, LevelGroup},
		{"mode", e.Mode, ModeLive, LevelConnection},
		{"max_usd", e.MaxUSD, 2.5, LevelGroup},
		// Set to false at the repository is a value, not an absence: it overrides true.
		{"drafts", e.Drafts, false, LevelRepo},
		// So is an empty header, which turns off the connection's.
		{"comment_header", e.CommentHeader, "", LevelRepo},
		{"model", e.Model, "heavy", LevelDefault},
	}
	for _, c := range checks {
		if c.got != c.want || e.Source[c.field] != c.from {
			t.Errorf("%s = %v from %s, want %v from %s", c.field, c.got, e.Source[c.field], c.want, c.from)
		}
	}
}

func TestResolveAddsListsUpBroadestFirst(t *testing.T) {
	chain := []LevelSettings{
		{LevelConnection, Settings{
			ExcludeAuthors: []string{"release-bot", "Octocat"},
			ReviewBots:     []string{"renovate[bot]"},
			IgnorePaths:    []string{"**/*.snap"},
			Instructions:   []string{"Money is integer cents."},
		}},
		{LevelGroup, Settings{
			ExcludeAuthors: []string{"octocat"}, // the same login, differently cased
			ReviewBots:     []string{"Renovate[bot]", "dependabot"},
			ContextRepos:   []string{"acme/api"},
		}},
		{LevelRepo, Settings{
			IgnorePaths:  []string{"dist/**", "**/*.snap", " "},
			Instructions: []string{"Prefer early returns.", "Money is integer cents."},
			ContextRepos: []string{"ACME/api", "acme/shared"},
		}},
	}
	e := Resolve(chain)
	checks := []struct {
		field string
		got   []string
		want  []string
		from  Level
	}{
		{"exclude_authors", e.ExcludeAuthors, []string{"release-bot", "Octocat"}, LevelConnection},
		{"review_bots", e.ReviewBots, []string{"renovate[bot]", "dependabot"}, LevelGroup},
		{"ignore_paths", e.IgnorePaths, []string{"**/*.snap", "dist/**"}, LevelRepo},
		{"instructions", e.Instructions, []string{"Money is integer cents.", "Prefer early returns."}, LevelRepo},
		{"context_repos", e.ContextRepos, []string{"acme/api", "acme/shared"}, LevelRepo},
	}
	for _, c := range checks {
		if !slices.Equal(c.got, c.want) || e.Source[c.field] != c.from {
			t.Errorf("%s = %q from %s, want %q from %s", c.field, c.got, e.Source[c.field], c.want, c.from)
		}
	}
}

func TestResolveTakesTheNearestWholeBranchRuleList(t *testing.T) {
	connRules := []BranchRule{{Base: "main", Types: []string{"release"}}, {}}
	repoRules := []BranchRule{{Base: "testing", Types: []string{"security"}}, {}}
	e := Resolve([]LevelSettings{
		{LevelConnection, Settings{BranchRules: connRules}},
		{LevelRepo, Settings{BranchRules: repoRules}},
	})
	if len(e.BranchRules) != 2 || e.BranchRules[0].Base != "testing" || e.Source["branch_rules"] != LevelRepo {
		t.Errorf("the repository's rules should replace the connection's, not merge: %+v from %s", e.BranchRules, e.Source["branch_rules"])
	}
	e.BranchRules[0].Types[0] = "changed"
	if repoRules[0].Types[0] != "security" {
		t.Error("the effective rules share memory with the stored ones")
	}

	inherit := Resolve([]LevelSettings{
		{LevelConnection, Settings{BranchRules: connRules}},
		{LevelRepo, Settings{BranchRules: []BranchRule{}}},
	})
	if inherit.BranchRules[0].Base != "main" || inherit.Source["branch_rules"] != LevelConnection {
		t.Error("an empty list at the repository inherits the connection's rules")
	}
}

func TestSettingsJSONLeavesUnsetFieldsOut(t *testing.T) {
	var s Settings
	if err := json.Unmarshal([]byte(`{"mode":"live","drafts":false,"strictness":null,"max_comments":3}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Mode == nil || *s.Mode != ModeLive {
		t.Error("mode should be set")
	}
	if s.Drafts == nil || *s.Drafts {
		t.Error("drafts:false is a value, not an absence")
	}
	if s.Strictness != nil {
		t.Error("null is how the console resets a field to inherit")
	}
	raw, _ := json.Marshal(s)
	if string(raw) != `{"mode":"live","drafts":false,"max_comments":3}` {
		t.Errorf("round trip = %s", raw)
	}
	raw, _ = json.Marshal(Settings{})
	if string(raw) != `{}` {
		t.Errorf("nothing set should be {}, got %s", raw)
	}
}

func TestSettingsValidate(t *testing.T) {
	if err := (Settings{}).Validate(); err != nil {
		t.Fatalf("nothing set is valid: %v", err)
	}
	if err := Defaults().Validate(); err != nil {
		t.Fatalf("the defaults must pass their own validation: %v", err)
	}
	full := Settings{
		Mode: ptr(ModeLive), Trigger: ptr(TriggerPush), Drafts: ptr(true), Forks: ptr(ForksOff),
		Strictness: ptr(StrictnessHigh), MaxComments: ptr(20), CommentHeader: ptr("From the review bot"),
		Model: ptr("z-ai/glm-5.3"), MaxUSD: ptr(0.10),
		Instructions:   []string{"Money is integer cents."},
		ExcludeAuthors: []string{"octocat", "renovate[bot]", "*-bot"},
		ReviewBots:     []string{"dependabot", "renovate[bot]", "*"},
		IgnorePaths:    []string{"**/*.snap", "/vendor/**"},
		ContextRepos:   []string{"acme/api", "octo-org/shared.lib"},
		BranchRules:    sampleRules,
	}
	if err := full.Validate(); err != nil {
		t.Fatalf("every field set to something sensible: %v", err)
	}

	long := strings.Repeat("x", MaxEntryLen+1)
	many := make([]string, MaxListEntries+1)
	for i := range many {
		many[i] = "p" + strings.Repeat("x", i)
	}
	cases := []struct {
		name string
		s    Settings
		want string
	}{
		{"an unknown mode", Settings{Mode: ptr(Mode("on"))}, `mode "on"`},
		{"an unknown trigger", Settings{Trigger: ptr(Trigger("always"))}, `trigger "always"`},
		{"forks reviewed automatically", Settings{Forks: ptr(Forks("auto"))}, `forks "auto": want command or off`},
		{"an unknown strictness", Settings{Strictness: ptr(Strictness("max"))}, `strictness "max"`},
		{"no comments at all", Settings{MaxComments: ptr(0)}, "max_comments must be between 1 and 20"},
		{"two hundred comments", Settings{MaxComments: ptr(200)}, "max_comments"},
		{"a review for a cent", Settings{MaxUSD: ptr(0.01)}, "max_usd must be between $0.10 and $5.00"},
		{"a fifty dollar review", Settings{MaxUSD: ptr(50.0)}, "max_usd"},
		{"not a number", Settings{MaxUSD: ptr(math.NaN())}, "max_usd"},
		{"infinite money", Settings{MaxUSD: ptr(math.Inf(1))}, "max_usd"},
		{"an empty model is not inherit", Settings{Model: ptr("")}, `model ""`},
		{"a header too long", Settings{CommentHeader: ptr(strings.Repeat("h", MaxHeaderLen+1))}, "comment_header"},
		{"an empty ignore entry would ignore every file", Settings{IgnorePaths: []string{"src/**", ""}}, "ignore_paths: an entry is empty"},
		{"a bare slash", Settings{IgnorePaths: []string{"/"}}, `ignore_paths: "/" is not a path glob`},
		{"an entry too long", Settings{Instructions: []string{long}}, "instructions:"},
		{"too many entries", Settings{IgnorePaths: many}, "at most 50 entries"},
		{"not a login", Settings{ExcludeAuthors: []string{"not a login"}}, "exclude_authors"},
		{"a suffix with no bot", Settings{ReviewBots: []string{"[bot]"}}, `review_bots: "[bot]" is not a GitHub login`},
		{"not a repository", Settings{ContextRepos: []string{"api"}}, "context_repos: \"api\" is not owner/name"},
		{"a dot repository", Settings{ContextRepos: []string{"acme/.."}}, "context_repos"},
		{"a bad branch rule", Settings{BranchRules: []BranchRule{{}, {Base: "main"}}}, "branch_rules: branch rule 1"},
	}
	for _, c := range cases {
		err := c.s.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
		}
	}
}

func TestWithRuleAppliesTheMatchedRulesOverrides(t *testing.T) {
	rule := BranchRule{Trigger: TriggerCommand, Strictness: StrictnessHigh, Post: ModeLive, Model: "flash"}
	// The rule is written at the same level as the mode it overrides: beside it, it is the more
	// specific of the two.
	e := Resolve([]LevelSettings{{LevelRepo, Settings{Mode: ptr(ModeShadow), ExcludeAuthors: []string{"octocat"},
		BranchRules: []BranchRule{rule}}}})
	got := e.WithRule(rule)
	if got.Trigger != TriggerCommand || got.Strictness != StrictnessHigh || got.Mode != ModeLive || got.Model != "flash" {
		t.Errorf("overrides not applied: %+v", got)
	}
	for _, f := range []string{"trigger", "strictness", "mode", "model"} {
		if got.Source[f] != LevelRule {
			t.Errorf("source[%s] = %s, want rule", f, got.Source[f])
		}
	}
	if e.Source["mode"] != LevelRepo || e.Mode != ModeShadow {
		t.Error("WithRule changed the settings it was called on")
	}
	got.ExcludeAuthors[0] = "changed"
	if e.ExcludeAuthors[0] != "octocat" {
		t.Error("WithRule's copy shares a list with the original")
	}

	if same := e.WithRule(BranchRule{}); same.Hash() != e.Hash() {
		t.Error("a rule with no overrides changes nothing")
	}
	live := BranchRule{Post: ModeLive}
	off := Resolve([]LevelSettings{{LevelRepo, Settings{Mode: ptr(ModeOff), BranchRules: []BranchRule{live}}}})
	if got := off.WithRule(live); got.Mode != ModeOff {
		t.Error("a branch rule must not turn on a repository whose reviews are off")
	}
}

// Rules are inherited as a whole list, so they usually come from the connection; a value set
// nearer the repository must still win over a rule written further up.
func TestWithRuleDoesNotOverruleANearerLevel(t *testing.T) {
	connRule := BranchRule{Base: "main", Post: ModeLive, Trigger: TriggerPush, Model: "flash", Strictness: StrictnessLow}
	e := Resolve([]LevelSettings{
		{LevelConnection, Settings{BranchRules: []BranchRule{connRule, {}}}},
		{LevelRepo, Settings{Mode: ptr(ModeShadow), Trigger: ptr(TriggerCommand), Model: ptr("heavy")}},
	})
	got := e.WithRule(connRule)
	if got.Mode != ModeShadow || got.Source["mode"] != LevelRepo {
		t.Errorf("a connection's rule posted live on a repository set to shadow: %s from %s", got.Mode, got.Source["mode"])
	}
	if got.Trigger != TriggerCommand || got.Model != "heavy" {
		t.Errorf("a connection's rule overruled the repository's trigger or model: %s, %s", got.Trigger, got.Model)
	}
	// Strictness was set nowhere but the defaults, so the connection's rule is the nearer choice.
	if got.Strictness != StrictnessLow || got.Source["strictness"] != LevelRule {
		t.Errorf("a rule should override what only the defaults set: %s from %s", got.Strictness, got.Source["strictness"])
	}

	// The same rule with the mode set at the connection, beside it: the rule is the narrower
	// choice there, and wins.
	e = Resolve([]LevelSettings{{LevelConnection, Settings{Mode: ptr(ModeShadow), BranchRules: []BranchRule{connRule, {}}}}})
	if got := e.WithRule(connRule); got.Mode != ModeLive {
		t.Errorf("a rule beside the mode should apply, got %s", got.Mode)
	}
	// A repository's own rule overrides a mode set further up.
	repoRule := BranchRule{Post: ModeLive}
	e = Resolve([]LevelSettings{
		{LevelConnection, Settings{Mode: ptr(ModeShadow)}},
		{LevelGroup, Settings{Trigger: ptr(TriggerCommand)}},
		{LevelRepo, Settings{BranchRules: []BranchRule{repoRule}}},
	})
	if got := e.WithRule(repoRule); got.Mode != ModeLive {
		t.Errorf("a repository's rule should override the connection's mode, got %s", got.Mode)
	}
}

func TestEffectiveHash(t *testing.T) {
	a := Resolve(nil)
	if a.Hash() != Resolve(nil).Hash() || len(a.Hash()) != 64 {
		t.Error("the same settings should hash the same")
	}
	b := Resolve([]LevelSettings{{LevelRepo, Settings{Strictness: ptr(StrictnessHigh)}}})
	if a.Hash() == b.Hash() {
		t.Error("a different strictness should hash differently")
	}
	// The same value from a different level runs the same review.
	c := Resolve([]LevelSettings{{LevelConnection, Settings{Strictness: ptr(StrictnessHigh)}}})
	if b.Hash() != c.Hash() {
		t.Error("where a value came from should not change the hash")
	}
	if a.Source == nil {
		t.Error("Hash must not clear the caller's Source")
	}
}

// The hash is what a cached review is found by (two runs on one head with the same hash share the
// first one's result), so a change to what is hashed makes every organisation's next request on every
// reviewed head pay for that review again. These are the hashes of the built-in settings and of a
// rule list as they were before the channel existed; a field added to Effective or BranchRule must
// keep them — left out of the JSON while it is zero — or say here why every cache is to go.
//
// The built-in one moved once on purpose, when the built-in fallback rule began running Security
// beside General: a pull request under the built-in rule is reviewed by another set of types from
// then on, which no cached review answers anyway, since the types are in the cache key too.
func TestEffectiveHashIsPinned(t *testing.T) {
	if got := Resolve(nil).Hash(); got != "08baa1b195b95aa6aaeccb3b6fd7d3a4906a7842cdb8e0cc875665ea112ae84a" {
		t.Errorf("the built-in settings hash %s: every cached review would be missed", got)
	}
	var s Settings
	if err := json.Unmarshal([]byte(`{"mode":"live","branch_rules":[{"base":"main","types":["security"],"post":"shadow"},`+
		`{"head":"dependabot/**","strictness":"low"},{}]}`), &s); err != nil {
		t.Fatal(err)
	}
	const rules = "010f80d48894fcdabde4635682fe98ad5436fc99c5a3ab8663fafe0bc5a83d5c"
	if got := Resolve([]LevelSettings{{LevelConnection, s}}).Hash(); got != rules {
		t.Errorf("a rule list hashes %s: every cached review would be missed", got)
	}
	// A channel, the settings' or a rule's, is not part of what runs: the same hash with one.
	s.Notify = &NotifyChannel{Team: "T1", Channel: "C1"}
	s.BranchRules[1].Notify = &NotifyChannel{}
	if got := Resolve([]LevelSettings{{LevelConnection, s}}).Hash(); got != rules {
		t.Errorf("a rule list with channels hashes %s, want the same as without", got)
	}
	// The automatic context repositories are off unless turned on: saying so changes nothing, and
	// turning them on changes what a review reads, so it is another review.
	s.ContextReposAuto = ptr(false)
	if got := Resolve([]LevelSettings{{LevelConnection, s}}).Hash(); got != rules {
		t.Errorf("context_repos_auto set to its default hashes %s, want the same as unset", got)
	}
	s.ContextReposAuto = ptr(true)
	if got := Resolve([]LevelSettings{{LevelConnection, s}}).Hash(); got == rules {
		t.Error("context_repos_auto turned on hashes the same as off; a review that reads more would be answered from one that read less")
	}
}

func TestChangedFields(t *testing.T) {
	before := Settings{Mode: ptr(ModeShadow), Model: ptr("heavy"), IgnorePaths: []string{"dist/**"}}
	if got := ChangedFields(before, before); len(got) != 0 {
		t.Errorf("saving the same values changes nothing, got %v", got)
	}
	// An editor re-saving a form that shows the admin's model must not be refused for it.
	same := Settings{Mode: ptr(ModeShadow), Model: ptr("heavy"), IgnorePaths: []string{"dist/**"}}
	if got := ChangedFields(before, same); len(got) != 0 {
		t.Errorf("equal values in fresh pointers are not a change, got %v", got)
	}
	after := Settings{Mode: ptr(ModeLive), IgnorePaths: []string{"dist/**", "**/*.snap"}, MaxComments: ptr(5)}
	want := []string{"ignore_paths", "max_comments", "mode", "model"}
	if got := ChangedFields(before, after); !slices.Equal(got, want) {
		t.Errorf("ChangedFields = %v, want %v", got, want)
	}
	if got := ChangedFields(Settings{}, Settings{Drafts: ptr(false)}); !slices.Equal(got, []string{"drafts"}) {
		t.Errorf("setting a field to false is a change from inheriting, got %v", got)
	}
	nan := Settings{MaxUSD: ptr(math.NaN())}
	if got := ChangedFields(nan, nan); len(got) != len(SettingFields()) {
		t.Errorf("settings that cannot be compared should report every field, got %v", got)
	}
}

func TestEffectiveMatchesAuthorsAndPaths(t *testing.T) {
	e := Resolve([]LevelSettings{{LevelRepo, Settings{
		ExcludeAuthors: []string{"Octocat", "*-bot"},
		IgnorePaths:    []string{"**/*.snap", "dist/**"},
	}}})
	for login, want := range map[string]bool{"octocat": true, "OCTOCAT": true, "release-bot": true, "octocats": false, "": false} {
		if e.ExcludesAuthor(login) != want {
			t.Errorf("ExcludesAuthor(%q) = %v, want %v", login, !want, want)
		}
	}
	for path, want := range map[string]bool{"web/__snapshots__/a.snap": true, "dist/app.js": true, "src/app.js": false} {
		if e.IgnoresPath(path) != want {
			t.Errorf("IgnoresPath(%q) = %v, want %v", path, !want, want)
		}
	}
}

// A bot is let through by its login with or without "[bot]", in any case, or by a glob of it; "*" lets
// every bot through, and none listed lets none. Which bots are let through changes no review's hash.
func TestEffectiveReviewsOnlyTheBotsListed(t *testing.T) {
	e := Resolve([]LevelSettings{{LevelConnection, Settings{ReviewBots: []string{"dependabot", "Renovate[bot]", "github-*"}}}})
	for login, want := range map[string]bool{
		"dependabot[bot]": true, "DEPENDABOT[bot]": true, "dependabot": true, "renovate[bot]": true, "renovate": true,
		"github-actions[bot]": true, "snyk-bot": false, "dependabot-preview[bot]": false, "": false,
	} {
		if e.ReviewsBot(login) != want {
			t.Errorf("ReviewsBot(%q) = %v, want %v", login, !want, want)
		}
	}
	if every := Resolve([]LevelSettings{{LevelRepo, Settings{ReviewBots: []string{"*"}}}}); !every.ReviewsBot("copilot-swe-agent[bot]") {
		t.Error(`"*" did not let a bot through`)
	}
	if Resolve(nil).ReviewsBot("dependabot[bot]") {
		t.Error("a bot was let through with none listed")
	}
	if e.Hash() != Resolve(nil).Hash() {
		t.Error("the bots let through changed the hash: every cached review would be missed")
	}
}

// The channel a pull request is announced in inherits like any single value — nearest level that
// sets one, an empty one turning off what is set above — and a matched rule's channel takes over
// where its list is at least as near as the channel it replaces, an empty one silencing the pull
// requests it matches. None of it is part of what a review runs under.
func TestNotifyInheritsAndARuleOverridesIt(t *testing.T) {
	team, frontend, releases := "T1", NotifyChannel{Team: "T1", Channel: "C1"}, NotifyChannel{Team: "T1", Channel: "C2"}
	e := Resolve([]LevelSettings{
		{LevelConnection, Settings{Notify: &frontend}},
		{LevelRepo, Settings{Strictness: ptr(StrictnessHigh)}},
	})
	if e.Notify != frontend || e.Source["notify"] != LevelConnection {
		t.Errorf("notify = %+v from %s, want the connection's", e.Notify, e.Source["notify"])
	}
	off := Resolve([]LevelSettings{{LevelConnection, Settings{Notify: &frontend}}, {LevelGroup, Settings{Notify: &NotifyChannel{}}}})
	if off.Notify.Set() || off.Source["notify"] != LevelGroup {
		t.Errorf("an empty channel below should turn the connection's off: %+v from %s", off.Notify, off.Source["notify"])
	}
	if d := Resolve(nil); d.Notify.Set() || d.Source["notify"] != LevelDefault {
		t.Errorf("nothing set announces nowhere, got %+v", d.Notify)
	}

	rule := BranchRule{Base: "main", Notify: &releases}
	quiet := BranchRule{Head: "dependabot/**", Notify: &NotifyChannel{}}
	e = Resolve([]LevelSettings{{LevelConnection, Settings{Notify: &frontend, BranchRules: []BranchRule{rule, quiet, {}}}}})
	if got := e.WithRule(rule); got.Notify != releases || got.Source["notify"] != LevelRule {
		t.Errorf("a rule beside the channel should send its pull requests elsewhere: %+v from %s", got.Notify, got.Source["notify"])
	}
	if got := e.WithRule(quiet); got.Notify.Set() {
		t.Errorf("an empty rule channel should silence its pull requests, got %+v", got.Notify)
	}
	if got := e.WithRule(BranchRule{}); got.Notify != frontend {
		t.Errorf("a rule naming no channel inherits the settings', got %+v", got.Notify)
	}
	// A repository pointed at its own channel is not moved by a connection-wide rule.
	near := Resolve([]LevelSettings{
		{LevelConnection, Settings{BranchRules: []BranchRule{rule, {}}}},
		{LevelRepo, Settings{Notify: &NotifyChannel{Team: team, Channel: "C9"}}},
	})
	if got := near.WithRule(rule); got.Notify.Channel != "C9" || got.Source["notify"] != LevelRepo {
		t.Errorf("a connection's rule overruled the repository's channel: %+v from %s", got.Notify, got.Source["notify"])
	}

	// Pointing the announcements elsewhere changes no review: not the hash a cached review is found by.
	plain := Resolve([]LevelSettings{{LevelConnection, Settings{BranchRules: []BranchRule{{Base: "main"}, {}}}}})
	told := Resolve([]LevelSettings{{LevelConnection, Settings{Notify: &frontend, BranchRules: []BranchRule{{Base: "main", Notify: &releases}, {}}}}})
	if plain.Hash() != told.Hash() || plain.WithRule(plain.BranchRules[0]).Hash() != told.WithRule(told.BranchRules[0]).Hash() {
		t.Error("the channel told about a review should not change the review's hash")
	}
	if told.BranchRules[0].Notify == nil || *told.BranchRules[0].Notify != releases {
		t.Error("Hash must not clear the caller's rules")
	}
	if got := ChangedFields(Settings{}, Settings{Notify: &frontend}); !slices.Equal(got, []string{"notify"}) {
		t.Errorf("setting a channel is a change of notify, got %v", got)
	}
	if got := ChangedFields(Settings{Notify: &frontend}, Settings{Notify: &NotifyChannel{}}); !slices.Equal(got, []string{"notify"}) {
		t.Errorf("turning a channel off is a change of notify, got %v", got)
	}
}

func TestNotifyChannelShape(t *testing.T) {
	for _, n := range []NotifyChannel{{}, {Team: "T1", Channel: "C1"}, {Channel: "C1"},
		{Team: "msteams:0f1e", Channel: "19:abc@thread.tacv2"}} {
		if err := (Settings{Notify: &n}).Validate(); err != nil {
			t.Errorf("%+v: %v", n, err)
		}
	}
	for _, n := range []NotifyChannel{{Team: "T1"}, {Team: "T1", Channel: "C 1"}, {Channel: strings.Repeat("C", 201)},
		{Team: "T1\n", Channel: "C1"}} {
		if err := (Settings{Notify: &n}).Validate(); err == nil || !strings.Contains(err.Error(), "notify") {
			t.Errorf("%+v: err = %v, want a notify refusal", n, err)
		}
		rules := []BranchRule{{Base: "main", Notify: &n}, {}}
		if err := (Settings{BranchRules: rules}).Validate(); err == nil || !strings.Contains(err.Error(), "notify") {
			t.Errorf("a rule's %+v: err = %v, want a notify refusal", n, err)
		}
	}
}

// Which events a channel hears of is a set taken whole from the nearest level that sets one, never
// added up — a repository may hear less than its connection — with every event when nothing sets
// it, and an empty set telling it nothing. It is not part of what a review runs under, and only the
// four events, each once, are taken.
func TestNotifyOnInheritsWholeAndIsChecked(t *testing.T) {
	if d := Resolve(nil); !slices.Equal(d.NotifyOn, NotifyEvents()) || d.Source["notify_on"] != LevelDefault {
		t.Errorf("nothing set tells %v from %s, want every event by default", d.NotifyOn, d.Source["notify_on"])
	}
	results := []NotifyEvent{NotifyFinished, NotifyMerged}
	e := Resolve([]LevelSettings{
		{LevelConnection, Settings{NotifyOn: &results}},
		{LevelRepo, Settings{NotifyOn: &[]NotifyEvent{NotifyFailed}}},
	})
	if !slices.Equal(e.NotifyOn, []NotifyEvent{NotifyFailed}) || e.Source["notify_on"] != LevelRepo {
		t.Errorf("notify_on = %v from %s, want the repository's alone", e.NotifyOn, e.Source["notify_on"])
	}
	if e.Notifies(NotifyFinished) || !e.Notifies(NotifyFailed) {
		t.Errorf("Notifies reads %v wrong", e.NotifyOn)
	}
	none := Resolve([]LevelSettings{{LevelGroup, Settings{NotifyOn: &[]NotifyEvent{}}}})
	if none.NotifyOn == nil || slices.ContainsFunc(NotifyEvents(), none.Notifies) {
		t.Errorf("an empty set should tell nothing, and stay a set: %#v", none.NotifyOn)
	}
	if raw, _ := json.Marshal(none); !strings.Contains(string(raw), `"notify_on":[]`) {
		t.Errorf("an empty set must reach the console as one, not as absent (every event): %s", raw)
	}
	if !(Effective{}).Notifies(NotifyStarted) {
		t.Error("settings nothing resolved should tell every event")
	}
	if w := e.WithRule(BranchRule{Base: "main"}); !slices.Equal(w.NotifyOn, e.NotifyOn) {
		t.Errorf("a rule changed notify_on to %v", w.NotifyOn)
	}
	if Resolve(nil).Hash() != none.Hash() {
		t.Error("what the channel is told should not change the review's hash")
	}
	if got := ChangedFields(Settings{}, Settings{NotifyOn: &results}); !slices.Equal(got, []string{"notify_on"}) {
		t.Errorf("setting the events is a change of notify_on, got %v", got)
	}
	for _, bad := range [][]NotifyEvent{{"posted"}, {NotifyFailed, NotifyFailed}, {""},
		{NotifyStarted, NotifyFinished, NotifyFailed, NotifyMerged, NotifyStarted}} {
		if err := (Settings{NotifyOn: &bad}).Validate(); err == nil || !strings.Contains(err.Error(), "notify_on") {
			t.Errorf("%v: err = %v, want a notify_on refusal", bad, err)
		}
	}
	for _, ok := range [][]NotifyEvent{{}, NotifyEvents(), {NotifyMerged, NotifyStarted}} {
		if err := (Settings{NotifyOn: &ok}).Validate(); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
}
