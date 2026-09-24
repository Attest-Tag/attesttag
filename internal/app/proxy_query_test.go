package app

import (
	"net/url"
	"strings"
	"testing"
)

// The model writes a Drive query the way every Drive example writes it, spaces and all. Go
// hands RawQuery to the transport untouched, so the space used to reach the request line and
// Google answered 400 without reading the query. Only bytes that cannot travel there are
// touched: a query that arrived encoded has to come back identical, because re-encoding one
// that an API reads literally would change the question being asked.
func TestWireSafeQueryEncodesOnlyWhatCannotTravel(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"drive query as the model types it",
			"q=fullText contains 'SOC 2'&fields=files(id,name)",
			"q=fullText%20contains%20'SOC%202'&fields=files(id,name)"},
		{"already encoded, left alone",
			"q=fullText%20contains%20%27SOC%202%27&pageSize=20",
			"q=fullText%20contains%20%27SOC%202%27&pageSize=20"},
		{"nothing to do", "a=1&b=2&fields=files(id,name)", "a=1&b=2&fields=files(id,name)"},
		{"empty", "", ""},
		{"a newline or a tab breaks the request line too", "q=a\nb\tc", "q=a%0Ab%09c"},
		{"non-ascii becomes its utf-8 bytes", "q=café", "q=caf%C3%A9"},
	} {
		if got := wireSafeQuery(tc.in); got != tc.want {
			t.Errorf("%s: wireSafeQuery(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// The fault was never in the helper, it was in what reached the wire: RequestURI is the
// request line. This pins both halves — no raw space leaves, and the service is still asked
// the same question it would have been asked if the space had been legal.
func TestNormalisedQueryLeavesTheRequestLineIntact(t *testing.T) {
	u, err := url.Parse("https://www.googleapis.com/drive/v3/files?q=name contains 'soc'&pageSize=50")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.RequestURI(), " ") {
		t.Fatal("url.Parse no longer passes a raw space through to the request line; wireSafeQuery may have outlived its reason")
	}
	u.RawQuery = wireSafeQuery(u.RawQuery)
	if strings.Contains(u.RequestURI(), " ") {
		t.Errorf("request line still carries a raw space: %q", u.RequestURI())
	}
	if got := u.Query().Get("q"); got != "name contains 'soc'" {
		t.Errorf("q decoded to %q, want %q", got, "name contains 'soc'")
	}
	if got := u.Query().Get("pageSize"); got != "50" {
		t.Errorf("pageSize decoded to %q, want 50", got)
	}
}
