package app

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"attesttag/internal/review"
)

// Code review's work and what it found: the pull requests it knows, the runs queued and done
// against them, and the findings it raised. migrations/*/0027_code_review.sql says what each column
// holds; this file is the statements.
//
// The run queue is a lane like the GitHub inbox (store_github_deliveries.go), with the same claim:
// the outer where repeats the subquery's, so dispatchers polling in the same instant on Postgres
// cannot each take the row the snapshot showed them. Every write after a claim is fenced on the
// lease it took. A review is minutes of model calls ending in a post to somebody's pull request,
// and a lane that stalled past its lease and lost the run to another must find that out at its
// next write rather than post a second review.
//
// On top of that, a pull request has a lease of its own (review_prs.lease_until), taken in the same
// transaction as the claim and with the same value, so one fence covers both. It is what makes "one
// run at a time per pull request" hard rather than likely: the claim's subquery skips a pull request
// with a running run, but on Postgres two claimers reading one snapshot can each pick a different
// run of the same pull request, and the second then finds the pull request's lease taken. Its run
// goes back to the queue half a minute out, and the attempt the claim counted is given back — being
// second in a race is not a failure of the run, and spending an attempt on it would retire a review
// that never ran. The organisation's cap of two running runs is soft for the same snapshot reason:
// concurrent claimers can each pass it, so it can be exceeded by one per claimer, which is the price
// of not taking a lock on every claim. It bounds a busy organisation's share of the lane; the money
// is bounded elsewhere, by what each run reserves before it starts.
const (
	reviewRunLease       = 60 * time.Second
	reviewRunMaxAttempts = 3
	reviewRunDeferral    = 30 * time.Second
	reviewOrgRunning     = 2
	// A dedupe key is built by the caller from what makes two requests one — a head, the types, a
	// comment id — and is never a person's text.
	reviewDedupeKeyMax = 200
)

var (
	// ErrReviewPRNotFound is a pull request that is not this organisation's, or not there.
	ErrReviewPRNotFound = errors.New("no such pull request under review")
	// ErrReviewRunNotFound is the same for a run, and ErrReviewFindingNotFound for a finding.
	ErrReviewRunNotFound     = errors.New("no such review run")
	ErrReviewFindingNotFound = errors.New("no such review finding")
	// ErrReviewRunInvalid is a run, a result or a finding whose fields are not ones the schema
	// documents.
	ErrReviewRunInvalid = errors.New("invalid review run")
)

var (
	// A reply run, and a resync after a finding changed in its thread, have the trigger reply: a
	// person answering one of the review's findings (review_replies.go). A try is a run of an unsaved
	// review type from the console, never the pull request's review (reviewTry). A fix is the review
	// of the commit a fix job pushed to the pull request (review_fix.go).
	reviewRunKinds    = []string{"review", "reply", "answer", "resync", "try"}
	reviewRunTriggers = []string{"open", "push", "command", "catchup", "console", "api", "reply", "label", "chat", "fix"}
	// The statuses a run ends in; queued and running are the lane's own.
	reviewRunEnds  = []string{"posted", "shadow", "noop", "skipped", "superseded", "failed", "cancelled"}
	reviewPRStates = []string{"open", "closed", "merged"}
)

// ---- pull requests ----

// ReviewPR is one pull request as the reviewer knows it: current state, not history.
type ReviewPR struct {
	ID               int64
	OrgID            int64
	Repo             string // owner/name, lower-cased
	Number           int
	State            string // open | closed | merged
	IsFork           bool
	IsPrivate        bool
	AuthorLogin      string
	HeadSHA          string
	LastReviewedSHA  string
	FileHashes       string // JSON, path → hash at LastReviewedSHA
	Paused           bool
	PausedAuto       bool // paused by the ceiling, not by a member (reviewPausedAuto)
	AutoReviews      int
	ReviewsCount     int
	Score            int // -1 before the first finished review
	SummaryCommentID int64
	BodyMirror       string
	SkipReason       string
	LeaseUntil       int64
	UpdatedAt        string
	// The pull request's announcement in a chat channel (review_notify.go): the workspace, the
	// channel and the message, and the last event it carried.
	NotifyTeam    string
	NotifyChannel string
	NotifyTS      string
	NotifyLast    string
}

const reviewPRCols = `id, org_id, repo, pr_number, state, is_fork, is_private, author_login, head_sha, last_reviewed_sha,
	file_hashes, paused, auto_reviews, reviews_count, score, summary_comment_id, body_mirror, skip_reason, lease_until, updated_at,
	notify_team, notify_channel, notify_ts, notify_last`

func scanReviewPR(row interface{ Scan(...any) error }) (*ReviewPR, error) {
	var p ReviewPR
	var fork, private, paused int
	if err := row.Scan(&p.ID, &p.OrgID, &p.Repo, &p.Number, &p.State, &fork, &private, &p.AuthorLogin, &p.HeadSHA,
		&p.LastReviewedSHA, &p.FileHashes, &paused, &p.AutoReviews, &p.ReviewsCount, &p.Score, &p.SummaryCommentID,
		&p.BodyMirror, &p.SkipReason, &p.LeaseUntil, &p.UpdatedAt, &p.NotifyTeam, &p.NotifyChannel, &p.NotifyTS,
		&p.NotifyLast); err != nil {
		return nil, err
	}
	p.IsFork, p.IsPrivate, p.Paused, p.PausedAuto = fork == 1, private == 1, paused != 0, paused == reviewPausedAuto
	return &p, nil
}

// ReviewPRFacts is what a delivery or a read of the pull request says about it. A nil or empty
// field is one the caller does not know — an issue_comment delivery says nothing of the head or of
// whether the branch is a fork — and leaves what is stored alone, so a thin delivery arriving after
// a full one never forgets what the full one said.
type ReviewPRFacts struct {
	Repo        string
	Number      int
	State       string // "" keeps what is stored; a new row starts open
	IsFork      *bool
	IsPrivate   *bool
	AuthorLogin string
	HeadSHA     string
}

