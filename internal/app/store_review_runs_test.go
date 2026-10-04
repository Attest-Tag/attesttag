package app

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"attesttag/internal/review"
)

// The review lane is what stands between a pull request and two reviews of it, posted by two
// instances that each believed the run was theirs. These run on both dialects; the races are only
// races on Postgres, where claimers really do read one snapshot at once.

func upsertPR(t *testing.T, st *Store, org int64, repo string, n int) *ReviewPR {
	t.Helper()
	p, err := st.UpsertReviewPR(context.Background(), org, ReviewPRFacts{Repo: repo, Number: n, HeadSHA: "head1"})
	if err != nil {
		t.Fatalf("upserting %s#%d for org %d: %v", repo, n, org, err)
	}
	return p
}

func enqueueRun(t *testing.T, st *Store, org int64, pr *ReviewPR, dedupe string) *ReviewRun {
	t.Helper()
	r, fresh, err := st.EnqueueReviewRun(context.Background(), org, ReviewRunRequest{ReviewPRID: pr.ID, Kind: "review",
		Trigger: "open", DedupeKey: dedupe, HeadSHA: "head1", Types: []ReviewRunType{{Key: "general", Version: 1}}})
	if err != nil || !fresh {
		t.Fatalf("enqueueing %s: fresh=%v err=%v", dedupe, fresh, err)
	}
	return r
}

