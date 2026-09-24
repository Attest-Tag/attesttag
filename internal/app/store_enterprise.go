package app

// The enterprise deal: one account's own plan size, written by the operator. See
// migrations/sqlite/0019_enterprise_terms.sql for what each column holds and why a deal is a row
// rather than an entry in STRIPE_SIZES.

import (
	"context"
	"database/sql"
	"errors"
)

// sizeEnterprise is the size key an enterprise account carries in billing_accounts.size. It names
// no rung of the ladder: whatever reads it finds the figures in the account's EnterpriseTerms.
const sizeEnterprise = "enterprise"

// EnterpriseTerms is one account's deal. The zero value, Exists false, is every account that is not
// on the enterprise plan.
type EnterpriseTerms struct {
	Exists        bool
	OrgID         int64
	UserLimit     int
	JobLimit      int
	FeeMinor      int64
	FeeInterval   string
	IncludedMinor int64
	PriceID       string
	PayURL        string
	Note          string
	UpdatedBy     string
	CreatedAt     string
	UpdatedAt     string
}

// size is the deal read as a plan size, so the code that turns a size into a fee, an allowance and
// a user ceiling handles an enterprise account without a second copy of itself.
func (t EnterpriseTerms) size() Size {
	return Size{Key: sizeEnterprise, PriceID: t.PriceID, AmountMinor: t.FeeMinor,
		IncludedMinor: t.IncludedMinor, UserLimit: t.UserLimit, JobLimit: t.JobLimit,
		Label: sizeLabels[sizeEnterprise]}
}

// paidBy is how the account pays for the deal, in the words the console and the operator page
// both use: "subscription" through a Stripe Price it subscribes to from Billing, "link" at a page
// the operator gave it, "invoice" by whatever the operator sends. One function, so the two
// screens cannot describe the same deal differently.
func (t EnterpriseTerms) paidBy() string {
	switch {
	case t.PriceID != "":
		return "subscription"
	case t.PayURL != "":
		return "link"
	}
	return "invoice"
}

const enterpriseCols = `org_id, user_limit, job_limit, fee_minor, fee_interval, included_minor,
	price_id, pay_url, note, updated_by, created_at, updated_at`

// EnterpriseTerms reads one account's deal. A missing row is not an error: it is the answer for
// every account that is not on the enterprise plan.
func (s *Store) EnterpriseTerms(ctx context.Context, orgID int64) (EnterpriseTerms, error) {
	var t EnterpriseTerms
	err := s.db.QueryRowContext(ctx, `select `+enterpriseCols+` from enterprise_terms where org_id=?`, orgID).
		Scan(&t.OrgID, &t.UserLimit, &t.JobLimit, &t.FeeMinor, &t.FeeInterval, &t.IncludedMinor,
			&t.PriceID, &t.PayURL, &t.Note, &t.UpdatedBy, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return EnterpriseTerms{}, nil
	}
	if err != nil {
		return EnterpriseTerms{}, err
	}
	t.Exists = true
	return t, nil
}

// PutEnterpriseTerms writes the whole deal. Only the operator's setEnterprise calls it; no console
// route can reach it (TestNoConsoleRouteCanMoveAPlanOrGrantCredit).
func (s *Store) PutEnterpriseTerms(ctx context.Context, t EnterpriseTerms) error {
	if t.FeeInterval == "" {
		t.FeeInterval = "month"
	}
	// The columns written out rather than taken from enterpriseCols, so that the statement names
	// org_id where TestEveryPerOrgQueryIsScoped can see it.
	_, err := s.db.ExecContext(ctx,
		`insert into enterprise_terms (org_id, user_limit, job_limit, fee_minor, fee_interval,
			included_minor, price_id, pay_url, note, updated_by, created_at, updated_at)
		 values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 on conflict (org_id) do update set
		   user_limit=excluded.user_limit, job_limit=excluded.job_limit,
		   fee_minor=excluded.fee_minor, fee_interval=excluded.fee_interval,
		   included_minor=excluded.included_minor, price_id=excluded.price_id,
		   pay_url=excluded.pay_url, note=excluded.note, updated_by=excluded.updated_by,
		   updated_at=excluded.updated_at`,
		t.OrgID, t.UserLimit, t.JobLimit, t.FeeMinor, t.FeeInterval, t.IncludedMinor,
		t.PriceID, t.PayURL, t.Note, t.UpdatedBy, now(), now())
	return err
}

// DeleteEnterpriseTerms is what leaving the enterprise plan does to the deal: it goes, so nothing
// stale is waiting to come back if the account is ever moved onto enterprise again.
func (s *Store) DeleteEnterpriseTerms(ctx context.Context, orgID int64) error {
	_, err := s.db.ExecContext(ctx, `delete from enterprise_terms where org_id=?`, orgID)
	return err
}
