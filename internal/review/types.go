package review

import (
	"embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Type is a review type: a rubric one finder pass is run against. General, Security, Tests,
// Performance and Release ship in the binary (types/*.md); an organisation edits a copy of one,
// or writes its own, in the console, and the app hands the result here in this shape.
type Type struct {
	// Key is the short name a branch rule and "@bot review <key>" use, e.g. "security".
	Key  string
	Name string
	// Purpose is "what this review is for", the paragraph the finder reads first.
	Purpose string
	// Strictness is the type's own default; empty inherits from the settings.
	Strictness Strictness
	// PathGlobs limit the files the type looks at; empty means every file.
	PathGlobs []string
	// InlineMinSeverity is the least severe finding posted on the diff; the rest go to the
	// summary. Empty means every severity, as P2 does. A release review sets P0: on a merge of
	// a hundred reviewed pull requests, only what would break production deserves a comment.
	InlineMinSeverity Severity
	Rules             []TypeRule
	// Skills are folders in GitHub repositories the finder follows besides the rules (SkillLink),
	// cited as S1, S2… The app reads them; the type only says where they are.
	Skills []SkillLink
}

// TypeRule is one line the finder is asked to check.
type TypeRule struct {
	Text string
	// SeverityCap is the most severe a finding citing this rule may be; empty is no cap.
	SeverityCap Severity
	// PathGlobs are the files the rule is about; empty means every file the type looks at.
	PathGlobs []string
	// Source is RuleBuiltin, RuleTeam or RuleLearned, for the console's chip.
	Source string
	// ExampleBad and ExampleGood are optional code the rule would flag and would accept.
	ExampleBad, ExampleGood string
	// Off is a rule somebody turned off. It is kept, so it can come back and so the ids of the
	// rules after it do not move, but no model is ever shown it.
	Off bool
}

// Where a rule came from.
const (
	RuleBuiltin = "builtin"
	RuleTeam    = "team"
	RuleLearned = "learned"
)

// The bounds a type is held to. Forty rules of four hundred characters is what a finder can be
// given on every pass without the rubric crowding out the diff — the same caps as memory —
// and a type that wants more is two types.
const (
	MaxTypeRules      = 40
	MaxRuleLen        = 400
	MinTypeNameLen    = 2
	MaxTypeNameLen    = 40
	MaxTypePurposeLen = 2000
	MaxRuleExampleLen = 2000

	// A type's own key is held to 2 to 30 characters, tighter than ValidTypeKey's 40, which
	// only says whether a word in a command could be a key at all.
	minTypeKeyLen = 2
	maxOwnKeyLen  = 30
	maxTypeGlobs  = 20
	maxTypeGlob   = 200
)

// RuleID is the id the i-th rule is cited by: "R1" for the first. It is the rule's place in
// the list, counting the ones that are off, so turning one off never renames the others and
// a finding that cites "R3" still means the same rule.
func (t Type) RuleID(i int) string { return "R" + strconv.Itoa(i+1) }

// Rule returns the rule a finding cites by id.
func (t Type) Rule(id string) (TypeRule, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(id), "R"))
	if err != nil || !strings.HasPrefix(strings.TrimSpace(id), "R") || n < 1 || n > len(t.Rules) {
		return TypeRule{}, false
	}
	return t.Rules[n-1], true
}

// Covers reports whether the type looks at path.
func (t Type) Covers(path string) bool { return len(t.PathGlobs) == 0 || MatchAny(t.PathGlobs, path) }

// Covers reports whether the rule is about path.
func (r TypeRule) Covers(path string) bool {
	return len(r.PathGlobs) == 0 || MatchAny(r.PathGlobs, path)
}

// builtinFiles are the built-in rubrics. They are text rather than Go so that they read as
// what they are, a rubric, and so that the console's "Reset to built-in" and a later release's
// new rules come from one place.
//
//go:embed types/*.md
var builtinFiles embed.FS

// builtinOrder is the order the built-ins are offered in: the default first.
var builtinOrder = []string{DefaultType, "security", "tests", "performance", "release"}

var builtins = sync.OnceValue(func() []Type {
	out := make([]Type, 0, len(builtinOrder))
	for _, key := range builtinOrder {
		b, err := builtinFiles.ReadFile("types/" + key + ".md")
		if err != nil {
			panic(fmt.Sprintf("review: built-in type %s: %v", key, err))
		}
		t, err := ParseType(string(b))
		if err != nil {
			// The files are compiled in, so this is a bad build, and TestBuiltinTypes fails
			// on it long before a binary ships.
			panic(fmt.Sprintf("review: built-in type %s: %v", key, err))
		}
		if t.Key != key {
			panic(fmt.Sprintf("review: types/%s.md says its key is %q", key, t.Key))
		}
		for i := range t.Rules {
			t.Rules[i].Source = RuleBuiltin
		}
		out = append(out, t)
	}
	return out
})

