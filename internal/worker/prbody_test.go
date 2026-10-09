package worker

import (
	"strings"
	"testing"

	"attesttag/internal/app"
)

// An engine whose model call was refused leaves the provider's words as its summary; the pull
// request and its commit say why in plain words, without the account those words name.
func TestPRBodyAndCommitSayWhyAProviderRefused(t *testing.T) {
	hash := strings.Repeat("0123456789abcdef", 4)
	summary := "Guarded the nil ticket id.\n[API Error: 402 This request requires more credits, or fewer max_tokens. " +
		"You requested up to 32000 tokens, but can only afford 1234. To increase, visit " +
		"https://provider.example/workspaces/ws-example/keys/" + hash + " and adjust the key's total limit]"
	spec := app.JobSpec{Title: "Null ticket id", Requirement: "Guard the nil ticket id.", Constraints: app.JobConstraints{Engine: "fake", Model: "m"}}
	recipe := &app.Recipe{Source: app.RecipeSourceDetected, Test: step("test", "go", "test", "./...")}
	res := &app.JobResult{Recipe: recipe, Tests: app.JobCheck{Command: "go test ./...",
		Before: app.JobTestRun{Ran: true, OK: true}, After: app.JobTestRun{Ran: true, OK: true}}}

	for name, out := range map[string]string{
		"pull request": prBody(spec, res, summary, recipe, 9, nil, nil, nil),
		"commit":       commitMessage(spec, summary, 9),
	} {
		t.Run(name, func(t *testing.T) {
			for _, bad := range []string{hash, "ws-example", "32000", "1234", "max_tokens", "API Error"} {
				if strings.Contains(out, bad) {
					t.Errorf("%q survived:\n%s", bad, out)
				}
			}
			if want := "Guarded the nil ticket id.\nThe model provider refused the request: credit or key limit reached."; !strings.Contains(out, want) {
				t.Errorf("lacks %q:\n%s", want, out)
			}
		})
	}
}
