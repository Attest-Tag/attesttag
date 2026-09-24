package app

import (
	"context"
	"testing"
	"time"
)

// The ladder as .env.testing sells it. Every rung above the first must sell more users for more
// money, with more credit and more jobs in it — a STRIPE_SIZES typo that sold 100 users for less
// than 50 would otherwise be found by a customer — and the conversation at the top is listed,
// named, and not for sale.
func TestTheLadderRisesAndEndsInAConversation(t *testing.T) {
	sizes := parseSizes("upto_10=price_a:4900:500,upto_25=price_b:9900:1000,upto_50=price_c:19900:2000," +
		"upto_100=price_d:34900:3500,upto_250=price_e:59900:6000,over_250=:0")
	if len(sizes) != 6 {
		t.Fatalf("got %d sizes, want 6: %+v", len(sizes), sizes)
	}
	for i := 1; i < 5; i++ {
		prev, cur := sizes[i-1], sizes[i]
		if cur.UserLimit <= prev.UserLimit || cur.AmountMinor <= prev.AmountMinor ||
			cur.IncludedMinor <= prev.IncludedMinor || cur.JobLimit <= prev.JobLimit {
			t.Errorf("%s does not rise from %s in every figure:\n  %+v\n  %+v", cur.Key, prev.Key, cur, prev)
		}
		if cur.PriceID == "" {
			t.Errorf("%s has no Price and would be offered as a conversation", cur.Key)
		}
	}
	if sizes[0].JobLimit != 25 || sizes[4].JobLimit != 500 {
		t.Errorf("job figures: bottom %d want 25, top %d want 500", sizes[0].JobLimit, sizes[4].JobLimit)
	}
	top := sizes[5]
	if top.PriceID != "" || top.AmountMinor != 0 || top.Label != "More than 250 users" || top.UserLimit != 0 || top.JobLimit != 0 {
		t.Errorf("over_250 = %+v, want listed under its label with no Price, no ceiling and no jobs figure", top)
	}
}

// The figure printed against the month's jobs follows the plan: a size's own number while the
// subscription is live (comped included), the free plan's when there is no plan, and nothing at
// all — never "unlimited" — for a pro plan granted by hand or a size sold as a conversation.
func TestJobLimitFollowsThePlan(t *testing.T) {
	cases := []struct {
		name string
		acct BillingAccount
		plan string
		want int
	}{
		{"free, never bought", BillingAccount{}, PlanFree, freePlanJobsPerMonth},
		{"pro by hand, no size", BillingAccount{}, PlanPro, 0},
		{"bought upto_50", BillingAccount{Exists: true, Status: "active", Size: "upto_50"}, PlanPro, 100},
		{"comped upto_10", BillingAccount{Exists: true, Status: "comped", Size: "upto_10"}, PlanPro, 25},
		{"a conversation", BillingAccount{Exists: true, Status: "comped", Size: "over_250"}, PlanPro, 0},
		{"a retired rung", BillingAccount{Exists: true, Status: "active", Size: "upto_500"}, PlanPro, 0},
		{"cancelled, back on free", BillingAccount{Exists: true, Status: "canceled", Size: "upto_50"}, PlanFree, freePlanJobsPerMonth},
	}
	for _, c := range cases {
		if got := jobLimitOf(c.acct, c.plan); got != c.want {
			t.Errorf("%s: jobLimitOf = %d, want %d", c.name, got, c.want)
		}
	}
}

// The count behind that figure is this calendar month's jobs for this organisation, whatever
// became of them: a job created last month is not in it, and neither is another tenant's.
func TestJobsThisMonthIsThisTenantsCalendarMonth(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	mk := func(org int64) int64 {
		id, err := st.InsertJob(ctx, &Job{OrgID: org, TeamID: "T1", Channel: "C1", ThreadTS: "1.1", Requester: "U1",
			ConnectionID: 7, Repo: "acme/app", Spec: `{"v":1}`, Engine: "fake", Dispatcher: "fake"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	mk(orgID)
	old := mk(orgID)
	mk(orgID + 1)
	lastMonth := time.Now().UTC().AddDate(0, -1, 0).Format("2006-01-02 15:04:05")
	if _, err := st.db.ExecContext(ctx, `update jobs set created_at=? where id=?`, lastMonth, old); err != nil {
		t.Fatal(err)
	}
	if got := st.JobsThisMonth(ctx, orgID); got != 1 {
		t.Errorf("JobsThisMonth = %d, want 1: one job this month, one last month, one another tenant's", got)
	}
	if got := st.JobsThisMonth(ctx, orgID+1); got != 1 {
		t.Errorf("the other tenant's JobsThisMonth = %d, want 1", got)
	}
}

// What the Billing screen reads: every offered size carries its jobs figure, the month's count
// sits against the figure of the plan the account is on, and an account that never bought a
// plan is held to the free plan's figure. The console draws these and nothing else.
func TestTheBillingScreenCarriesTheJobsFigures(t *testing.T) {
	b, _, org, _ := sizeBot(t, "upto_50", 199)
	b.cfg.StripeSizes = parseSizes("upto_10=price_a:4900:500,upto_50=price_c:19900:2000,over_250=:0")
	v := billingViewFor(t, b, org)
	if v.Jobs.Limit != 100 || v.Jobs.Used != 0 {
		t.Errorf("jobs = %+v on upto_50 with no jobs run, want limit 100 and 0 used", v.Jobs)
	}
	got := map[string]int{}
	for _, s := range v.Sizes {
		got[s.Key] = s.JobsPerMonth
	}
	if got["upto_10"] != 25 || got["upto_50"] != 100 || got["over_250"] != 0 {
		t.Errorf("jobs_per_month by size = %v, want upto_10 25, upto_50 100, over_250 none", got)
	}
	for _, s := range v.Sizes {
		if s.Key == "over_250" && s.Available {
			t.Error("the conversation at the top of the ladder is offered for sale")
		}
	}

	if _, err := b.store.InsertJob(context.Background(), &Job{OrgID: org.ID, TeamID: "T1", Channel: "C1", ThreadTS: "1.1",
		Requester: "U1", ConnectionID: 7, Repo: "acme/app", Spec: `{"v":1}`, Engine: "fake", Dispatcher: "fake"}); err != nil {
		t.Fatal(err)
	}
	if v = billingViewFor(t, b, org); v.Jobs.Used != 1 {
		t.Errorf("after one job, used = %d, want 1", v.Jobs.Used)
	}

	free := testOrg(t, b.store, "Never Bought Ltd")
	if v = billingViewFor(t, b, free); v.Jobs.Limit != freePlanJobsPerMonth || v.Jobs.Used != 0 {
		t.Errorf("a free account reads jobs = %+v, want the free plan's %d and 0 used", v.Jobs, freePlanJobsPerMonth)
	}
}