// BuiltinTypes returns the types that ship with attest_tag, the default first. Each call
// returns fresh copies, so a caller may change what it gets.
func BuiltinTypes() []Type {
	out := make([]Type, 0, len(builtinOrder))
	for _, t := range builtins() {
		out = append(out, t.clone())
	}
	return out
}

// BuiltinType returns the built-in type with key, as a copy.
func BuiltinType(key string) (Type, bool) {
	for _, t := range builtins() {
		if t.Key == key {
			return t.clone(), true
		}
	}
	return Type{}, false
}

func (t Type) clone() Type {
	t.PathGlobs = slices.Clone(t.PathGlobs)
	t.Skills = slices.Clone(t.Skills)
	t.Rules = slices.Clone(t.Rules)
	for i := range t.Rules {
		t.Rules[i].PathGlobs = slices.Clone(t.Rules[i].PathGlobs)
	}
	return t
}

// ruleLine is a rule in a type file: "- [P1] text" or "- [P1; paths: a/**, b/**] text". The
// text may start on the next, indented, line, when the bracket is long.
var ruleLine = regexp.MustCompile(`^- \[([^\]]*)\](?:\s+(.*))?$`)

// ParseType reads a type written in the rubric format the built-ins are kept in:
//
//	# Comment lines start with '#'.
//	key: security
//	name: Security
//	strictness: medium
//	inline: P1
//	paths: src/**, lib/**
//	purpose: What this review is for, running on over as many lines as it
//	needs; a blank line starts a new paragraph.
//
//	- [P0] A rule, one per item.
//	- [P1; paths: **/migrations/**] A rule about some files only. A long rule
//	  continues on lines indented under it.
//
// key, name and purpose are required; strictness, inline (the least severe finding posted
// inline) and paths are optional, as is a rule's paths.
//
// It is strict — an unknown key, a line that is neither a rule nor a rule's continuation — so
// a typo in a rubric is an error in a test and not a rule silently missing from every review.
// The result is checked with ValidateType.
func ParseType(text string) (Type, error) {
	var (
		t       Type
		seen    = map[string]bool{}
		inBody  bool     // past "purpose:"
		inRules bool     // past the first rule
		para    []string // the purpose paragraph being read
		paras   []string
	)
	endPara := func() {
		if len(para) > 0 {
			paras = append(paras, strings.Join(para, " "))
			para = nil
		}
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for n, raw := range lines {
		at := n + 1
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case !inBody:
			if line == "" {
				continue
			}
			k, v, ok := strings.Cut(line, ":")
			k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
			if !ok {
				return Type{}, fmt.Errorf("line %d: want \"key: value\" before the purpose, got %q", at, clip(line))
			}
			if seen[k] {
				return Type{}, fmt.Errorf("line %d: %s is set twice", at, k)
			}
			seen[k] = true
			switch k {
			case "key":
				t.Key = v
			case "name":
				t.Name = v
			case "strictness":
				t.Strictness = Strictness(strings.ToLower(v))
			case "inline":
				sev, err := ParseSeverity(v)
				if err != nil {
					return Type{}, fmt.Errorf("line %d: inline: %w", at, err)
				}
				t.InlineMinSeverity = sev
			case "paths":
				t.PathGlobs = splitGlobs(v)
			case "purpose":
				inBody = true
				if v != "" {
					para = append(para, v)
				}
			default:
				return Type{}, fmt.Errorf("line %d: unknown key %q (want key, name, strictness, inline, paths or purpose)", at, clip(k))
			}
		case strings.HasPrefix(line, "- ["):
			if !inRules {
				endPara()
				inRules = true
			}
			r, err := parseRuleLine(line)
			if err != nil {
				return Type{}, fmt.Errorf("line %d: %w", at, err)
			}
			t.Rules = append(t.Rules, r)
		case line == "":
			endPara()
		case inRules:
			// Only an indented line continues a rule; anything else after the rules began is
			// text that would otherwise be quietly dropped.
			if !(raw[0] == ' ' || raw[0] == '\t') || strings.TrimSpace(lines[n-1]) == "" {
				return Type{}, fmt.Errorf("line %d: want a rule (\"- [P1] text\") or an indented continuation, got %q", at, clip(line))
			}
			last := &t.Rules[len(t.Rules)-1]
			last.Text += " " + line
		default:
			para = append(para, line)
		}
	}
	endPara()
	if !inBody {
		return Type{}, errors.New(`no "purpose:" line`)
	}
	for i := range t.Rules {
		t.Rules[i].Text = strings.TrimSpace(t.Rules[i].Text)
	}
	t.Purpose = strings.Join(paras, "\n\n")
	if err := ValidateType(t); err != nil {
		return Type{}, err
	}
	return t, nil
}

