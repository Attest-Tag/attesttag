package app

import (
	"strings"
	"testing"
)

// Synthetic provider identifiers: a key's hash is 64 hex characters, and the link that names it.
var (
	testKeyHash = strings.Repeat("0123456789abcdef", 4)
	testKeyURL  = "https://provider.example/workspaces/ws-example/keys/" + testKeyHash
	// testCredit402 is a provider's refusal for credit as an engine prints it, numbers and all.
	testCredit402 = "[API Error: 402 This request requires more credits, or fewer max_tokens. You requested up to 32000 tokens, " +
		"but can only afford 1234. To increase, visit " + testKeyURL + " and adjust the key's total limit]"
)

// leaked fails t when s still carries anything that names the provider account or its credit.
func leaked(t *testing.T, s string) {
	t.Helper()
	for _, bad := range []string{testKeyHash, "ws-example", "provider.example/workspaces", "32000", "1234", "max_tokens", "total limit"} {
		if strings.Contains(s, bad) {
			t.Errorf("%q survived in %q", bad, s)
		}
	}
}

func TestPublicJobText(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"credit refusal as the whole summary", testCredit402,
			"The model provider refused the request: credit or key limit reached."},
		{"credit refusal after the engine's own words", "Guarded the retry path in client.go.\n" + testCredit402,
			"Guarded the retry path in client.go.\nThe model provider refused the request: credit or key limit reached."},
		{"credit refusal without a wrapper", "402 This request requires more credits, or fewer max_tokens. You requested up to 32000 tokens, but can only afford 1234.",
			"The model provider refused the request: credit or key limit reached."},
		{"credit refusal as a JSON body", `{"error":{"message":"Insufficient credits. Add more using https://provider.example/credits","code":402}}`,
			"The model provider refused the request: credit or key limit reached."},
		{"quota refusal under a 429", "[API Error: 429 You exceeded your current quota, please check your plan and billing details.]",
			"The model provider refused the request: credit or key limit reached."},
		{"rate limit", "[API Error: 429 Rate limit exceeded: too many requests per minute for this key]",
			"The model provider refused the request: rate limited."},
		{"rate limit on a line of its own", "Started on the fix.\nAPI Error: 429 slow down",
			"Started on the fix.\nThe model provider refused the request: rate limited."},
		{"generic 5xx", "[API Error: 503 Service Unavailable]",
			"The model provider refused the request: provider error 503."},
		{"generic 5xx from an engine's exit", "pi exited with code 1: 502 Bad Gateway",
			"The model provider refused the request: provider error 502."},
		{"no status at all", "✕ [API Error: Connection error.]",
			"✕ the model provider returned an error"},
		{"a link with a 64-hex segment", "push rejected; see " + testKeyURL + " for details",
			"push rejected; see [link removed] for details"},
		{"a 64-hex string alone", "used key " + testKeyHash + " for the call",
			"used key [id removed] for the call"},
		{"a markdown link keeps its brackets", "[the key](" + testKeyURL + ")",
			"[the key]([link removed])"},
		// A fix job is as likely as not about status codes in the repository's own code.
		{"a summary about somebody's 429 is left alone", "Return 429 with a Retry-After header; the rate limit is now 100/min and an API Error: 503 is retried.",
			"Return 429 with a Retry-After header; the rate limit is now 100/min and an API Error: 503 is retried."},
		{"an ordinary link is left alone", "See https://example.com/docs/retry for the policy.",
			"See https://example.com/docs/retry for the policy."},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PublicJobText(tc.in)
			if got != tc.want {
				t.Errorf("PublicJobText(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			leaked(t, got)
		})
	}
}

func TestPublicJobError(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"credit refusal inside an engine's exit", "engine_error: qwen exited with code 1: (node:12) warning: something\n" + testCredit402 + "\nat retry (client.js:4)",
			"the model provider refused the request: credit or key limit reached"},
		{"credit refusal cut short by the stored error's cap", "engine_error: " + testCredit402[:150],
			"the model provider refused the request: credit or key limit reached"},
		{"rate limit", "engine_error: [API Error: 429 Too Many Requests]",
			"the model provider refused the request: rate limited"},
		{"generic 5xx", "engine_error: pi exited with code 1: 500 internal error",
			"the model provider refused the request: provider error 500"},
		{"a link with a 64-hex segment", "engine_error: the engine could not start, see " + testKeyURL,
			"engine_error: the engine could not start, see [link removed]"},
		// GitHub's 403 is GitHub's: only an engine's words around a failed model call are the provider's.
		{"somebody else's status is left alone", "clone_failed: git clone: The requested URL returned error: 403",
			"clone_failed: git clone: The requested URL returned error: 403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := publicJobError(tc.in)
			if got != tc.want {
				t.Errorf("publicJobError(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			leaked(t, got)
		})
	}
}

// Every place a job speaks outside the console says why in plain words: the thread's checklist and
// report, and the answer on a pull request.
func TestJobOutputOutsideTheConsoleDropsProviderDetail(t *testing.T) {
	failed := &Job{ID: 4, Status: JobFailed, Repo: "acme/app", Phase: "engine", Error: "engine_error: " + testCredit402}
	res := &JobResult{Status: JobFailed, Summary: testCredit402, Error: JobError{Code: "engine_error", Message: testCredit402}}
	running := &Job{ID: 4, Status: jobRunning, Repo: "acme/app"}
	events := []JobEvent{{Kind: JobKindWarn, Message: "qwen: " + testCredit402}}

	done := &Job{ID: 5, Status: JobSucceeded, Repo: "acme/app"}
	pushed := &JobResult{Status: JobSucceeded, HeadSHA: "fedcba9", Summary: "Renamed the flag.\n" + testCredit402, Note: "stopped early"}
	spec := JobSpec{Repo: "acme/app", Branch: "feature/x", Mode: JobModePR, PR: &JobPRRef{Number: 3, AskedBy: "someone"}}

	for name, out := range map[string]string{
		"report":           jobReport(failed, res),
		"checklist":        jobChecklist(running, events),
		"failed checklist": jobChecklist(failed, nil),
		"pull request":     reviewFixReport(done, pushed, spec, "attesttag", ""),
	} {
		t.Run(name, func(t *testing.T) {
			leaked(t, out)
			if !strings.Contains(strings.ToLower(out), "credit or key limit reached") {
				t.Errorf("no plain reason in:\n%s", out)
			}
		})
	}
}
