package app

import (
	"context"
	"time"
)

// The series behind the overview's charts. They are a separate read from OverviewStats because
// they are a separate question: the tiles answer "where does the account stand", which a script
// polling /v1/usage wants every minute, while these answer "what has it been doing", which only
// somebody looking at the page asks. Keeping them apart means the API key's poll does not pay
// for three grouped scans it never reads.
type OverviewCharts struct {
	// One entry per day, oldest first, with no gaps: a day nobody spoke to the bot is a zero
	// rather than a missing point, or the chart would draw a quiet week as a short one.
	Daily []DayUsage `json:"daily"`
	// This month, by model and by tool. Both are what the money and the failures are made of,
	// and neither is visible anywhere else in the console.
	Models []ModelUsage `json:"models"`
	Tools  []ToolUsage  `json:"tools"`
}

// DayUsage is one UTC day of turns. The day is the string the database groups on, not a time,
// so the browser formats it without a zone conversion that would slide the bars by a day.
type DayUsage struct {
	Day   string  `json:"day"` // YYYY-MM-DD, UTC, like every other window this console counts in
	Turns int     `json:"turns"`
	In    int     `json:"in"`
	Out   int     `json:"out"`
	Cost  float64 `json:"cost"`
}

// ModelUsage is what one model cost this month. An empty Model is a row written before the
// column was, or by a caller that did not name one; the console labels it rather than hiding it,
// because its cost is real and has to add up to the tile.
type ModelUsage struct {
	Model string  `json:"model"`
	Turns int     `json:"turns"`
	In    int     `json:"in"`
	Out   int     `json:"out"`
	Cost  float64 `json:"cost"`
}

// ToolUsage is one tool's month: how often the bot reached for it and how often that failed.
// Failures are counted here as well as listed in RecentErrors, because five recent names cannot
// say whether a tool fails twice a month or every other call.
type ToolUsage struct {
	Name   string `json:"name"`
	Calls  int    `json:"calls"`
	Failed int    `json:"failed"`
}

// OverviewChartData reads all three series. days is the width of the daily window, counted back
// from today inclusive, so 30 means the 29 days before today and today.
//
// Every bucket is cut with substr(created_at, 1, 10) rather than a date function: created_at is
// a text column in both dialects, holding "YYYY-MM-DD HH:MM:SS" written in UTC, and substr is
// the one expression SQLite and Postgres spell the same. date(created_at) would work on SQLite
// and fail on Postgres, where the column is not a timestamp.
func (s *Store) OverviewChartData(ctx context.Context, orgID int64, days int) OverviewCharts {
	if days < 1 {
		days = 30
	}
	out := OverviewCharts{Daily: []DayUsage{}, Models: []ModelUsage{}, Tools: []ToolUsage{}}
	first := time.Now().UTC().AddDate(0, 0, -(days - 1))
	from := first.Format("2006-01-02") + " 00:00:00"
	month := time.Now().UTC().Format("2006-01") + "-01 00:00:00"

	byDay := map[string]DayUsage{}
	rows, err := s.db.QueryContext(ctx, `select substr(created_at,1,10) as day, count(*),
		coalesce(sum(tokens_in),0), coalesce(sum(tokens_out),0), coalesce(sum(cost_usd),0)
		from usage where org_id=? and created_at >= ?
		group by substr(created_at,1,10)`, orgID, from)
	if err == nil {
		for rows.Next() {
			var d DayUsage
			if rows.Scan(&d.Day, &d.Turns, &d.In, &d.Out, &d.Cost) == nil {
				byDay[d.Day] = d
			}
		}
		rows.Close()
	}
	// Fill the window from the calendar, not from the rows: the axis is a fixed number of days
	// whatever the database holds, and an account with one busy day gets one bar rather than a
	// chart that looks full.
	for i := 0; i < days; i++ {
		key := first.AddDate(0, 0, i).Format("2006-01-02")
		if d, ok := byDay[key]; ok {
			out.Daily = append(out.Daily, d)
			continue
		}
		out.Daily = append(out.Daily, DayUsage{Day: key})
	}

	rows, err = s.db.QueryContext(ctx, `select coalesce(model,''), count(*),
		coalesce(sum(tokens_in),0), coalesce(sum(tokens_out),0), coalesce(sum(cost_usd),0)
		from usage where org_id=? and created_at >= ?
		group by coalesce(model,'') order by sum(cost_usd) desc limit 8`, orgID, month)
	if err == nil {
		for rows.Next() {
			var m ModelUsage
			if rows.Scan(&m.Model, &m.Turns, &m.In, &m.Out, &m.Cost) == nil {
				out.Models = append(out.Models, m)
			}
		}
		rows.Close()
	}

	// ok is an integer in both dialects rather than a boolean, so the failed count is a sum of a
	// CASE and not count(*) filter (Postgres) or sum(not ok) (SQLite): one expression, both.
	rows, err = s.db.QueryContext(ctx, `select coalesce(name,''), count(*),
		sum(case when ok=0 then 1 else 0 end)
		from tool_calls where org_id=? and created_at >= ?
		group by coalesce(name,'') order by count(*) desc limit 8`, orgID, month)
	if err == nil {
		for rows.Next() {
			var t ToolUsage
			if rows.Scan(&t.Name, &t.Calls, &t.Failed) == nil {
				out.Tools = append(out.Tools, t)
			}
		}
		rows.Close()
	}
	return out
}
