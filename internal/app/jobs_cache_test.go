package app

import (
	"context"
	"strings"
	"testing"
)

// A fix job re-sends its whole conversation every turn, so most of its input is the same prefix
// again, and what the provider served from its cache is most of what the job cost. The cached
// count must survive the wire, the events, the finish and the pricing.
func TestJobCachedTokensRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	id, err := st.InsertJob(ctx, &Job{OrgID: orgID, TeamID: "T1", Channel: "C1", ThreadTS: "1.1", ConnectionID: 7, Repo: "acme/app",
		BaseBranch: "main", Spec: `{"v":1}`, Engine: "fake", Model: "m", BudgetUSD: 3, TimeoutS: 600, Dispatcher: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	_, used, err := st.AddJobEvents(ctx, orgID, id, []JobEvent{{Seq: 1, Kind: JobKindUsage, Usage: &JobUsage{In: 1000, Cached: 800, Out: 50}}})
	if err != nil || used.Cached != 800 {
		t.Fatalf("events: used=%+v err=%v", used, err)
	}
	evs, _ := st.JobEvents(ctx, orgID, id, 10)
	if len(evs) != 1 || evs[0].Usage == nil || evs[0].Usage.Cached != 800 {
		t.Fatalf("event read back: %+v", evs)
	}
	delta, ok, err := st.FinishJob(ctx, orgID, id, JobSucceeded, &JobResult{Usage: JobUsage{In: 3000, Cached: 2700, Out: 90}})
	if err != nil || !ok || delta.Cached != 1900 || delta.In != 2000 {
		t.Fatalf("finish: delta=%+v ok=%v err=%v", delta, ok, err)
	}
	j, _ := st.Job(ctx, orgID, id)
	if j.TokensIn != 3000 || j.TokensCached != 2700 || j.TokensOut != 90 {
		t.Errorf("job row: in=%d cached=%d out=%d", j.TokensIn, j.TokensCached, j.TokensOut)
	}
	// The usage row a job writes carries the cached part too, so the usage page's cache rate
	// counts fix jobs instead of reading them as never cached.
	if u := delta.usage(); u.CachedIn != 1900 {
		t.Errorf("usage from a job delta: %+v", u)
	}
}

// Cached prompt tokens are priced as cache reads, which for a long job is most of the difference
// between a few dollars and a few cents.
func TestJobPriceCountsTheCache(t *testing.T) {
	p := JobPrice{InPerM: 1, CachedPerM: 0.1, OutPerM: 4}
	got := p.Cost(JobUsage{In: 10_000_000, Cached: 9_000_000, Out: 100_000})
	// 1M fresh at $1/M + 9M cached at $0.10/M + 0.1M out at $4/M = 1 + 0.9 + 0.4.
	if got < 2.299 || got > 2.301 {
		t.Errorf("cost = %g, want 2.30", got)
	}
}

func TestJobTokensWordsShowsTheCacheShare(t *testing.T) {
	if got := jobTokensWords(9_842_872, 8_900_000, 74_103); got != "9.8M in (90% cached) / 74k out" {
		t.Errorf("got %q", got)
	}
	if got := jobTokensWords(1200, 0, 300); strings.Contains(got, "cached") {
		t.Errorf("nothing cached should not say so: %q", got)
	}
	if got := jobTokensWords(0, 0, 0); got != "" {
		t.Errorf("no tokens: %q", got)
	}
}

// worker_providers names OpenRouter providers in order; it reaches the job's constraints as a
// list, and anything that is not a provider slug is refused at the setting.
func TestWorkerProvidersSetting(t *testing.T) {
	for _, ok := range []string{"", "deepinfra", "deepinfra, novita/fp8", "z-ai\nmistral"} {
		if err := validateWorkerSetting("worker_providers", ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"deep infra!", "https://evil.example", strings.Repeat("a,", 11)} {
		if err := validateWorkerSetting("worker_providers", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	c := (&JobRunner{}).constraints(Settings{WorkerProviders: "DeepInfra, novita/fp8", WorkerEngine: "pi"}, nil, "")
	if len(c.Providers) != 2 || c.Providers[0] != "deepinfra" || c.Providers[1] != "novita/fp8" || c.Engine != "pi" {
		t.Errorf("constraints = %+v", c)
	}
}
