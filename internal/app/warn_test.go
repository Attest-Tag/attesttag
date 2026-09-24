package app

// The banner that fires before anything stops. Its whole value is being early and being right:
// a warning that fires late is decoration, and one that fires at the wrong threshold teaches
// people to ignore the one that matters.

import (
	"testing"
	"time"
)

func warnCfg() Config {
	cfg := includedCfg()
	cfg.CreditLowUSD = 10
	return cfg
}

// live is an account on a size, with an allowance whose period has not passed.
func live(size string, granted, left, prepaid int64) BillingAccount {
	return BillingAccount{
		Exists: true, Status: "active", Size: size,
		AllowanceGrantedMicros: granted, AllowanceMicros: left,
		AllowancePeriodEnd:  time.Now().UTC().AddDate(0, 0, 7).Format(time.DateTime),
		CreditBalanceMicros: prepaid,
	}
}

func TestTheCreditWarningFiresAtNinetyPercentOfTheMonthsAllowance(t *testing.T) {
	cfg, st := warnCfg(), Settings{}
	// $20 a month. $2.00 left is exactly ten percent, which is the boundary and counts.
	if got := warnedBy(st, live("upto_25", usdToMicros(20), usdToMicros(2), 0), cfg, 0, 0); got != warnCredit {
		t.Errorf("at exactly 90%% spent the warning was %q, want credit", got)
	}
	if got := warnedBy(st, live("upto_25", usdToMicros(20), usdToMicros(2)+1, 0), cfg, 0, 0); got != warnNone {
		t.Errorf("a cent inside the threshold warned %q; the bound is the warning, not the round number", got)
	}
	// Prepaid credit counts towards what is left: somebody holding $500 is not nearly out of
	// anything, and warning them would be the fastest way to teach them to ignore the banner.
	if got := warnedBy(st, live("upto_25", usdToMicros(20), 0, usdToMicros(500)), cfg, 0, 0); got != warnNone {
		t.Errorf("an account with $500 of prepaid credit warned %q", got)
	}
}

// Without an allowance there is no denominator to take a fraction of — a balance is a balance,
// not a budget — so it falls back to the threshold that already exists for this question.
func TestWithoutAnAllowanceTheWarningUsesTheLowBalanceSetting(t *testing.T) {
	cfg, st := warnCfg(), Settings{}
	prepaid := func(usd float64) BillingAccount {
		return BillingAccount{Exists: true, Status: "active", CreditEnforced: true,
			CreditBalanceMicros: usdToMicros(usd)}
	}
	if got := warnedBy(st, prepaid(10), cfg, 0, 0); got != warnCredit {
		t.Errorf("at the low-balance threshold the warning was %q, want credit", got)
	}
	if got := warnedBy(st, prepaid(10.01), cfg, 0, 0); got != warnNone {
		t.Errorf("above the threshold it warned %q", got)
	}
}

// An account nothing meters has no credit to be nearly out of. Warning a free workspace that its
// credit is low would be telling it about money it never had.
func TestAnUnmeteredAccountIsNeverWarnedAboutCredit(t *testing.T) {
	if got := warnedBy(Settings{}, BillingAccount{}, warnCfg(), 0, 0); got != warnNone {
		t.Errorf("an account with no billing row warned %q", got)
	}
}

func TestTheUserWarningFiresAtNinetyPercentOfTheSize(t *testing.T) {
	cfg, st := warnCfg(), Settings{}
	// upto_25 allows 25, so 23 is under and 23 is not — 22.5 rounds the boundary to 23.
	full := live("upto_25", usdToMicros(20), usdToMicros(20), 0)
	if got := warnedBy(st, full, cfg, 22, userLimitOf(full, cfg)); got != warnNone {
		t.Errorf("22 of 25 warned %q, want nothing", got)
	}
	if got := warnedBy(st, full, cfg, 23, userLimitOf(full, cfg)); got != warnUsers {
		t.Errorf("23 of 25 warned %q, want users", got)
	}
	// Over the limit still warns. Going quiet at exactly the point the account has the problem
	// would be the worst possible moment to stop talking.
	if got := warnedBy(st, full, cfg, 40, userLimitOf(full, cfg)); got != warnUsers {
		t.Errorf("40 of 25 warned %q, want users", got)
	}
}

// A size sold without a ceiling can never be near one.
func TestAnUnlimitedSizeNeverWarnsAboutUsers(t *testing.T) {
	full := live("unlimited", usdToMicros(250), usdToMicros(250), 0)
	if got := warnedBy(Settings{}, full, warnCfg(), 100000, userLimitOf(full, warnCfg())); got != warnNone {
		t.Errorf("unlimited warned %q at a hundred thousand users", got)
	}
}

// Credit first, for the reason pausedBy checks it first: it is the one that stops the bot, and
// somebody told about their user count while their credit runs out has been told the lesser thing.
func TestCreditIsWarnedAboutBeforeUsers(t *testing.T) {
	both := live("upto_25", usdToMicros(20), usdToMicros(1), 0)
	if got := warnedBy(Settings{}, both, warnCfg(), 25, userLimitOf(both, warnCfg())); got != warnCredit {
		t.Errorf("with both limits close the warning was %q, want credit", got)
	}
}

// A lapsed allowance is not credit, so an account whose plan ended is not warned about running
// out of something it no longer has — it is simply no longer metered.
func TestALapsedAllowanceDoesNotWarn(t *testing.T) {
	stale := live("upto_25", usdToMicros(20), usdToMicros(1), 0)
	stale.AllowancePeriodEnd = time.Now().UTC().AddDate(0, 0, -1).Format(time.DateTime)
	if got := warnedBy(Settings{}, stale, warnCfg(), 0, 0); got != warnNone {
		t.Errorf("a lapsed allowance warned %q", got)
	}
}

// The limit the banner quotes has to be the limit the tile quotes.
func TestTheUserLimitHasOneDefinition(t *testing.T) {
	cfg := warnCfg()
	if got := userLimitOf(live("upto_25", 0, 0, 0), cfg); got != 25 {
		t.Errorf("userLimitOf says %d for upto_25, want 25", got)
	}
	if got := userLimitOf(live("unlimited", 0, 0, 0), cfg); got != 0 {
		t.Errorf("userLimitOf says %d for unlimited, want 0", got)
	}
	// A cancelled plan sells no ceiling.
	gone := live("upto_25", 0, 0, 0)
	gone.Status = "canceled"
	if got := userLimitOf(gone, cfg); got != 0 {
		t.Errorf("a cancelled plan still claimed a limit of %d", got)
	}
}
