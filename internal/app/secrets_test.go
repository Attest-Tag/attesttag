package app

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// secretTestRand builds credential-shaped values at run time, so this file never holds one: GitHub
// push protection refuses a push with a provider key in it, fake or not.
func secretTestRand(seed uint64, alphabet string, n int) string {
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

const (
	alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	hexLo = "0123456789abcdef"
)

// The betterleaks rules load, and in numbers: a release that drops most of them, or a regex the
// standard library cannot compile, shows here rather than as keys that quietly stop being masked.
func TestSecretRulesLoad(t *testing.T) {
	set := secretRules()
	if len(set.rules) < 300 {
		t.Fatalf("%d betterleaks rules loaded, want 300 or more", len(set.rules))
	}
	for _, r := range set.rules {
		if strings.HasPrefix(r.id, "generic-") {
			t.Errorf("generic rule %s loaded; those mask ordinary code without their context filters", r.id)
		}
	}
}

// Formats attest_tag's own patterns never knew are masked now, and named by their rule.
func TestRedactProviderKeys(t *testing.T) {
	for _, c := range []struct{ name, line, kind string }{
		{"anthropic", `ANTHROPIC_API_KEY="sk-ant-api03-` + secretTestRand(1, alnum, 93) + `AA"`, "an API key"},
		{"openrouter", `key: sk-or-v1-` + secretTestRand(2, hexLo, 64), "an API key"},
		{"stripe", `stripe.Key = "` + "sk_" + "live_" + secretTestRand(3, alnum, 24) + `"`, "a credential (stripe access token)"},
		{"google", `const k = "AIza` + secretTestRand(4, alnum, 35) + `"`, "an API key"},
		{"sendgrid", `SENDGRID=SG.` + secretTestRand(5, alnum, 22) + "." + secretTestRand(6, alnum, 43), "a credential (sendgrid api token)"},
	} {
		got, hits := redactWith(c.line, "[x]")
		if len(hits) == 0 || !strings.Contains(got, "[x]") {
			t.Errorf("%s: not masked: %q", c.name, got)
			continue
		}
		if k := hits[0].kind(); k != c.kind {
			t.Errorf("%s: kind %q, want %q (rule %s)", c.name, k, c.kind, hits[0].Rule)
		}
	}
}

// Templates and variables are where a key would go, not keys: betterleaks' global filter.
func TestRedactLeavesTemplatesAlone(t *testing.T) {
	for _, in := range []string{
		`SLACK_BOT_TOKEN: ${SLACK_BOT_TOKEN}`,
		`password: "{{ .Values.db.password }}"`,
		`GITHUB_TOKEN=$GITHUB_TOKEN`,
		`api_key = "%API_KEY%"`,
	} {
		if got := redact(in); got != in {
			t.Errorf("redact(%q) = %q, want it unchanged", in, got)
		}
	}
}

// One credential is masked once, whichever of the rules that know it found it.
func TestRedactOverlappingRulesMaskOnce(t *testing.T) {
	tok := "ghp_" + secretTestRand(7, alnum, 36)
	got, hits := redactWith(`token = "`+tok+`"`, "[x]")
	if got != `token = "[x]"` || len(hits) != 1 {
		t.Errorf("got %q with %d hits, want the token masked once", got, len(hits))
	}
}

// redact runs on every prompt that leaves for a model, so it has to stay cheap on a long one.
func BenchmarkRedactPrompt(b *testing.B) {
	var sb strings.Builder
	for sb.Len() < 100_000 {
		sb.WriteString("The deploy token is read from the secret store; ask the API for a new key when it expires.\n")
		sb.WriteString("func handler(w http.ResponseWriter, r *http.Request) { auth := r.Header.Get(\"Authorization\") }\n")
	}
	s := sb.String()
	b.SetBytes(int64(len(s)))
	for b.Loop() {
		redact(s)
	}
}
