package app

import (
	"math"
	"testing"
)

// A provider's cost field is a number we are told, not a number we computed, and strconv accepts
// several strings that are not money. This is the guard on that boundary; the bug it was written
// for is in validCost's own comment.
func TestValidCostRefusesWhatIsNotMoney(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want float64
	}{
		{"0.0031", 0.0031},
		{"0", 0},
		{"1e-7", 1e-7},

		// The one that mattered: ParseFloat accepts every spelling of these, and a single such
		// row used to make sum(cost_usd) NaN for the rest of the month on Postgres.
		{"NaN", 0},
		{"nan", 0},
		{"+Inf", 0},
		{"Infinity", 0},
		{"-Inf", 0},

		{"-1.5", 0}, // a negative charge is a credit nobody asked for
		{"", 0},
		{"null", 0},
		{`"0.01"`, 0}, // quoted: the raw JSON of a string, not of a number
	} {
		if got := validCost(tc.raw); got != tc.want {
			t.Errorf("validCost(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// The property the budget check actually depends on, stated on its own: whatever validCost
// returns can be compared. Against NaN every comparison is false, which is how a budget stops
// binding without anything failing.
func TestValidCostAlwaysComparesAgainstABudget(t *testing.T) {
	for _, raw := range []string{"NaN", "Infinity", "-Inf", "abc", "-1"} {
		spent := validCost(raw)
		if math.IsNaN(spent) || math.IsInf(spent, 0) {
			t.Fatalf("validCost(%q) returned %v, which no budget comparison can be true against", raw, spent)
		}
		if !(spent >= 0) {
			t.Fatalf("validCost(%q) = %v: a spend that is not >= 0 cannot exhaust a budget", raw, spent)
		}
	}
}
