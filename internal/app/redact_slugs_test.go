package app

import "testing"

// MCP results now pass through redact before the model reads them, so a slug that merely contains
// "sk-" or a long token-like run must come through as it was: the model reuses these ids and URLs.
func TestRedactLeavesSlugsAndIDsAlone(t *testing.T) {
	for _, s := range []string{
		"https://www.notion.so/Task-list-for-the-launch-1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d",
		"risk-assessment-for-the-quarterly-review",
		"LIN-1234 disk-usage-alerts-are-noisy",
		"page_id 2f8e6c1a-9b4d-4e7a-8c3f-1d2e3f4a5b6c",
	} {
		if got := redact(s); got != s {
			t.Errorf("redact(%q) = %q, want it unchanged", s, got)
		}
	}
}
