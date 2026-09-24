package app

import (
	"strconv"
	"testing"
)

// The caps are sized for a service with open signup. On a twenty-person deployment behind an
// invitation they are in the way, so they can be moved — and a value that is not a positive
// number must be refused rather than quietly applied, because a limit nobody chose is worse
// than the default nobody thought about.
func TestLimitOverrides(t *testing.T) {
	// Every entry points at a real variable and each is named once.
	seen := map[string]bool{}
	for _, o := range limitOverrides {
		if o.to == nil {
			t.Fatalf("%s points at nothing", o.env)
		}
		if seen[o.env] {
			t.Errorf("%s appears twice", o.env)
		}
		seen[o.env] = true
	}

	for _, o := range limitOverrides {
		was := *o.to
		t.Run(o.env, func(t *testing.T) {
			t.Cleanup(func() { *o.to = was })

			// A number is taken.
			t.Setenv(o.env, "7")
			applyLimitOverrides()
			if *o.to != 7 {
				t.Errorf("%s=7 left the limit at %d", o.env, *o.to)
			}

			// Nonsense is refused, and the previous value stands.
			for _, bad := range []string{"0", "-1", "lots", "7.5", " "} {
				*o.to = 42
				t.Setenv(o.env, bad)
				applyLimitOverrides()
				if *o.to != 42 {
					t.Errorf("%s=%q was applied as %d", o.env, bad, *o.to)
				}
			}

			// Unset leaves it alone, which is what every existing deployment gets.
			*o.to = 99
			t.Setenv(o.env, "")
			applyLimitOverrides()
			if *o.to != 99 {
				t.Errorf("%s unset changed the limit to %d", o.env, *o.to)
			}
		})
	}
}

// The defaults have not moved: this was meant to make them adjustable, not to change them.
func TestLimitDefaultsUnchanged(t *testing.T) {
	for name, got := range map[string]int{
		"signupsPerIPPerHour":   signupsPerIPPerHour,
		"invitesPerOrgPerHour":  invitesPerOrgPerHour,
		"invitesPerAddressADay": invitesPerAddressADay,
		"maxRoutinesPerOrg":     maxRoutinesPerOrg,
		"maxDocumentsPerOrg":    maxDocumentsPerOrg,
		"maxConnectionsPerOrg":  maxConnectionsPerOrg,
		"maxBundlesPerOrg":      maxBundlesPerOrg,
		"maxMemoriesPerOrg":     maxMemoriesPerOrg,
		"maxDriveSyncsPerOrg":   maxDriveSyncsPerOrg,
	} {
		want := map[string]int{
			"signupsPerIPPerHour": 5, "invitesPerOrgPerHour": 20, "invitesPerAddressADay": 3,
			"maxRoutinesPerOrg": 50, "maxDocumentsPerOrg": 2000, "maxConnectionsPerOrg": 100,
			"maxBundlesPerOrg": 50, "maxMemoriesPerOrg": 500, "maxDriveSyncsPerOrg": 25,
		}[name]
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	_ = strconv.Itoa(0)
}
