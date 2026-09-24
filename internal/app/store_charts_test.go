package app

import (
	"context"
	"testing"
	"time"
)

// dayStamp is a timestamp n days before today, at noon UTC so no rounding puts it on a
// neighbouring day whatever hour the test runs at.
func dayStamp(n int) (day, at string) {
	d := time.Now().UTC().AddDate(0, 0, -n)
	return d.Format("2006-01-02"), d.Format("2006-01-02") + " 12:00:00"
}

// The daily series is what the chart's x-axis is made of, so the two things that matter about it
// are that it is a fixed width and that it has no holes: a quiet Tuesday has to arrive as a zero
// or the bars either side of it close up and the week reads as busier than it was.
func TestOverviewChartDataFillsEveryDayInTheWindow(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	today, todayAt := dayStamp(0)
	three, threeAt := dayStamp(3)
	_, oldAt := dayStamp(40) // outside a 30-day window, and must stay outside it

	seed := func(org int64, at, model string, cost float64) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, `insert into usage (org_id, team_id, channel, model, tokens_in, tokens_out, cost_usd, created_at)
			values (?, 'T1', 'C1', ?, 10, 5, ?, ?)`, org, model, cost, at); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	seed(orgID, todayAt, "glm", 1.0)
	seed(orgID, todayAt, "glm", 0.5)
	seed(orgID, threeAt, "glm", 0.25)
	seed(orgID, oldAt, "glm", 9.0)
	// Another tenant's turn, on the same day, to prove the grouping is narrowed the way every
	// other read here is.
	seed(orgID+1, todayAt, "glm", 100.0)

	got := st.OverviewChartData(ctx, orgID, 30)
	if len(got.Daily) != 30 {
		t.Fatalf("Daily has %d entries, want 30 — the axis is the window, not the rows", len(got.Daily))
	}
	if got.Daily[29].Day != today {
		t.Errorf("last day is %q, want today (%q)", got.Daily[29].Day, today)
	}
	for i := 1; i < len(got.Daily); i++ {
		prev, err := time.Parse("2006-01-02", got.Daily[i-1].Day)
		if err != nil {
			t.Fatalf("parse %q: %v", got.Daily[i-1].Day, err)
		}
		if want := prev.AddDate(0, 0, 1).Format("2006-01-02"); got.Daily[i].Day != want {
			t.Fatalf("day %d is %q, want %q — the series has a hole", i, got.Daily[i].Day, want)
		}
	}
	byDay := map[string]DayUsage{}
	for _, d := range got.Daily {
		byDay[d.Day] = d
	}
	if d := byDay[today]; d.Turns != 2 || d.Cost < 1.49 || d.Cost > 1.51 {
		t.Errorf("today = %+v, want 2 turns and $1.50 (the other tenant's $100 is not ours)", d)
	}
	if d := byDay[three]; d.Turns != 1 {
		t.Errorf("three days ago = %+v, want 1 turn", d)
	}
	var total float64
	for _, d := range got.Daily {
		total += d.Cost
	}
	if total > 2 {
		t.Errorf("the window sums to %.2f, want under $2 — the 40-day-old turn leaked in", total)
	}
}

// Models and tools are read over the month, ordered by what a reader is looking for: the model
// that costs the most and the tool that runs the most.
func TestOverviewChartDataRanksModelsAndTools(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	// Inside this month whatever day of it today is: activitySince is the same bound the tiles
	// count from, and a minute past it is in the month on the 1st as well as the 28th.
	at := shift(t, activitySince("month"), time.Minute)
	for _, row := range []struct {
		model string
		cost  float64
	}{{"cheap", 0.10}, {"pricey", 5.00}, {"cheap", 0.10}} {
		if _, err := st.db.ExecContext(ctx, `insert into usage (org_id, team_id, channel, model, tokens_in, tokens_out, cost_usd, created_at)
			values (?, 'T1', 'C1', ?, 10, 5, ?, ?)`, orgID, row.model, row.cost, at); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	for _, row := range []struct {
		name string
		ok   int
	}{{"search", 1}, {"search", 1}, {"search", 0}, {"fetch", 1}} {
		if _, err := st.db.ExecContext(ctx, `insert into tool_calls (org_id, team_id, channel, name, args, result, ok, ms, created_at)
			values (?, 'T1', 'C1', ?, '{}', '', ?, 3, ?)`, orgID, row.name, row.ok, at); err != nil {
			t.Fatalf("seed tool_calls: %v", err)
		}
	}

	got := st.OverviewChartData(ctx, orgID, 30)
	if len(got.Models) != 2 || got.Models[0].Model != "pricey" {
		t.Fatalf("Models = %+v, want the dearest first", got.Models)
	}
	if got.Models[1].Turns != 2 {
		t.Errorf("cheap = %+v, want its two turns added up", got.Models[1])
	}
	if len(got.Tools) != 2 || got.Tools[0].Name != "search" || got.Tools[0].Calls != 3 {
		t.Fatalf("Tools = %+v, want the busiest first with all three of its calls", got.Tools)
	}
	if got.Tools[0].Failed != 1 {
		t.Errorf("search failed %d times, want 1", got.Tools[0].Failed)
	}
	if got.Tools[1].Failed != 0 {
		t.Errorf("fetch failed %d times, want 0", got.Tools[1].Failed)
	}
}