// UpsertReviewPR records a pull request, or brings the stored one up to date with what is known of
// it now, and returns it as stored.
func (s *Store) UpsertReviewPR(ctx context.Context, orgID int64, in ReviewPRFacts) (*ReviewPR, error) {
	repo, err := reviewRepoName(in.Repo)
	if err != nil {
		return nil, err
	}
	if in.Number <= 0 || in.State != "" && !slices.Contains(reviewPRStates, in.State) {
		return nil, fmt.Errorf("%w: pull request %d in state %q", ErrReviewRunInvalid, in.Number, in.State)
	}
	known := func(b *bool) (set, val int) {
		if b == nil {
			return 0, 0
		}
		return 1, boolInt(*b)
	}
	forkSet, fork := known(in.IsFork)
	privSet, private := known(in.IsPrivate)
	stateSet := boolInt(in.State != "")
	// The three "is it known" flags travel as integers and are cast where they are read, so
	// Postgres has a type for each parameter without being told the dialect.
	return scanReviewPR(s.db.QueryRowContext(ctx, `insert into review_prs
		(org_id, repo, pr_number, state, is_fork, is_private, author_login, head_sha, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict (org_id, repo, pr_number) do update set
		  state=case when cast(? as integer)=1 then excluded.state else review_prs.state end,
		  is_fork=case when cast(? as integer)=1 then excluded.is_fork else review_prs.is_fork end,
		  is_private=case when cast(? as integer)=1 then excluded.is_private else review_prs.is_private end,
		  author_login=case when excluded.author_login<>'' then excluded.author_login else review_prs.author_login end,
		  head_sha=case when excluded.head_sha<>'' then excluded.head_sha else review_prs.head_sha end,
		  updated_at=excluded.updated_at
		returning `+reviewPRCols,
		orgID, repo, in.Number, cmp.Or(in.State, "open"), fork, private, in.AuthorLogin, in.HeadSHA, now(),
		stateSet, forkSet, privSet))
}