func parseRuleLine(line string) (TypeRule, error) {
	m := ruleLine.FindStringSubmatch(line)
	if m == nil {
		return TypeRule{}, fmt.Errorf("want \"- [P1] text\", got %q", clip(line))
	}
	parts := strings.Split(m[1], ";")
	sev, err := ParseSeverity(parts[0])
	if err != nil {
		return TypeRule{}, err
	}
	r := TypeRule{Text: m[2], SeverityCap: sev}
	for _, part := range parts[1:] {
		k, v, ok := strings.Cut(part, ":")
		if !ok || strings.TrimSpace(strings.ToLower(k)) != "paths" {
			return TypeRule{}, fmt.Errorf("rule option %q: want paths: glob, glob", clip(strings.TrimSpace(part)))
		}
		r.PathGlobs = splitGlobs(v)
	}
	return r, nil
}

func splitGlobs(v string) []string {
	var out []string
	for _, g := range strings.Split(v, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// ValidateType reports everything wrong with a type at once, joined, in terms a console form
// can show beside its fields. Rule and purpose text is held to one line and a paragraph
// because it goes into every finder prompt verbatim: a rule that could hold a line break
// could hold a fake end to the criteria and a new section after it.
func ValidateType(t Type) error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !ValidTypeKey(t.Key) || len(t.Key) < minTypeKeyLen || len(t.Key) > maxOwnKeyLen {
		bad("key %q: want %d to %d lowercase letters, digits and hyphens, starting with a letter", clip(t.Key), minTypeKeyLen, maxOwnKeyLen)
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(t.Name)); n < MinTypeNameLen || n > MaxTypeNameLen || strings.ContainsFunc(t.Name, unicode.IsControl) {
		bad("name must be one line of %d to %d characters", MinTypeNameLen, MaxTypeNameLen)
	}
	if utf8.RuneCountInString(t.Purpose) > MaxTypePurposeLen {
		bad("purpose must be at most %d characters", MaxTypePurposeLen)
	}
	if strings.ContainsFunc(t.Purpose, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }) {
		bad("purpose contains a control character")
	}
	if t.Strictness != "" && !t.Strictness.Valid() {
		bad("strictness %q: want low, medium or high", clip(string(t.Strictness)))
	}
	if t.InlineMinSeverity != "" && !t.InlineMinSeverity.Valid() {
		bad("inline_min_severity %q: want P0, P1 or P2", clip(string(t.InlineMinSeverity)))
	}
	if err := validateGlobs(t.PathGlobs); err != nil {
		bad("paths: %v", err)
	}
	if len(t.Rules) > MaxTypeRules {
		bad("at most %d rules, got %d; split the type in two", MaxTypeRules, len(t.Rules))
	}
	for i, r := range t.Rules {
		id := t.RuleID(i)
		switch n := utf8.RuneCountInString(strings.TrimSpace(r.Text)); {
		case n == 0:
			bad("rule %s is empty", id)
		case n > MaxRuleLen:
			bad("rule %s must be at most %d characters, got %d", id, MaxRuleLen, n)
		}
		if strings.ContainsFunc(r.Text, unicode.IsControl) {
			bad("rule %s must be one line", id)
		}
		if r.SeverityCap != "" && !r.SeverityCap.Valid() {
			bad("rule %s: severity %q: want P0, P1 or P2", id, clip(string(r.SeverityCap)))
		}
		if err := validateGlobs(r.PathGlobs); err != nil {
			bad("rule %s: paths: %v", id, err)
		}
		if !slices.Contains([]string{"", RuleBuiltin, RuleTeam, RuleLearned}, r.Source) {
			bad("rule %s: source %q: want builtin, team or learned", id, clip(r.Source))
		}
		if utf8.RuneCountInString(r.ExampleBad) > MaxRuleExampleLen || utf8.RuneCountInString(r.ExampleGood) > MaxRuleExampleLen {
			bad("rule %s: an example must be at most %d characters", id, MaxRuleExampleLen)
		}
	}
	errs = append(errs, validateSkills(t.Skills)...)
	return errors.Join(errs...)
}

func validateGlobs(globs []string) error {
	if len(globs) > maxTypeGlobs {
		return fmt.Errorf("at most %d patterns, got %d", maxTypeGlobs, len(globs))
	}
	for _, g := range globs {
		if strings.TrimSpace(g) == "" || len(g) > maxTypeGlob || !validPathGlob(g) {
			return fmt.Errorf("%q is not a path pattern of at most %d characters", clip(g), maxTypeGlob)
		}
	}
	return nil
}
