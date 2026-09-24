package app

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// /v1/usage's top_channels speaks the snake_case the rest of the API does, in the words
// /v1/activity uses for a turn. Its rows used to be the console's UsageRow as it is, so a caller
// who coded to the reference's "channel" and "cost_usd" got nothing, and "Channel" and "Cost"
// arrived instead. The console's /api/overview keeps its PascalCase (TestOverviewNamesEveryRow).
func TestV1UsageTopChannelsIsSnakeCase(t *testing.T) {
	b, mux, st := identityBot(t)
	ctx := context.Background()
	_, orgID, session := signedUp(t, b, mux, st, "founder@example.com")
	raw, _ := mintKey(t, mux, session, "usage poller")

	if _, err := st.UpsertChannelScope(ctx, orgID, "T1", "C1", "#support", false); err != nil {
		t.Fatal(err)
	}
	st.LogUsageBy(ctx, orgID, "T1", "C1", "", "U1", "test", Usage{In: 100, Out: 20, CostUSD: 0.5})
	st.LogUsageBy(ctx, orgID, "T1", "C1", "", "U1", "test", Usage{In: 50, Out: 10, CostUSD: 0.25})

	code, body := authReq(t, mux, "GET", "/v1/usage", nil, raw)
	if code != 200 {
		t.Fatalf("GET /v1/usage = %d: %v", code, body)
	}
	rows, _ := body["top_channels"].([]any)
	if len(rows) != 1 {
		t.Fatalf("top_channels = %v, want one row", body["top_channels"])
	}
	row, _ := rows[0].(map[string]any)
	want := map[string]any{
		"team_id": "T1", "channel": "C1", "channel_name": "#support",
		"turns": 2.0, "tokens_in": 150.0, "tokens_out": 30.0, "cost_usd": 0.75,
	}
	var got, wanted []string
	for k := range row {
		got = append(got, k)
	}
	for k := range want {
		wanted = append(wanted, k)
	}
	sort.Strings(got)
	sort.Strings(wanted)
	if strings.Join(got, ",") != strings.Join(wanted, ",") {
		t.Fatalf("top_channels row has keys %v, want exactly %v", got, wanted)
	}
	for k, v := range want {
		if row[k] != v {
			t.Errorf("top_channels[0].%s = %v, want %v", k, row[k], v)
		}
	}
}
