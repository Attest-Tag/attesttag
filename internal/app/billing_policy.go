package app

// The policy half of billing: the numbers and sentences that decide when an account stops, kept
// apart from the routes (billing.go) and the store (store_billing.go) because these are the parts
// a reader is most likely to be looking for and the parts a change to needs arguing.

import (
	"context"
	"fmt"
	"time"
)

// maxOverdraftMicros is the most an account may spend past a zero balance before it is refused.
//
// Some overshoot is arithmetic rather than a bug: a turn is checked before it runs and priced
// after it finishes, so whatever is already in flight when the balance reaches zero will still be
// charged. The window is wider than it looks, and the figure below is measured rather than
// guessed:
//
//   - PlatformMaxInFlightPerOrg (8) is counted from a map in this process (runs.go), so it is
//     eight per container, not eight per account.
//   - The routine scheduler adds schedulerConcurrency (8) of its own.
//   - budgetRoom reserves nothing: two containers reading the same remaining balance both pass.
//   - MaxToolRounds is 50 and every round re-sends the transcript, so one turn can be dollars.
//
// That puts the worst case in the low hundreds on a multi-container deployment. $50 is under it,
// deliberately: it is the number the terms name, so it is the number a customer has agreed to,
// and anything beyond it is refused outright rather than quietly spent. Moving it is a deliberate
// act — TestSpendStopsWithinTheDocumentedOverdraft cites this constant, and the fees clause on the
// website quotes it.
const maxOverdraftMicros = 50 * microsPerUSD

// creditLowMicros is where an account starts being warned, from the deployment's setting. The
// warning is not a refusal: the bot keeps answering all the way to the overdraft floor.
func creditLowMicros(cfg Config) int64 { return usdToMicros(cfg.CreditLowUSD) }

// creditAmount renders a balance for a person, negative included. A balance below zero is not an
// error to be hidden — it is work that was already running when the money ran out — so it is shown
// as what it is rather than clamped to $0.00, which would leave somebody topping up and wondering
// where their first dollars went.
func creditAmount(micros int64) int64Amount { return int64Amount(micros) }

type int64Amount int64

