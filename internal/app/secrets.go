package app

import (
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	blconfig "github.com/betterleaks/betterleaks/v2/config"
)

// Credentials are found with betterleaks' rules (github.com/betterleaks/betterleaks, MIT: some 400
// providers' key formats, kept current by the people who wrote gitleaks) plus attest_tag's own
// (secretRes). Only betterleaks' config package is imported, for its rule list; its scanner would
// bring in a dozen archive formats, an expression language and a tokenizer. The rules run here on
// the standard regexp package, and what their filter expressions do is approximated: the entropy
// floor a filter names is kept, "reads like words" (tokenRatio) is secretShaped, and the global
// filter's templates and placeholders are secretTemplate.

// secretRes are attest_tag's own patterns: its tokens (atj1, atk1), which no public list knows, and
// the prefixes it has always masked. Each prefix starts at a word boundary: a key is pasted after a
// quote, a space, an = or a colon, never in the middle of a word, and without one sk- matches inside
// names such as --clr-ask-ai-gradient-start. A hit counts only when secretShaped agrees. A pattern
// runs only on text that holds one of its literals, lower-cased, as betterleaks' keywords do.
var secretRes = []struct {
	literals []string
	re       *regexp.Regexp
}{
	{[]string{"xox"}, regexp.MustCompile(`\bxox[abpers]-[A-Za-z0-9-]{10,}`)},
	{[]string{"xapp-"}, regexp.MustCompile(`\bxapp-[A-Za-z0-9-]{10,}`)},
	{[]string{"sk-"}, regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)},
	{[]string{"akia"}, regexp.MustCompile(`\bAKIA[0-9A-Z]{16}`)},
	{[]string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"}, regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`)},
	{[]string{"github_pat_"}, regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}`)}, // fine-grained GitHub tokens
	{[]string{"atj1."}, regexp.MustCompile(`\batj1\.[0-9]+\.[A-Za-z0-9_-]{40,}`)},   // fix-job worker tokens (jobs.go)
	{[]string{"atk1."}, regexp.MustCompile(`\batk1\.[A-Za-z0-9_-]{40,}`)},           // developer API keys (api_keys.go): 32 random bytes, base64url
	{[]string{"private key-----"}, regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{[]string{"eyj"}, regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}`)}, // JWT
}

// ownSecretRule names a hit of secretRes.
const ownSecretRule = "attest_tag"

