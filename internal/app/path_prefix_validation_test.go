package app

import "strings"

import "testing"

// A path prefix is compared literally against a URL path, which always starts with "/". One that
// does not — "api/v2", or a space in front of it — therefore matches nothing, and a connection
// carrying it allows no path at all while reading in the console as though it allows that path.
// It is refused on save rather than repaired: adding the slash would turn a rule that allows
// nothing into one that allows a whole path tree, which is not a correction anybody asked for.
func TestPathPrefixMustBeAPath(t *testing.T) {
	b := &Bot{}
	sec := &Secret{Token: "t"}
	build := func(prefixes []string) error {
		_, _, err := b.buildConnection(&connectionInput{
			BundleID: 1, Name: "Billing", Preset: "custom", CredType: "bearer",
			AllowedHosts: []string{"app.example.com"}, PathPrefixes: prefixes, Secret: sec,
		}, nil)
		return err
	}

	for _, bad := range []string{"api/v2", " /api/v2", "/api/v2 ", "", "   "} {
		if err := build([]string{bad}); err == nil {
			t.Errorf("prefix %q was accepted; it can never match a URL path", bad)
		}
	}
	// One bad prefix beside a good one is still refused: the wide one would carry the traffic
	// and the narrow one would look like it was doing the work.
	if err := build([]string{"api/v1/billing/", "/"}); err == nil {
		t.Error(`["api/v1/billing/", "/"] was accepted; the first prefix matches nothing`)
	} else if !strings.Contains(err.Error(), "api/v1/billing/") {
		t.Errorf("error does not name the offending prefix: %v", err)
	}

	for _, ok := range [][]string{nil, {}, {"/"}, {"/api/v2"}, {"/api/v1/billing/", "/calendar/v3"}} {
		if err := build(ok); err != nil {
			t.Errorf("prefixes %q were refused: %v", ok, err)
		}
	}
}
