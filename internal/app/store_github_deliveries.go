package app

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The GitHub inbox: webhook deliveries held, sealed, until a dispatcher has done what they ask.
//
// It is the Slack inbox (store_slack_deliveries.go) with what that one learned already in it. The
// handler answers GitHub inside its ten-second window by writing the delivery here and nothing
// else, so a slow database, or a container replaced between receipt and dispatch, never turns into
// a delivery GitHub believes failed. A dispatch is routing, not reviewing: it updates the stored
// installation, or turns the delivery into a review_runs row for the review lane, whose own lease
// and fence cover the minutes a review takes. GitHub does not retry a failed delivery on its own —
// only a person pressing Redeliver does — so a delivery this table refuses is, as far as the
// reviewer is concerned, gone; the caps are therefore per organisation first, for the reason the
// Slack ones are: one busy tenant filling a small global queue refused everybody else.
//
// Two things here go further than the Slack table, both because what a dispatch does is acted on
// elsewhere — an installation revoked, a review enqueued that will post to somebody's pull
// request — and doing it twice, or on a lease somebody else now holds, is not harmless:
//
//   - Every write after the claim is fenced on the lease the claim took (lease_until=?), touch,
//     finish and fail alike. A dispatcher that stalled past its lease — a database or GitHub
//     having a bad moment mid-dispatch — and lost the row to another must find out at its next
//     write, not overwrite the new holder's lease or mark finished work it no longer owns.
//     investigations_store.go keys those writes by id alone, which is the shape not to copy.
//   - A failure backs off. The lease is also the row's not-before, so a retry waits out a database
//     or GitHub outage instead of spending its five attempts inside one minute.
const (
	githubPendingLimit        = 4096
	githubOrgPendingLimit     = 64
	githubDeliveryLease       = 60 * time.Second
	githubDeliveryMaxAttempts = 5
	// A finished or dead delivery's receipt is kept a week. GitHub offers Redeliver on a delivery for
	// a few days after it was sent, and a redelivery carries the same delivery id, so the receipt is
	// what stops a person's well-meant click from reviewing the same push twice.
	githubDeliveryKeep = 7 * 24 * time.Hour
)

// errGitHubInboxFull is the inbox refusing a new delivery because the queue, or this
// organisation's share of it, is full. The handler answers 503, which GitHub records as a failed
// delivery somebody can redeliver once the backlog has drained.
var errGitHubInboxFull = errors.New("GitHub delivery inbox full")

// errLeaseLost is a fenced write that matched nothing: the lease this process held has run out
// and another holder has the work, or it was finished or dead-lettered meanwhile. Whoever gets it
// has stopped owning the work and must not post, finish or retry anything on its behalf.
var errLeaseLost = errors.New("lease lost: another worker holds this now")

type githubDelivery struct {
	ID                    string
	OrgID, InstallationID int64
	Event, Action         string
	Payload               []byte // sealed; the dispatcher opens it
	Lease                 int64  // the lease_until this claim took, which every later write is fenced on
	Attempts              int
}

