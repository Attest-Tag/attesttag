package app

// The figure a per-user size is charged on. These tests are about the two things that would be
// expensive to get wrong: counting somebody who is not a person, and losing the record.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// activeCfg sells the user ladder with its ceilings, so the over-size decision has something to
// decide against. Its own config rather than billingCfg()'s, because the limits are the subject.
func activeCfg() Config {
	return Config{
		StripeSecretKey: "sk_test_x", StripeWebhookSecret: testWhsec, BillingCurrency: "usd",
		StripeSizes: []Size{
			{Key: "upto_10", PriceID: "price_10", AmountMinor: 4900, UserLimit: 10, Label: sizeLabels["upto_10"]},
			{Key: "unlimited", PriceID: "price_max", AmountMinor: 250000, Label: sizeLabels["unlimited"]},
		},
	}
}

func activeBot(t *testing.T, cfg Config) (*Bot, *Store) {
	t.Helper()
	st := testStore(t)
	return &Bot{cfg: cfg, store: st, settings: newSettingsCache(st, cfg)}, st
}

// markOn writes a row for a day other than today, which MarkActive cannot do — it always writes
// the day it is called on, which is the point of it.
func markOn(t *testing.T, st *Store, orgID int64, teamID, user, day, emailHashHex string) {
	t.Helper()
	at := day + " 12:00:00"
	if _, err := st.db.ExecContext(context.Background(),
		`insert into active_users (org_id, identity, day, team_id, email_hash, turns, first_seen, last_seen)
		 values (?, ?, ?, ?, ?, 1, ?, ?)`,
		orgID, teamID+":"+user, day, teamID, emailHashHex, at, at); err != nil {
		t.Fatalf("could not seed an active user: %v", err)
	}
}

func activeCount(t *testing.T, st *Store, orgID int64) int {
	t.Helper()
	n, err := st.ActiveUsers(context.Background(), orgID, activeUserWindow)
	if err != nil {
		t.Fatalf("could not count active users: %v", err)
	}
	return n
}

// ---- who counts ----

// The rule the turn path applies, read directly. Each of these three would be a person on an
// invoice who is not a person.
func TestOnlyPeopleCountAsUsers(t *testing.T) {
	const bot = "U_BOT"
	for _, c := range []struct {
		user string
		want bool
		why  string
	}{
		{"U_REAL", true, "somebody in a connected workspace"},
		{"W_GRID", true, "an Enterprise Grid id is still a person"},
		{bot, false, "the bot's own id, which a self-test turn is rewritten to"},
		{"email:C123", false, "the forwarded-mail lane is keyed per channel, so it is nobody"},
		{"", false, "no id at all"},
	} {
		if got := countsAsUser(c.user, bot); got != c.want {
			t.Errorf("countsAsUser(%q) = %v, want %v — %s", c.user, got, c.want, c.why)
		}
	}
}

// A routine runs under the Slack id of whoever created it. It never reaches incoming(), which is
// the only thing that marks anybody, so a routine that has been posting a digest every morning
// since its author left does not keep them billable. Asserted because the property is a
// consequence of where the call site is, and a later refactor could move it without noticing.
func TestTheOnlyCallSiteIsTheHumanTurnPath(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var where []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, "b.markActive(") {
				where = append(where, fmt.Sprintf("%s:%d", f, i+1))
			}
		}
	}
	if len(where) != 1 {
		t.Fatalf("markActive is called from %v; it must be called from exactly one place — "+
			"incoming(), which routines, the playground and investigations never reach, which is "+
			"what makes machine activity not a user without a flag anybody has to set", where)
	}
	if !strings.HasPrefix(where[0], "bot.go:") {
		t.Errorf("markActive is called from %s, not bot.go — if the turn path moved, check that "+
			"the new home is still past every gate a refused turn hits", where[0])
	}
}

// ---- counting ----

