package app

import (
	"strings"
	"testing"
)

// gcp_query_metrics was truncated on every call it made in production: unaggregated, Cloud
// Monitoring returns every raw point of every series, and one plain hour of request_count came
// back at a quarter of a million characters, of which the model saw the first twelve thousand.
// The aggregation arguments are how it asks for less, so they have to reach the URL — an aligner
// dropped on the way out is a tool that still cannot be narrowed.
func metricsURL(t *testing.T, args map[string]any) string {
	t.Helper()
	req, err := gcpMetricsRequest(args)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return req.URL
}

func TestMetricsAggregationReachesTheAPI(t *testing.T) {
	plain := metricsURL(t, map[string]any{"project_id": "p", "filter": `metric.type="run.googleapis.com/request_count"`})
	if strings.Contains(plain, "aggregation") {
		t.Errorf("nothing was asked for, so no aggregation should be sent: %s", plain)
	}
	if !strings.Contains(plain, "projects/p/timeSeries") {
		t.Errorf("the project is not in the path: %s", plain)
	}

	full := metricsURL(t, map[string]any{"project_id": "p", "filter": "f",
		"aligner": "ALIGN_RATE", "alignment_period": "600s", "reducer": "REDUCE_SUM"})
	for _, want := range []string{
		"aggregation.perSeriesAligner=ALIGN_RATE",
		"aggregation.alignmentPeriod=600s",
		"aggregation.crossSeriesReducer=REDUCE_SUM",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("missing %s in %s", want, full)
		}
	}

	// An aligner with no period does nothing at all at the far end, so one is supplied.
	if got := metricsURL(t, map[string]any{"project_id": "p", "filter": "f", "aligner": "ALIGN_MEAN"}); !strings.Contains(got, "aggregation.alignmentPeriod=300s") {
		t.Errorf("an aligner went out with no period, so it is ignored: %s", got)
	}
	// And a reducer alone is rejected by the API, so it never goes alone.
	if got := metricsURL(t, map[string]any{"project_id": "p", "filter": "f", "reducer": "REDUCE_SUM"}); strings.Contains(got, "crossSeriesReducer") {
		t.Errorf("a reducer went out with no aligner: %s", got)
	}
}
