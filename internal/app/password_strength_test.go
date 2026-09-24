package app

import (
	"strings"
	"testing"
)

// The password from the report. "1234567890" is ten characters, which was the whole of the
// policy, and it is also somewhere in the first dozen guesses anybody makes.
func TestPredictablePasswordsAreRefused(t *testing.T) {
	refused := []string{
		// runs along the keyboard
		"1234567890", "0987654321", "qwertyuiop", "poiuytrewq", "1q2w3e4r5t",
		"abcdefghij", "asdfghjkl", "qazwsxedcr",
		// a word with decoration on it
		"password12", "Password123", "P@ssw0rd!23", "passw0rd11", "letmein2024",
		"welcome2024", "iloveyou123", "monkey12345", "dragon12345", "football24",
		"princess99", "superman123", "trustno1234", "chocolate1", "sunshine11",
		// a shorter password written twice
		"abababababab", "password1password1", "aaaaaaaaaa", "abcabcabcabc",
	}
	for _, pw := range refused {
		if p := passwordProblem(pw); p == "" {
			t.Errorf("passwordProblem(%q) allowed it", pw)
		}
	}
}

// The other half of the bargain: the point of judging the guess rather than its shape is that
// anything not on the list is fine. No capital is demanded, no digit, no symbol.
func TestOrdinaryPasswordsAreAllowed(t *testing.T) {
	allowed := []string{
		"a perfectly ordinary password",
		"correct horse battery staple",
		"unremarkable sentence here",
		"Tr0ub4dor&3xyz",
		"quiet-harbour-lantern",
		"zbnqwleirjas",
		"harbourlightharbourlight", // the unit is long enough to have passed on its own
		"acme.com is where I work",     // a common word inside is not a common password
	}
	for _, pw := range allowed {
		if p := passwordProblem(pw); p != "" {
			t.Errorf("passwordProblem(%q) refused it: %s", pw, p)
		}
	}
}

// A password made out of the things written on the account is a password an attacker starts
// with rather than arrives at.
func TestPasswordCannotBeTheAccount(t *testing.T) {
	email, name, org := "grace@example.com", "Grace Hopper", "Example Co"
	for _, pw := range []string{"grace-secret-1", "ExampleDocs123", "xxgracexxyy", "hopper-is-here"} {
		if p := passwordProblem(pw, email, name, org); p == "" {
			t.Errorf("passwordProblem(%q) allowed a password built from the account", pw)
		}
	}
	// ...and the same password is fine on an account it says nothing about.
	if p := passwordProblem("correct-horse-9", "grace@example.com", "Grace Hopper", "Analytical"); p != "" {
		t.Errorf("refused a password unrelated to this account: %s", p)
	}
}

// Length still comes first, and its message is still the one the form shows.
func TestLengthRulesStillApply(t *testing.T) {
	if p := passwordProblem("short"); !strings.Contains(p, "at least") {
		t.Errorf("a short password should be refused for its length, got %q", p)
	}
	if p := passwordProblem(strings.Repeat("x", 73)); !strings.Contains(p, "shorter") {
		t.Errorf("an over-long password should be refused for its length, got %q", p)
	}
}

// passwordBase is what lets a few hundred words answer for the thousands of ways people dress
// them up.
func TestPasswordBaseUndoesDecoration(t *testing.T) {
	for in, want := range map[string]string{
		"password123": "password",
		"p@ssw0rd!":   "password",
		"password":    "password",
		"letmein2024": "letmein",
		"!!monkey!!":  "monkey",
	} {
		if got := passwordBase(strings.ToLower(in)); got != want {
			t.Errorf("passwordBase(%q) = %q, want %q", in, got, want)
		}
	}
}