func TestAPersonIsOneUserHoweverManyTurns(t *testing.T) {
	_, st := activeBot(t, activeCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Busy Ltd")

	for i := 0; i < 7; i++ {
		if err := st.MarkActive(ctx, ActiveMark{OrgID: org.ID, TeamID: "T1", SlackUser: "U1"}); err != nil {
			t.Fatalf("mark %d: %v", i, err)
		}
	}
	if n := activeCount(t, st, org.ID); n != 1 {
		t.Fatalf("seven turns by one person counted %d users, want 1", n)
	}
	rows, err := st.ActiveUserList(ctx, org.ID, activeUserWindow, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list returned %d rows (%v), want 1", len(rows), err)
	}
	if rows[0].Turns != 7 {
		t.Errorf("turns on the row is %d, want 7 — the count is per person, the turns say why", rows[0].Turns)
	}
	if rows[0].SlackUser != "U1" || rows[0].Identity != "T1:U1" {
		t.Errorf("identity %q / user %q — it must carry the workspace, never a bare id", rows[0].Identity, rows[0].SlackUser)
	}
}

// The reason a hash is stored at all. Slack ids are unique inside one workspace and nowhere else,
// so the same human in two connected workspaces is two ids and would otherwise be two users.
func TestOneHumanInTwoWorkspacesIsOneUser(t *testing.T) {
	_, st := activeBot(t, activeCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Two Workspaces Ltd")

	st.MarkActive(ctx, ActiveMark{OrgID: org.ID, TeamID: "T1", SlackUser: "U1", Email: "Sam@Example.com"})
	st.MarkActive(ctx, ActiveMark{OrgID: org.ID, TeamID: "T2", SlackUser: "U9", Email: " sam@example.com "})
	if n := activeCount(t, st, org.ID); n != 1 {
		t.Fatalf("one person in two workspaces counted %d users, want 1", n)
	}

	// And the breakdown deliberately does not sum to it: they are in both, and an admin looking
	// at why the total is smaller than the parts needs to see exactly that.
	byTeam, err := st.ActiveUsersByTeam(ctx, org.ID, activeUserWindow)
	if err != nil || len(byTeam) != 2 {
		t.Fatalf("by-team returned %d rows (%v), want 2", len(byTeam), err)
	}
}

// Without users:read.email there is no address to dedupe on, and the honest answer is to
// over-count rather than to guess. Written down so that it is a known limit and not a surprise.
func TestWithoutAnEmailTheSameHumanCountsTwice(t *testing.T) {
	_, st := activeBot(t, activeCfg())
	ctx := context.Background()
	org := testOrg(t, st, "No Scope Ltd")

	st.MarkActive(ctx, ActiveMark{OrgID: org.ID, TeamID: "T1", SlackUser: "U1"})
	st.MarkActive(ctx, ActiveMark{OrgID: org.ID, TeamID: "T2", SlackUser: "U9"})
	if n := activeCount(t, st, org.ID); n != 2 {
		t.Fatalf("counted %d, want 2 — with no address there is nothing to dedupe on", n)
	}
}

// An address learned later must attach to the row already there, or one person splits in two on
// the day their workspace was granted the scope.
func TestAnAddressLearnedLaterIsKept(t *testing.T) {
	_, st := activeBot(t, activeCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Late Scope Ltd")

	st.MarkActive(ctx, ActiveMark{OrgID: org.ID, TeamID: "T1", SlackUser: "U1"})
	st.MarkActive(ctx, ActiveMark{OrgID: org.ID, TeamID: "T1", SlackUser: "U1", Email: "sam@example.com"})
	// And a later mark without one must not wipe it: a users.info that failed for a moment
	// should not split somebody into two people.
	st.MarkActive(ctx, ActiveMark{OrgID: org.ID, TeamID: "T1", SlackUser: "U1"})

	st.MarkActive(ctx, ActiveMark{OrgID: org.ID, TeamID: "T2", SlackUser: "U9", Email: "sam@example.com"})
	if n := activeCount(t, st, org.ID); n != 1 {
		t.Fatalf("counted %d, want 1 — the address was learned on the second turn and must stick", n)
	}
}

func TestTheWindowRolls(t *testing.T) {
	_, st := activeBot(t, activeCfg())
	org := testOrg(t, st, "Rolling Ltd")
	day := func(ago int) string { return time.Now().UTC().AddDate(0, 0, -ago).Format(time.DateOnly) }

	markOn(t, st, org.ID, "T1", "U_RECENT", day(29), "")
	markOn(t, st, org.ID, "T1", "U_GONE", day(31), "")
	if n := activeCount(t, st, org.ID); n != 1 {
		t.Fatalf("counted %d, want 1 — 29 days ago is inside the window and 31 is not", n)
	}
}

// ---- the record surviving ----

// The whole reason this is a table and not a query over `usage`. A tenant may set
// data_retention_days to seven; if that took the user count with it, a customer could shrink the
// figure they are billed on by shortening their own retention.
func TestActiveUsersSurviveTheTenantsRetentionPolicy(t *testing.T) {
	_, st := activeBot(t, activeCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Short Retention Ltd")
	day := func(ago int) string { return time.Now().UTC().AddDate(0, 0, -ago).Format(time.DateOnly) }

	markOn(t, st, org.ID, "T1", "U1", day(20), "")
	// Dated well before this month: usage from the current month is deliberately protected from
	// the sweep (it is what MonthSpend, a budget floor, reads — see TestRetentionKeepsThisMonthsUsage),
	// so the control row here has to be old enough that the policy still clears it.
	if _, err := st.db.ExecContext(ctx,
		`insert into usage (org_id, team_id, channel, user_id, model, tokens_in, tokens_out, cost_usd, created_at)
		 values (?, 'T1', 'C1', 'U1', 'm', 1, 1, 0.01, ?)`, org.ID, day(45)+" 12:00:00"); err != nil {
		t.Fatalf("could not seed usage: %v", err)
	}

	if _, err := st.PurgeOrgData(ctx, org.ID, 7*24*time.Hour); err != nil {
		t.Fatalf("retention sweep failed: %v", err)
	}

	var usageLeft int
	st.db.QueryRowContext(ctx, `select count(*) from usage where org_id=?`, org.ID).Scan(&usageLeft)
	if usageLeft != 0 {
		t.Fatalf("the sweep left %d usage rows; the premise of this test is that it clears them", usageLeft)
	}
	if n := activeCount(t, st, org.ID); n != 1 {
		t.Fatalf("a seven-day retention policy erased the user count: %d, want 1", n)
	}
}

// It is not kept forever, though. Its own ceiling, swept from the billing loop.
func TestActiveUsersHaveTheirOwnCeiling(t *testing.T) {
	_, st := activeBot(t, activeCfg())
	org := testOrg(t, st, "Old Ltd")
	old := time.Now().UTC().AddDate(0, 0, -500).Format(time.DateOnly)
	markOn(t, st, org.ID, "T1", "U_ANCIENT", old, "")

	st.SweepActiveUsers(context.Background(), activeUserRetention)
	var left int
	st.db.QueryRowContext(context.Background(), `select count(*) from active_users where org_id=?`, org.ID).Scan(&left)
	if left != 0 {
		t.Fatalf("%d rows survived a 400-day sweep at 500 days old", left)
	}
}

func TestDeletingAnOrgRemovesItsActiveUsers(t *testing.T) {
	_, st := activeBot(t, activeCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Leaving Ltd")
	st.MarkActive(ctx, ActiveMark{OrgID: org.ID, TeamID: "T1", SlackUser: "U1"})

	if _, err := st.DeleteOrg(ctx, org.ID); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	var left int
	st.db.QueryRowContext(ctx, `select count(*) from active_users where org_id=?`, org.ID).Scan(&left)
	if left != 0 {
		t.Fatalf("%d active_users rows outlived the organisation", left)
	}
}

// ---- the size, and being over it ----

func TestSizeUserLimitsComeFromTheCodeNotTheEnvironment(t *testing.T) {
	sizes := parseSizes("upto_10=price_a:2500:500,unlimited=price_b:250000:25000,mystery=price_c:100")
	if len(sizes) != 3 {
		t.Fatalf("parsed %d sizes, want 3", len(sizes))
	}
	if sizes[0].UserLimit != 10 {
		t.Errorf("upto_10 has UserLimit %d, want 10", sizes[0].UserLimit)
	}
	if sizes[1].UserLimit != 0 {
		t.Errorf("unlimited has UserLimit %d, want 0 — it is sold without a ceiling", sizes[1].UserLimit)
	}
	if sizes[2].UserLimit != 0 {
		t.Errorf("a size nobody gave a limit has UserLimit %d, want 0", sizes[2].UserLimit)
	}
}

func TestOverSizeIsDecidedByTheServer(t *testing.T) {
	b, st := activeBot(t, activeCfg())
	ctx := context.Background()
	org := testOrg(t, st, "Growing Ltd")
	day := time.Now().UTC().Format(time.DateOnly)

	for _, u := range []string{"U1", "U2", "U3", "U4", "U5", "U6", "U7", "U8", "U9", "U10", "U11"} {
		markOn(t, st, org.ID, "T1", u, day, "")
	}

	// No subscription: nothing to be over. Telling a free account to upgrade from a plan it has
	// not bought would be the wrong sentence on the wrong screen.
	if v := b.activeUsers(ctx, org.ID, false); v.Users != 11 || v.Limit != 0 || v.Over {
		t.Fatalf("free account: %+v, want 11 users, no limit, not over", v)
	}
	if v := b.activeUsers(ctx, org.ID, false); v.WindowDays != 30 {
		t.Errorf("window is %d days, want 30 — every surface reads it off the wire", v.WindowDays)
	}

	if err := st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_g", SubscriptionID: "sub_g",
		Status: "active", Size: "upto_10", Quantity: 1, Currency: "usd"}); err != nil {
		t.Fatalf("could not record the subscription: %v", err)
	}
	if v := b.activeUsers(ctx, org.ID, false); !v.Over || v.Limit != 10 {
		t.Fatalf("on upto_10 with eleven people: %+v, want over with a limit of 10", v)
	}

	// A size with no ceiling is never over, whatever the figure.
	if err := st.PutSubscription(ctx, org.ID, SubscriptionState{CustomerID: "cus_g", SubscriptionID: "sub_g",
		Status: "active", Size: "unlimited", Quantity: 1, Currency: "usd"}); err != nil {
		t.Fatalf("could not move the size: %v", err)
	}
	if v := b.activeUsers(ctx, org.ID, false); v.Over || v.Limit != 0 {
		t.Fatalf("on unlimited: %+v, want a limit of 0 and not over", v)
	}
}

func TestEmailHashIgnoresCaseAndSpace(t *testing.T) {
	if emailHash("") != "" {
		t.Error("an empty address must hash to empty, or every person without one dedupes onto the same row")
	}
	if emailHash(" Sam@Example.COM ") != emailHash("sam@example.com") {
		t.Error("the same address written two ways must be one person")
	}
	if emailHash("a@x.com") == emailHash("b@x.com") {
		t.Error("two addresses collided")
	}
}

// One organisation's people must never be counted into another's.
func TestActiveUsersAreScopedToOneOrganisation(t *testing.T) {
	_, st := activeBot(t, activeCfg())
	ctx := context.Background()
	mine := testOrg(t, st, "Mine Ltd")
	theirs := testOrg(t, st, "Theirs Ltd")

	st.MarkActive(ctx, ActiveMark{OrgID: mine.ID, TeamID: "T1", SlackUser: "U1"})
	for _, u := range []string{"U7", "U8", "U9"} {
		st.MarkActive(ctx, ActiveMark{OrgID: theirs.ID, TeamID: "T2", SlackUser: u})
	}
	if n := activeCount(t, st, mine.ID); n != 1 {
		t.Fatalf("counted %d for my organisation, want 1", n)
	}
	if n := activeCount(t, st, theirs.ID); n != 3 {
		t.Fatalf("counted %d for the other organisation, want 3", n)
	}
}
