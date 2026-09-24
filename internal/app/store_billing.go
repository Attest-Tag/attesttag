package app

// The money tables: an organisation's subscription, its prepaid credit balance, and the statement
// of every movement in it. See migrations/sqlite/0009_billing.sql for what each column is and why.
//
// One rule governs this whole file: credit_balance_micros is a cache of sum(credit_ledger) and
// nothing else, and every write of it happens in the same transaction as the row that justifies
// it. The only exception is the per-turn debit, which moves the balance immediately and is written
// up into the ledger once an hour by the roll-up — the two counters exist to keep that exact
// rather than approximate. Nothing outside this file may update that column; there is a test.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"
)

// Micro-dollars. Money is integer throughout: a turn can cost $0.0003, which cents cannot hold,
// and floats would leave the reconciliation a tolerance and balances sitting at -1e-15 where the
// gate asks whether they are at or below zero.
const microsPerUSD = 1_000_000

// usdToMicros converts a price a provider quoted into the unit this package counts in. It rounds
// to nearest rather than truncating: truncation across a million turns is a systematic gift in one
// direction, which is the kind of error nobody notices until it is large.
//
// A cost that is not a finite, non-negative number is zero. validCost (llm.go) already refuses
// those at the boundary; this is the second of the two places, because the first one protects a
// budget and this one moves money.
func usdToMicros(usd float64) int64 {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd <= 0 {
		return 0
	}
	return int64(math.Round(usd * microsPerUSD))
}

func microsToUSD(m int64) float64 { return float64(m) / microsPerUSD }

// Ledger kinds. A row's kind says where the money came from, not which way it went — the sign of
// amount_micros says that, so a balance is a plain sum.
const (
	creditTopUp      = "topup"      // a card payment
	creditDebit      = "debit"      // model spend, rolled up hourly
	creditRefund     = "refund"     // a refund or a chargeback at Stripe
	creditAdjustment = "adjustment" // an operator's correction, with a note saying why
	creditGrant      = "grant"      // an operator giving credit without a payment
	creditIncluded   = "included"   // the monthly allowance a paid size carries, one per invoice
)

// BillingAccount is one organisation's billing state. The zero value is an organisation that has
// never bought anything, which is every self-host and every free account, and every caller has to
// read correctly: Exists false means no credit floor, no subscription, nothing to show.
type BillingAccount struct {
	Exists              bool
	OrgID               int64
	Provider            string
	CustomerID          string
	SubscriptionID      string
	Status              string
	Size                string
	UnitPriceMicros     int64
	Quantity            int64
	Currency            string
	PeriodStart         string
	PeriodEnd           string
	CancelAtPeriodEnd   bool
	CreditBalanceMicros int64
	CreditEnforced      bool
	LifetimeTopUpMicros int64
	LifetimeDebitMicros int64
	DebitRolledMicros   int64
	// The plan's monthly allowance. Its own bucket and deliberately not on the ledger — see
	// migrations/sqlite/0014_credit_allowance.sql. AllowanceMicros is what is left,
	// AllowanceGrantedMicros what the period started with, AllowancePeriodEnd when it lapses.
	AllowanceMicros        int64
	AllowanceGrantedMicros int64
	AllowancePeriodEnd     string
	AllowanceSize          string
	CreatedAt, UpdatedAt   string
}

// AllowanceLive is whether this period's allowance is still spendable. Expiry is this comparison
// and nothing else: there is no scheduled job whose failure would hand somebody a free month. The
// hourly sweep zeroes lapsed rows so that stored state does not lie, but nothing depends on it
// having run.
func (b BillingAccount) AllowanceLive() bool {
	return b.AllowancePeriodEnd != "" && b.AllowancePeriodEnd >= now()
}

// SpendableAllowanceMicros is the allowance a turn may actually draw on: zero once the period it
// belonged to has passed, whatever the column still says.
func (b BillingAccount) SpendableAllowanceMicros() int64 {
	if !b.AllowanceLive() {
		return 0
	}
	return b.AllowanceMicros
}

