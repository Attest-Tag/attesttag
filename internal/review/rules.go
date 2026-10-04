package review

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DefaultType is the review type that runs when nothing says otherwise: logic bugs, broken
// contracts, concurrency, data loss, missing tests for new logic.
const DefaultType = "general"

// BranchRule chooses what kind of review a pull request gets from where it comes from and
// where it goes. A list of them is ordered and the first match wins, so a team can say "into
// testing: General, Security and Tests; testing into main: Release summary and Security" and
// have the narrow rule sit above the broad one.
type BranchRule struct {
	// Base and Head are globs (see Match) on the branch the pull request merges into and the
	// one it comes from. Empty means any branch, and a rule with both empty is the fallback,
	// which belongs last: nothing after it can ever match.
	Base string `json:"base,omitempty"`
	Head string `json:"head,omitempty"`
	// Labels make the rule a label rule. It chooses nothing by itself: a pull request carrying
	// any of these labels — compared without regard to case, as GitHub compares label names —
	// gets the rule's Types on top of the ones its branch rule chose, so "security-review" can add
	// Security to whatever a branch already runs without every branch rule listing it twice.
	// Base and Head, when set, narrow it to those branches. It overrides nothing (Validate holds
	// it to that): a label is put on by anybody with triage rights on the repository, and how a
	// review is published, how sure it must be and what it costs are the branch rule's to say.
	Labels []string `json:"labels,omitempty"`
	// Types are review type keys, run in this order. Empty means DefaultType, on a branch rule;
	// a label rule must name the ones it adds.
	Types []string `json:"types,omitempty"`
	// The rest override the settings the rule was resolved under, for pull requests it
	// matches; empty inherits. Post may only be shadow or live: a rule chooses how a review
	// is published, not whether reviews are on, which is the repository's mode.
	Trigger    Trigger    `json:"trigger,omitempty"`
	Strictness Strictness `json:"strictness,omitempty"`
	Post       Mode       `json:"post,omitempty"`
	Model      string     `json:"model,omitempty"`
	// Notify sends the announcements of the pull requests it matches to another channel, or, as
	// an empty object, to none: a release rule can tell #releases while the rest go to the team's
	// channel, and dependabot's can stay quiet. Nil inherits the settings' channel.
	Notify *NotifyChannel `json:"notify,omitempty"`
}

// Fallback reports whether r matches every pull request: a branch rule naming no branch.
func (r BranchRule) Fallback() bool { return r.Base == "" && r.Head == "" && len(r.Labels) == 0 }

// LabelRule reports whether r adds types for a label rather than choosing them for branches.
func (r BranchRule) LabelRule() bool { return len(r.Labels) > 0 }

// String names a rule the way the summary footer and the console show it, head first, as a
// pull request reads: "hotfix/* → main", "any → testing", "any → any". A label rule is named by
// its labels, and its branches when it has any: "label:perf", "label:perf on any → main".
func (r BranchRule) String() string {
	or := func(s string) string {
		if s == "" {
			return "any"
		}
		return s
	}
	branches := or(r.Head) + " → " + or(r.Base)
	if !r.LabelRule() {
		return branches
	}
	name := "label:" + strings.Join(r.Labels, "|")
	if r.Base != "" || r.Head != "" {
		name += " on " + branches
	}
	return name
}

// MatchRule returns the first branch rule whose base and head globs both match, with its position
// in the list so a run can record which rule chose its types. Label rules are passed over: they
// add to a branch rule's choice (LabelTypes) and never stand in for one. ok is false when none
// matches, which a list ending in a fallback never allows. The rule returned is a copy whose Types
// is never empty: a rule that names no type runs the default one, and the caller does not have to
// know that.
func MatchRule(rules []BranchRule, base, head string) (index int, rule BranchRule, ok bool) {
	for i, r := range rules {
		if r.LabelRule() || !Match(r.Base, base) || !Match(r.Head, head) {
			continue
		}
		r.Labels = append([]string(nil), r.Labels...)
		r.Types = append([]string(nil), r.Types...)
		if r.Notify != nil {
			n := *r.Notify
			r.Notify = &n
		}
		if len(r.Types) == 0 {
			r.Types = []string{DefaultType}
		}
		return i, r, true
	}
	return -1, BranchRule{}, false
}

