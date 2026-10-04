package review

import (
	"slices"
	"strings"
	"testing"
)

// The branching model the feature was designed around: work goes into testing, testing goes
// into main, and bots and hotfixes are special.
var sampleRules = []BranchRule{
	{Base: "main", Head: "testing", Types: []string{"release", "security"}},
	{Base: "testing", Types: []string{"general", "security", "tests"}},
	{Head: "dependabot/**", Trigger: TriggerCommand},
	{Base: "release/*", Head: "hotfix/*", Strictness: StrictnessHigh, Post: ModeLive},
	{Types: []string{"general"}},
}

func TestMatchRuleTakesTheFirstRuleWhoseBranchesBothMatch(t *testing.T) {
	cases := []struct {
		base, head string
		index      int
		types      []string
	}{
		{"main", "testing", 0, []string{"release", "security"}},
		{"testing", "feature/login", 1, []string{"general", "security", "tests"}},
		{"testing", "dependabot/npm_and_yarn/x", 1, []string{"general", "security", "tests"}}, // order wins
		{"main", "dependabot/go_modules/y", 2, []string{DefaultType}},                         // no types: the default
		{"release/2.1", "hotfix/crash", 3, []string{DefaultType}},
		{"release/2.1", "feature/x", 4, []string{"general"}},
		{"main", "feature/x", 4, []string{"general"}},
	}
	for _, c := range cases {
		i, r, ok := MatchRule(sampleRules, c.base, c.head)
		if !ok || i != c.index || !slices.Equal(r.Types, c.types) {
			t.Errorf("%s → %s: got rule %d %v (ok=%v), want rule %d %v", c.head, c.base, i, r.Types, ok, c.index, c.types)
		}
	}
}

func TestMatchRuleWithoutAFallback(t *testing.T) {
	i, r, ok := MatchRule(sampleRules[:2], "main", "feature/x")
	if ok || i != -1 || r.Types != nil {
		t.Errorf("nothing matches, got %d %+v %v", i, r, ok)
	}
	if _, _, ok := MatchRule(nil, "main", "x"); ok {
		t.Error("no rules match nothing")
	}
}

func TestMatchRuleReturnsACopy(t *testing.T) {
	rules := []BranchRule{{Types: []string{"general"}, Notify: &NotifyChannel{Channel: "C1"}}}
	_, r, _ := MatchRule(rules, "main", "x")
	r.Types[0], r.Notify.Channel = "security", "C2"
	if rules[0].Types[0] != "general" || rules[0].Notify.Channel != "C1" {
		t.Error("changing the matched rule changed the settings it came from")
	}
	empty := []BranchRule{{}}
	_, r, _ = MatchRule(empty, "main", "x")
	if empty[0].Types != nil || len(r.Types) != 1 {
		t.Error("defaulting the types must not write into the settings")
	}
}

func TestBranchRuleString(t *testing.T) {
	for _, c := range []struct {
		r    BranchRule
		want string
	}{
		{BranchRule{Base: "testing"}, "any → testing"},
		{BranchRule{Base: "main", Head: "testing"}, "testing → main"},
		{BranchRule{Head: "hotfix/*"}, "hotfix/* → any"},
		{BranchRule{}, "any → any"},
		{BranchRule{Labels: []string{"perf"}, Types: []string{"performance"}}, "label:perf"},
		{BranchRule{Base: "main", Labels: []string{"perf", "slow"}, Types: []string{"performance"}}, "label:perf|slow on any → main"},
	} {
		if got := c.r.String(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.r, got, c.want)
		}
	}
}