// SpendableMicros is what the gate compares against the overdraft floor: this month's allowance
// plus whatever prepaid credit is left. One number, because a turn does not care which pocket it
// comes out of — only the statement does.
func (b BillingAccount) SpendableMicros() int64 {
	return b.SpendableAllowanceMicros() + b.CreditBalanceMicros
}

// Metered is whether the credit floor applies at all. Two ways in, and the second is why
// credit_enforced is not simply set from a plan: that flag never goes back to 0, so an allowance
// setting it would leave an account whose subscription later ended with the floor on, no allowance
// and a zero balance — a bricked workspace. A live allowance period lapses on its own instead.
func (b BillingAccount) Metered() bool { return b.CreditEnforced || b.AllowanceLive() }

// Active is whether the subscription is one Stripe would bill. past_due is deliberately active:
// Stripe retries a failed card for weeks, and cutting a paying customer off on the first decline
// is the wrong failure. comped and invoiced are live too without Stripe billing anything: one was
// granted by the operator, the other is an enterprise deal paid outside Stripe subscriptions.
func (b BillingAccount) Active() bool {
	switch b.Status {
	case "active", "trialing", "past_due", "comped", statusInvoiced:
		return b.Exists
	}
	return false
}

// AmountMicros is what the subscription costs a month: the size's price, times a quantity that is
// 1 for a size and would be a seat count if seats were ever sold.
func (b BillingAccount) AmountMicros() int64 { return b.UnitPriceMicros * b.Quantity }

const billingCols = `org_id, provider, customer_id, subscription_id, status, size,
	unit_price_micros, quantity, currency, period_start, period_end, cancel_at_period_end,
	credit_balance_micros, credit_enforced, lifetime_topup_micros, lifetime_debit_micros,
	debit_rolled_micros, allowance_micros, allowance_granted_micros, allowance_period_end,
	allowance_size, created_at, updated_at`

func scanBilling(row interface{ Scan(...any) error }) (BillingAccount, error) {
	var b BillingAccount
	var cancel, enforced int64
	err := row.Scan(&b.OrgID, &b.Provider, &b.CustomerID, &b.SubscriptionID, &b.Status, &b.Size,
		&b.UnitPriceMicros, &b.Quantity, &b.Currency, &b.PeriodStart, &b.PeriodEnd, &cancel,
		&b.CreditBalanceMicros, &enforced, &b.LifetimeTopUpMicros, &b.LifetimeDebitMicros,
		&b.DebitRolledMicros, &b.AllowanceMicros, &b.AllowanceGrantedMicros, &b.AllowancePeriodEnd,
		&b.AllowanceSize, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return BillingAccount{}, err
	}
	b.Exists, b.CancelAtPeriodEnd, b.CreditEnforced = true, cancel != 0, enforced != 0
	return b, nil
}