// LabelTypes is what label rules add to a pull request's review on top of have, the types its
// branch rule chose: every label rule, in the list's order, whose branches match — an empty base
// or head is any — and which names a label the pull request carries adds those of its types that
// are not there yet. add is the types added, in order; matched is the labels that added any, as
// the pull request spells them, which is what a run records beside its rule ("+label:perf") and
// what makes a review with the label a different review from one without it. A label whose rule
// added nothing new is not matched: it changed nothing, and naming it would make a review asked for
// before the label was put on look different from the same review after.
func LabelTypes(rules []BranchRule, base, head string, labels, have []string) (add, matched []string) {
	if len(labels) == 0 {
		return nil, nil
	}
	seen := slices.Clone(have)
	for _, r := range rules {
		if !r.LabelRule() || !Match(r.Base, base) || !Match(r.Head, head) {
			continue
		}
		label := ""
		for _, want := range r.Labels {
			if i := slices.IndexFunc(labels, func(l string) bool { return strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(want)) }); i >= 0 {
				label = labels[i]
				break
			}
		}
		if label == "" {
			continue
		}
		added := false
		for _, t := range r.Types {
			if !slices.Contains(seen, t) {
				seen, add, added = append(seen, t), append(add, t), true
			}
		}
		if added && !slices.ContainsFunc(matched, func(m string) bool { return strings.EqualFold(m, label) }) {
			matched = append(matched, label)
		}
	}
	return add, matched
}

// typeKey is a review type's short name, the word a person types after "review": lowercase
// letters and digits in hyphen-separated parts, starting with a letter, e.g. "api-contract".
var typeKey = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

// MaxTypeKeyLen bounds a review type key; it has to fit in a comment line and a chip.
const MaxTypeKeyLen = 40

// ValidTypeKey reports whether s is shaped like a review type key. Whether such a type exists
// in an organisation is a question for its stored types, not for this.
func ValidTypeKey(s string) bool { return len(s) <= MaxTypeKeyLen && typeKey.MatchString(s) }

// Bounds on a rule list. Twenty rules is far beyond any branching model we have seen, and ten
// types on one rule is ten finder passes on every matching pull request, which the per-review
// money cap would cut short anyway.
const (
	MaxBranchRules  = 20
	maxRuleTypes    = 10
	maxRuleLabels   = 10
	maxBranchGlob   = 200
	maxLabelLen     = 50
	maxModelNameLen = 100
)

