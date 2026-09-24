package app

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The inbox's limits are per organisation first. The deployment-wide figure is a ceiling on
// disk, not a fair share: with a small global cap, four busy workspaces filled it and every
// other tenant's messages were refused with 503 until Slack gave up on them.
const (
	slackPendingLimit        = 4096
	slackOrgPendingLimit     = 64
	slackTeamPendingLimit    = 32
	slackDeliveryLease       = 60 * time.Second
	slackDeliveryMaxAttempts = 5
)

type slackDelivery struct {
	Key, Team, Kind string
	OrgID           int64
	Payload         []byte
	Lease           int64
	Attempts        int
}

// This table is an app-wide transport inbox, not a console data surface. Only
// signature-verified deliveries enter it, and the signed workspace is retained.
// Payloads (including interaction response URLs) are encrypted with MASTER_KEY.

func (s *Store) enqueueSlackDelivery(ctx context.Context, key string, orgID int64, team, kind string, payload []byte) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var found int
	err = tx.QueryRowContext(ctx, `select 1 from slack_deliveries where delivery_key=?`, key).Scan(&found)
	if err == nil {
		return nil
	} // retries also succeed while the queue is full
	if err != sql.ErrNoRows {
		return err
	}
	var total, perOrg, perTeam int
	if err := tx.QueryRowContext(ctx, `select count(*), coalesce(sum(case when org_id=? then 1 else 0 end),0), coalesce(sum(case when team_id=? then 1 else 0 end),0)
		from slack_deliveries where done_at=0 and dead_at=0`, orgID, team).Scan(&total, &perOrg, &perTeam); err != nil {
		return err
	}
	if total >= slackPendingLimit || perOrg >= slackOrgPendingLimit || perTeam >= slackTeamPendingLimit {
		return errors.New("Slack inbox full")
	}
	_, err = tx.ExecContext(ctx, `insert into slack_deliveries (delivery_key,team_id,org_id,kind,payload_enc,accepted_at)
		values (?,?,?,?,?,?)`, key, team, orgID, kind, payload, time.Now().UnixNano())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) claimSlackDelivery(ctx context.Context) (*slackDelivery, error) {
	now := time.Now()
	lease := now.Add(slackDeliveryLease).UnixNano()
	var d slackDelivery
	// A single atomic UPDATE prevents two dispatchers from claiming the same row. A claim is
	// an attempt: a delivery that keeps failing is retired rather than re-claimed forever.
	err := s.db.QueryRowContext(ctx, `update slack_deliveries set lease_until=?, attempts=attempts+1
		where delivery_key=(select delivery_key from slack_deliveries where done_at=0 and dead_at=0 and attempts<? and lease_until<=?
		order by accepted_at limit 1)
		returning delivery_key,team_id,org_id,kind,payload_enc,lease_until,attempts`, lease, slackDeliveryMaxAttempts, now.UnixNano()).
		Scan(&d.Key, &d.Team, &d.OrgID, &d.Kind, &d.Payload, &d.Lease, &d.Attempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &d, err
}

// failSlackDelivery records why a dispatch failed and, once the attempts are used up,
// dead-letters the row: the payload is dropped, the receipt stays so the retry window still
// deduplicates, and the slot it held in the inbox is freed.
func (s *Store) failSlackDelivery(ctx context.Context, d *slackDelivery, why string) error {
	if d.Attempts >= slackDeliveryMaxAttempts {
		_, err := s.db.ExecContext(ctx, `update slack_deliveries set dead_at=?, payload_enc=null, last_error=?
			where delivery_key=? and done_at=0`, time.Now().UnixNano(), truncate(why, 500), d.Key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `update slack_deliveries set last_error=? where delivery_key=?`, truncate(why, 500), d.Key)
	return err
}

// touchSlackDelivery extends the lease of a delivery whose work is still running. A turn may take
// minutes and the lease is a minute, so without this the row would fall back into the queue and a
// second dispatcher would start the same turn again. Losing the row to someone else is reported,
// because at that point this process no longer owns the work.
func (s *Store) touchSlackDelivery(ctx context.Context, d *slackDelivery) error {
	lease := time.Now().Add(slackDeliveryLease).UnixNano()
	res, err := s.db.ExecContext(ctx, `update slack_deliveries set lease_until=?
		where delivery_key=? and lease_until=? and done_at=0 and dead_at=0`, lease, d.Key, d.Lease)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("delivery lease lost")
	}
	d.Lease = lease
	return nil
}

func (s *Store) finishSlackDelivery(ctx context.Context, d *slackDelivery) error {
	_, err := s.db.ExecContext(ctx, `update slack_deliveries set done_at=?,payload_enc=null
		where delivery_key=? and team_id=? and lease_until=? and done_at=0`, time.Now().UnixNano(), d.Key, d.Team, d.Lease)
	return err
}

func (s *Store) purgeSlackDeliveries(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `delete from slack_deliveries where done_at>0 and done_at<?`, time.Now().Add(-24*time.Hour).UnixNano()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `delete from slack_deliveries where dead_at>0 and dead_at<?`, time.Now().Add(-7*24*time.Hour).UnixNano())
	return err
}
