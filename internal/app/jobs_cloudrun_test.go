package app

import "testing"

// The execution-level deep link 404s when it is opened cold, so the console must
// link the job's Executions tab instead.
func TestExecutionConsoleURL(t *testing.T) {
	ref := "projects/example-project-1234/locations/us-central1/jobs/attesttag-worker/executions/attesttag-worker-88sgn"
	want := "https://console.cloud.google.com/run/jobs/details/us-central1/attesttag-worker/executions?project=example-project-1234"
	if got := executionConsoleURL(ref); got != want {
		t.Errorf("executionConsoleURL(%q)\n got %q\nwant %q", ref, got, want)
	}
	for _, bad := range []string{"", "pid:10800", "projects/p/locations/r/jobs/j"} {
		if got := executionConsoleURL(bad); got != "" {
			t.Errorf("executionConsoleURL(%q) = %q, want \"\"", bad, got)
		}
	}
}