// ReviewPR reads one pull request by id; nil when it is not this organisation's.
func (s *Store) ReviewPR(ctx context.Context, orgID, id int64) (*ReviewPR, error) {
	p, err := scanReviewPR(s.db.QueryRowContext(ctx, `select `+reviewPRCols+` from review_prs
		where org_id=? and id=?`, orgID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// ReviewPRByNumber reads one pull request by its repository and number; nil when unknown.
func (s *Store) ReviewPRByNumber(ctx context.Context, orgID int64, repo string, number int) (*ReviewPR, error) {
	repo, err := reviewRepoName(repo)
	if err != nil {
		return nil, err
	}
	p, err := scanReviewPR(s.db.QueryRowContext(ctx, `select `+reviewPRCols+` from review_prs
		where org_id=? and repo=? and pr_number=?`, orgID, repo, number))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// SetReviewPRSkipReason records why the gate did not review the pull request, which the status
// command and the console say out loud; "" clears it once one is reviewed.
func (s *Store) SetReviewPRSkipReason(ctx context.Context, orgID, id int64, why string) error {
	res, err := s.db.ExecContext(ctx, `update review_prs set skip_reason=?, updated_at=? where org_id=? and id=?`,
		truncate(why, 300), now(), orgID, id)
	return reviewPRWritten(res, err)
}

// review_prs.paused: 0 not paused, reviewPausedMember after a member's `@… pause`, reviewPausedAuto
// after reviewAutoPauseAfter automatic reviews. Who paused is recorded rather than guessed from the
// count, which a member pausing once the fifth review has run would read as the ceiling's — in the
// footer, `status` and the console alike. Every read of it as a flag is paused != 0.
const (
	reviewPausedMember = 1
	reviewPausedAuto   = 2
)

// pauseReviewPR stops the automatic reviews of a pull request — by the ceiling (auto) or by a member
// — and reports whether this call is the one that did: conditional on it not being paused already,
// so two requests racing past the ceiling, or a command beside them, pause it once, and it is
// audited and said once, under whichever got there first.
func (s *Store) pauseReviewPR(ctx context.Context, orgID, id int64, auto bool) (bool, error) {
	by := reviewPausedMember
	if auto {
		by = reviewPausedAuto
	}
	res, err := s.db.ExecContext(ctx, `update review_prs set paused=?, updated_at=? where org_id=? and id=? and paused=0`,
		by, now(), orgID, id)
	if err := fencedWrite(res, err); errors.Is(err, errLeaseLost) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// resumeReviewPR starts a pull request's automatic reviews again, with the count towards the next
// pause back at nothing: a person who resumes them has decided they want the next ones, and a
// count left at five would pause them again at the very next push. The gate's last reason goes with
// the pause when the pause was it, or `status` and the console would go on saying the pull request
// was not reviewed because it is paused; any other reason is still true and stays.
func (s *Store) resumeReviewPR(ctx context.Context, orgID, id int64) error {
	res, err := s.db.ExecContext(ctx, `update review_prs set paused=0, auto_reviews=0,
		skip_reason=case when skip_reason='paused' then '' else skip_reason end, updated_at=? where org_id=? and id=?`,
		now(), orgID, id)
	return reviewPRWritten(res, err)
}

// SetReviewPRSummaryComment records the issue comment the summary lives in, which every later
// review edits in place rather than posting another.
func (s *Store) SetReviewPRSummaryComment(ctx context.Context, orgID, id, commentID int64) error {
	res, err := s.db.ExecContext(ctx, `update review_prs set summary_comment_id=?, updated_at=? where org_id=? and id=?`,
		commentID, now(), orgID, id)
	return reviewPRWritten(res, err)
}

// setReviewPRNotifyRoot records where the pull request's announcement now is, provided it is still
// where the caller read it to be (was): two events racing — a merge delivered while a review ends —
// would otherwise each post a message and keep the last, leaving the first orphaned in the channel.
// It reports whether this write was the one that took; false means somebody else's message is the
// announcement now, and the caller's own is to go.
func (s *Store) setReviewPRNotifyRoot(ctx context.Context, orgID, id int64, team, channel, ts, was string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `update review_prs set notify_team=?, notify_channel=?, notify_ts=?
		where org_id=? and id=? and notify_ts=?`, team, channel, ts, orgID, id, was)
	if err := fencedWrite(res, err); errors.Is(err, errLeaseLost) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// setReviewPRNotified records the last event announced for the pull request, so it is not said
// twice: a merge GitHub delivers again, a review whose announcement was made once already. The merge
// is the last word, and stays recorded: a review that ends after it — one already with the model when
// the pull request was merged — is announced too, and must not make a merge delivered again look new.
func (s *Store) setReviewPRNotified(ctx context.Context, orgID, id int64, last string) error {
	res, err := s.db.ExecContext(ctx, `update review_prs set notify_last=case when notify_last=? then notify_last else ? end
		where org_id=? and id=?`, reviewNoticeMerged, truncate(last, 100), orgID, id)
	return reviewPRWritten(res, err)
}

func reviewPRWritten(res sql.Result, err error) error {
	if err := fencedWrite(res, err); errors.Is(err, errLeaseLost) {
		return ErrReviewPRNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// takeReviewPRLease takes the pull request's lease, only if nobody holds it: the hard half of one
// run at a time per pull request. until is the claim's own lease, so the run and the pull request
// are fenced on one value.
func takeReviewPRLease(ctx context.Context, tx *dbTx, orgID, prID, until int64, at time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `update review_prs set lease_until=?
		where org_id=? and id=? and lease_until<=?`, until, orgID, prID, at.UnixNano())
	if err := fencedWrite(res, err); errors.Is(err, errLeaseLost) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// ---- runs ----

// ReviewRunType is one review type a run used, at the version it used.
type ReviewRunType struct {
	Key     string `json:"key"`
	Version int    `json:"version"`
}

// ReviewRun is one piece of review work: what was asked for, and once it is done, what came of it.
type ReviewRun struct {
	ID         int64
	PublicID   string
	OrgID      int64
	ReviewPRID int64
	Repo       string
	PRNumber   int
	// InstallationID is the GitHub App installation the trigger came through, which every token
	// the run mints is minted from.
	InstallationID int64
	Kind           string // review | reply | answer | resync
	DedupeKey      string
	Trigger        string // open | push | command | catchup | console | api | reply | label
	TriggerRef     string
	RequestedBy    string
	HeadSHA        string
	BaseSHA        string
	CacheKey       string
	Types          []ReviewRunType
	RuleLabel      string
	ConfigHash     string
	Status         string
	NotBefore      int64
	// Lease is lease_until: the value a claim took, which every later write by its holder is
	// fenced on, for the run and its pull request alike.
	Lease          int64
	Attempts       int
	Cancel         bool
	ReservedUSD    float64
	GitHubReviewID int64
	FilesReviewed  int
	NotReviewed    []review.NotReviewedFile
	Candidates     int
	Dropped        int
	Kept           int
	Score          int
	Summary        string
	Risk           string
	Model          string
	TokensIn       int64
	TokensOut      int64
	TokensCached   int64
	CostUSD        float64
	Error          string
	CreatedAt      string
	StartedAt      string
	FinishedAt     string
	// RequestJSON is what was asked for beyond the kind and the trigger, and OutcomeJSON the
	// engine's result once it is checkpointed; the lane reads and writes both (review_lane.go).
	RequestJSON string
	OutcomeJSON string
}

const reviewRunCols = `id, public_id, org_id, review_pr_id, repo, pr_number, installation_id, kind, dedupe_key, trigger,
	trigger_ref, requested_by, head_sha, base_sha, cache_key, types_json, rule_label, config_hash, status, not_before,
	lease_until, attempts, cancel, reserved_usd, github_review_id, files_reviewed, not_reviewed, candidates, dropped, kept,
	score, summary, risk, model, tokens_in, tokens_out, tokens_cached, cost_usd, error, created_at, started_at, finished_at,
	request_json, outcome_json`

// reviewNotReviewed is review.NotReviewedFile as review_runs.not_reviewed stores it.
type reviewNotReviewed struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func scanReviewRun(row interface{ Scan(...any) error }) (*ReviewRun, error) {
	var r ReviewRun
	var types, notReviewed string
	var cancel int
	if err := row.Scan(&r.ID, &r.PublicID, &r.OrgID, &r.ReviewPRID, &r.Repo, &r.PRNumber, &r.InstallationID, &r.Kind,
		&r.DedupeKey, &r.Trigger, &r.TriggerRef, &r.RequestedBy, &r.HeadSHA, &r.BaseSHA, &r.CacheKey, &types, &r.RuleLabel,
		&r.ConfigHash, &r.Status, &r.NotBefore, &r.Lease, &r.Attempts, &cancel, &r.ReservedUSD, &r.GitHubReviewID,
		&r.FilesReviewed, &notReviewed, &r.Candidates, &r.Dropped, &r.Kept, &r.Score, &r.Summary, &r.Risk, &r.Model,
		&r.TokensIn, &r.TokensOut, &r.TokensCached, &r.CostUSD, &r.Error, &r.CreatedAt, &r.StartedAt, &r.FinishedAt,
		&r.RequestJSON, &r.OutcomeJSON); err != nil {
		return nil, err
	}
	r.Cancel = cancel == 1
	// Both columns are written only by this file, so a value that does not decode is a bug to see
	// in the console rather than a run that cannot be read at all.
	json.Unmarshal([]byte(types), &r.Types)
	var nr []reviewNotReviewed
	json.Unmarshal([]byte(notReviewed), &nr)
	for _, f := range nr {
		r.NotReviewed = append(r.NotReviewed, review.NotReviewedFile{Path: f.Path, Reason: f.Reason})
	}
	return &r, nil
}

func reviewTypesJSON(types []ReviewRunType) string {
	if types == nil {
		types = []ReviewRunType{}
	}
	b, _ := json.Marshal(types)
	return string(b)
}

// ReviewRunRequest is a run to queue.
type ReviewRunRequest struct {
	ReviewPRID     int64
	InstallationID int64
	Kind           string
	// DedupeKey is what makes two requests the same request. A second enqueue with the key of a
	// run already on this pull request — a redelivered webhook, two instances racing on one
	// command — returns that run instead of queueing another.
	DedupeKey   string
	Trigger     string
	TriggerRef  string
	RequestedBy string
	HeadSHA     string
	BaseSHA     string
	CacheKey    string
	Types       []ReviewRunType
	RuleLabel   string
	ConfigHash  string
	ReservedUSD float64
	// NotBefore delays the first claim; zero is now.
	NotBefore time.Time
	// RequestJSON is the lane's own record of what was asked for; "" is "{}".
	RequestJSON string
}

// EnqueueReviewRun queues a run on one of the organisation's pull requests. fresh is false when the
// dedupe key was already taken on that pull request, and the run returned is the one that took it.
func (s *Store) EnqueueReviewRun(ctx context.Context, orgID int64, in ReviewRunRequest) (run *ReviewRun, fresh bool, err error) {
	switch {
	case !slices.Contains(reviewRunKinds, in.Kind):
		return nil, false, fmt.Errorf("%w: kind %q", ErrReviewRunInvalid, in.Kind)
	case !slices.Contains(reviewRunTriggers, in.Trigger):
		return nil, false, fmt.Errorf("%w: trigger %q", ErrReviewRunInvalid, in.Trigger)
	case in.DedupeKey == "" || len(in.DedupeKey) > reviewDedupeKeyMax:
		return nil, false, fmt.Errorf("%w: a dedupe key of 1 to %d bytes is required", ErrReviewRunInvalid, reviewDedupeKeyMax)
	case in.ReservedUSD < 0:
		return nil, false, fmt.Errorf("%w: a negative reservation", ErrReviewRunInvalid)
	}
	var notBefore int64
	if !in.NotBefore.IsZero() {
		notBefore = in.NotBefore.UnixNano()
	}
	// The repository and the number are copied from the pull request in the statement that relies
	// on it being this organisation's, so a run can neither land on another tenant's pull request
	// nor disagree with its own about which one it is.
	var id int64
	err = s.db.QueryRowContext(ctx, `insert into review_runs
		(public_id, org_id, review_pr_id, repo, pr_number, installation_id, kind, dedupe_key, trigger, trigger_ref,
		 requested_by, head_sha, base_sha, cache_key, types_json, rule_label, config_hash, not_before, reserved_usd,
		 request_json, created_at)
		select ?, org_id, id, repo, pr_number, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		from review_prs where org_id=? and id=?
		on conflict (org_id, review_pr_id, dedupe_key) do nothing
		returning id`,
		newPublicID(), in.InstallationID, in.Kind, in.DedupeKey, in.Trigger, truncate(in.TriggerRef, 200),
		truncate(in.RequestedBy, 200), in.HeadSHA, in.BaseSHA, in.CacheKey, reviewTypesJSON(in.Types),
		truncate(in.RuleLabel, 200), in.ConfigHash, notBefore, in.ReservedUSD, cmp.Or(in.RequestJSON, "{}"), now(),
		orgID, in.ReviewPRID).Scan(&id)
	switch {
	case err == nil:
		run, err = s.ReviewRun(ctx, orgID, id)
		return run, true, err
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, err
	}
	// No row: the key was taken, or the pull request is not this organisation's.
	run, err = scanReviewRun(s.db.QueryRowContext(ctx, `select `+reviewRunCols+` from review_runs
		where org_id=? and review_pr_id=? and dedupe_key=?`, orgID, in.ReviewPRID, in.DedupeKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrReviewPRNotFound
	}
	return run, false, err
}

// ReviewRun reads one run by id; nil when it is not this organisation's.
func (s *Store) ReviewRun(ctx context.Context, orgID, id int64) (*ReviewRun, error) {
	r, err := scanReviewRun(s.db.QueryRowContext(ctx, `select `+reviewRunCols+` from review_runs
		where org_id=? and id=?`, orgID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ReviewRunsForPR lists a pull request's runs, newest first, at most limit of them.
func (s *Store) ReviewRunsForPR(ctx context.Context, orgID, prID int64, limit int) ([]*ReviewRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `select `+reviewRunCols+` from review_runs
		where org_id=? and review_pr_id=? order by id desc limit ?`, orgID, prID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ReviewRun{}
	for rows.Next() {
		r, err := scanReviewRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// claimReviewRun hands the oldest claimable run to one lane, holding its pull request's lease as
// well, or returns nil when nothing can be claimed now. Claimable is queued, or running under a
// lease that lapsed — its lane died — with attempts left and nobody having asked it to stop; and
// not on a pull request that has a run going, nor for an organisation with reviewOrgRunning going.
func (s *Store) claimReviewRun(ctx context.Context) (*ReviewRun, error) {
	// A run that loses the pull request's lease is deferred, which takes it out of the next claim,
	// so trying again finds a different run. A few tries keep a lane from going idle behind one.
	for range 3 {
		r, deferred, err := s.claimReviewRunOnce(ctx)
		if err != nil || !deferred {
			return r, err
		}
	}
	return nil, nil
}

func (s *Store) claimReviewRunOnce(ctx context.Context) (r *ReviewRun, deferred bool, err error) {
	at := time.Now()
	nowNS, lease := at.UnixNano(), at.Add(reviewRunLease).UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	r, err = scanReviewRun(tx.QueryRowContext(ctx, `update review_runs set status='running', lease_until=?, attempts=attempts+1,
		started_at=case when started_at='' then ? else started_at end
		where id=(select r.id from review_runs r
			where r.status in ('queued','running') and r.cancel=0 and r.lease_until<=? and r.not_before<=? and r.attempts<?
			  and (select count(*) from review_runs o
			       where o.org_id=r.org_id and o.status='running' and o.lease_until>?) < ?
			  and not exists (select 1 from review_runs o
			       where o.org_id=r.org_id and o.review_pr_id=r.review_pr_id and o.status='running' and o.lease_until>?)
			order by r.not_before, r.id limit 1)
		  and status in ('queued','running') and cancel=0 and lease_until<=? and not_before<=? and attempts<?
		returning `+reviewRunCols,
		lease, now(), nowNS, nowNS, reviewRunMaxAttempts, nowNS, reviewOrgRunning, nowNS,
		nowNS, nowNS, reviewRunMaxAttempts))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	took, err := takeReviewPRLease(ctx, tx, r.OrgID, r.ReviewPRID, lease, at)
	if err != nil {
		return nil, false, err
	}
	if !took {
		// Second in a race for this pull request: back in the queue a little later, with the
		// attempt the claim just counted handed back.
		if _, err := tx.ExecContext(ctx, `update review_runs set status='queued', lease_until=0, not_before=?, attempts=attempts-1
			where org_id=? and id=? and lease_until=?`, at.Add(reviewRunDeferral).UnixNano(), r.OrgID, r.ID, lease); err != nil {
			return nil, false, err
		}
		return nil, true, tx.Commit()
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return r, false, nil
}

// touchReviewRun extends the lease of a run still being worked on, the pull request's with it, and
// reports whether somebody has asked the run to stop — the pull request was closed, say. errLeaseLost
// means another lane has the run now: this one stops, and writes nothing more on its behalf.
func (s *Store) touchReviewRun(ctx context.Context, r *ReviewRun) (cancel bool, err error) {
	lease := time.Now().Add(reviewRunLease).UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var c int
	err = tx.QueryRowContext(ctx, `update review_runs set lease_until=?
		where org_id=? and id=? and lease_until=? and status='running' returning cancel`,
		lease, r.OrgID, r.ID, r.Lease).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return false, errLeaseLost
	}
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `update review_prs set lease_until=? where org_id=? and id=? and lease_until=?`,
		lease, r.OrgID, r.ReviewPRID, r.Lease)
	if err := fencedWrite(res, err); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	r.Lease = lease
	return c == 1, nil
}

// setReviewRunGitHubReview records the review a run has just posted, the moment GitHub says so and
// before anything else can go wrong: a run that dies after posting is found by this id, or failing
// that by its marker, and adopts the review instead of posting a second one. Fenced on the pull
// request's lease as well as the run's (reviewHeldPR).
func (s *Store) setReviewRunGitHubReview(ctx context.Context, r *ReviewRun, reviewID int64) error {
	res, err := s.db.ExecContext(ctx, `update review_runs set github_review_id=?
		where org_id=? and id=? and lease_until=? and status='running' and `+reviewHeldPR,
		reviewID, r.OrgID, r.ID, r.Lease, r.Lease)
	if err := fencedWrite(res, err); err != nil {
		return err
	}
	r.GitHubReviewID = reviewID
	return nil
}

// reviewHeldPR is the half of a fence that a run's own lease_until cannot give: that its pull
// request is still held under the same value. A run whose lease lapsed is not touched when another
// run of the same pull request is claimed, so its own row still matches a lane that stalled and
// woke; only the pull request's lease shows that somebody else holds it now. It follows a where on
// review_runs, and takes the lease once more as its argument.
const reviewHeldPR = `exists (select 1 from review_prs p where p.org_id=review_runs.org_id and p.id=review_runs.review_pr_id
	and p.lease_until=?)`

// ReviewRunResult is what a run came to. The run's own HeadSHA, BaseSHA, CacheKey, Types, RuleLabel
// and ConfigHash are written with it — the lane may have settled them while it ran, a command's head
// being read only once the run starts.
type ReviewRunResult struct {
	// Reviewed, set for a review that was posted or recorded, is what its pull request records of
	// it, written in the same transaction as the run's ending: a run that dies between the two would
	// otherwise be resumed and count the same review twice.
	Reviewed       *ReviewPRReviewed
	Status         string // one of reviewRunEnds
	GitHubReviewID int64
	FilesReviewed  int
	NotReviewed    []review.NotReviewedFile
	Candidates     int
	Dropped        int
	Kept           int
	Score          int // -1 for a run that produced none
	Summary        string
	Risk           string
	Model          string
	TokensIn       int64
	TokensOut      int64
	TokensCached   int64
	CostUSD        float64
	Error          string
}

// ReviewPRReviewed is a finished review as its pull request records it: the head it read, the file
// hashes the next review compares against (review.PatchHash, as JSON), the score, and whether it
// was one nobody asked for.
type ReviewPRReviewed struct {
	SHA        string
	FileHashes string
	Score      int
	Automatic  bool
}

// finishReviewRun records how a run ended and lets go of its pull request, and with res.Reviewed
// records the review on the pull request in the same transaction. A run that answered nothing and
// posted no review leaves its findings retired as outdated: nobody was shown them, and a later run
// must be free to raise them and to list them. errLeaseLost means the run is no longer this lane's,
// and nothing was written.
func (s *Store) finishReviewRun(ctx context.Context, r *ReviewRun, res ReviewRunResult) error {
	if !slices.Contains(reviewRunEnds, res.Status) || res.Score < -1 || res.Score > 5 {
		return fmt.Errorf("%w: status %q, score %d", ErrReviewRunInvalid, res.Status, res.Score)
	}
	score := res.Score
	switch res.Status {
	case "failed", "cancelled", "skipped", "superseded":
		// Nothing was reviewed, so there is no score, whatever the zero value of the field says:
		// 0 is a real score — two or more open P0s — and a run that never looked must not read as
		// one that looked and found the worst.
		score = -1
	}
	nr := []reviewNotReviewed{}
	for _, f := range res.NotReviewed {
		nr = append(nr, reviewNotReviewed{Path: f.Path, Reason: f.Reason})
	}
	notReviewed, _ := json.Marshal(nr)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `update review_runs set status=?, lease_until=0, finished_at=?,
		head_sha=?, base_sha=?, cache_key=?, types_json=?, rule_label=?, config_hash=?,
		github_review_id=?, files_reviewed=?, not_reviewed=?, candidates=?, dropped=?, kept=?, score=?,
		summary=?, risk=?, model=?, tokens_in=?, tokens_out=?, tokens_cached=?, cost_usd=?, error=?
		where org_id=? and id=? and lease_until=? and status='running'`,
		res.Status, now(), r.HeadSHA, r.BaseSHA, r.CacheKey, reviewTypesJSON(r.Types), truncate(r.RuleLabel, 200), r.ConfigHash,
		cmp.Or(res.GitHubReviewID, r.GitHubReviewID), res.FilesReviewed, string(notReviewed), res.Candidates, res.Dropped,
		res.Kept, score, res.Summary, res.Risk, res.Model, res.TokensIn, res.TokensOut, res.TokensCached, res.CostUSD,
		truncate(res.Error, 1000), r.OrgID, r.ID, r.Lease)
	if err := fencedWrite(result, err); err != nil {
		return err
	}
	if v := res.Reviewed; v != nil {
		// The gate's last skip is cleared, since the pull request was reviewed after all; fenced on the
		// pull request's lease, which the run holds with the same value as its own.
		upd, err := tx.ExecContext(ctx, `update review_prs set last_reviewed_sha=?, file_hashes=?, score=?,
			reviews_count=reviews_count+1, auto_reviews=auto_reviews+?, skip_reason='', updated_at=?
			where org_id=? and id=? and lease_until=?`,
			v.SHA, cmp.Or(v.FileHashes, "{}"), v.Score, boolInt(v.Automatic), now(), r.OrgID, r.ReviewPRID, r.Lease)
		if err := fencedWrite(upd, err); err != nil {
			return err
		}
	}
	if res.Status == "failed" || res.Status == "cancelled" || res.Status == "skipped" || res.Status == "superseded" {
		if cmp.Or(res.GitHubReviewID, r.GitHubReviewID) == 0 {
			if _, err := tx.ExecContext(ctx, `update review_findings set status='outdated', status_reason=?, status_by='code review',
				updated_at=? where org_id=? and first_run_id=? and status='open' and github_comment_id=0`,
				"never posted: the run that raised it ended "+res.Status, now(), r.OrgID, r.ID); err != nil {
				return err
			}
		}
	}
	if err := releaseReviewPRLease(ctx, tx, r); err != nil {
		return err
	}
	return tx.Commit()
}

// requeueReviewRun puts a run this lane holds back in the queue, to be claimed again from
// notBefore: GitHub asked it to wait (githubRetryError), or something failed that another attempt
// may not hit. spend says whether the claim that took it counts as one of its attempts — a wait
// GitHub asked for is not the run failing, and must not use up the attempts a real failure needs.
func (s *Store) requeueReviewRun(ctx context.Context, r *ReviewRun, notBefore time.Time, why string, spend bool) error {
	refund := 1
	if spend {
		refund = 0
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `update review_runs set status='queued', lease_until=0, not_before=?, error=?,
		attempts=attempts-?
		where org_id=? and id=? and lease_until=? and status='running'`,
		notBefore.UnixNano(), truncate(why, 1000), refund, r.OrgID, r.ID, r.Lease)
	if err := fencedWrite(res, err); err != nil {
		return err
	}
	if err := releaseReviewPRLease(ctx, tx, r); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.Status, r.Lease, r.NotBefore, r.Attempts = "queued", 0, notBefore.UnixNano(), r.Attempts-refund
	return nil
}

// releaseReviewPRLease lets go of the pull request, if this run still holds it. It took it with
// the run's own lease value, and every touch moved both together.
func releaseReviewPRLease(ctx context.Context, tx *dbTx, r *ReviewRun) error {
	_, err := tx.ExecContext(ctx, `update review_prs set lease_until=0 where org_id=? and id=? and lease_until=?`,
		r.OrgID, r.ReviewPRID, r.Lease)
	return err
}

// CancelReviewRuns stops a pull request's runs: the queued ones at once, and the running ones by
// asking — their lane sees it at its next touch, and ends the run itself, so nothing it is halfway
// through posting is cut off by somebody else's write. A running run whose lane has died is
// cancelled outright, since nobody is left to ask. It returns how many runs it reached.
func (s *Store) CancelReviewRuns(ctx context.Context, orgID, prID int64, why string) (int64, error) {
	at := time.Now()
	why = truncate(why, 300)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `update review_runs set status='cancelled', cancel=1, lease_until=0, error=?, finished_at=?
		where org_id=? and review_pr_id=? and (status='queued' or (status='running' and lease_until<=?))`,
		why, now(), orgID, prID, at.UnixNano())
	if err != nil {
		return 0, err
	}
	stopped, _ := res.RowsAffected()
	res, err = tx.ExecContext(ctx, `update review_runs set cancel=1, error=?
		where org_id=? and review_pr_id=? and status='running' and lease_until>? and cancel=0`,
		why, orgID, prID, at.UnixNano())
	if err != nil {
		return 0, err
	}
	asked, _ := res.RowsAffected()
	return stopped + asked, tx.Commit()
}

// sweepReviewRuns retires runs no lane will ever finish: running under a lease that lapsed, with
// their attempts used up or a cancel asked for, so the claim will not take them again and they
// would otherwise read as running in the console for ever. Run by whoever runs the lane, it returns
// the runs it retired, as they now stand, for their pull requests' channels to hear how they ended:
// nobody else is left to say (noticeStartOrphaned).
func (s *Store) sweepReviewRuns(ctx context.Context) ([]*ReviewRun, error) {
	at := time.Now()
	rows, err := s.db.QueryContext(ctx, `update review_runs set status=case when cancel=1 then 'cancelled' else 'failed' end,
		error=case when error='' then 'abandoned: the lane running it stopped reporting back' else error end,
		lease_until=0, finished_at=?
		where status='running' and lease_until<=? and (attempts>=? or cancel=1)
		returning `+reviewRunCols,
		now(), at.UnixNano(), reviewRunMaxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ReviewRun
	for rows.Next() {
		r, err := scanReviewRun(rows)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// reviewTypesAtHead is the review types a pull request's reviews of head have run or are about to,
// other than except's: those of every review of it queued, running, or finished with a result —
// posted, recorded in shadow, or answered from an earlier one. A try is not the pull request's
// review, and a run that was skipped, failed, cancelled or stood aside reviewed nothing. A noop
// counts only as the cache's answer, which records the key it was answered under: a label's review
// that stood aside for another (processReview) also ends noop, with the label's types on it, and if
// the run it stood aside for then failed, counting it would leave those types never reviewed at this
// head however often the label went back on.
func (s *Store) reviewTypesAtHead(ctx context.Context, orgID, prID int64, head string, except int64) ([]string, error) {
	return s.reviewTypesWhere(ctx, orgID, prID, head, except,
		`status in ('queued','running','posted','shadow') or (status='noop' and cache_key<>'')`)
}

// reviewLabelTypesReviewed is the review types labels' own reviews of head have reviewed — finished
// with a result, as reviewTypesAtHead counts one — for an opening claimed after a label's review of
// the same head ran: what the label added is reviewed there already (processReview).
func (s *Store) reviewLabelTypesReviewed(ctx context.Context, orgID, prID int64, head string, except int64) ([]string, error) {
	return s.reviewTypesWhere(ctx, orgID, prID, head, except,
		`trigger='label' and (status in ('posted','shadow') or (status='noop' and cache_key<>''))`)
}

// reviewTypesWhere is the type keys of a pull request's reviews of head, other than except's, whose
// row meets cond — one of the two Go constants above, never anything a caller was given.
func (s *Store) reviewTypesWhere(ctx context.Context, orgID, prID int64, head string, except int64, cond string) ([]string, error) {
	if head == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `select types_json from review_runs
		where org_id=? and review_pr_id=? and kind='review' and head_sha=? and id<>? and (`+cond+`)`, orgID, prID, head, except)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var types []ReviewRunType
		json.Unmarshal([]byte(raw), &types) // written only by this file
		for _, t := range types {
			if !slices.Contains(out, t.Key) {
				out = append(out, t.Key)
			}
		}
	}
	return out, rows.Err()
}

// ---- findings ----

// ReviewFinding is one problem a review raised, as stored. Finding holds what the finder said;
// Scenario is stored as the comment's body. Two of its fields are not kept: Symbol, which lives on
// in Fingerprint, and the finder's own Confidence, which ranked candidates for the verifier and is
// spent once they are verified.
type ReviewFinding struct {
	ID         int64
	PublicID   string
	OrgID      int64
	ReviewPRID int64
	FirstRunID int64
	LastRunID  int64
	review.Finding
	AnchorSHA          string
	CodeHash           string
	Placement          review.Placement
	Kind               string
	Fingerprint        string
	VerifierConfidence int
	Status             review.FindingStatus
	StatusReason       string
	StatusBy           string
	ClaimedFixedSHA    string
	ClaimedBy          string
	GitHubCommentID    int64
	ThreadNodeID       string
	BotReplies         int
	// PossiblyOutdated is a finding kept off the diff because its file changed after the commit
	// it was found on and before it could be posted (review_post.go).
	PossiblyOutdated bool
	// Place is where the engine put it (reviewWhere*), which says which heading of the summary
	// lists a finding that is not on the diff.
	Place string
	// Snippet is the masked code the finding points at (review_findings.snippet), for the summary.
	Snippet   *review.Snippet
	CreatedAt string
	UpdatedAt string
}

var reviewFindingStatuses = []review.FindingStatus{review.FindingOpen, review.FindingWithdrawn, review.FindingFixed,
	review.FindingResolved, review.FindingOutdated, review.FindingDisputed, review.FindingAcknowledged}

const reviewFindingCols = `id, public_id, org_id, review_pr_id, first_run_id, last_run_id, review_type, review_types, path,
	side, start_line, line, anchor_sha, code_hash, placement, kind, severity, category, title, body, suggestion, evidence,
	rule_refs, pre_existing, fingerprint, verifier_confidence, status, status_reason, status_by, claimed_fixed_sha,
	claimed_by, github_comment_id, thread_node_id, bot_replies, created_at, updated_at, possibly_outdated, place, snippet`

func scanReviewFinding(row interface{ Scan(...any) error }) (*ReviewFinding, error) {
	var f ReviewFinding
	var types, suggestion, evidence, rules, snippet string
	var side, severity, category, placement, status string
	var pre, outdated int
	if err := row.Scan(&f.ID, &f.PublicID, &f.OrgID, &f.ReviewPRID, &f.FirstRunID, &f.LastRunID, &f.ReviewType, &types,
		&f.Path, &side, &f.StartLine, &f.Line, &f.AnchorSHA, &f.CodeHash, &placement, &f.Kind, &severity, &category,
		&f.Title, &f.Scenario, &suggestion, &evidence, &rules, &pre, &f.Fingerprint, &f.VerifierConfidence, &status,
		&f.StatusReason, &f.StatusBy, &f.ClaimedFixedSHA, &f.ClaimedBy, &f.GitHubCommentID, &f.ThreadNodeID,
		&f.BotReplies, &f.CreatedAt, &f.UpdatedAt, &outdated, &f.Place, &snippet); err != nil {
		return nil, err
	}
	f.Side, f.Severity, f.Category = review.Side(side), review.Severity(severity), review.Category(category)
	f.Placement, f.Status, f.PreExisting = review.Placement(placement), review.FindingStatus(status), pre == 1
	f.PossiblyOutdated = outdated == 1
	// Written only here, so a column that does not decode is left empty rather than failing the read.
	var keys []string
	json.Unmarshal([]byte(types), &keys)
	for _, k := range keys {
		if k != f.ReviewType {
			f.AlsoTypes = append(f.AlsoTypes, k)
		}
	}
	if suggestion != "" {
		var sg review.Suggestion
		if json.Unmarshal([]byte(suggestion), &sg) == nil {
			f.Suggestion = &sg
		}
	}
	if evidence != "" {
		json.Unmarshal([]byte(evidence), &f.Evidence)
	}
	json.Unmarshal([]byte(rules), &f.RuleIDs)
	if snippet != "" {
		var sn review.Snippet
		if json.Unmarshal([]byte(snippet), &sn) == nil && sn.Start > 0 && len(sn.Lines) > 0 {
			f.Snippet = &sn
		}
	}
	return &f, nil
}

// InsertReviewFinding stores a finding raised by run on a pull request, both the organisation's;
// its public id is made here. A zero Status is open.
func (s *Store) InsertReviewFinding(ctx context.Context, orgID, prID, runID int64, f *ReviewFinding) (*ReviewFinding, error) {
	id, err := insertReviewFinding(ctx, s.db, orgID, prID, runID, f)
	if err != nil {
		return nil, err
	}
	return s.ReviewFinding(ctx, orgID, id)
}

// insertReviewFinding is InsertReviewFinding inside whichever handle the caller is in: the lane
// stores a run's findings in the same transaction as its outcome (saveReviewCheckpoint).
func insertReviewFinding(ctx context.Context, q reviewQuerier, orgID, prID, runID int64, f *ReviewFinding) (int64, error) {
	status := cmp.Or(f.Status, review.FindingOpen)
	if !slices.Contains(reviewFindingStatuses, status) || f.Path == "" || runID <= 0 {
		return 0, fmt.Errorf("%w: a finding needs a path, a run and a known status, not %q", ErrReviewRunInvalid, status)
	}
	types, _ := json.Marshal(f.TypeKeys())
	ruleIDs := f.RuleIDs
	if ruleIDs == nil {
		ruleIDs = []string{}
	}
	rules, _ := json.Marshal(ruleIDs)
	var suggestion, evidence string
	if f.Suggestion != nil {
		b, _ := json.Marshal(f.Suggestion)
		suggestion = string(b)
	}
	if len(f.Evidence) > 0 {
		b, _ := json.Marshal(f.Evidence)
		evidence = string(b)
	}
	var snippet string
	if f.Snippet != nil && f.Snippet.Start > 0 && len(f.Snippet.Lines) > 0 {
		b, _ := json.Marshal(f.Snippet)
		snippet = string(b)
	}
	at := now()
	var id int64
	// The pull request and the run are both checked in the statement that relies on them: the
	// finding lands on this organisation's pull request, raised by one of that pull request's runs.
	err := q.QueryRowContext(ctx, `insert into review_findings
		(public_id, org_id, review_pr_id, first_run_id, last_run_id, review_type, review_types, path, side, start_line,
		 line, anchor_sha, code_hash, placement, kind, severity, category, title, body, suggestion, evidence, rule_refs,
		 pre_existing, fingerprint, verifier_confidence, status, status_reason, status_by, possibly_outdated, place,
		 snippet, created_at, updated_at)
		select ?, org_id, id, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		from review_prs p where p.org_id=? and p.id=?
		  and exists (select 1 from review_runs r where r.org_id=p.org_id and r.id=? and r.review_pr_id=p.id)
		returning id`,
		newPublicID(), runID, runID, cmp.Or(f.ReviewType, review.DefaultType), string(types), f.Path,
		string(cmp.Or(f.Side, review.Right)), f.StartLine, f.Line, f.AnchorSHA, f.CodeHash, string(f.Placement), f.Kind,
		string(f.Severity), string(f.Category), f.Title, f.Scenario, suggestion, evidence, string(rules),
		boolInt(f.PreExisting), f.Fingerprint, f.VerifierConfidence, string(status), f.StatusReason, f.StatusBy,
		boolInt(f.PossiblyOutdated), truncate(f.Place, 40), snippet, at, at, orgID, prID, runID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrReviewPRNotFound
	}
	return id, err
}

// reviewFindingWhere reads one finding of the organisation's matching cond, which follows the
// organisation in the where and may end in an order by.
func (s *Store) reviewFindingWhere(ctx context.Context, orgID int64, cond string, args ...any) (*ReviewFinding, error) {
	f, err := scanReviewFinding(s.db.QueryRowContext(ctx, `select `+reviewFindingCols+` from review_findings
		where org_id=? and `+cond, append([]any{orgID}, args...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return f, err
}

// ReviewFinding reads one finding by id; nil when it is not this organisation's.
func (s *Store) ReviewFinding(ctx context.Context, orgID, id int64) (*ReviewFinding, error) {
	return s.reviewFindingWhere(ctx, orgID, `id=?`, id)
}

// ReviewFindingByComment is the finding an inline comment was posted for, which is how a reply in
// its thread finds what it is replying about.
func (s *Store) ReviewFindingByComment(ctx context.Context, orgID, commentID int64) (*ReviewFinding, error) {
	if commentID <= 0 {
		return nil, nil
	}
	return s.reviewFindingWhere(ctx, orgID, `github_comment_id=? order by id desc limit 1`, commentID)
}

// ReviewFindingByFingerprint is the latest finding on a pull request with this fingerprint, in any
// status, withdrawn included: a re-review that finds it again must see that it was posted, or that
// somebody argued it away, and not raise it a second time either way.
func (s *Store) ReviewFindingByFingerprint(ctx context.Context, orgID, prID int64, fingerprint string) (*ReviewFinding, error) {
	if fingerprint == "" {
		return nil, nil
	}
	return s.reviewFindingWhere(ctx, orgID, `review_pr_id=? and fingerprint=? order by id desc limit 1`, prID, fingerprint)
}

// ReviewFindings lists a pull request's findings in every status, oldest first, which is what the
// summary is rendered from.
func (s *Store) ReviewFindings(ctx context.Context, orgID, prID int64) ([]*ReviewFinding, error) {
	return s.reviewFindingList(ctx, `select `+reviewFindingCols+` from review_findings
		where org_id=? and review_pr_id=? order by id`, orgID, prID)
}

// OpenReviewFindings lists the findings still standing on a pull request — open, or disputed, which
// the bot still stands by — oldest first: what the score counts and a re-review dedupes against.
func (s *Store) OpenReviewFindings(ctx context.Context, orgID, prID int64) ([]*ReviewFinding, error) {
	return s.reviewFindingList(ctx, `select `+reviewFindingCols+` from review_findings
		where org_id=? and review_pr_id=? and status in ('open','disputed') order by id`, orgID, prID)
}

func (s *Store) reviewFindingList(ctx context.Context, q string, args ...any) ([]*ReviewFinding, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ReviewFinding{}
	for rows.Next() {
		f, err := scanReviewFinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetReviewFindingStatus records what happened to a finding — withdrawn after a reply, fixed by a
// push — with why and on whose word.
func (s *Store) SetReviewFindingStatus(ctx context.Context, orgID, id int64, status review.FindingStatus, reason, by string) error {
	if !slices.Contains(reviewFindingStatuses, status) {
		return fmt.Errorf("%w: finding status %q", ErrReviewRunInvalid, status)
	}
	res, err := s.db.ExecContext(ctx, `update review_findings set status=?, status_reason=?, status_by=?, updated_at=?
		where org_id=? and id=?`, string(status), truncate(strings.TrimSpace(reason), 600), truncate(by, 200), now(), orgID, id)
	return reviewFindingWritten(res, err)
}

// SetReviewFindingComment records the inline comment a finding was posted as.
func (s *Store) SetReviewFindingComment(ctx context.Context, orgID, id, commentID int64) error {
	res, err := s.db.ExecContext(ctx, `update review_findings set github_comment_id=?, updated_at=? where org_id=? and id=?`,
		commentID, now(), orgID, id)
	return reviewFindingWritten(res, err)
}

// SetReviewFindingThread records the review thread a finding's comment opened. A REST comment's
// node id is not the thread's, so this arrives separately — from a thread event, or GraphQL.
func (s *Store) SetReviewFindingThread(ctx context.Context, orgID, id int64, threadNodeID string) error {
	res, err := s.db.ExecContext(ctx, `update review_findings set thread_node_id=?, updated_at=? where org_id=? and id=?`,
		truncate(threadNodeID, 200), now(), orgID, id)
	return reviewFindingWritten(res, err)
}

func reviewFindingWritten(res sql.Result, err error) error {
	if err := fencedWrite(res, err); errors.Is(err, errLeaseLost) {
		return ErrReviewFindingNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// ---- the console's reads ----

// ReviewRunByPublicID reads one run by the id the console and the API know it by; nil when it is
// not this organisation's.
func (s *Store) ReviewRunByPublicID(ctx context.Context, orgID int64, publicID string) (*ReviewRun, error) {
	r, err := scanReviewRun(s.db.QueryRowContext(ctx, `select `+reviewRunCols+` from review_runs
		where org_id=? and public_id=?`, orgID, publicID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// ReviewRunFilter narrows the console's list of runs. Kinds are the run kinds to list, none
// meaning reviews and tries; Before is the public id of the last run of the page before, which is
// where this page starts — a public id rather than the serial, so the cursor says nothing about how
// many runs anybody else has had.
type ReviewRunFilter struct {
	Repo   string
	PR     int
	Status string
	Kinds  []string
	Before string
	Limit  int
}

// ReviewRunsPage lists the organisation's runs newest first, one page of them.
func (s *Store) ReviewRunsPage(ctx context.Context, orgID int64, f ReviewRunFilter) ([]*ReviewRun, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	kinds := f.Kinds
	if len(kinds) == 0 {
		kinds = []string{"review", "try"}
	}
	q := `select ` + reviewRunCols + ` from review_runs where org_id=?`
	args := []any{orgID}
	q += ` and kind in (` + strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",") + `)`
	for _, k := range kinds {
		args = append(args, k)
	}
	if f.Repo != "" {
		q += ` and repo=?`
		args = append(args, strings.ToLower(strings.TrimSpace(f.Repo)))
	}
	if f.PR > 0 {
		q += ` and pr_number=?`
		args = append(args, f.PR)
	}
	if f.Status != "" {
		q += ` and status=?`
		args = append(args, f.Status)
	}
	if f.Before != "" {
		// A cursor that names no run of this organisation ends the list rather than restarting it.
		q += ` and id < coalesce((select c.id from review_runs c where c.org_id=? and c.public_id=?), 0)`
		args = append(args, orgID, f.Before)
	}
	q += ` order by id desc limit ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ReviewRun{}
	for rows.Next() {
		r, err := scanReviewRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// reviewRunsOpenFindings counts, for each of runIDs, the findings that run raised itself and that
// are still open now, in one read for a page of the console's list. A run's kept count says what it
// found; this says how much of that still stands once replies, pushes and people have had their say.
func (s *Store) reviewRunsOpenFindings(ctx context.Context, orgID int64, runIDs []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(runIDs) == 0 {
		return out, nil
	}
	args := []any{orgID, string(review.FindingOpen)}
	for _, id := range runIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `select first_run_id, count(*) from review_findings
		where org_id=? and status=? and first_run_id in (`+strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",")+`)
		group by first_run_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ReviewPRsOfRepo lists what the reviewer knows of one repository's pull requests, by number.
func (s *Store) ReviewPRsOfRepo(ctx context.Context, orgID int64, repo string) (map[int]*ReviewPR, error) {
	rows, err := s.db.QueryContext(ctx, `select `+reviewPRCols+` from review_prs where org_id=? and repo=?`,
		orgID, strings.ToLower(strings.TrimSpace(repo)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]*ReviewPR{}
	for rows.Next() {
		p, err := scanReviewPR(rows)
		if err != nil {
			return nil, err
		}
		out[p.Number] = p
	}
	return out, rows.Err()
}

// latestReviewRunsOfRepo is each of a repository's pull requests' newest review, by pull request
// id, in one read: the console lists a repository's open pull requests with how each last went,
// and a query per pull request would be a hundred of them.
func (s *Store) latestReviewRunsOfRepo(ctx context.Context, orgID int64, repo string) (map[int64]*ReviewRun, error) {
	repo = strings.ToLower(strings.TrimSpace(repo))
	rows, err := s.db.QueryContext(ctx, `select `+reviewRunCols+` from review_runs
		where org_id=? and repo=? and kind='review' and id in
		  (select max(l.id) from review_runs l where l.org_id=? and l.repo=? and l.kind='review' group by l.review_pr_id)`,
		orgID, repo, orgID, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*ReviewRun{}
	for rows.Next() {
		r, err := scanReviewRun(rows)
		if err != nil {
			return nil, err
		}
		out[r.ReviewPRID] = r
	}
	return out, rows.Err()
}