// BillingAccountOf reads one organisation's billing state. A missing row is not an error: it is
// the answer for every organisation that has never bought anything.
func (s *Store) BillingAccountOf(ctx context.Context, orgID int64) (BillingAccount, error) {
	b, err := scanBilling(s.db.QueryRowContext(ctx,
		`select `+billingCols+` from billing_accounts where org_id=?`, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return BillingAccount{}, nil
	}
	return b, err
}

// BillingAccountByCustomer is how a webhook finds the organisation an event concerns. An invoice
// or a subscription event carries a Stripe customer and nothing of ours, so this lookup is
// deliberately org-less — it is what produces the organisation, and everything after it is scoped
// by what it returns. It is named in globalQueries with that reason.
func (s *Store) BillingAccountByCustomer(ctx context.Context, customerID string) (BillingAccount, error) {
	if customerID == "" {
		return BillingAccount{}, nil
	}
	b, err := scanBilling(s.db.QueryRowContext(ctx,
		`select `+billingCols+` from billing_accounts where customer_id=?`, customerID))
	if errors.Is(err, sql.ErrNoRows) {
		return BillingAccount{}, nil
	}
	return b, err
}

// BillingFacts is the little of this the settings cache carries: two booleans that change when a
// webhook lands and not otherwise. The balance is deliberately NOT here — it moves every turn, and
// a fifteen-second cache of it is a fifteen-second window of spending money that is gone.
//
// Unknown is the third state, and it is the one that matters. "No row" and "the table could not be
// read" both produce false for the other two, and treating the second as the first would switch
// every credit floor on the deployment off without a word — an account with no billing
// relationship and an account whose record we cannot see must not be the same answer.
type BillingFacts struct {
	Active, CreditEnforced bool
	// AllowanceActive is the second way the credit floor binds: a plan allowance whose period is
	// still live. Like the two above it changes at a period boundary rather than on a turn, so a
	// cached copy is safe; the amount is read live by SpendableCredit.
	AllowanceActive bool
	Unknown         bool
}

func (s *Store) BillingFacts(ctx context.Context, orgID int64) BillingFacts {
	var status, allowanceEnd string
	var enforced int64
	err := s.db.QueryRowContext(ctx,
		`select status, credit_enforced, allowance_period_end from billing_accounts where org_id=?`,
		orgID).Scan(&status, &enforced, &allowanceEnd)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The ordinary answer for every organisation that has never bought anything.
		return BillingFacts{}
	case err != nil:
		slog.Error("billing state could not be read; treating the account as gated until it can", "org", orgID, "err", err)
		return BillingFacts{Unknown: true}
	}
	acct := BillingAccount{Exists: true, Status: status, AllowancePeriodEnd: allowanceEnd}
	return BillingFacts{Active: acct.Active(), CreditEnforced: enforced != 0, AllowanceActive: acct.AllowanceLive()}
}

// CreditBalance is the number the gate reads, and it is read fresh on every turn rather than from
// the settings cache. One indexed lookup on the primary key; a missing row answers zero, which no
// caller acts on because it checks CreditEnforced first.
func (s *Store) CreditBalance(ctx context.Context, orgID int64) (int64, error) {
	var micros int64
	err := s.db.QueryRowContext(ctx,
		`select credit_balance_micros from billing_accounts where org_id=?`, orgID).Scan(&micros)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return micros, err
}

// SpendableCredit is what the gate reads every turn: this month's allowance plus prepaid credit,
// and whether the floor applies at all. Read fresh rather than from the settings cache, for the
// reason CreditBalance is — both halves move on every turn, and a fifteen-second cache of them is
// a fifteen-second window of spending money that is gone.
//
// One indexed lookup on the primary key. A missing row answers (0, false), which is every
// organisation that has never bought anything and every self-host.
func (s *Store) SpendableCredit(ctx context.Context, orgID int64) (int64, bool, error) {
	var balance, allowance, enforced int64
	var periodEnd string
	err := s.db.QueryRowContext(ctx,
		`select credit_balance_micros, credit_enforced, allowance_micros, allowance_period_end
		   from billing_accounts where org_id=?`, orgID).Scan(&balance, &enforced, &allowance, &periodEnd)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	acct := BillingAccount{Exists: true, CreditBalanceMicros: balance, CreditEnforced: enforced != 0,
		AllowanceMicros: allowance, AllowancePeriodEnd: periodEnd}
	return acct.SpendableMicros(), acct.Metered(), nil
}

