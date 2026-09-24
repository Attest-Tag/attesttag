package app

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// The queue behind the digging lane. It is the slack_deliveries discipline applied to work the
// bot gives itself: a row is claimed under a lease, the lease is renewed while the run holds it,
// and a lease that runs out is what makes an abandoned run visible to the next container. A
// question that took three minutes of log reading is worth resuming; the alternative — the
// goroutine and everything it found dying with the process — is what this table exists to stop.

const (
	investigationLease       = 90 * time.Second
	investigationMaxAttempts = 3
)

type Investigation struct {
	ID                  int64
	OrgID               int64
	TeamID              string
	Channel, ThreadTS   string
	Requester           string
	Question, Brief     string
	Rounds, Minutes     int
	Status              string
	Attempts            int
	TokensIn, TokensOut int
	CostUSD             float64
	Error               string
	CreatedAt           string
}

const investigationCols = `id, org_id, team_id, channel, thread_ts, requester, question, brief,
	rounds, minutes, status, attempts, tokens_in, tokens_out, cost_usd, coalesce(error,''), created_at`

func scanInvestigation(row interface{ Scan(...any) error }) (*Investigation, error) {
	var v Investigation
	err := row.Scan(&v.ID, &v.OrgID, &v.TeamID, &v.Channel, &v.ThreadTS, &v.Requester, &v.Question, &v.Brief,
		&v.Rounds, &v.Minutes, &v.Status, &v.Attempts, &v.TokensIn, &v.TokensOut, &v.CostUSD, &v.Error, &v.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// EnqueueInvestigation records one question for the lane and returns its id.
func (s *Store) EnqueueInvestigation(ctx context.Context, v *Investigation) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `insert into investigations
		(org_id, team_id, channel, thread_ts, requester, question, brief, rounds, minutes)
		values (?,?,?,?,?,?,?,?,?) returning id`,
		v.OrgID, v.TeamID, v.Channel, v.ThreadTS, v.Requester, v.Question, v.Brief, v.Rounds, v.Minutes).Scan(&id)
	return id, err
}

// claimInvestigation takes the oldest piece of unleased work — queued, or running under a lease
// that expired because whoever held it is gone. One atomic UPDATE, so two workers in the same
// process (or two containers mid-handover) cannot both hold the same run.
func (s *Store) claimInvestigation(ctx context.Context) (*Investigation, error) {
	now := time.Now()
	v, err := scanInvestigation(s.db.QueryRowContext(ctx, `update investigations
		set status='running', attempts=attempts+1, lease_until=?,
		    started_at=case when started_at='' then ? else started_at end
		where id=(select id from investigations
			where status in ('queued','running') and lease_until<=? and attempts<?
			order by id limit 1)
		returning `+investigationCols, now.Add(investigationLease).UnixNano(), now.UTC().Format(time.DateTime),
		now.UnixNano(), investigationMaxAttempts))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return v, err
}

// abandonedInvestigations are the runs that used up their attempts without ever finishing —
// each one a thread still waiting for an answer. They are reported once and closed.
func (s *Store) abandonedInvestigations(ctx context.Context) ([]*Investigation, error) {
	rows, err := s.db.QueryContext(ctx, `select `+investigationCols+` from investigations
		where status in ('queued','running') and attempts>=? and lease_until<=? order by id limit 20`,
		investigationMaxAttempts, time.Now().UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Investigation
	for rows.Next() {
		v, err := scanInvestigation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// touchInvestigation renews the lease of a run that is still going.
func (s *Store) touchInvestigation(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `update investigations set lease_until=? where id=? and status='running'`,
		time.Now().Add(investigationLease).UnixNano(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("investigation %d is no longer ours", id)
	}
	return nil
}

// finishInvestigation closes a run out with what it spent. A failed run keeps its lease at zero
// so the queue can retry it until its attempts are spent; done, stopped and failed-for-good are
// terminal, and terminal rows are never claimed again.
func (s *Store) finishInvestigation(ctx context.Context, id int64, status, errMsg string, u Usage) error {
	_, err := s.db.ExecContext(ctx, `update investigations
		set status=?, error=?, tokens_in=tokens_in+?, tokens_out=tokens_out+?, cost_usd=cost_usd+?,
		    lease_until=0, finished_at=?
		where id=?`, status, truncate(errMsg, 500), u.In, u.Out, u.CostUSD, now(), id)
	return err
}

// releaseInvestigation hands a run back to the queue without marking it finished: the lease is
// dropped so the next worker can pick it up, and the attempt it just used stands.
func (s *Store) releaseInvestigation(ctx context.Context, id int64, why string, u Usage) error {
	_, err := s.db.ExecContext(ctx, `update investigations
		set status='queued', error=?, lease_until=0,
		    tokens_in=tokens_in+?, tokens_out=tokens_out+?, cost_usd=cost_usd+?
		where id=? and status='running'`, truncate(why, 500), u.In, u.Out, u.CostUSD, id)
	return err
}

// CountOpenInvestigations is the per-organisation cap's input: queued plus running.
func (s *Store) CountOpenInvestigations(ctx context.Context, orgID int64) int {
	var n int
	s.db.QueryRowContext(ctx, `select count(*) from investigations where org_id=? and status in ('queued','running')`, orgID).Scan(&n)
	return n
}

// OpenInvestigationsInThread returns the unfinished runs in one thread, newest last.
func (s *Store) OpenInvestigationsInThread(ctx context.Context, orgID int64, teamID, channel, threadTS string) ([]*Investigation, error) {
	rows, err := s.db.QueryContext(ctx, `select `+investigationCols+` from investigations
		where org_id=? and team_id=? and channel=? and thread_ts=? and status in ('queued','running') order by id`,
		orgID, teamID, channel, threadTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Investigation
	for rows.Next() {
		v, err := scanInvestigation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// StopQueuedInvestigations calls off the runs in a thread that have not started yet. One already
// running is stopped through its turn (StopRuns), which is what cancels the work in flight; this
// is for the ones that would otherwise start after someone said stop.
func (s *Store) StopQueuedInvestigations(ctx context.Context, orgID int64, teamID, channel, threadTS, by string) int {
	res, err := s.db.ExecContext(ctx, `update investigations
		set status='stopped', error=?, lease_until=0, finished_at=?
		where org_id=? and team_id=? and channel=? and thread_ts=? and status='queued'`,
		"stopped by "+by, now(), orgID, teamID, channel, threadTS)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}