// enqueueGitHubDelivery stores one delivery for dispatch. payload is the body already sealed under
// MASTER_KEY. fresh is false when the delivery id is already here: a redelivery, which the handler
// answers exactly as it answers a new one — the work is queued or done either way. That answer
// holds while the inbox is full, too: the dedup is checked before the caps, so a redelivery of
// something already accepted is never refused for a lack of room it does not need.
//
// The caps are counted and then inserted, so two deliveries arriving together on Postgres can both
// see room for one and the organisation ends one over. They bound a backlog, they do not meter
// anything, and one over is not worth a lock on every webhook.
func (s *Store) enqueueGitHubDelivery(ctx context.Context, id string, orgID, installationID int64, event, action string, payload []byte) (fresh bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var found int
	err = tx.QueryRowContext(ctx, `select 1 from github_deliveries where delivery_id=?`, id).Scan(&found)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var total, perOrg int
	if err := tx.QueryRowContext(ctx, `select count(*), coalesce(sum(case when org_id=? then 1 else 0 end),0)
		from github_deliveries where done_at=0 and dead_at=0`, orgID).Scan(&total, &perOrg); err != nil {
		return false, err
	}
	if total >= githubPendingLimit || perOrg >= githubOrgPendingLimit {
		return false, errGitHubInboxFull
	}
	// on conflict, because the check above read before this insert: a concurrent insert of the same
	// delivery may have landed in between, and that one is a duplicate, not an error.
	res, err := tx.ExecContext(ctx, `insert into github_deliveries
		(delivery_id, org_id, installation_id, event, action, payload_enc, accepted_at)
		values (?, ?, ?, ?, ?, ?, ?) on conflict (delivery_id) do nothing`,
		id, orgID, installationID, event, action, payload, time.Now().UnixNano())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// claimGitHubDelivery takes the oldest delivery nobody holds and hands it to one dispatcher only.
// A claim is an attempt, so a delivery that keeps failing runs out of them rather than being
// re-claimed forever.
//
// The outer where repeats the subquery's, for the reason claimSlackDelivery spells out: on
// Postgres the subquery reads the snapshot the statement began with, so dispatchers polling in
// the same instant all pick the same row, queue on its lock, and then re-check only the outer
// conditions against the row as the first of them left it. With the id alone there, each of them
// would claim it in turn and the pull request would get one review per dispatcher.
func (s *Store) claimGitHubDelivery(ctx context.Context) (*githubDelivery, error) {
	now := time.Now()
	lease := now.Add(githubDeliveryLease).UnixNano()
	var d githubDelivery
	err := s.db.QueryRowContext(ctx, `update github_deliveries set lease_until=?, attempts=attempts+1
		where delivery_id=(select delivery_id from github_deliveries
			where done_at=0 and dead_at=0 and attempts<? and lease_until<=?
			order by accepted_at limit 1)
		  and done_at=0 and dead_at=0 and attempts<? and lease_until<=?
		returning delivery_id, org_id, installation_id, event, action, payload_enc, lease_until, attempts`,
		lease, githubDeliveryMaxAttempts, now.UnixNano(), githubDeliveryMaxAttempts, now.UnixNano()).
		Scan(&d.ID, &d.OrgID, &d.InstallationID, &d.Event, &d.Action, &d.Payload, &d.Lease, &d.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// touchGitHubDelivery extends the lease of a delivery whose dispatch is still running. A dispatch
// is milliseconds when all is well, but one held up by a slow database or a GitHub call that hangs
// can outlast the minute the lease runs, and the row would then fall back into the queue and a
// second dispatcher would route the same delivery — enqueue the same review, revoke the same
// installation — while the first was still at it. errLeaseLost means that has already happened.
func (s *Store) touchGitHubDelivery(ctx context.Context, d *githubDelivery) error {
	lease := time.Now().Add(githubDeliveryLease).UnixNano()
	res, err := s.db.ExecContext(ctx, `update github_deliveries set lease_until=?
		where delivery_id=? and lease_until=? and done_at=0 and dead_at=0`, lease, d.ID, d.Lease)
	if err := fencedWrite(res, err); err != nil {
		return err
	}
	d.Lease = lease
	return nil
}

// finishGitHubDelivery closes a delivery this process still holds and drops its payload: from here
// the row is a receipt and nothing more. errLeaseLost means somebody else owns the outcome.
func (s *Store) finishGitHubDelivery(ctx context.Context, d *githubDelivery) error {
	res, err := s.db.ExecContext(ctx, `update github_deliveries set done_at=?, payload_enc=null
		where delivery_id=? and lease_until=? and done_at=0 and dead_at=0`, time.Now().UnixNano(), d.ID, d.Lease)
	return fencedWrite(res, err)
}

// failGitHubDelivery records why a dispatch failed and puts the delivery back with a delay, or,
// once its attempts are used up, dead-letters it: the payload is dropped, the receipt stays so a
// redelivery still deduplicates, and the slot it held in the inbox is freed.
func (s *Store) failGitHubDelivery(ctx context.Context, d *githubDelivery, why string) error {
	now := time.Now()
	why = truncate(why, 500)
	if d.Attempts >= githubDeliveryMaxAttempts {
		res, err := s.db.ExecContext(ctx, `update github_deliveries set dead_at=?, payload_enc=null, last_error=?
			where delivery_id=? and lease_until=? and done_at=0 and dead_at=0`, now.UnixNano(), why, d.ID, d.Lease)
		return fencedWrite(res, err)
	}
	retry := now.Add(githubDeliveryBackoff(d.Attempts)).UnixNano()
	res, err := s.db.ExecContext(ctx, `update github_deliveries set lease_until=?, last_error=?
		where delivery_id=? and lease_until=? and done_at=0 and dead_at=0`, retry, why, d.ID, d.Lease)
	if err := fencedWrite(res, err); err != nil {
		return err
	}
	d.Lease = retry
	return nil
}

// githubDeliveryBackoff is how long a delivery waits after its n-th failed attempt: 15 seconds,
// then a minute, four, sixteen. The four retries span about twenty minutes, long enough to ride
// out a database or GitHub blip during routing and short enough that the review it starts still
// lands while the author is looking at the pull request.
func githubDeliveryBackoff(attempts int) time.Duration {
	d := 15 * time.Second
	for i := 1; i < attempts && d < 30*time.Minute; i++ {
		d *= 4
	}
	return min(d, 30*time.Minute)
}

// purgeGitHubDeliveries is the inbox's own housekeeping, run by whoever runs the dispatchers.
//
// It first retires rows that ran out of attempts without anybody failing them — a claim on the
// last attempt whose process died holds the row pending forever otherwise, since the claim will
// not take it again, and every such row is a slot in this organisation's 64 that nothing will
// ever free. Then it forgets receipts older than a week, done and dead alike.
func (s *Store) purgeGitHubDeliveries(ctx context.Context) error {
	now := time.Now()
	if _, err := s.db.ExecContext(ctx, `update github_deliveries set dead_at=?, payload_enc=null,
		last_error=case when last_error='' then 'abandoned: the last attempt never reported back' else last_error end
		where done_at=0 and dead_at=0 and attempts>=? and lease_until<=?`,
		now.UnixNano(), githubDeliveryMaxAttempts, now.UnixNano()); err != nil {
		return err
	}
	cutoff := now.Add(-githubDeliveryKeep).UnixNano()
	_, err := s.db.ExecContext(ctx, `delete from github_deliveries
		where (done_at>0 and done_at<?) or (dead_at>0 and dead_at<?)`, cutoff, cutoff)
	return err
}

// LastGitHubDeliveryAt is when an organisation's installation last delivered anything, for the
// console's "webhook receiving" line; 0 when nothing has arrived in the week receipts are kept.
func (s *Store) LastGitHubDeliveryAt(ctx context.Context, orgID, installationID int64) (time.Time, error) {
	var at sql.NullInt64
	err := s.db.QueryRowContext(ctx, `select max(accepted_at) from github_deliveries
		where org_id=? and installation_id=?`, orgID, installationID).Scan(&at)
	if err != nil || !at.Valid || at.Int64 == 0 {
		return time.Time{}, err
	}
	return time.Unix(0, at.Int64).UTC(), nil
}

// fencedWrite turns a fenced update that matched no row into errLeaseLost. The id is the holder's
// own, so no row means the lease moved on to somebody else or the row was finished or retired
// meanwhile — either way this process is no longer the holder.
func fencedWrite(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return errLeaseLost
	}
	return nil
}