// SyncAllowance brings one account's allowance into line with the size it is on and the period it
// is in. Idempotent, and cheap enough to call from every webhook that mentions the account — which
// is the point of it. The first version of this feature granted the allowance from invoice.paid
// alone, and a size change produces no invoice, so an account that moved size saw a plan promising
// $100 of credit next to a balance of $0.00 until its next renewal.
//
// want is the size's included figure, 0 for an account with no live subscription or a size that
// includes nothing. periodEnd is the subscription's, and is what the allowance is pinned to.
//
// Four cases, and the two middle ones are the ones worth arguing about:
//
//   - The period moved: granted and remaining both become want. Whatever was left of the old
//     period is overwritten, and that overwrite IS the expiry — atomic with the renewal, with no
//     scheduled job whose failure would carry a month's unspent allowance into the next one.
//   - The size moved up mid-period: top up by the difference, scaled to the share of the period
//     left, since that is the share of the larger fee Stripe charges for it. Somebody who upgrades
//     on the 10th has started paying the larger fee and should see what it includes now, not in
//     three weeks.
//   - The size moved down mid-period: record the size and change nothing else. Never take back
//     what they were already told they had; the next period resets to the new size exactly.
//   - No subscription: zero, so a cancelled plan stops being metered rather than being metered
//     against an allowance it no longer has.
func (s *Store) SyncAllowance(ctx context.Context, orgID int64, size, periodEnd string, want int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ensureBillingRow(ctx, tx, orgID); err != nil {
		return err
	}
	var haveSize, havePeriod, subStart, subEnd string
	var granted, remaining int64
	err = tx.QueryRowContext(ctx,
		`select allowance_size, allowance_period_end, allowance_granted_micros, allowance_micros,
		        coalesce(period_start,''), coalesce(period_end,'')
		   from billing_accounts where org_id=?`, orgID).Scan(&haveSize, &havePeriod, &granted, &remaining, &subStart, &subEnd)
	if err != nil {
		return err
	}

	switch {
	case want <= 0 || periodEnd == "":
		granted, remaining, periodEnd = 0, 0, ""
	case havePeriod != periodEnd:
		granted, remaining = want, want
	case want > granted:
		// A move to a bigger size is charged at Stripe for the part of the period left, so it
		// brings that part of the difference, not all of it. Handing over the whole difference let
		// an account upgrade on the last day, pay a thirtieth of the fee, take the full allowance
		// and schedule the downgrade back for nothing. The same size's figure going up — an
		// operator changing a deal — is not a purchase and still arrives whole. granted records the
		// size's figure either way, so a later call in the period does not top up again.
		add := want - granted
		if haveSize != "" && haveSize != size {
			add = prorateLeft(add, subStart, subEnd, time.Now())
		}
		remaining += add
		granted = want
	default:
		// Same period, and the size did not move up. Nothing to do but record which size it is,
		// so a later call can tell a downgrade from a period that has not rolled yet.
	}

	if _, err := tx.ExecContext(ctx,
		`update billing_accounts
		    set allowance_micros = ?, allowance_granted_micros = ?, allowance_period_end = ?,
		        allowance_size = ?, updated_at = ?
		  where org_id=?`, remaining, granted, periodEnd, size, now(), orgID); err != nil {
		return err
	}
	return tx.Commit()
}

// prorateLeft scales an amount by the share of a period still to run, the way Stripe prorates the
// fee for a mid-period upgrade. Without a period to measure — a comped size, or a subscription whose
// start is not known — it returns the amount whole.
func prorateLeft(amount int64, start, end string, now time.Time) int64 {
	s, err1 := time.Parse(time.DateTime, start)
	e, err2 := time.Parse(time.DateTime, end)
	if err1 != nil || err2 != nil || !e.After(s) {
		return amount
	}
	left, total := e.Sub(now), e.Sub(s)
	switch {
	case left <= 0:
		return 0
	case left >= total:
		return amount
	}
	return int64(float64(amount) * float64(left) / float64(total))
}

