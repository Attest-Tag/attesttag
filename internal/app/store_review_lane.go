package app

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"attesttag/internal/review"
)

// The statements the review lane needs beyond the queue itself (store_review_runs.go): what the
// gate reads before it queues a run, what the money check adds up, the checkpoint a run's findings
// are stored in, and what the poster records once GitHub has the review. Every one names the
// organisation; every write a run makes while it holds a lease is fenced on that lease.

// ReviewRunByDedupeKey is the run already queued, or done, for one request on a pull request: a
// redelivered webhook, or two instances racing on one trigger, find it here and go no further —
// before a throttle is counted or money is held for a run that will never be queued.
func (s *Store) ReviewRunByDedupeKey(ctx context.Context, orgID, prID int64, key string) (*ReviewRun, error) {
	r, err := scanReviewRun(s.db.QueryRowContext(ctx, `select `+reviewRunCols+` from review_runs
		where org_id=? and review_pr_id=? and dedupe_key=?`, orgID, prID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// reviewCachedRun is the latest finished review of a pull request under exactly this cache key —
// the same commits, instructions, context, settings, types and engine — other than except, or nil.
// Only a review that was posted or recorded counts: a failed or skipped one answered nothing.
func (s *Store) reviewCachedRun(ctx context.Context, orgID, prID int64, cacheKey string, except int64) (*ReviewRun, error) {
	if cacheKey == "" {
		return nil, nil
	}
	r, err := scanReviewRun(s.db.QueryRowContext(ctx, `select `+reviewRunCols+` from review_runs
		where org_id=? and review_pr_id=? and kind='review' and cache_key=? and status in ('posted','shadow') and id<>?
		order by id desc limit 1`, orgID, prID, cacheKey, except))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// reviewReservedUSD is the money the organisation's running runs other than except are holding:
// what they may still spend, which the month's usage cannot show until they finish and log it.
// A run whose lease lapsed is not counted — its lane is gone, and so is its spending.
func (s *Store) reviewReservedUSD(ctx context.Context, orgID, except int64) (float64, error) {
	var v float64
	err := s.db.QueryRowContext(ctx, `select coalesce(sum(reserved_usd),0) from review_runs
		where org_id=? and status='running' and lease_until>? and id<>?`, orgID, time.Now().UnixNano(), except).Scan(&v)
	return v, err
}

// reviewSpendSince is what code review has spent on one key since a time in the stored format:
// the usage rows its runs log, which are the ones filed under a "github:" conversation.
func (s *Store) reviewSpendSince(ctx context.Context, orgID int64, keyOwner, since string) (float64, error) {
	var v float64
	err := s.db.QueryRowContext(ctx, `select coalesce(sum(cost_usd),0) from usage
		where org_id=? and key_owner=? and channel like 'github:%' and created_at >= ?`, orgID, keyOwner, since).Scan(&v)
	return v, err
}

// newerReviewQueued reports whether a review queued after run is waiting on the same pull request:
// the run that will review the newer head, which makes posting this one's late answer pointless.
func (s *Store) newerReviewQueued(ctx context.Context, orgID, prID, runID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `select count(*) from review_runs
		where org_id=? and review_pr_id=? and kind='review' and status='queued' and cancel=0 and id>?`,
		orgID, prID, runID).Scan(&n)
	return n > 0, err
}

// saveReviewCheckpoint stores what a run's engine came to — its outcome and its new findings — in
// one transaction fenced on the run's lease, and marks the earlier findings it saw again as seen
// by this run. It is written once, after the model work and before anything is posted, which is
// what lets a run that is requeued or taken over after it — GitHub's rate limit, a lane that died
// — post from it rather than pay for the model again, and never store its findings twice. The
// run's head, base and cache key go with it, since the poster and a later cache lookup read them.
// errLeaseLost means another lane has the run, or another run has its pull request — a lane that
// stalled past its lease and woke after a newer run of the same pull request was claimed, whose
// findings that run's dedupe never saw: nothing was written.
func (s *Store) saveReviewCheckpoint(ctx context.Context, r *ReviewRun, outcomeJSON string, found []*ReviewFinding, seen []string) error {
	return s.saveReviewOutcome(ctx, r, outcomeJSON, found, seen, nil)
}

// saveReviewOutcome is saveReviewCheckpoint with what the run decided about earlier findings
// (review_resolve.go), written in the same transaction: a finding moved to its lines at the head,
// closed as fixed or outdated, opened again because the code a re-review found fixed is back, or its
// "fixed in" claim cleared once a check found the problem still there. Each change is made only to a
// finding still in the status the run found it in — open or disputed, or fixed by a re-review for
// one back — so a reply or a person that settled it while the run worked is not overruled;
// StatusApplied says which status changes were written, which is what the lane audits.
func (s *Store) saveReviewOutcome(ctx context.Context, r *ReviewRun, outcomeJSON string, found []*ReviewFinding, seen []string,
	resolved []*reviewResolution) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `update review_runs set outcome_json=?, head_sha=?, base_sha=?, cache_key=?, types_json=?,
		rule_label=?, config_hash=?
		where org_id=? and id=? and lease_until=? and status='running' and `+reviewHeldPR,
		outcomeJSON, r.HeadSHA, r.BaseSHA, r.CacheKey, reviewTypesJSON(r.Types), truncate(r.RuleLabel, 200), r.ConfigHash,
		r.OrgID, r.ID, r.Lease, r.Lease)
	if err := fencedWrite(res, err); err != nil {
		return err
	}
	for _, f := range found {
		if _, err := insertReviewFinding(ctx, tx, r.OrgID, r.ReviewPRID, r.ID, f); err != nil {
			return err
		}
	}
	at := now()
	for _, publicID := range seen {
		// Found again on this head, so it still applies here whatever an earlier post decided.
		if _, err := tx.ExecContext(ctx, `update review_findings set last_run_id=?, possibly_outdated=0, updated_at=?
			where org_id=? and review_pr_id=? and public_id=?`, r.ID, at, r.OrgID, r.ReviewPRID, publicID); err != nil {
			return err
		}
	}
	for _, res := range resolved {
		if err := applyReviewResolution(ctx, tx, r, res, at); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.OutcomeJSON = outcomeJSON
	return nil
}

// applyReviewResolution writes one earlier finding's resolution inside the run's checkpoint.
func applyReviewResolution(ctx context.Context, tx *dbTx, r *ReviewRun, res *reviewResolution, at string) error {
	f := res.F
	if res.Status == review.FindingOpen {
		// Back: open again, from nothing but the fixed a re-review closed it as.
		upd, err := tx.ExecContext(ctx, `update review_findings set status=?, status_reason=?, status_by=?, last_run_id=?, updated_at=?
			where org_id=? and review_pr_id=? and id=? and status=? and status_by=?`,
			string(review.FindingOpen), truncate(res.Reason, 600), reviewResolvedBy, r.ID, at, r.OrgID, r.ReviewPRID, f.ID,
			string(review.FindingFixed), reviewResolvedBy)
		if err != nil {
			return err
		}
		n, err := upd.RowsAffected()
		if err != nil {
			return err
		}
		res.StatusApplied = n > 0
	}
	if a := res.At; a != nil {
		var snippet string
		if a.Snippet != nil && a.Snippet.Start > 0 && len(a.Snippet.Lines) > 0 {
			b, _ := json.Marshal(a.Snippet)
			snippet = string(b)
		}
		// Its lines now belong to this head, and so does its anchor: the two always go together. Moved
		// to another file, its fingerprint names the new path, or the next run's dedupe would not
		// know a candidate there for its duplicate.
		if _, err := tx.ExecContext(ctx, `update review_findings set path=?, start_line=?, line=?, anchor_sha=?, code_hash=?,
			snippet=?, fingerprint=?, possibly_outdated=0, last_run_id=?, updated_at=?
			where org_id=? and review_pr_id=? and id=? and status in ('open','disputed')`,
			a.Path, a.StartLine, a.Line, r.HeadSHA, a.CodeHash, snippet, cmp.Or(a.Fingerprint, f.Fingerprint), r.ID, at,
			r.OrgID, r.ReviewPRID, f.ID); err != nil {
			return err
		}
	}
	if res.Refuted {
		if _, err := tx.ExecContext(ctx, `update review_findings set claimed_fixed_sha='', claimed_by='', last_run_id=?, updated_at=?
			where org_id=? and review_pr_id=? and id=? and status in ('open','disputed')`,
			r.ID, at, r.OrgID, r.ReviewPRID, f.ID); err != nil {
			return err
		}
	}
	if res.Status == "" || res.Status == review.FindingOpen {
		return nil
	}
	upd, err := tx.ExecContext(ctx, `update review_findings set status=?, status_reason=?, status_by=?, last_run_id=?, updated_at=?
		where org_id=? and review_pr_id=? and id=? and status in ('open','disputed')`,
		string(res.Status), truncate(res.Reason, 600), reviewResolvedBy, r.ID, at, r.OrgID, r.ReviewPRID, f.ID)
	if err != nil {
		return err
	}
	n, err := upd.RowsAffected()
	if err != nil {
		return err
	}
	res.StatusApplied = n > 0
	return nil
}

// reviewRunFindings are the findings a run raised itself, oldest first: what its review posts.
func (s *Store) reviewRunFindings(ctx context.Context, orgID, runID int64) ([]*ReviewFinding, error) {
	return s.reviewFindingList(ctx, `select `+reviewFindingCols+` from review_findings
		where org_id=? and first_run_id=? order by id`, orgID, runID)
}

// setReviewFindingPlacement moves a finding between the diff and the summary: off the diff when
// GitHub refused its anchor or its file moved on before it was posted, which outdated says.
func (s *Store) setReviewFindingPlacement(ctx context.Context, orgID, id int64, placement string, outdated bool) error {
	res, err := s.db.ExecContext(ctx, `update review_findings set placement=?, possibly_outdated=?, updated_at=?
		where org_id=? and id=?`, placement, boolInt(outdated), now(), orgID, id)
	return reviewFindingWritten(res, err)
}

// setReviewRunReserved raises what a claimed run holds against the money, when the settings let it
// spend more than it reserved at enqueue: what every other run's check subtracts must be what this
// one may really spend. Fenced on the run's lease.
func (s *Store) setReviewRunReserved(ctx context.Context, r *ReviewRun, usd float64) error {
	res, err := s.db.ExecContext(ctx, `update review_runs set reserved_usd=?
		where org_id=? and id=? and lease_until=? and status='running'`, usd, r.OrgID, r.ID, r.Lease)
	if err := fencedWrite(res, err); err != nil {
		return err
	}
	r.ReservedUSD = usd
	return nil
}

// reviewRunsStarted counts the reviews that started the engine on a pull request, and on its
// repository, since a time in the stored format, other than except: the throttles' measure. Only
// runs that went to the model count — running, or ended posted, recorded or failed — so a burst of
// pushes whose runs stood aside for the newest at claim, a cache hit and a skip cost no place in
// the day: the cap is for a loop that spends, and those spend nothing.
func (s *Store) reviewRunsStarted(ctx context.Context, orgID, prID int64, repo, since string, except int64) (pr, repoN int, err error) {
	err = s.db.QueryRowContext(ctx, `select coalesce(sum(case when review_pr_id=? then 1 else 0 end),0), count(*) from review_runs
		where org_id=? and repo=? and kind='review' and status in ('running','posted','shadow','failed') and started_at>=? and id<>?`,
		prID, orgID, repo, since, except).Scan(&pr, &repoN)
	return pr, repoN, err
}

// reviewFindingsSaid lists a pull request's findings that somebody was shown, oldest first: on GitHub
// — an inline comment, or a run that posted its review or its summary — or, with shadow, in the
// console by a recorded run; and run's own, whatever became of it, since it is the run asking. A
// finding of a run that recorded in shadow, or answered nothing, was said to nobody on GitHub: a
// live review must not drop a problem as "already open" because of it, nor list it in a summary
// nobody saw it in before. A try's findings were said to nobody either, wherever it was recorded:
// they are the tried type's, not the pull request's (reviewTry).
func (s *Store) reviewFindingsSaid(ctx context.Context, orgID, prID int64, shadow bool, run int64) ([]*ReviewFinding, error) {
	shadowStatus := ""
	if shadow {
		shadowStatus = "shadow"
	}
	return s.reviewFindingList(ctx, `select `+reviewFindingCols+` from review_findings
		where org_id=? and review_pr_id=? and (github_comment_id>0 or first_run_id=? or first_run_id in
		  (select id from review_runs where org_id=? and review_pr_id=? and kind='review' and (status in ('posted',?) or github_review_id>0)))
		order by id`, orgID, prID, run, orgID, prID, shadowStatus)
}

// latestReviewOutcome is the pull request's last review that was posted or recorded, which a
// resync renders the summary from; nil when there has been none.
func (s *Store) latestReviewOutcome(ctx context.Context, orgID, prID int64) (*ReviewRun, error) {
	r, err := scanReviewRun(s.db.QueryRowContext(ctx, `select `+reviewRunCols+` from review_runs
		where org_id=? and review_pr_id=? and kind='review' and status in ('posted','shadow')
		order by id desc limit 1`, orgID, prID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ---- catch-up ----

// reviewCatchupConn is one reviewed connection the catch-up walks (review_catchup.go).
type reviewCatchupConn struct {
	OrgID, InstallationID int64
}

// reviewCatchupConnections is every connection the catch-up walks, across the deployment: in an
// organisation's review tree and not stopped, on an installation that organisation still holds and
// GitHub has not suspended — a suspended installation is refused every token, and asking for one
// every ten minutes would only fill the log. In organisation order, so a pass can hold each one to
// its own share of the work.
func (s *Store) reviewCatchupConnections(ctx context.Context) ([]reviewCatchupConn, error) {
	rows, err := s.db.QueryContext(ctx, `select r.org_id, r.installation_id from review_settings r
		join github_installs g on g.org_id=r.org_id and g.installation_id=r.installation_id
		where r.kind='connection' and r.removed_at is null and coalesce(g.status,'active')='active'
		order by r.org_id, r.installation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reviewCatchupConn
	for rows.Next() {
		var c reviewCatchupConn
		if err := rows.Scan(&c.OrgID, &c.InstallationID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// reviewReposOfInstallation is every repository a run has been queued on through one installation.
// It is how the catch-up knows of a repository the organisation never connected or set anything on
// — an installation on every repository of an account reviews one as its pull requests open — which
// neither the connections nor the settings tree would name.
func (s *Store) reviewReposOfInstallation(ctx context.Context, orgID, installationID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `select distinct repo from review_runs where org_id=? and installation_id=?`,
		orgID, installationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return nil, err
		}
		out = append(out, repo)
	}
	return out, rows.Err()
}