// secretShaped says whether a hit reads as random rather than as words. A key's body past its
// prefix is random characters, so it holds a digit or a capital; a body of lowercase words joined
// by - or _ (sk-ai-gradient-start, sk-learn-model-selection) is an identifier.
func secretShaped(hit string) bool {
	if strings.HasPrefix(hit, "-----") {
		return true
	}
	body := hit[strings.IndexAny(hit, "-_.")+1:]
	return strings.ContainsFunc(body, func(r rune) bool { return r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' })
}

// secretTemplateRe is betterleaks' global filter: a value that is a template or a variable rather
// than a credential (${TOKEN}, {{ .Values.key }}, %PASSWORD%, true, xxxxxxxx).
var secretTemplateRe = regexp.MustCompile(`(?i)^(?:true|false|null)$|^(?:a+|b+|c+|d+|e+|f+|g+|h+|i+|j+|k+|l+|m+|n+|o+|p+|q+|r+|s+|t+|u+|v+|w+|x+|y+|z+|\*+|\.+)$|` +
	`^\$(?:\d+|\{\d+\})$|^\$\{?[A-Za-z_]+\}?$|^\$?\{\{.*\}\}$|^%[A-Za-z_]+%$|^@[A-Za-z_]+@$|abcdefghijklmnopqrstuvwxyz`)

func secretTemplate(v string) bool { return secretTemplateRe.MatchString(v) }

// secretRule is one betterleaks rule as this file runs it.
type secretRule struct {
	id         string
	re         *regexp.Regexp
	group      int     // the capture group holding the value; 0 is the first that matched, else the whole match
	minEntropy float64 // a value at or under this is discarded
	wordy      bool    // its filter discards values that read like words
}

type secretRuleSet struct {
	rules []secretRule
	// byKeyword lists, for each lower-case keyword, the rules it lets run: a rule's regex runs only
	// on text that holds one of its keywords, which is what keeps 400 rules cheap on every prompt.
	byKeyword map[string][]int
	keywords  []string
}

// secretFilterEntropy reads the entropy floor out of a betterleaks filter expression.
var secretFilterEntropy = regexp.MustCompile(`entropy\(finding\["secret"\]\)\s*<=?\s*([0-9.]+)`)

// secretRules loads betterleaks' rules once. A rule left out: one that needs a second, nearby
// match to mean anything (required components), one that only serves as such a component
// (skipReport), one that matches paths only, and the generic ones
// (generic-password, generic-api-key …), whose filters weigh context this file does not have and
// which, without them, mask ordinary code.
var secretRules = sync.OnceValue(func() *secretRuleSet {
	set := &secretRuleSet{byKeyword: map[string][]int{}}
	cfg, err := blconfig.Default()
	if err != nil {
		// The rules are embedded in the module, so this is a broken build, not a bad input: say so,
		// and keep masking with attest_tag's own patterns.
		slog.Error("secret rules did not load; masking with attest_tag's own patterns only", "err", err)
		return set
	}
	for _, r := range cfg.Rules {
		if r.Regex == "" || r.SkipReport || strings.HasPrefix(r.ID, "generic-") || slices.ContainsFunc(r.Components, func(c blconfig.Component) bool { return !c.Optional }) {
			continue
		}
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			continue
		}
		sr := secretRule{id: r.ID, re: re, group: r.ValueGroup, wordy: strings.Contains(r.FilterExpr, "tokenRatio")}
		if m := secretFilterEntropy.FindStringSubmatch(r.FilterExpr); m != nil {
			sr.minEntropy, _ = strconv.ParseFloat(m[1], 64)
		}
		set.rules = append(set.rules, sr)
		for _, k := range r.Keywords {
			if _, ok := set.byKeyword[k]; !ok {
				set.keywords = append(set.keywords, k)
			}
			set.byKeyword[k] = append(set.byKeyword[k], len(set.rules)-1)
		}
	}
	return set
})

// secretHit is one credential found: the rule that found it and the value it masked.
type secretHit struct {
	Rule  string
	Value string
}

// kind names what a credential is, for a finding that must say what was committed without saying
// what it was.
func (h secretHit) kind() string {
	id, v := h.Rule, h.Value
	switch {
	case id == "private-key" || strings.HasPrefix(v, "-----"):
		return "a private key"
	case strings.HasPrefix(id, "slack-app") || strings.HasPrefix(v, "xapp-"):
		return "a Slack app token"
	case strings.HasPrefix(id, "slack-") || strings.HasPrefix(v, "xox"):
		return "a Slack token"
	case strings.HasPrefix(id, "github-") || strings.HasPrefix(v, "github_pat_") || strings.HasPrefix(v, "gh"):
		return "a GitHub token"
	case id == "aws-access-token" || strings.HasPrefix(v, "AKIA"):
		return "an AWS access key"
	case strings.HasPrefix(id, "jwt") || strings.HasPrefix(v, "eyJ"):
		return "a JWT"
	case strings.HasPrefix(v, "atj1."):
		return "an attest_tag worker token"
	case strings.HasPrefix(v, "atk1."):
		return "an attest_tag API key"
	case id == ownSecretRule || strings.HasSuffix(id, "-api-key"):
		return "an API key"
	}
	// stripe-access-token → "a credential (stripe access token)": the rule says what it is.
	return "a credential (" + strings.ReplaceAll(id, "-", " ") + ")"
}

type secretSpan struct {
	start, end int
	hit        secretHit
}

// findSecrets returns the credentials in s, earliest first, none overlapping another.
func findSecrets(s string) []secretSpan {
	var spans []secretSpan
	lower := strings.ToLower(s)
	for _, own := range secretRes {
		if !slices.ContainsFunc(own.literals, func(l string) bool { return strings.Contains(lower, l) }) {
			continue
		}
		for _, m := range own.re.FindAllStringIndex(s, -1) {
			if v := s[m[0]:m[1]]; secretShaped(v) {
				spans = append(spans, secretSpan{m[0], m[1], secretHit{ownSecretRule, v}})
			}
		}
	}
	set := secretRules()
	run := map[int]bool{}
	for _, k := range set.keywords {
		if strings.Contains(lower, k) {
			for _, i := range set.byKeyword[k] {
				run[i] = true
			}
		}
	}
	for i := range run {
		r := &set.rules[i]
		for _, m := range r.re.FindAllStringSubmatchIndex(s, -1) {
			a, b := secretValueSpan(m, r.group)
			if a < 0 || a == b {
				continue
			}
			v := s[a:b]
			if secretTemplate(v) || (r.minEntropy > 0 && secretEntropy(v) <= r.minEntropy) || (r.wordy && !secretShaped(v)) {
				continue
			}
			spans = append(spans, secretSpan{a, b, secretHit{r.id, v}})
		}
	}
	// Earliest first, the longest of those first, a named rule before attest_tag's catch-all; then
	// whatever overlaps a span already kept is the same credential seen twice.
	slices.SortFunc(spans, func(x, y secretSpan) int {
		if x.start != y.start {
			return x.start - y.start
		}
		if x.end != y.end {
			return y.end - x.end
		}
		if (x.hit.Rule == ownSecretRule) != (y.hit.Rule == ownSecretRule) {
			if x.hit.Rule == ownSecretRule {
				return 1
			}
			return -1
		}
		return strings.Compare(x.hit.Rule, y.hit.Rule)
	})
	out := spans[:0]
	for _, sp := range spans {
		if len(out) > 0 && sp.start < out[len(out)-1].end {
			continue
		}
		out = append(out, sp)
	}
	return out
}

// secretValueSpan is where the value sits in one match: the rule's group, or the first group
// that matched, or the whole match.
func secretValueSpan(m []int, group int) (int, int) {
	if group > 0 {
		if 2*group+1 < len(m) {
			return m[2*group], m[2*group+1]
		}
		return -1, -1
	}
	for g := 1; 2*g+1 < len(m); g++ {
		if m[2*g] >= 0 && m[2*g+1] > m[2*g] {
			return m[2*g], m[2*g+1]
		}
	}
	return m[0], m[1]
}

// secretEntropy is the Shannon entropy of v in bits per byte, as betterleaks' filters measure it.
func secretEntropy(v string) float64 {
	var counts [256]int
	for i := 0; i < len(v); i++ {
		counts[v[i]]++
	}
	var e float64
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / float64(len(v))
			e -= p * math.Log2(p)
		}
	}
	return e
}

// redactWith replaces every credential in s with mask and returns what it replaced.
func redactWith(s, mask string) (string, []secretHit) {
	spans := findSecrets(s)
	if len(spans) == 0 {
		return s, nil
	}
	var b strings.Builder
	hits := make([]secretHit, 0, len(spans))
	at := 0
	for _, sp := range spans {
		b.WriteString(s[at:sp.start])
		b.WriteString(mask)
		at = sp.end
		hits = append(hits, sp.hit)
	}
	b.WriteString(s[at:])
	return b.String(), hits
}

// redact masks credentials before anything leaves for the model provider.
func redact(s string) string {
	s, _ = redactWith(s, "[redacted-secret]")
	return s
}