// OrgsWithAllowanceDrift is every account whose stored allowance may no longer match the size and
// period it is on: an active subscription, or a row still holding an allowance. Deployment-wide by
// design, like OrgsWithUnrolledDebit, and read behind the leader lease rather than on a request.
//
// It exists because every other caller of SyncAllowance is an event, and the one failure worth
// engineering against is the event that never comes — a webhook Stripe gave up retrying, an
// endpoint that was misconfigured for a week, a period that rolled while the deployment was down.
func (s *Store) OrgsWithAllowanceDrift(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`select org_id from billing_accounts
		  where status in ('active','trialing','past_due','comped','invoiced') or allowance_period_end <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ExpireAllowances zeroes allowances whose period has passed. Housekeeping rather than policy:
// every read already treats a lapsed allowance as spent (BillingAccount.AllowanceLive), so nothing
// depends on this having run. It exists so that a row somebody looks at in the database says what
// the product would say, and so an account whose subscription stopped sending webhooks does not
// sit there appearing to hold credit it cannot spend.
func (s *Store) ExpireAllowances(ctx context.Context) {
	s.db.ExecContext(ctx,
		`update billing_accounts set allowance_micros=0, allowance_granted_micros=0,
		        allowance_period_end='', updated_at=?
		  where allowance_period_end <> '' and allowance_period_end < ?`, now(), now())
}

// ensureBillingRow creates the organisation's billing row if it has none, inside a transaction the
// caller owns. Every write path goes through it, so a top-up on an account that has never had a
// subscription works without a separate "create the customer first" step.
func ensureBillingRow(ctx context.Context, tx *dbTx, orgID int64) error {
	_, err := tx.ExecContext(ctx,
		`insert into billing_accounts (org_id, created_at, updated_at) values (?, ?, ?)
		 on conflict (org_id) do nothing`, orgID, now(), now())
	return err
}

// CreditEntry is one line of the statement.
type CreditEntry struct {
	ID         int64   `json:"-"`
	At         string  `json:"at"`
	Kind       string  `json:"kind"`
	AmountUSD  float64 `json:"amount_usd"`
	BalanceUSD float64 `json:"balance_after_usd"`
	Currency   string  `json:"currency"`
	ExternalID string  `json:"external_id"`
	Note       string  `json:"note"`
	Actor      string  `json:"actor"`
}

// CreditLedger is one organisation's statement, newest first.
func (s *Store) CreditLedger(ctx context.Context, orgID int64, limit int) ([]CreditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`select id, created_at, kind, amount_micros, balance_after_micros, currency, external_id, note, actor
		   from credit_ledger where org_id=? order by id desc limit ?`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CreditEntry
	for rows.Next() {
		var e CreditEntry
		var amount, balance int64
		if err := rows.Scan(&e.ID, &e.At, &e.Kind, &amount, &balance, &e.Currency, &e.ExternalID, &e.Note, &e.Actor); err != nil {
			return nil, err
		}
		e.AmountUSD, e.BalanceUSD = microsToUSD(amount), microsToUSD(balance)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LedgerSum is the authority the materialised balance is checked against. Unused by the hot path
// on purpose: it walks the whole statement, and the gate reads one column instead.
func (s *Store) LedgerSum(ctx context.Context, orgID int64) (int64, error) {
	var sum sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`select coalesce(sum(amount_micros),0) from credit_ledger where org_id=?`, orgID).Scan(&sum)
	return sum.Int64, err
}

// CreditedPayment finds the top-up a Stripe payment became, by the payment intent it was credited
// under. It is how a refund or a dispute is tied to an account: a dispute carries no customer and
// none of our metadata, and a refund of a subscription fee was never credit at all — so "which
// account's credit did this payment become, and how much" is answered by the ledger row itself,
// or not at all. external_id is unique across the ledger, so there is at most one.
func (s *Store) CreditedPayment(ctx context.Context, paymentIntent string) (orgID, micros int64, ok bool, err error) {
	if paymentIntent == "" {
		return 0, 0, false, nil
	}
	err = s.db.QueryRowContext(ctx,
		`select org_id, amount_micros from credit_ledger where external_id=? and kind=?`,
		paymentIntent, creditTopUp).Scan(&orgID, &micros)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return orgID, micros, true, nil
}

// LedgerByExternalID is one account's ledger movements of one kind, summed by external id. Refunds
// and disputes are few per account, so matching them by id is done by the caller rather than with
// LIKE, whose _ would match any character of a Stripe id.
func (s *Store) LedgerByExternalID(ctx context.Context, orgID int64, kind string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`select coalesce(external_id,''), amount_micros from credit_ledger where org_id=? and kind=?`, orgID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var ext string
		var m int64
		if err := rows.Scan(&ext, &m); err != nil {
			return nil, err
		}
		out[ext] += m
	}
	return out, rows.Err()
}

// ErrAlreadyCredited says the money in hand has already been recorded. It is not a failure: it is
// what a redelivered webhook, a retried roll-up or a double-clicked button is supposed to produce,
// and the caller answers 200 to it.
var ErrAlreadyCredited = errors.New("this payment is already on the ledger")

// CreditMovement is a single addition to (or subtraction from) an organisation's balance.
type CreditMovement struct {
	OrgID      int64
	Kind       string
	Micros     int64  // signed: positive adds, negative takes away
	ExternalID string // the idempotency key; never empty
	Note       string
	Actor      string
	Currency   string
	// Enforce turns the credit floor on. True for money the customer can spend — a top-up, an
	// operator's grant — and false for a refund, which must not switch enforcement on for an
	// account that never had it.
	Enforce bool
}

// MoveCredit writes one ledger row and moves the balance by the same amount, in one transaction.
//
// The uniqueness of external_id is the whole of the idempotency, and it is checked by INSERT
// rather than by a SELECT first: two instances handling the same redelivered event race, and only
// one row can exist. `on conflict do nothing returning id` and sql.ErrNoRows, never a failed insert
// caught afterwards — on Postgres a failed statement aborts the surrounding transaction, which is
// the trap db_dialect.go documents and which broke the first Postgres boot.
func (s *Store) MoveCredit(ctx context.Context, m CreditMovement) (balance int64, err error) {
	if m.ExternalID == "" {
		return 0, errors.New("a credit movement needs an idempotency key")
	}
	if m.Currency == "" {
		m.Currency = "usd"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if err := ensureBillingRow(ctx, tx, m.OrgID); err != nil {
		return 0, err
	}

	// The balance moves first, so the row can record the balance it produced. Locking is the
	// row's own: on SQLite the pool is a single writer, and on Postgres the UPDATE takes the row.
	enforce := 0
	if m.Enforce {
		enforce = 1
	}
	topUp := int64(0)
	if m.Micros > 0 && (m.Kind == creditTopUp || m.Kind == creditGrant || m.Kind == creditIncluded) {
		topUp = m.Micros
	}
	var after int64
	err = tx.QueryRowContext(ctx,
		`update billing_accounts
		    set credit_balance_micros = credit_balance_micros + ?,
		        lifetime_topup_micros = lifetime_topup_micros + ?,
		        credit_enforced       = case when ?=1 then 1 else credit_enforced end,
		        updated_at            = ?
		  where org_id=?
		returning credit_balance_micros`, m.Micros, topUp, enforce, now(), m.OrgID).Scan(&after)
	if err != nil {
		return 0, err
	}

	var id int64
	err = tx.QueryRowContext(ctx,
		`insert into credit_ledger (org_id, created_at, kind, amount_micros, balance_after_micros,
			currency, external_id, note, actor)
		 values (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 on conflict (external_id) do nothing
		 returning id`,
		m.OrgID, now(), m.Kind, m.Micros, after, m.Currency, m.ExternalID, m.Note, m.Actor).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Already on the ledger. Roll back, including the balance move above: the money was
		// counted the first time and counting it again is the bug this index exists to prevent.
		return 0, ErrAlreadyCredited
	}
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return after, nil
}

// ---- the per-turn debit ----

// chargeCredit takes one turn's cost off the balance, inside the transaction that is also writing
// the usage row. It writes no ledger row: the roll-up does that once an hour, and the two counters
// keep the arithmetic exact in between (see the invariant in the migration).
//
// The predicate is credit_enforced=1, so an organisation with no billing row and a subscriber who
// has never topped up both match nothing and cost one index probe. sql.ErrNoRows is the normal
// answer here, not a failure.
//
// Nothing clamps at zero. A turn is gated before it runs and costed after, so the balance can go
// negative by the work already in flight; clamping would destroy money that is owed and make the
// ledger stop adding up. budgetOK refuses the next turn instead, at maxOverdraftMicros.
//
// THE SPLIT. The plan's monthly allowance is spent before prepaid credit, because it is the money
// that expires — spending a customer's own balance while an allowance evaporates beside it would
// be the wrong order in the one direction they would notice. Only the remainder, the part no
// allowance covered, touches credit_balance_micros and lifetime_debit_micros; allowance-funded
// spend touches neither, which is exactly what keeps the ledger invariant exact.
//
// It stays ONE statement. A read-then-write would lose updates under the eight concurrent turns
// per container that billing_policy.go describes; the current form is safe because every SET
// expression reads the pre-update row, on both dialects. Hence the CASEs rather than min() —
// which is scalar in SQLite and aggregate-only in Postgres, and so would be dialect-specific SQL
// outside db_dialect.go, which TestNoDialectSpecificSQL refuses.
func chargeCredit(ctx context.Context, tx *dbTx, orgID, micros int64) error {
	if micros <= 0 {
		return nil
	}
	// Written out rather than built, so that the statement a reader sees is the statement that
	// runs, and so TestEveryBalanceWriteLivesInTheLedgerFile can find it.
	//
	// The parameters are, in order: micros (allowance test), micros (allowance subtraction),
	// now (liveness), micros (balance test), micros, micros (balance remainder), now (liveness),
	// micros (debit test), micros, micros (debit remainder), now (liveness), now (updated_at),
	// orgID, now (the where clause's own liveness test).
	_, err := tx.ExecContext(ctx,
		`update billing_accounts
		    set allowance_micros = case
		          when allowance_micros > ? and allowance_period_end <> '' and allowance_period_end >= ?
		          then allowance_micros - ?
		          else 0 end,
		        credit_balance_micros = credit_balance_micros - case
		          when allowance_period_end = '' or allowance_period_end < ? then ?
		          when allowance_micros >= ? then 0
		          else ? - allowance_micros end,
		        lifetime_debit_micros = lifetime_debit_micros + case
		          when allowance_period_end = '' or allowance_period_end < ? then ?
		          when allowance_micros >= ? then 0
		          else ? - allowance_micros end,
		        updated_at = ?
		  where org_id=? and (credit_enforced=1 or (allowance_period_end <> '' and allowance_period_end >= ?))`,
		micros, now(), micros,
		now(), micros, micros, micros,
		now(), micros, micros, micros,
		now(), orgID, now())
	return err
}

// ---- the hourly roll-up ----

// OrgsWithUnrolledDebit is every organisation that has spent since its statement was last written
// up. Deployment-wide by design: the roll-up runs for all of them behind the leader lease.
func (s *Store) OrgsWithUnrolledDebit(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`select org_id from billing_accounts where lifetime_debit_micros > debit_rolled_micros`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RollUpDebit writes one debit line for everything spent since the last one, and advances the
// watermark. Returns the amount written, 0 when there was nothing to write.
//
// Two things make it safe to run twice. The ledger key carries the organisation and the hour, so a
// retry inside the same hour hits the unique index and writes nothing; and the watermark advances
// by compare-and-swap on the value this call read, so an instance that lapped the lease cannot
// move it twice. A claim-and-lock would be the obvious alternative and is not available: `skip
// locked` is banned in Go SQL literals because SQLite has no spelling for it.
func (s *Store) RollUpDebit(ctx context.Context, orgID int64, orgPublic string, at time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var lifetime, rolled, balance int64
	err = tx.QueryRowContext(ctx,
		`select lifetime_debit_micros, debit_rolled_micros, credit_balance_micros
		   from billing_accounts where org_id=?`, orgID).Scan(&lifetime, &rolled, &balance)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	delta := lifetime - rolled
	if delta <= 0 {
		return 0, nil
	}

	key := fmt.Sprintf("debit:%s:%s", orgPublic, at.UTC().Format("2006-01-02T15"))
	var id int64
	err = tx.QueryRowContext(ctx,
		`insert into credit_ledger (org_id, created_at, kind, amount_micros, balance_after_micros,
			currency, external_id, note, actor)
		 values (?, ?, ?, ?, ?, 'usd', ?, 'Model spend', 'system')
		 on conflict (external_id) do nothing
		 returning id`, orgID, now(), creditDebit, -delta, balance, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil // this hour is already written up
	}
	if err != nil {
		return 0, err
	}

	// Compare-and-swap: only advance the watermark from the value this transaction read.
	res, err := tx.ExecContext(ctx,
		`update billing_accounts set debit_rolled_micros=?, updated_at=?
		  where org_id=? and debit_rolled_micros=?`, lifetime, now(), orgID, rolled)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, nil // somebody else got there first; their row stands and ours rolls back
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return delta, nil
}

// ---- the subscription record ----

// SubscriptionState is what a webhook knows about a subscription after reading one event.
type SubscriptionState struct {
	CustomerID        string
	SubscriptionID    string
	Status            string
	Size              string
	UnitPriceMicros   int64
	Quantity          int64
	Currency          string
	PeriodStart       string
	PeriodEnd         string
	CancelAtPeriodEnd bool
}

// PutSubscription writes the subscription half of an organisation's billing row, leaving the
// credit half alone. Every field comes from a signature-verified Stripe event; nothing here is
// ever taken from a browser.
func (s *Store) PutSubscription(ctx context.Context, orgID int64, st SubscriptionState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ensureBillingRow(ctx, tx, orgID); err != nil {
		return err
	}
	cancel := 0
	if st.CancelAtPeriodEnd {
		cancel = 1
	}
	if st.Currency == "" {
		st.Currency = "usd"
	}
	if _, err := tx.ExecContext(ctx,
		`update billing_accounts
		    set customer_id=?, subscription_id=?, status=?, size=?, unit_price_micros=?, quantity=?,
		        currency=?, period_start=?, period_end=?, cancel_at_period_end=?, updated_at=?
		  where org_id=?`,
		st.CustomerID, st.SubscriptionID, st.Status, st.Size, st.UnitPriceMicros, st.Quantity,
		st.Currency, st.PeriodStart, st.PeriodEnd, cancel, now(), orgID); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- webhook idempotency ----

// SeenBillingEvent puts an event on record by its id. A handler calls it last, once everything the
// event does has happened, so an event on record has had its whole effect and a failure before
// that leaves it for Stripe's retry. One this deployment deliberately does not act on — for an
// organisation it does not have, or of a type it does not handle — is recorded straight away, so
// its redeliveries are recognised rather than reconsidered.
func (s *Store) SeenBillingEvent(ctx context.Context, provider, eventID, eventType string) {
	s.db.ExecContext(ctx,
		`insert into billing_events (event_id, provider, type, received_at) values (?, ?, ?, ?)
		 on conflict (event_id) do nothing`, eventID, provider, eventType, now())
}

// HasBillingEvent says whether an event is on record: applyStripeEvent skips one that is, and an
// "ended:" row is how subscriptionEnded knows a subscription has ended.
func (s *Store) HasBillingEvent(ctx context.Context, eventID string) bool {
	var got string
	err := s.db.QueryRowContext(ctx, `select event_id from billing_events where event_id=?`, eventID).Scan(&got)
	return err == nil
}

// SweepBillingEvents drops dedup keys older than Stripe could still be retrying. Its own age GC,
// not anybody's retention policy — the rows carry no tenant content. The "ended:" rows are not
// dedup keys but the record that a subscription has ended (billing.go, subscriptionEnded), which
// stays true however old it is, so they are kept.
func (s *Store) SweepBillingEvents(ctx context.Context, older time.Duration) {
	s.db.ExecContext(ctx, `delete from billing_events where received_at < ? and event_id not like 'ended:%'`, nowMinus(older))
}