// Validate checks one rule's fields.
func (r BranchRule) Validate() error {
	var errs []string
	for _, g := range []struct{ name, glob string }{{"base", r.Base}, {"head", r.Head}} {
		if len(g.glob) > maxBranchGlob {
			errs = append(errs, fmt.Sprintf("%s is longer than %d characters", g.name, maxBranchGlob))
		}
		// git refuses spaces and control characters in a ref, so a glob with one can never
		// match and is a typo worth saying so about.
		if strings.ContainsFunc(g.glob, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			errs = append(errs, fmt.Sprintf("%s %q cannot contain spaces", g.name, clip(g.glob)))
		}
	}
	if len(r.Types) > maxRuleTypes {
		errs = append(errs, fmt.Sprintf("at most %d types, got %d", maxRuleTypes, len(r.Types)))
	}
	seen := map[string]bool{}
	for _, t := range r.Types {
		switch {
		case !ValidTypeKey(t):
			errs = append(errs, fmt.Sprintf("type %q is not a review type key", clip(t)))
		case seen[t]:
			errs = append(errs, fmt.Sprintf("type %q is listed twice", t))
		}
		seen[t] = true
	}
	if len(r.Labels) > maxRuleLabels {
		errs = append(errs, fmt.Sprintf("at most %d labels, got %d", maxRuleLabels, len(r.Labels)))
	}
	seenLabel := map[string]bool{}
	for _, l := range r.Labels {
		switch {
		case strings.TrimSpace(l) == "" || utf8.RuneCountInString(l) > maxLabelLen:
			// Characters, as GitHub counts a label's fifty and the console does: a label of seventeen
			// Japanese characters is 51 bytes, exists at GitHub, and could otherwise never be used here.
			errs = append(errs, fmt.Sprintf("label %q must be 1 to %d characters", clip(l), maxLabelLen))
		case strings.ContainsFunc(l, unicode.IsControl):
			errs = append(errs, fmt.Sprintf("label %q must be one line", clip(l)))
		case seenLabel[strings.ToLower(strings.TrimSpace(l))]:
			errs = append(errs, fmt.Sprintf("label %q is listed twice", clip(l)))
		}
		seenLabel[strings.ToLower(strings.TrimSpace(l))] = true
	}
	if r.LabelRule() {
		// A label rule adds types and nothing else. One that named none would read as "the default"
		// on a branch rule and here would add nothing, which is a rule that looks like it does
		// something and does not.
		if len(r.Types) == 0 {
			errs = append(errs, "a label rule adds review types: name at least one")
		}
		if r.Trigger != "" || r.Strictness != "" || r.Post != "" || r.Model != "" || r.Notify != nil {
			errs = append(errs, "a label rule only adds review types; when to review, strictness, where it posts, the model and "+
				"the channel are its branch rule's")
		}
	}
	if r.Trigger != "" && !r.Trigger.Valid() {
		errs = append(errs, fmt.Sprintf("trigger %q: want command, open or push", clip(string(r.Trigger))))
	}
	if r.Strictness != "" && !r.Strictness.Valid() {
		errs = append(errs, fmt.Sprintf("strictness %q: want low, medium or high", clip(string(r.Strictness))))
	}
	if r.Post != "" && r.Post != ModeShadow && r.Post != ModeLive {
		errs = append(errs, fmt.Sprintf("post %q: want shadow or live", clip(string(r.Post))))
	}
	if r.Model != "" && !validModelName(r.Model) {
		errs = append(errs, fmt.Sprintf("model %q is not a model name", clip(r.Model)))
	}
	if r.Notify != nil {
		if err := r.Notify.Validate(); err != nil {
			errs = append(errs, "notify: "+err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// ValidateRules checks a whole rule list: its length, each rule, and that it ends in the
// fallback and has no other. A fallback higher up would silently make every rule below it
// dead, and a list that looks right in the console but never runs half its rules is the worst
// kind of setting.
//
// The fallback is required, not merely allowed, because a level's list replaces the one above
// it whole: a repository that saved [{base: main}] would drop the inherited "any → any" and
// leave every pull request into any other branch matching nothing, so silently not reviewed.
// The console keeps the row there; this is what holds an API or MCP caller to the same. An
// empty list is still valid — it saves nothing, and the level inherits its parent's. Label rules
// sit above the fallback with the branch rules: where one is in the list changes only the order
// the types it adds are run in, since every matching label rule adds its types.
func ValidateRules(rules []BranchRule) error {
	var errs []error
	if len(rules) > MaxBranchRules {
		errs = append(errs, fmt.Errorf("at most %d branch rules, got %d", MaxBranchRules, len(rules)))
	}
	for i, r := range rules {
		if err := r.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("branch rule %d (%s): %w", i+1, r, err))
		}
		if r.Fallback() && i != len(rules)-1 {
			errs = append(errs, fmt.Errorf("branch rule %d matches every branch, so the %d after it can never run; move it to the end", i+1, len(rules)-1-i))
		}
	}
	if n := len(rules); n > 0 && !rules[n-1].Fallback() {
		errs = append(errs, errors.New(`the last branch rule must be "any → any", the fallback for every other branch; without it a pull request no rule matches is not reviewed`))
	}
	return errors.Join(errs...)
}

// validModelName accepts a model alias ("heavy", "flash") or a provider's model id
// ("z-ai/glm-5.3"). Which models an organisation may use is decided where the providers are
// known; this only refuses what cannot be a name at all.
func validModelName(s string) bool {
	return s != "" && len(s) <= maxModelNameLen &&
		!strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}
