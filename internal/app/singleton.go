package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// The loops that must run once across the deployment, however many instances there are.
//
// Six goroutines start at boot and each of them writes: the scope sync, the Slack inbox
// dispatcher, the document ingest, the Drive sync, the job reconciler, the investigation
// sweeper. With one instance that is fine and has been for months. With two, every one of them
// runs twice — the same routine posted twice into the same channel, the same document ingested
// twice, the same abandoned investigation reported twice to the same thread.
//
// A lease on a row rather than pg_advisory_lock, for three reasons: the advisory lock is
// Postgres-only and this deployment also runs on SQLite; it needs a connection pinned out of
// the pool for its whole life, with its own health to babysit; and it is invisible — nobody can
// ask which instance holds it, whereas `select * from leader_leases` answers that during an
// incident. The cost is a row and a poll.
//
// One lease per role, not one leader. A single leader would put every loop on one instance and
// leave the others idle, and a slow ingest would hold up the reconciler behind it.

const (
	leaderTTL   = 30 * time.Second
	leaderRenew = 10 * time.Second
	leaderPoll  = 5 * time.Second
)

// instanceID names this process in the lease table. It is for a human reading the row, so it
// says where rather than being unique for its own sake: the Cloud Run revision when there is
// one, otherwise the hostname, and the pid to tell two processes on one machine apart.
func instanceID() string {
	who := os.Getenv("K_REVISION")
	if who == "" {
		who, _ = os.Hostname()
	}
	if who == "" {
		who = "unknown"
	}
	return fmt.Sprintf("%s/%d", who, os.Getpid())
}

// leaderLoop runs fn for as long as this instance holds the named lease, and keeps trying to
// take it when it does not.
//
// On a single instance the first poll succeeds and fn runs for the life of the process, which
// is exactly what happened before this existed — the reason it is safe to ship long before a
// second instance is ever started.
//
// fn is given a context that is cancelled the moment the lease is lost, so work in flight stops
// rather than continuing against a database another instance now believes it owns.
func (b *Bot) leaderLoop(ctx context.Context, role string, fn func(context.Context)) {
	me := instanceID()
	for {
		token, ok, err := b.store.TakeLeaderLease(ctx, role, me, leaderTTL)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			slog.Warn("leader lease unavailable", "role", role, "err", err)
		case ok:
			b.holdLease(ctx, role, me, token, fn)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(leaderPoll):
		}
	}
}

// holdLease runs fn and renews underneath it, returning when either stops.
func (b *Bot) holdLease(ctx context.Context, role, me string, token int64, fn func(context.Context)) {
	held, drop := context.WithCancel(ctx)
	defer drop()
	slog.Info("holding leader lease", "role", role, "holder", me)

	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(held)
	}()

	tick := time.NewTicker(leaderRenew)
	defer tick.Stop()
	for {
		select {
		case <-done:
			// fn returned on its own. Give the lease back rather than making the next instance
			// wait out the TTL for work nobody is doing.
			b.store.ReleaseLeaderLease(context.WithoutCancel(ctx), role, me, token)
			return
		case <-ctx.Done():
			b.store.ReleaseLeaderLease(context.WithoutCancel(ctx), role, me, token)
			<-done
			return
		case <-tick.C:
			next, ok, err := b.store.RenewLeaderLease(ctx, role, me, token, leaderTTL)
			if err != nil {
				// A database hiccup is not proof somebody else has taken it. Keep going; the
				// next tick decides, and the TTL is three renewals long for this reason.
				slog.Warn("could not renew leader lease", "role", role, "err", err)
				continue
			}
			if !ok {
				slog.Warn("lost leader lease; stopping", "role", role, "holder", me)
				drop()
				<-done
				return
			}
			token = next
		}
	}
}

// TakeLeaderLease claims a role if nobody holds it or the holder's lease has run out. The token
// it returns is the expiry it wrote, and every later call carries it: that is what makes a
// renewal fail after somebody else has taken the role, rather than quietly stealing it back.
func (s *Store) TakeLeaderLease(ctx context.Context, role, holder string, ttl time.Duration) (int64, bool, error) {
	nowNano, until := time.Now().UnixNano(), time.Now().Add(ttl).UnixNano()
	// Insert if the role has never been held. Not an error when it has — the update decides.
	if _, err := s.db.ExecContext(ctx,
		`insert into leader_leases (role, holder, expires_at) values (?, ?, ?) on conflict do nothing`,
		role, holder, until); err != nil {
		return 0, false, err
	}
	res, err := s.db.ExecContext(ctx,
		`update leader_leases set holder=?, expires_at=? where role=? and (holder=? or expires_at<=?)`,
		holder, until, role, holder, nowNano)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, false, nil // somebody else holds it and their lease is live
	}
	return until, true, nil
}

// RenewLeaderLease extends a lease this instance still holds. Keyed on the expiry it last
// wrote, so an instance that paused long enough to be replaced finds out here instead of
// carrying on beside its replacement.
func (s *Store) RenewLeaderLease(ctx context.Context, role, holder string, token int64, ttl time.Duration) (int64, bool, error) {
	until := time.Now().Add(ttl).UnixNano()
	res, err := s.db.ExecContext(ctx,
		`update leader_leases set expires_at=? where role=? and holder=? and expires_at=?`,
		until, role, holder, token)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	return until, n == 1, nil
}

// ReleaseLeaderLease hands a role back early. Best effort: if it fails, the lease expires.
func (s *Store) ReleaseLeaderLease(ctx context.Context, role, holder string, token int64) {
	s.db.ExecContext(ctx, `update leader_leases set expires_at=0 where role=? and holder=? and expires_at=?`,
		role, holder, token)
}
