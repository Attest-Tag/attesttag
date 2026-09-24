package app

import (
	"context"
	"testing"
	"time"
)

// Retention deletes an organisation's own records and nothing else. The failure this guards
// against has happened here before in another form — one organisation's retention preference
// deleting another organisation's job data, in the pre-release security review — so the boundary
// is the thing under test, not the sweeping.
func TestRetentionStopsAtTheOrganisation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	old, recent := nowMinus(90*24*time.Hour), nowMinus(time.Hour)

	seed := func(orgID int64, at string) {
		t.Helper()
		team := "T" + itoa(orgID)
		for _, q := range []string{
			`insert into usage (org_id, team_id, channel, cost_usd, created_at) values (?, 'T1', 'C1', 0.01, ?)`,
			`insert into tool_calls (org_id, team_id, channel, thread_ts, name, args, created_at) values (?, 'T1', 'C1', '1.1', 'peek', '{}', ?)`,
			// turns is team-keyed, not org-keyed — reached through teams, like the account
			// deletion reaches it. Seeded that way so the sweep is exercised on both shapes.
		} {
			if _, err := st.db.ExecContext(ctx, q, orgID, at); err != nil {
				t.Fatalf("seeding org %d: %v", orgID, err)
			}
		}
		// turns is keyed by workspace rather than organisation, so it is seeded by team id.
		if _, err := st.db.ExecContext(ctx,
			`insert into turns (team_id, channel, thread_ts, role, content, created_at) values (?, 'C1', '1.1', 'user', 'hi', ?)`,
			team, at); err != nil {
			t.Fatalf("seeding turns for org %d: %v", orgID, err)
		}
	}
	// Each organisation owns the workspace its turns are in.
	for _, o := range []int64{1, 2} {
		if _, err := st.db.ExecContext(ctx,
			`insert into teams (team_id, org_id, name, status) values (?, ?, 'W', 'active')`,
			"T"+itoa(o), o); err != nil {
			t.Fatal(err)
		}
	}
	seed(1, old)
	seed(1, recent)
	seed(2, old) // another tenant, with rows just as old

	count := func(orgID int64) int {
		t.Helper()
		var n, m, o int
		st.db.QueryRowContext(ctx, `select count(*) from usage where org_id=?`, orgID).Scan(&n)
		st.db.QueryRowContext(ctx, `select count(*) from tool_calls where org_id=?`, orgID).Scan(&m)
		st.db.QueryRowContext(ctx, `select count(*) from turns where team_id=?`, "T"+itoa(orgID)).Scan(&o)
		return n + m + o
	}
	if count(1) != 6 || count(2) != 3 {
		t.Fatalf("seed: org1=%d org2=%d", count(1), count(2))
	}

	// Organisation 1 keeps 30 days. Its old rows go; its recent ones stay.
	n, err := st.PurgeOrgData(ctx, 1, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("swept %d rows, want 3", n)
	}
	if got := count(1); got != 3 {
		t.Errorf("org 1 has %d rows left, want its 3 recent ones", got)
	}
	// And organisation 2, which asked for nothing, is untouched.
	if got := count(2); got != 3 {
		t.Errorf("org 2 lost %d rows to another organisation's policy", 3-got)
	}
}

// The current month's usage is protected from the sweep, because MonthSpend sums usage as a
// budget floor. Without this an admin who set the seven-day retention floor would delete most of
// the month's usage and so quadruple the effective monthly budget. A row at the very start of
// this month survives even the shortest policy; one from a previous month still goes.
func TestRetentionKeepsThisMonthsUsage(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.DateTime)
	lastMonth := now.AddDate(0, 0, -40).Format(time.DateTime)
	for _, at := range []string{monthStart, lastMonth} {
		if _, err := st.db.ExecContext(ctx,
			`insert into usage (org_id, team_id, channel, cost_usd, created_at) values (1, 'T1', 'C1', 0.01, ?)`, at); err != nil {
			t.Fatalf("seeding usage at %s: %v", at, err)
		}
	}
	if _, err := st.PurgeOrgData(ctx, 1, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var n int
	st.db.QueryRowContext(ctx, `select count(*) from usage where org_id=1`).Scan(&n)
	if n != 1 {
		t.Fatalf("usage rows left = %d, want 1 (this month kept, a previous month swept)", n)
	}
}

// Nothing is deleted unless somebody asked. The default is 0, and a deployment upgrading into
// this must not find its audit trail being trimmed because of a number nobody set.
func TestRetentionDeletesNothingByDefault(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := st.db.ExecContext(ctx,
		`insert into usage (org_id, team_id, channel, cost_usd, created_at) values (1, 'T1', 'C1', 0.01, ?)`,
		nowMinus(5*365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The default setting.
	if days := (Settings{}).DataRetentionDays; days != 0 {
		t.Fatalf("the zero value of the setting is %d, want 0", days)
	}
	// And the sweep itself refuses a zero or negative window rather than treating it as "now".
	for _, keep := range []time.Duration{0, -time.Hour} {
		if n, err := st.PurgeOrgData(ctx, 1, keep); err != nil || n != 0 {
			t.Errorf("PurgeOrgData(keep=%v) deleted %d rows (err %v)", keep, n, err)
		}
	}
	var left int
	st.db.QueryRowContext(ctx, `select count(*) from usage where org_id=1`).Scan(&left)
	if left != 1 {
		t.Errorf("a five-year-old row was deleted with no policy set")
	}
}

// The setting refuses a number that is more likely a typo than a policy: what a typo costs
// here is the audit trail, and nobody notices until they need it.
func TestRetentionSettingRefusesATypo(t *testing.T) {
	for _, v := range []string{"1", "3", "6", "-1", "abc"} {
		if err := validateSecuritySetting("data_retention_days", v); err == nil {
			t.Errorf("data_retention_days=%q was accepted", v)
		}
	}
	for _, v := range []string{"", "0", "7", "30", "365", "3650"} {
		if err := validateSecuritySetting("data_retention_days", v); err != nil {
			t.Errorf("data_retention_days=%q was refused: %v", v, err)
		}
	}
}