func claimRun(t *testing.T, st *Store) *ReviewRun {
	t.Helper()
	r, err := st.claimReviewRun(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return r
}

// lapseReviewRun ends a run's lease now, the pull request's with it, as a stalled lane's would.
func lapseReviewRun(t *testing.T, st *Store, r *ReviewRun) {
	t.Helper()
	past := time.Now().Add(-time.Second).UnixNano()
	ctx := context.Background()
	if _, err := st.db.ExecContext(ctx, `update review_runs set lease_until=? where id=?`, past, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `update review_prs set lease_until=? where id=?`, past, r.ReviewPRID); err != nil {
		t.Fatal(err)
	}
}

func prLease(t *testing.T, st *Store, prID int64) int64 {
	t.Helper()
	return int64(countWhere(t, st, `select lease_until from review_prs where id=?`, prID))
}

// A thin delivery — a comment, which says nothing of the head or of forks — must not make the
// stored pull request forget what a full one said.
func TestUpsertReviewPRKeepsWhatAThinDeliveryDoesNotSay(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	yes, no := true, false
	first, err := st.UpsertReviewPR(ctx, orgID, ReviewPRFacts{Repo: "Acme/Web", Number: 7, State: "open",
		IsFork: &yes, IsPrivate: &yes, AuthorLogin: "octocat", HeadSHA: "aaa"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Repo != "acme/web" || !first.IsFork || !first.IsPrivate || first.Score != -1 || first.State != "open" {
		t.Errorf("first upsert stored %+v", first)
	}
	thin, err := st.UpsertReviewPR(ctx, orgID, ReviewPRFacts{Repo: "acme/web", Number: 7})
	if err != nil {
		t.Fatal(err)
	}
	if thin.ID != first.ID || !thin.IsFork || !thin.IsPrivate || thin.HeadSHA != "aaa" || thin.AuthorLogin != "octocat" {
		t.Errorf("a thin upsert forgot what was known: %+v", thin)
	}
	moved, err := st.UpsertReviewPR(ctx, orgID, ReviewPRFacts{Repo: "acme/web", Number: 7, State: "closed", IsPrivate: &no, HeadSHA: "bbb"})
	if err != nil {
		t.Fatal(err)
	}
	if moved.State != "closed" || moved.IsPrivate || !moved.IsFork || moved.HeadSHA != "bbb" {
		t.Errorf("a fuller upsert did not take: %+v", moved)
	}
	if _, err := st.UpsertReviewPR(ctx, orgID, ReviewPRFacts{Repo: "acme/web", Number: 7, State: "draft"}); err == nil {
		t.Error("an unknown state was stored")
	}

	if _, err := st.UpsertReviewPR(ctx, orgID, ReviewPRFacts{Repo: "acme/web", Number: 7, State: "merged", HeadSHA: "ccc"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReviewPRSkipReason(ctx, orgID, first.ID, "a draft"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReviewPRSummaryComment(ctx, orgID, first.ID, 555); err != nil {
		t.Fatal(err)
	}
	got, err := st.ReviewPRByNumber(ctx, orgID, "ACME/web", 7)
	if err != nil || got == nil {
		t.Fatalf("by number: %v %v", got, err)
	}
	if got.HeadSHA != "ccc" || got.State != "merged" || got.SkipReason != "a draft" || got.SummaryCommentID != 555 {
		t.Errorf("setters left %+v", got)
	}
}

// A redelivered webhook, or two instances racing on one command, produce one run.
func TestEnqueueReviewRunDedupes(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	pr := upsertPR(t, st, orgID, "acme/web", 7)
	first := enqueueRun(t, st, orgID, pr, "review:head1:general")
	again, fresh, err := st.EnqueueReviewRun(ctx, orgID, ReviewRunRequest{ReviewPRID: pr.ID, Kind: "review", Trigger: "command",
		DedupeKey: "review:head1:general"})
	if err != nil || fresh || again.ID != first.ID || again.Trigger != "open" {
		t.Errorf("a second enqueue of one key: %+v fresh=%v %v", again, fresh, err)
	}
	if first.Repo != "acme/web" || first.PRNumber != 7 || first.Status != "queued" || first.Score != -1 ||
		!slices.Equal(first.Types, []ReviewRunType{{Key: "general", Version: 1}}) || len(first.PublicID) != 32 {
		t.Errorf("enqueued %+v", first)
	}
	for _, bad := range []ReviewRunRequest{
		{ReviewPRID: pr.ID, Kind: "approve", Trigger: "open", DedupeKey: "x"},
		{ReviewPRID: pr.ID, Kind: "review", Trigger: "whenever", DedupeKey: "x"},
		{ReviewPRID: pr.ID, Kind: "review", Trigger: "open"},
	} {
		if _, _, err := st.EnqueueReviewRun(ctx, orgID, bad); !errors.Is(err, ErrReviewRunInvalid) {
			t.Errorf("enqueued %+v: %v", bad, err)
		}
	}
	if _, _, err := st.EnqueueReviewRun(ctx, orgID, ReviewRunRequest{ReviewPRID: 999, Kind: "review", Trigger: "open", DedupeKey: "x"}); !errors.Is(err, ErrReviewPRNotFound) {
		t.Errorf("a run on a pull request nobody has: %v", err)
	}
}

func TestReviewRunClaimRaceHasOneWinner(t *testing.T) {
	st := testStore(t)
	pr := upsertPR(t, st, orgID, "acme/web", 7)
	run := enqueueRun(t, st, orgID, pr, "r1")

	var wg sync.WaitGroup
	var won atomic.Int32
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := st.claimReviewRun(context.Background())
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if r != nil {
				won.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := won.Load(); n != 1 {
		t.Fatalf("%d lanes claimed one run; the pull request would be reviewed %d times", n, n)
	}
	got, _ := st.ReviewRun(context.Background(), orgID, run.ID)
	if got.Attempts != 1 || got.Status != "running" || got.StartedAt == "" {
		t.Errorf("after one claim: %+v", got)
	}
	if prLease(t, st, pr.ID) != got.Lease {
		t.Error("the claim did not take the pull request's lease with the run's own value")
	}
}

// Two runs on one pull request, claimed at once, run one at a time — and the one that has to wait
// has spent nothing by waiting.
func TestReviewRunsOnOnePullRequestRunOneAtATime(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	pr := upsertPR(t, st, orgID, "acme/web", 7)
	a := enqueueRun(t, st, orgID, pr, "a")
	b := enqueueRun(t, st, orgID, pr, "b")

	var wg sync.WaitGroup
	var mu sync.Mutex
	var claimed []*ReviewRun
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := st.claimReviewRun(ctx)
			if err != nil {
				t.Errorf("claim: %v", err)
			}
			if r != nil {
				mu.Lock()
				claimed = append(claimed, r)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(claimed) != 1 {
		t.Fatalf("%d runs of one pull request are running at once", len(claimed))
	}
	waiting := a
	if claimed[0].ID == a.ID {
		waiting = b
	}
	w, _ := st.ReviewRun(ctx, orgID, waiting.ID)
	if w.Status != "queued" || w.Attempts != 0 {
		t.Errorf("the run that waited is %s with %d attempts spent", w.Status, w.Attempts)
	}

	// Once the first finishes, the second is the pull request's.
	if err := st.finishReviewRun(ctx, claimed[0], ReviewRunResult{Status: "posted", Score: 5}); err != nil {
		t.Fatal(err)
	}
	if prLease(t, st, pr.ID) != 0 {
		t.Error("finishing did not let go of the pull request")
	}
	// The waiting run may have been deferred by the race; bring its time forward.
	st.db.ExecContext(ctx, `update review_runs set not_before=0 where id=?`, waiting.ID)
	next := claimRun(t, st)
	if next == nil || next.ID != waiting.ID || next.Attempts != 1 {
		t.Fatalf("after the first finished, claimed %+v", next)
	}
}

// The hard fence: a run claimed while its pull request's lease is held by somebody else goes back
// half a minute out, with the attempt the claim counted given back.
func TestReviewRunDefersWhenThePullRequestIsHeld(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	pr := upsertPR(t, st, orgID, "acme/web", 7)
	run := enqueueRun(t, st, orgID, pr, "r1")
	held := time.Now().Add(time.Minute).UnixNano()
	if _, err := st.db.ExecContext(ctx, `update review_prs set lease_until=? where id=?`, held, pr.ID); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if r := claimRun(t, st); r != nil {
		t.Fatalf("a run was claimed on a pull request somebody holds: %+v", r)
	}
	got, _ := st.ReviewRun(ctx, orgID, run.ID)
	if got.Status != "queued" || got.Attempts != 0 || got.Lease != 0 {
		t.Errorf("the deferred run is %s, attempts %d, lease %d", got.Status, got.Attempts, got.Lease)
	}
	if d := time.Unix(0, got.NotBefore).Sub(before); d < reviewRunDeferral-time.Second || d > reviewRunDeferral+5*time.Second {
		t.Errorf("deferred by %s, want about %s", d, reviewRunDeferral)
	}
	if prLease(t, st, pr.ID) != held {
		t.Error("the deferral touched the holder's lease")
	}
	// Held or not, it is not claimable again before its time.
	if r := claimRun(t, st); r != nil {
		t.Error("a deferred run was claimed before its not_before")
	}
}

// A lane that stalls past its lease has lost the run to the next one. Everything it then tries to
// write must miss: extending the lease, recording a posted review, finishing, putting it back.
func TestReviewRunLapsedLeaseFencesTheOldHolder(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	pr := upsertPR(t, st, orgID, "acme/web", 7)
	enqueueRun(t, st, orgID, pr, "r1")

	first := claimRun(t, st)
	if first == nil {
		t.Fatal("nothing to claim")
	}
	if again := claimRun(t, st); again != nil {
		t.Fatal("a held run was claimed a second time")
	}
	lapseReviewRun(t, st, first)
	second := claimRun(t, st)
	if second == nil || second.ID != first.ID || second.Attempts != 2 {
		t.Fatalf("a lapsed run was not reclaimable: %+v", second)
	}

	stale := *first
	if _, err := st.touchReviewRun(ctx, &stale); !errors.Is(err, errLeaseLost) {
		t.Errorf("old holder's touch: %v", err)
	}
	if err := st.setReviewRunGitHubReview(ctx, &stale, 99); !errors.Is(err, errLeaseLost) {
		t.Errorf("old holder's posted review: %v", err)
	}
	if err := st.finishReviewRun(ctx, &stale, ReviewRunResult{Status: "posted", Score: 5}); !errors.Is(err, errLeaseLost) {
		t.Errorf("old holder's finish: %v", err)
	}
	if err := st.requeueReviewRun(ctx, &stale, time.Now(), "gave up", true); !errors.Is(err, errLeaseLost) {
		t.Errorf("old holder's requeue: %v", err)
	}
	if prLease(t, st, pr.ID) != second.Lease {
		t.Error("the old holder moved the pull request's lease")
	}

	// The new holder's writes land, the touch moving both leases together.
	if cancel, err := st.touchReviewRun(ctx, second); err != nil || cancel {
		t.Fatalf("new holder's touch: %v cancel=%v", err, cancel)
	}
	if prLease(t, st, pr.ID) != second.Lease {
		t.Error("the touch did not carry the pull request's lease with it")
	}
	if err := st.setReviewRunGitHubReview(ctx, second, 99); err != nil {
		t.Fatal(err)
	}
	second.HeadSHA, second.ConfigHash, second.RuleLabel = "head2", "cfg", "any → testing"
	second.Types = []ReviewRunType{{Key: "general", Version: 3}, {Key: "security", Version: 1}}
	res := ReviewRunResult{Status: "posted", FilesReviewed: 4, NotReviewed: []review.NotReviewedFile{{Path: "big.bin", Reason: "binary"}},
		Candidates: 6, Dropped: 4, Kept: 2, Score: 3, Summary: "Adds totals.", Risk: "One P1.", Model: "z-ai/glm",
		TokensIn: 1000, TokensOut: 200, TokensCached: 300, CostUSD: 0.41}
	if err := st.finishReviewRun(ctx, second, res); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ReviewRun(ctx, orgID, second.ID)
	if got.Status != "posted" || got.GitHubReviewID != 99 || got.Score != 3 || got.Kept != 2 || got.CostUSD != 0.41 ||
		got.HeadSHA != "head2" || got.ConfigHash != "cfg" || got.RuleLabel != "any → testing" || len(got.Types) != 2 ||
		got.TokensCached != 300 || got.FinishedAt == "" || got.Lease != 0 ||
		!slices.Equal(got.NotReviewed, []review.NotReviewedFile{{Path: "big.bin", Reason: "binary"}}) {
		t.Errorf("finished run reads %+v", got)
	}
	if prLease(t, st, pr.ID) != 0 {
		t.Error("finishing did not let go of the pull request")
	}
	if err := st.finishReviewRun(ctx, second, res); !errors.Is(err, errLeaseLost) {
		t.Errorf("a second finish: %v", err)
	}
}

// A wait GitHub asked for is not the run failing: it goes back for as long as GitHub said, with
// its attempt given back. A failure that may pass spends the attempt.
func TestReviewRunRequeue(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	pr := upsertPR(t, st, orgID, "acme/web", 7)
	enqueueRun(t, st, orgID, pr, "r1")
	r := claimRun(t, st)
	if err := st.requeueReviewRun(ctx, r, time.Now().Add(-time.Second), "rate limited", false); err != nil {
		t.Fatal(err)
	}
	if prLease(t, st, pr.ID) != 0 {
		t.Error("a requeue kept the pull request")
	}
	r = claimRun(t, st)
	if r == nil || r.Attempts != 1 || r.Error != "rate limited" {
		t.Fatalf("after a free requeue: %+v", r)
	}
	if err := st.requeueReviewRun(ctx, r, time.Now().Add(-time.Second), "GitHub 502", true); err != nil {
		t.Fatal(err)
	}
	if r = claimRun(t, st); r == nil || r.Attempts != 2 {
		t.Fatalf("after a spent requeue: %+v", r)
	}
}

// Closing a pull request stops its runs: a queued one at once, a running one by asking — its lane
// hears at the next touch — and one whose lane died outright.
func TestCancelReviewRuns(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	pr := upsertPR(t, st, orgID, "acme/web", 7)
	other := upsertPR(t, st, orgID, "acme/web", 8)
	running := enqueueRun(t, st, orgID, pr, "running")
	if r := claimRun(t, st); r == nil || r.ID != running.ID {
		t.Fatalf("claimed %+v", r)
	}
	queued := enqueueRun(t, st, orgID, pr, "queued")
	elsewhere := enqueueRun(t, st, orgID, other, "elsewhere")

	n, err := st.CancelReviewRuns(ctx, orgID, pr.ID, "closed")
	if err != nil || n != 2 {
		t.Fatalf("cancel reached %d runs: %v", n, err)
	}
	if q, _ := st.ReviewRun(ctx, orgID, queued.ID); q.Status != "cancelled" || q.FinishedAt == "" {
		t.Errorf("the queued run is %+v", q)
	}
	r, _ := st.ReviewRun(ctx, orgID, running.ID)
	if r.Status != "running" || !r.Cancel {
		t.Errorf("the running run is %s, cancel=%v: it should be asked, not stopped", r.Status, r.Cancel)
	}
	if cancel, err := st.touchReviewRun(ctx, r); err != nil || !cancel {
		t.Errorf("the lane was not told at its touch: %v %v", cancel, err)
	}
	if e, _ := st.ReviewRun(ctx, orgID, elsewhere.ID); e.Status != "queued" {
		t.Errorf("another pull request's run was cancelled: %s", e.Status)
	}
	// Its lane dies; the sweep retires it rather than leaving it running for ever, and the claim
	// never takes it back.
	lapseReviewRun(t, st, r)
	if c := claimRun(t, st); c != nil && c.ID == r.ID {
		t.Fatal("a cancelled run was claimed again")
	}
	if _, err := st.sweepReviewRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ = st.ReviewRun(ctx, orgID, running.ID); r.Status != "cancelled" {
		t.Errorf("the abandoned cancelled run is %s", r.Status)
	}
}

// A run whose last attempt's lane died is retired by the sweep, not left reading as running.
func TestSweepReviewRunsRetiresAbandonedRuns(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	pr := upsertPR(t, st, orgID, "acme/web", 7)
	run := enqueueRun(t, st, orgID, pr, "r1")
	for i := range reviewRunMaxAttempts {
		r := claimRun(t, st)
		if r == nil || r.Attempts != i+1 {
			t.Fatalf("claim %d: %+v", i+1, r)
		}
		lapseReviewRun(t, st, r)
	}
	if r := claimRun(t, st); r != nil {
		t.Fatalf("a run past its attempts was claimed: %+v", r)
	}
	// It names what it retired, as it now stands, for the run's channel to hear how it ended; a
	// second sweep retires nothing.
	retired, err := st.sweepReviewRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 1 || retired[0].ID != run.ID || retired[0].OrgID != orgID || retired[0].Status != "failed" {
		t.Errorf("the sweep returned %+v", retired)
	}
	if again, err := st.sweepReviewRuns(ctx); err != nil || len(again) != 0 {
		t.Errorf("a second sweep returned %+v, %v", again, err)
	}
	got, _ := st.ReviewRun(ctx, orgID, run.ID)
	if got.Status != "failed" || got.Error == "" || got.FinishedAt == "" {
		t.Errorf("abandoned run reads %+v", got)
	}
}

// One organisation runs two reviews at a time and no more, so a busy one does not fill the lane.
func TestReviewRunOrgCap(t *testing.T) {
	st := testStore(t)
	for n := 1; n <= 3; n++ {
		enqueueRun(t, st, orgID, upsertPR(t, st, orgID, "acme/web", n), "r")
	}
	enqueueRun(t, st, 2, upsertPR(t, st, 2, "octo-org/api", 1), "r")
	var orgs []int64
	var mine []*ReviewRun
	for range 4 {
		if r := claimRun(t, st); r != nil {
			orgs = append(orgs, r.OrgID)
			if r.OrgID == orgID {
				mine = append(mine, r)
			}
		}
	}
	slices.Sort(orgs)
	if !slices.Equal(orgs, []int64{orgID, orgID, 2}) {
		t.Fatalf("claimed runs of organisations %v, want two of %d's and the other's", orgs, orgID)
	}
	// A finished run frees its organisation's slot for the third.
	if err := st.finishReviewRun(context.Background(), mine[0], ReviewRunResult{Status: "posted", Score: 5}); err != nil {
		t.Fatal(err)
	}
	if r := claimRun(t, st); r == nil || r.OrgID != orgID {
		t.Errorf("after one finished, claimed %+v", r)
	}
}

// A failed run produced no score, whatever the zero value of the field says.
func TestFinishReviewRunGivesAFailedRunNoScore(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	pr := upsertPR(t, st, orgID, "acme/web", 7)
	enqueueRun(t, st, orgID, pr, "r1")
	r := claimRun(t, st)
	if err := st.finishReviewRun(ctx, r, ReviewRunResult{Status: "failed", Error: "model provider"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ReviewRun(ctx, orgID, r.ID); got.Score != -1 || got.Status != "failed" {
		t.Errorf("a failed run reads score %d, status %s", got.Score, got.Status)
	}
	if err := st.finishReviewRun(ctx, r, ReviewRunResult{Status: "queued"}); !errors.Is(err, ErrReviewRunInvalid) {
		t.Errorf("finishing as queued: %v", err)
	}
}

func TestReviewFindings(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	pr := upsertPR(t, st, orgID, "acme/web", 7)
	run := enqueueRun(t, st, orgID, pr, "r1")
	otherPR := upsertPR(t, st, orgID, "acme/web", 8)
	otherRun := enqueueRun(t, st, orgID, otherPR, "r1")

	in := &ReviewFinding{
		Finding: review.Finding{Path: "src/totals.ts", Side: review.Right, StartLine: 40, Line: 52, Severity: review.P1,
			Category: review.CategoryConcurrency, Title: "Older totals overwrite newer ones", Scenario: "Two saves race.",
			Evidence:   []review.Evidence{{Path: "src/totals.ts", Ref: "head", StartLine: 44, EndLine: 47, Quote: "setTotals(t)"}},
			RuleIDs:    []string{"R2"},
			Suggestion: &review.Suggestion{StartLine: 50, Line: 52, Code: "if (stale) return"},
			ReviewType: "security", AlsoTypes: []string{"general"}},
		AnchorSHA: "head1", CodeHash: "h", Placement: review.PlacementInline, Fingerprint: "fp1", VerifierConfidence: 86,
	}
	f, err := st.InsertReviewFinding(ctx, orgID, pr.ID, run.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != review.FindingOpen || f.FirstRunID != run.ID || f.LastRunID != run.ID || len(f.PublicID) != 32 ||
		f.Title != in.Title || f.Scenario != in.Scenario || f.Suggestion == nil || f.Suggestion.Code != "if (stale) return" ||
		len(f.Evidence) != 1 || f.Evidence[0].Quote != "setTotals(t)" || !slices.Equal(f.RuleIDs, []string{"R2"}) ||
		!slices.Equal(f.TypeKeys(), []string{"security", "general"}) || f.VerifierConfidence != 86 || f.Side != review.Right {
		t.Errorf("stored finding reads %+v", f)
	}
	// A finding lands on the pull request its run belongs to, and nowhere else.
	if _, err := st.InsertReviewFinding(ctx, orgID, pr.ID, otherRun.ID, in); !errors.Is(err, ErrReviewPRNotFound) {
		t.Errorf("a finding from another pull request's run: %v", err)
	}

	second, err := st.InsertReviewFinding(ctx, orgID, pr.ID, run.ID, &ReviewFinding{Finding: review.Finding{Path: "a.go",
		Line: 3, Severity: review.P2, Category: review.CategoryBug, Title: "Off by one"}, Fingerprint: "fp2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetReviewFindingStatus(ctx, orgID, second.ID, review.FindingWithdrawn, "the author showed the guard", "octocat"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReviewFindingStatus(ctx, orgID, second.ID, "deleted", "", ""); !errors.Is(err, ErrReviewRunInvalid) {
		t.Errorf("an unknown status: %v", err)
	}
	open, err := st.OpenReviewFindings(ctx, orgID, pr.ID)
	if err != nil || len(open) != 1 || open[0].ID != f.ID {
		t.Errorf("open findings: %v %v", open, err)
	}
	all, _ := st.ReviewFindings(ctx, orgID, pr.ID)
	if len(all) != 2 {
		t.Errorf("all findings: %d", len(all))
	}
	// A withdrawn finding is still found by its fingerprint, so a re-review does not raise it again.
	w, err := st.ReviewFindingByFingerprint(ctx, orgID, pr.ID, "fp2")
	if err != nil || w == nil || w.Status != review.FindingWithdrawn || w.StatusBy != "octocat" || w.StatusReason == "" {
		t.Errorf("by fingerprint: %+v %v", w, err)
	}
	if none, _ := st.ReviewFindingByFingerprint(ctx, orgID, otherPR.ID, "fp2"); none != nil {
		t.Error("a fingerprint was found on another pull request")
	}

	if err := st.SetReviewFindingComment(ctx, orgID, f.ID, 3131); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReviewFindingThread(ctx, orgID, f.ID, "PRRT_kw"); err != nil {
		t.Fatal(err)
	}
	byComment, err := st.ReviewFindingByComment(ctx, orgID, 3131)
	if err != nil || byComment == nil || byComment.ID != f.ID || byComment.ThreadNodeID != "PRRT_kw" {
		t.Errorf("by comment: %+v %v", byComment, err)
	}
}

// Every statement here names the organisation, so another tenant's ids are as good as missing:
// nothing read, nothing written, nothing cancelled. Both tenants hold acme/web#7.
func TestReviewRunsAndFindingsStayInTheirOrganisation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	const other = int64(2)
	mine := upsertPR(t, st, orgID, "acme/web", 7)
	theirs := upsertPR(t, st, other, "acme/web", 7)
	if mine.ID == theirs.ID {
		t.Fatal("two organisations share one pull request row")
	}
	myRun := enqueueRun(t, st, orgID, mine, "r1")
	theirRun := enqueueRun(t, st, other, theirs, "r1")
	myFinding, err := st.InsertReviewFinding(ctx, orgID, mine.ID, myRun.ID, &ReviewFinding{
		Finding: review.Finding{Path: "a.go", Line: 1, Title: "Mine"}, Fingerprint: "fp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetReviewFindingComment(ctx, orgID, myFinding.ID, 77); err != nil {
		t.Fatal(err)
	}

	if p, _ := st.ReviewPR(ctx, other, mine.ID); p != nil {
		t.Error("read another organisation's pull request by id")
	}
	if r, _ := st.ReviewRun(ctx, other, myRun.ID); r != nil {
		t.Error("read another organisation's run by id")
	}
	if f, _ := st.ReviewFinding(ctx, other, myFinding.ID); f != nil {
		t.Error("read another organisation's finding by id")
	}
	if f, _ := st.ReviewFindingByComment(ctx, other, 77); f != nil {
		t.Error("found another organisation's finding by its comment")
	}
	if f, _ := st.ReviewFindingByFingerprint(ctx, other, mine.ID, "fp"); f != nil {
		t.Error("found another organisation's finding by its fingerprint")
	}
	if list, _ := st.ReviewFindings(ctx, other, mine.ID); len(list) != 0 {
		t.Error("listed another organisation's findings")
	}
	if list, _ := st.ReviewRunsForPR(ctx, other, mine.ID, 10); len(list) != 0 {
		t.Error("listed another organisation's runs")
	}
	for name, err := range map[string]error{
		"skip":    st.SetReviewPRSkipReason(ctx, other, mine.ID, "x"),
		"summary": st.SetReviewPRSummaryComment(ctx, other, mine.ID, 1),
		"status":  st.SetReviewFindingStatus(ctx, other, myFinding.ID, review.FindingWithdrawn, "", ""),
		"comment": st.SetReviewFindingComment(ctx, other, myFinding.ID, 1),
	} {
		if err == nil {
			t.Errorf("%s: wrote another organisation's row", name)
		}
	}
	if _, _, err := st.EnqueueReviewRun(ctx, other, ReviewRunRequest{ReviewPRID: mine.ID, Kind: "review", Trigger: "open", DedupeKey: "x"}); !errors.Is(err, ErrReviewPRNotFound) {
		t.Errorf("queued a run on another organisation's pull request: %v", err)
	}
	if _, err := st.InsertReviewFinding(ctx, other, mine.ID, myRun.ID, &ReviewFinding{Finding: review.Finding{Path: "a.go"}}); !errors.Is(err, ErrReviewPRNotFound) {
		t.Errorf("filed a finding on another organisation's pull request: %v", err)
	}
	if n, _ := st.CancelReviewRuns(ctx, other, mine.ID, "x"); n != 0 {
		t.Errorf("cancelled %d of another organisation's runs", n)
	}
	if r, _ := st.ReviewRun(ctx, orgID, myRun.ID); r.Status != "queued" {
		t.Errorf("my run is %s after the other organisation's cancel", r.Status)
	}
	if r, _ := st.ReviewRun(ctx, other, theirRun.ID); r == nil || r.OrgID != other {
		t.Error("the other organisation lost its own run")
	}
	if p, _ := st.ReviewPR(ctx, orgID, mine.ID); p.HeadSHA != "head1" || p.SummaryCommentID != 0 {
		t.Errorf("my pull request was changed: %+v", p)
	}
}

// What the store functions write goes with the account, like every row naming org_id.
func TestReviewRunRowsGoWithTheOrganisation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	for _, org := range []int64{1, 2} {
		pr := upsertPR(t, st, org, "acme/web", 7)
		run := enqueueRun(t, st, org, pr, "r1")
		if _, err := st.InsertReviewFinding(ctx, org, pr.ID, run.ID, &ReviewFinding{Finding: review.Finding{Path: "a.go", Line: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DeleteOrg(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"review_prs", "review_runs", "review_findings"} {
		if n := countWhere(t, st, `select count(*) from `+table+` where org_id=1`); n != 0 {
			t.Errorf("%s kept %d of the deleted organisation's rows", table, n)
		}
		if n := countWhere(t, st, `select count(*) from `+table+` where org_id=2`); n != 1 {
			t.Errorf("%s has %d of the other organisation's rows, want 1", table, n)
		}
	}
}

// A lane that stalled past its lease, while another run of the same pull request was claimed, still
// matches its own row — nothing touched it — but no longer holds the pull request: its checkpoint
// and its posted review are refused, so it can neither file findings the newer run's dedupe never
// saw nor record a review the newer run would post again.
func TestReviewRunWritesAreFencedOnThePullRequestToo(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	pr := upsertPR(t, st, orgID, "acme/web", 7)
	enqueueRun(t, st, orgID, pr, "r1")
	enqueueRun(t, st, orgID, pr, "r2")
	first := claimRun(t, st)
	lapseReviewRun(t, st, first)
	stale := *first
	stale.Lease = int64(countWhere(t, st, `select lease_until from review_runs where id=?`, first.ID))
	// The stalled run is not claimable again for now, so the claim takes the other one.
	if _, err := st.db.ExecContext(ctx, `update review_runs set not_before=? where id=?`, time.Now().Add(time.Hour).UnixNano(), first.ID); err != nil {
		t.Fatal(err)
	}
	second := claimRun(t, st)
	if second == nil || second.ID == first.ID {
		t.Fatalf("claimed %+v, want the pull request's other run", second)
	}
	found := []*ReviewFinding{{Finding: review.Finding{Path: "a.go", Line: 1, Title: "Late"}, Fingerprint: "fp"}}
	if err := st.saveReviewCheckpoint(ctx, &stale, `{"summary":"late"}`, found, nil); !errors.Is(err, errLeaseLost) {
		t.Errorf("the stalled run's checkpoint: %v", err)
	}
	if err := st.setReviewRunGitHubReview(ctx, &stale, 99); !errors.Is(err, errLeaseLost) {
		t.Errorf("the stalled run's posted review: %v", err)
	}
	if n := countWhere(t, st, `select count(*) from review_findings where review_pr_id=?`, pr.ID); n != 0 {
		t.Errorf("the stalled run filed %d findings", n)
	}
	if err := st.saveReviewCheckpoint(ctx, second, `{"summary":"now"}`, found, nil); err != nil {
		t.Errorf("the holder's checkpoint: %v", err)
	}
}

// A run that answered nothing and posted no review retires the findings it stored, since nobody was
// shown them; one that posted its review keeps them, since they are on GitHub. Ending posted records
// the review on its pull request in the same write.
func TestFinishReviewRunRetiresWhatNobodyWasShown(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	pr := upsertPR(t, st, orgID, "acme/web", 7)
	file := func(r *ReviewRun, title string) {
		t.Helper()
		if err := st.saveReviewCheckpoint(ctx, r, `{}`, []*ReviewFinding{{Finding: review.Finding{Path: "a.go", Line: 1, Title: title},
			Fingerprint: title}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	enqueueRun(t, st, orgID, pr, "failed")
	r := claimRun(t, st)
	file(r, "never shown")
	if err := st.finishReviewRun(ctx, r, ReviewRunResult{Status: "failed", Score: -1}); err != nil {
		t.Fatal(err)
	}
	enqueueRun(t, st, orgID, pr, "posted-then-failed")
	r = claimRun(t, st)
	file(r, "on GitHub")
	if err := st.setReviewRunGitHubReview(ctx, r, 42); err != nil {
		t.Fatal(err)
	}
	if err := st.finishReviewRun(ctx, r, ReviewRunResult{Status: "failed", Score: -1}); err != nil {
		t.Fatal(err)
	}
	enqueueRun(t, st, orgID, pr, "posted")
	r = claimRun(t, st)
	if err := st.finishReviewRun(ctx, r, ReviewRunResult{Status: "posted", Score: 4,
		Reviewed: &ReviewPRReviewed{SHA: "head2", FileHashes: `{"a.go":"h"}`, Score: 4, Automatic: true}}); err != nil {
		t.Fatal(err)
	}
	status := map[string]review.FindingStatus{}
	all, _ := st.ReviewFindings(ctx, orgID, pr.ID)
	for _, f := range all {
		status[f.Title] = f.Status
	}
	if status["never shown"] != review.FindingOutdated || status["on GitHub"] != review.FindingOpen {
		t.Errorf("finding statuses after their runs ended: %v", status)
	}
	if got, _ := st.ReviewPR(ctx, orgID, pr.ID); got.LastReviewedSHA != "head2" || got.ReviewsCount != 1 || got.AutoReviews != 1 ||
		got.Score != 4 || got.FileHashes != `{"a.go":"h"}` || got.LeaseUntil != 0 {
		t.Errorf("the pull request after a posted run: %+v", got)
	}
}

// The lane's own statements name the organisation too, and the money ones most of all: another
// tenant's running reviews, spend, cached results and queued runs on the same acme/web#7 must
// neither refuse this one's reviews nor answer them, and this one's fenced writes, given the other's
// ids, change nothing.
func TestReviewLaneStatementsStayInTheirOrganisation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	const other = int64(2)
	mine := upsertPR(t, st, orgID, "acme/web", 7)
	theirs := upsertPR(t, st, other, "acme/web", 7)
	// The other organisation: a posted run with a cache key and a finding, a running run holding a
	// dollar, a queued one behind it, and spend under its repository.
	if _, _, err := st.EnqueueReviewRun(ctx, other, ReviewRunRequest{ReviewPRID: theirs.ID, Kind: "review", Trigger: "open",
		DedupeKey: "posted", ReservedUSD: 1}); err != nil {
		t.Fatal(err)
	}
	done := claimRun(t, st)
	done.CacheKey = "ck"
	if err := st.saveReviewCheckpoint(ctx, done, `{}`, []*ReviewFinding{{Finding: review.Finding{Path: "a.go", Line: 1, Title: "Theirs"},
		Fingerprint: "fp"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.finishReviewRun(ctx, done, ReviewRunResult{Status: "posted", Score: 3}); err != nil {
		t.Fatal(err)
	}
	enqueueRun(t, st, other, theirs, "running")
	running := claimRun(t, st)
	if _, err := st.db.ExecContext(ctx, `update review_runs set reserved_usd=1 where id=?`, running.ID); err != nil {
		t.Fatal(err)
	}
	queued := enqueueRun(t, st, other, theirs, "queued")
	st.LogUsageBy(ctx, other, "", "github:acme/web", "pr:7", "github:octocat", "m", Usage{In: 10, CostUSD: 3})
	theirFindings, _ := st.ReviewFindings(ctx, other, theirs.ID)
	if len(theirFindings) != 1 || running == nil || running.OrgID != other {
		t.Fatalf("setup: %d findings, running %+v", len(theirFindings), running)
	}
	// What the other organisation's own reads see, so the misses below are misses and not nothing.
	held, _ := st.reviewReservedUSD(ctx, other, 0)
	spent, _ := st.reviewSpendSince(ctx, other, keyOwnerPlatform, "2000-01-01 00:00:00")
	newer, _ := st.newerReviewQueued(ctx, other, theirs.ID, 0)
	nPR, _, _ := st.reviewRunsStarted(ctx, other, theirs.ID, "acme/web", "2000-01-01 00:00:00", 0)
	cached, _ := st.reviewCachedRun(ctx, other, theirs.ID, "ck", 0)
	if held != 1 || spent != 3 || !newer || nPR != 2 || cached == nil {
		t.Fatalf("the other organisation's own view: held %v, spent %v, newer %v, started %d, cached %v", held, spent, newer, nPR, cached != nil)
	}

	if r, _ := st.ReviewRunByDedupeKey(ctx, orgID, theirs.ID, "queued"); r != nil {
		t.Error("found another organisation's run by its dedupe key")
	}
	if r, _ := st.reviewCachedRun(ctx, orgID, theirs.ID, "ck", 0); r != nil {
		t.Error("answered from another organisation's cached review")
	}
	if usd, _ := st.reviewReservedUSD(ctx, orgID, 0); usd != 0 {
		t.Errorf("another organisation's running review holds $%.2f of this one's money", usd)
	}
	if usd, _ := st.reviewSpendSince(ctx, orgID, keyOwnerPlatform, "2000-01-01 00:00:00"); usd != 0 {
		t.Errorf("another organisation's spend counts $%.2f against this one", usd)
	}
	if newer, _ := st.newerReviewQueued(ctx, orgID, theirs.ID, 0); newer {
		t.Error("another organisation's queued run counts as newer")
	}
	if nPR, nRepo, _ := st.reviewRunsStarted(ctx, orgID, theirs.ID, "acme/web", "2000-01-01 00:00:00", 0); nPR+nRepo != 0 {
		t.Errorf("another organisation's reviews count against this one's throttles: %d, %d", nPR, nRepo)
	}
	if fs, _ := st.reviewFindingsSaid(ctx, orgID, theirs.ID, true, done.ID); len(fs) != 0 {
		t.Error("listed another organisation's findings as said")
	}
	if fs, _ := st.reviewRunFindings(ctx, orgID, done.ID); len(fs) != 0 {
		t.Error("listed another organisation's run's findings")
	}
	if r, _ := st.latestReviewOutcome(ctx, orgID, theirs.ID); r != nil {
		t.Error("read another organisation's last review")
	}

	if err := st.setReviewFindingPlacement(ctx, orgID, theirFindings[0].ID, string(review.PlacementSummary), true); !errors.Is(err, ErrReviewFindingNotFound) {
		t.Errorf("moved another organisation's finding: %v", err)
	}
	forged := *running
	forged.OrgID, forged.ReviewPRID = orgID, mine.ID
	for name, err := range map[string]error{
		"checkpoint": st.saveReviewCheckpoint(ctx, &forged, `{}`, nil, nil),
		"review id":  st.setReviewRunGitHubReview(ctx, &forged, 9),
		"reserve":    st.setReviewRunReserved(ctx, &forged, 5),
		"finish":     st.finishReviewRun(ctx, &forged, ReviewRunResult{Status: "failed", Score: -1}),
	} {
		if !errors.Is(err, errLeaseLost) {
			t.Errorf("%s: wrote another organisation's run: %v", name, err)
		}
	}
	got, _ := st.ReviewRun(ctx, other, running.ID)
	if got.Status != "running" || got.ReservedUSD != 1 || got.GitHubReviewID != 0 || got.OutcomeJSON != "{}" {
		t.Errorf("the other organisation's running run was changed: %+v", got)
	}
	if q, _ := st.ReviewRun(ctx, other, queued.ID); q.Status != "queued" {
		t.Errorf("the other organisation's queued run is %s", q.Status)
	}
}