func TestValidateRules(t *testing.T) {
	if err := ValidateRules(sampleRules); err != nil {
		t.Fatalf("the sample rules are valid: %v", err)
	}
	if err := ValidateRules(nil); err != nil {
		t.Fatalf("no rules is valid (the level inherits): %v", err)
	}
	tooMany := make([]BranchRule, MaxBranchRules+1)
	for i := range tooMany[:MaxBranchRules] {
		tooMany[i] = BranchRule{Base: "b" + strings.Repeat("x", i)}
	}
	// Each case ends in the fallback unless the case is about the fallback, so it fails for its
	// own reason only.
	fb := func(rules ...BranchRule) []BranchRule { return append(rules, BranchRule{}) }
	cases := []struct {
		name  string
		rules []BranchRule
		want  string
	}{
		{"a fallback above other rules", []BranchRule{{}, {Base: "main"}}, "matches every branch, so the 1 after it can never run"},
		{"two fallbacks", []BranchRule{{Base: "main"}, {}, {}}, "rule 2 matches every branch"},
		{"no fallback at all", []BranchRule{{Base: "main"}}, `the last branch rule must be "any → any"`},
		{"a fallback that is not last", []BranchRule{{Base: "testing"}, {}, {Base: "main"}}, `the last branch rule must be "any → any"`},
		{"too many rules", tooMany, "at most 20 branch rules"},
		{"a type that is not a key", fb(BranchRule{Base: "main", Types: []string{"Security"}}), `type "Security" is not a review type key`},
		{"a type twice", fb(BranchRule{Base: "main", Types: []string{"security", "security"}}), "listed twice"},
		{"too many types", fb(BranchRule{Base: "main", Types: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}}), "at most 10 types"},
		{"a branch with a space", fb(BranchRule{Base: "release 1"}), "cannot contain spaces"},
		{"an unknown trigger", fb(BranchRule{Base: "main", Trigger: "merge"}), `trigger "merge"`},
		{"an unknown strictness", fb(BranchRule{Base: "main", Strictness: "max"}), `strictness "max"`},
		{"a rule cannot turn reviews off", fb(BranchRule{Base: "main", Post: ModeOff}), `post "off": want shadow or live`},
		{"a model name with a space", fb(BranchRule{Base: "main", Model: "glm 5"}), "is not a model name"},
		{"an empty label", fb(BranchRule{Base: "main", Labels: []string{" "}}), "label"},
		{"a label of 51 characters", fb(BranchRule{Labels: []string{strings.Repeat("é", 51)}, Types: []string{"performance"}}), "must be 1 to 50 characters"},
		{"a label on two lines", fb(BranchRule{Labels: []string{"perf\nx"}, Types: []string{"performance"}}), "must be one line"},
		{"a label twice, in another case", fb(BranchRule{Labels: []string{"perf", "Perf"}, Types: []string{"performance"}}), `label "Perf" is listed twice`},
		{"a label rule that adds nothing", fb(BranchRule{Labels: []string{"perf"}}), "name at least one"},
		{"a label rule that posts live", fb(BranchRule{Labels: []string{"perf"}, Types: []string{"performance"}, Post: ModeLive}), "only adds review types"},
		{"a label rule with a model", fb(BranchRule{Labels: []string{"perf"}, Types: []string{"performance"}, Model: "heavy"}), "only adds review types"},
		{"a label rule with a channel", fb(BranchRule{Labels: []string{"perf"}, Types: []string{"performance"}, Notify: &NotifyChannel{}}), "only adds review types"},
		{"a list ending in a label rule", []BranchRule{{Labels: []string{"perf"}, Types: []string{"performance"}}}, `the last branch rule must be "any → any"`},
	}
	for _, c := range cases {
		err := ValidateRules(c.rules)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
		}
	}
	// Fifty characters, as GitHub counts a label's: eighteen Japanese ones are 54 bytes, and allowed.
	if err := ValidateRules(fb(BranchRule{Labels: []string{"セキュリティレビューが必要です。至急", strings.Repeat("é", 50)},
		Types: []string{"security"}})); err != nil {
		t.Errorf("labels of at most fifty characters, more bytes: %v", err)
	}
}