func (a int64Amount) String() string {
	usd := microsToUSD(int64(a))
	if usd < 0 {
		return fmt.Sprintf("-$%.2f", -usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

// creditState is which limit, if any, has stopped an account — the one thing the console must not
// work out for itself. Deriving it there would be a second copy of the rule the bot applies, and
// the two would drift; the server refuses the turn, so the server says why.
const (
	pausedNone   = ""
	pausedCredit = "credit"
	pausedBudget = "budget"
)

// pausedBy answers that question for one account: out of credit, over its own monthly budget, or
// running. Credit is checked first for the same reason budgetOK checks it first.
//
// "Out of credit" now covers two pockets. An account may be metered because it has bought credit
// (credit_enforced, which never goes back off) or because its plan includes a monthly allowance
// and that period is still live. The second lapses on its own when a subscription ends, which is
// what stops a cancelled plan from leaving the floor on over a zero balance.
// spendableMicros is this month's allowance plus prepaid credit, and metered says whether the
// floor applies to either. Both come from the account rather than from st, which is a cache.
//
// An organisation on its own model key spends no credit, so credit cannot be what stopped it.
func pausedBy(st Settings, metered bool, spendableMicros int64, monthSpendUSD float64) string {
	if metered && !st.OwnKey.Active() && spendableMicros <= -maxOverdraftMicros {
		return pausedCredit
	}
	if b := st.EffectiveBudget(); b > 0 && monthSpendUSD >= b {
		return pausedBudget
	}
	return pausedNone
}

// budgetSpend is the month's spend an account's monthly limits are measured against: what it has
// spent on the key it is using now. The deployment's ceilings exist to protect the deployment's
// key, so spend on the organisation's own key must not count towards them once that key is
// removed — or an account that spent $300 on its own provider would come back to the included
// models already over a $100 deal ceiling, stopped for the rest of the month by money it never
// spent here. The same the other way: while its own key is in use, its own budget measures what
// that key spends. The workspace and channel budgets are the organisation's own guard rails on
// everything, and keep reading MonthSpend.
func budgetSpend(ctx context.Context, st *Store, orgID int64, set Settings) (float64, error) {
	owner := keyOwnerPlatform
	if set.OwnKey.Active() {
		owner = keyOwnerOrg
	}
	return st.MonthSpendOn(ctx, orgID, owner)
}

// ---- warning, before anything stops ----

// The two things a person can still do something about, named the way pausedBy names the two that
// have already stopped the bot. Empty means nothing is close.
const (
	warnNone   = ""
	warnCredit = "credit"
	warnUsers  = "users"
)

// warnAtFraction is how far into a limit counts as "nearly there". One constant for both, because
// a banner that fired at 90% of one thing and 75% of another would be a rule nobody could state.
const warnAtFraction = 0.9

// warnedBy is pausedBy's earlier sibling: which limit this account is about to reach, while there
// is still time to act. Same principle as pausedBy and for the same reason — the server decides,
// because the console deriving "are they nearly out" from two numbers would be a second copy of a
// rule that would then drift from this one.
//
// Credit is checked first, exactly as in pausedBy: it is the one that stops the bot, and a person
// told about their user count while their credit is about to run out has been told the less
// urgent of two things.
//
// The credit rule has two halves because an account can hold two different kinds of money:
//
//   - With a plan allowance, "nearly out" is measured against a month's worth of it. Ten percent
//     of the month's allowance left, counting prepaid credit too — so somebody holding a large
//     balance is correctly not warned, because they are not nearly out of anything.
//   - Without one, there is no denominator to take a fraction of: a prepaid balance is a balance,
//     not a budget. So it falls back to CreditLowUSD, which already exists for exactly this
//     question and which alertLowCredit already warns on in Slack and by mail.
//
// userLimit is the ceiling the tile shows — ActiveUserCount.Limit, from activeUsers — passed in
// rather than looked up here, so the banner and the tile cannot quote different limits. An
// enterprise deal's ceiling lives in the database, which this function cannot see.
func warnedBy(st Settings, acct BillingAccount, cfg Config, activeUsers, userLimit int) string {
	if acct.Metered() && !st.OwnKey.Active() { // own-key spend draws no credit to run out of
		left := acct.SpendableMicros()
		switch granted := acct.AllowanceGrantedMicros; {
		case granted > 0:
			if left <= int64(float64(granted)*(1-warnAtFraction)) {
				return warnCredit
			}
		default:
			if left <= creditLowMicros(cfg) {
				return warnCredit
			}
		}
	}
	// Deliberately not gated on being under the limit. Over it is also worth a banner, and the
	// wording the console picks differs; what it must not do is go quiet at exactly the point the
	// account has the problem.
	if userLimit > 0 && float64(activeUsers) >= float64(userLimit)*warnAtFraction {
		return warnUsers
	}
	return warnNone
}

// userLimitOf is the ceiling the account's current plan size sells, 0 for a size with none and for
// an account that has not bought one. One place, so the banner, the tile and the API cannot
// disagree about what the limit is.
func userLimitOf(acct BillingAccount, cfg Config) int {
	if !acct.Exists || !acct.Active() {
		return 0
	}
	if size, ok := cfg.SizeByKey(acct.Size); ok {
		return size.UserLimit
	}
	return 0
}

// jobLimitOf is the fix jobs a month the account's plan is sold with: the size's figure for a
// bought or comped size, the free plan's for an account on it, and 0 where nothing was printed —
// a pro plan granted by hand, a retired size, or one sold as a conversation. Read from
// sizeJobLimits rather than through SizeByKey so a comped account on a size this deployment no
// longer sells still sees the figure it was granted. Printed and counted, never enforced.
func jobLimitOf(acct BillingAccount, plan string) int {
	if acct.Exists && acct.Active() && acct.Size != "" {
		return sizeJobLimits[acct.Size]
	}
	if plan == PlanFree {
		return freePlanJobsPerMonth
	}
	return 0
}

// alertLowCredit is the AlertOnce key for "this account is running out of credit". One key for
// both the Slack alert and the email, so an account is not told twice by two routes, and cleared
// on a top-up (Store.ClearAlert) so the next time it runs low is news rather than a repeat.
//
// Deliberately not a column on billing_accounts: AlertOnce already counts in the database rather
// than in this process, is already swept with the organisation, and is already tested.
const alertLowCredit = "billing:low-credit"

// lowCreditWindow is how long one warning suppresses the next. Long enough that an account
// hovering near the threshold is not mailed daily, short enough that a slow burn is still noticed
// before it stops.
const lowCreditWindow = 72 * time.Hour
