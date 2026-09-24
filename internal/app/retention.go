package app

import (
	"context"
	"log/slog"
	"time"
)

// Keeping less than everything, when an organisation asks to.
//
// The tables that grow without bound are the ones that record what happened rather than what is
// configured: every turn, every tool call, every proxied request, every usage row. They are the
// audit trail, which is why nothing here deletes anything unless somebody sets a number —
// data_retention_days defaults to 0, meaning keep it all, because a deployment that quietly
// started deleting its own audit trail on upgrade would be the worst possible surprise.
//
// Per organisation, like every other setting. One tenant asking to keep ninety days must not
// decide anything about another's — a bug of exactly that shape (one organisation's retention
// preference deleting another's job data) is in the security review that led to the tenant
// guard tests, so the statements here all carry org_id and the sweep runs per organisation.

// retentionTables are swept by the organisation's own policy: the table, the column saying when
// the row was written, and how the row is tied to an organisation.
//
// Configuration is not on this list. A connection, a routine, a bundle is current state rather
// than a record of something that happened, and deleting it because it is old would be deleting
// the deployment.
//
// Neither is audit_log. It records what the people did, this policy included, and it has a
// policy of its own (audit_retention_days) so that an admin shortening the bot's history is not
// also shortening the record of having done so.
//
// Two shapes, because the schema has two. Most of these carry org_id. turns and file_texts do
// not: a thread belongs to the workspace it happened in, so they are reached through teams,
// exactly as the account deletion reaches them (store_org_delete.go, teamKeyedTables). Getting
// that wrong is not a failed sweep — it is a statement that matches nothing, silently, and a
// retention policy that has quietly never run.
var retentionTables = []struct {
	table, at string
	byTeam    bool
	// protectMonth keeps rows from the current UTC month regardless of the policy. usage is what
	// MonthSpend sums, and MonthSpend is a budget floor; without this, an admin setting the
	// 7-day retention floor would delete most of the month's usage and turn a monthly budget
	// into a roughly weekly one — a control over their own cap. See
	// TestRetentionKeepsThisMonthsUsage.
	protectMonth bool
}{
	// credit_ledger is deliberately absent, and this is the note saying so rather than an
	// omission somebody later "fixes". It carries org_id and a created_at like everything here,
	// so it would fit the shape -- but it is the record of money the customer paid and money we
	// drew down, and a retention policy an admin shortens must not shorten that. It is also
	// unrecoverable: usage is on this list, so spend can never be recomputed from it. The ledger
	// goes when the organisation goes (store_org_delete.go) and not a day before.
	// active_users is deliberately absent for the same reason, and this note is here so that its
	// absence reads as a decision. It is who the account's size is counted from — the basis of
	// what they are charged, exactly as the ledger is the record of what they paid — and a
	// customer able to shrink that figure by shortening their own retention would be holding a
	// control over their own bill. It has its own 400-day sweep in the billing loop, and it goes
	// with the organisation like everything else carrying org_id (store_org_delete.go).
	{table: "usage", at: "created_at", protectMonth: true},
	{table: "tool_calls", at: "created_at"},
	{table: "proxy_audit", at: "created_at"},
	{table: "artifacts", at: "created_at"},
	{table: "assistant_turns", at: "created_at"},
	{table: "turns", at: "created_at", byTeam: true},
	{table: "file_texts", at: "created_at", byTeam: true},
}

// retentionFloor is the shortest history an organisation may ask to keep. A number below this
// is more likely a typo than a policy, and the cost of the typo is the audit trail.
const retentionFloor = 7

// PurgeOrgData deletes one organisation's records older than its policy, in one transaction, and
// reports how many rows went. keep <= 0 deletes nothing.
func (s *Store) PurgeOrgData(ctx context.Context, orgID int64, keep time.Duration) (int64, error) {
	if keep <= 0 || orgID == 0 {
		return 0, nil
	}
	now := time.Now().UTC()
	cutoffTime := now.Add(-keep)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var total int64
	for _, t := range retentionTables {
		// A protected table never loses a current-month row: use whichever cutoff is earlier, the
		// policy's or the first of this month, so the month MonthSpend reads stays whole.
		c := cutoffTime
		if t.protectMonth && monthStart.Before(c) {
			c = monthStart
		}
		cutoff := c.Format(time.DateTime)
		// Built from a name rather than written as a literal, which puts these out of reach of
		// TestEveryPerOrgQueryIsScoped — so the organisation is spelled out in both branches
		// where a reader can see it, and TestRetentionStopsAtTheOrganisation stands in for the
		// guard that cannot see them.
		where := `where org_id=? and `
		if t.byTeam {
			where = `where team_id in (select team_id from teams where org_id=?) and `
		}
		res, err := tx.ExecContext(ctx, `delete from `+t.table+` `+where+t.at+` < ?`, orgID, cutoff)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, tx.Commit()
}

// runRetention sweeps every organisation that asked for one, once a day.
//
// Behind a leader lease at the call site (bot.go): several instances running this would do the
// same deletes concurrently and take the same locks for no reason.
func (b *Bot) runRetention(ctx context.Context) {
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		b.purgeRetainedOrgData(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (b *Bot) purgeRetainedOrgData(ctx context.Context) {
	orgs, err := b.store.OrgIDs(ctx)
	if err != nil {
		slog.Warn("retention enumeration", "err", err)
		return
	}
	for _, orgID := range orgs {
		set := b.settings.Get(ctx, orgID)
		if days := set.DataRetentionDays; days >= retentionFloor { // 0 is the default and means keep everything
			n, err := b.store.PurgeOrgData(ctx, orgID, time.Duration(days)*24*time.Hour)
			if err != nil {
				slog.Warn("retention", "org", orgID, "err", err)
			} else if n > 0 {
				slog.Info("retention swept", "org", orgID, "days", days, "rows", n)
				// In the organisation's own record, beside whoever set the policy: "rows were
				// deleted on this date, by policy" is the kind of thing an auditor asks about.
				b.auditSystem(ctx, orgID, "retention.swept", AuditEvent{TargetKind: "tables", TargetID: "activity",
					Details: auditDetails(map[string]any{"rows": n, "days": days})})
			}
		}
		// The audit log has its own policy and its own floor, and is swept after the tables
		// above so the row saying they were swept is written before anything of its own goes.
		if days := set.AuditRetentionDays; days >= auditRetentionFloor {
			n, err := b.store.PurgeAuditLog(ctx, orgID, time.Duration(days)*24*time.Hour)
			if err != nil {
				slog.Warn("audit retention", "org", orgID, "err", err)
			} else if n > 0 {
				slog.Info("audit log swept", "org", orgID, "days", days, "rows", n)
				b.auditSystem(ctx, orgID, "retention.swept", AuditEvent{TargetKind: "tables", TargetID: "audit_log",
					Details: auditDetails(map[string]any{"rows": n, "days": days})})
			}
		}
	}
}