func TestValidTypeKey(t *testing.T) {
	for k, want := range map[string]bool{
		"general": true, "security": true, "api-contract": true, "a11y": true, "x": true,
		"": false, "Security": false, "api_contract": false, "-api": false, "api-": false,
		"api--contract": false, "1st": false, "has space": false, strings.Repeat("a", 41): false,
	} {
		if ValidTypeKey(k) != want {
			t.Errorf("ValidTypeKey(%q) = %v, want %v", k, !want, want)
		}
	}
}

// A label rule never chooses a pull request's types by itself, whatever its branches say: it is
// passed over for the branch rule below it, and with no branches it is not the fallback either.
func TestMatchRulePassesOverLabelRules(t *testing.T) {
	rules := []BranchRule{
		{Labels: []string{"security-review"}, Types: []string{"security"}},
		{Base: "main", Labels: []string{"perf"}, Types: []string{"performance"}},
		{Base: "main", Types: []string{"release"}},
		{},
	}
	if err := ValidateRules(rules); err != nil {
		t.Fatalf("label rules above the fallback are valid: %v", err)
	}
	if i, r, ok := MatchRule(rules, "main", "feature"); !ok || i != 2 || !slices.Equal(r.Types, []string{"release"}) {
		t.Errorf("into main: rule %d %v, want the branch rule 2", i, r.Types)
	}
	if i, _, ok := MatchRule(rules, "testing", "feature"); !ok || i != 3 {
		t.Errorf("into testing: rule %d, want the fallback", i)
	}
	if (BranchRule{Labels: []string{"perf"}}).Fallback() {
		t.Error("a label rule with no branches reads as the fallback")
	}
}

// Every label rule whose label the pull request carries adds its types, in list order, after the
// branch rule's and never twice; one that would add nothing new is not counted as matched.
func TestLabelTypesAddToTheBranchRule(t *testing.T) {
	rules := []BranchRule{
		{Labels: []string{"security-review"}, Types: []string{"security"}},
		{Labels: []string{"perf", "slow"}, Types: []string{"performance", "tests"}},
		{Base: "main", Labels: []string{"perf"}, Types: []string{"release"}},
		{Head: "hotfix/*", Labels: []string{"perf"}, Types: []string{"general"}},
		{},
	}
	cases := []struct {
		name               string
		base, head         string
		labels, have       []string
		wantAdd, wantLabel []string
	}{
		{"no labels", "main", "feature", nil, []string{"general"}, nil, nil},
		{"a label nobody named", "main", "feature", []string{"wip"}, []string{"general"}, nil, nil},
		{"one label, in another case", "testing", "feature", []string{"Security-Review"}, []string{"general"}, []string{"security"},
			[]string{"Security-Review"}},
		{"two labels, list order", "testing", "feature", []string{"slow", "security-review"}, []string{"general"},
			[]string{"security", "performance", "tests"}, []string{"security-review", "slow"}},
		{"a rule narrowed to main", "main", "feature", []string{"perf"}, []string{"general"},
			[]string{"performance", "tests", "release"}, []string{"perf"}},
		{"a type the branch rule already runs", "testing", "feature", []string{"security-review"}, []string{"general", "security"}, nil, nil},
		{"a head glob that does not match", "testing", "feature", []string{"perf"}, []string{"tests"}, []string{"performance"}, []string{"perf"}},
	}
	for _, c := range cases {
		add, labels := LabelTypes(rules, c.base, c.head, c.labels, c.have)
		if !slices.Equal(add, c.wantAdd) || !slices.Equal(labels, c.wantLabel) {
			t.Errorf("%s: added %v by %v, want %v by %v", c.name, add, labels, c.wantAdd, c.wantLabel)
		}
	}
	have := []string{"general"}
	LabelTypes(rules, "main", "x", []string{"perf"}, have)
	if !slices.Equal(have, []string{"general"}) {
		t.Errorf("the branch rule's types were changed: %v", have)
	}
}
