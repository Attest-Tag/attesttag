package app

import (
	"net/url"
	"strings"
	"testing"
)

// Two ClickUp workspaces and two GCP keys on one host is the case a channel cannot express:
// whichever sorts first answers every question, and being right half the time is the part
// nobody notices. A name chooses between credentials the channel already holds.
func TestMatchNamedChoosesBetweenConnectionsOnOneHost(t *testing.T) {
	prod := &Connection{ID: 1, Name: "ClickUp", AllowedHosts: []string{"api.clickup.com"}}
	test := &Connection{ID: 2, Name: "ClickUp (testing)", AllowedHosts: []string{"api.clickup.com"}}
	gcp := &Connection{ID: 3, Name: "gcp_testing", AllowedHosts: []string{"api.clickup.com"}}
	acc := &Access{Rules: []Rule{{Conn: prod, Rank: 9}, {Conn: test, Rank: 5}, {Conn: gcp, Rank: 1}}}
	p := NewProxy(nil, nil)
	u, _ := url.Parse("https://api.clickup.com/api/v2/team")

	for _, tc := range []struct {
		want string
		id   int64
	}{
		{"", 1},                  // unchanged: the highest-ranked connection still wins
		{"ClickUp", 1},           // exact
		{"clickup (testing)", 2}, // case and spacing are forgiven
		{"clickup_testing", 2},   // and so is punctuation the model invents
		{"testing", 0},           // ambiguous across two names: refuse rather than guess
		{"gcp testing", 3},       // an underscored name found by its words
		{"Notion", 0},            // a name this channel does not hold
	} {
		conn, why := p.MatchNamed(acc, "GET", u, tc.want)
		if tc.id == 0 {
			if conn != nil {
				t.Errorf("connection=%q picked %q, want a refusal", tc.want, conn.Name)
			}
			if !strings.Contains(why, "ClickUp") {
				t.Errorf("connection=%q was refused with %q, which does not name the alternatives", tc.want, why)
			}
			continue
		}
		if why != "" {
			t.Errorf("connection=%q blocked: %s", tc.want, why)
			continue
		}
		if conn == nil || conn.ID != tc.id {
			t.Errorf("connection=%q picked %+v, want id %d", tc.want, conn, tc.id)
		}
	}
}

// A name is a choice between credentials already granted, never a way to reach one that is
// not. Naming a connection the channel does not hold, or naming one that holds the host but
// not this path or method, has to stay blocked for exactly the reason it was blocked before.
func TestNameCannotWidenWhatAChannelReaches(t *testing.T) {
	drive := &Connection{ID: 1, Name: "Google Drive", AllowedHosts: []string{"www.googleapis.com"}, PathPrefixes: []string{"/drive/v3/"}}
	acc := &Access{Rules: []Rule{{Conn: drive, Rank: 9}}}
	p := NewProxy(nil, nil)

	// A host nobody in the channel holds stays blocked, name or no name.
	other, _ := url.Parse("https://api.stripe.com/v1/charges")
	if conn, why := p.MatchNamed(acc, "GET", other, "Google Drive"); conn != nil || why == "" {
		t.Errorf("naming a connection reached an unrelated host: conn=%+v why=%q", conn, why)
	}
	// The path guard runs before any name is consulted, so it still reports the real reason.
	gmail, _ := url.Parse("https://www.googleapis.com/gmail/v1/users/me/messages")
	if conn, why := p.MatchNamed(acc, "GET", gmail, "Google Drive"); conn != nil || !strings.Contains(why, "outside the allowed prefixes") {
		t.Errorf("naming a connection escaped its path prefixes: conn=%+v why=%q", conn, why)
	}
}

// The model is only told to choose when there is a choice; a channel with one credential per
// host should not pay prompt for a parameter it can never need.
func TestSharedHostsNamesOnlyTheContestedOnes(t *testing.T) {
	acc := &Access{Rules: []Rule{
		{Conn: &Connection{Name: "ClickUp", AllowedHosts: []string{"api.clickup.com"}}},
		{Conn: &Connection{Name: "ClickUp (testing)", AllowedHosts: []string{"api.clickup.com"}}},
		{Conn: &Connection{Name: "Notion", AllowedHosts: []string{"api.notion.com"}}},
	}}
	got := acc.SharedHosts()
	if len(got) != 1 || got[0] != "api.clickup.com (ClickUp, ClickUp (testing))" {
		t.Errorf("SharedHosts() = %q", got)
	}
	if len((&Access{Rules: []Rule{{Conn: &Connection{Name: "Notion", AllowedHosts: []string{"api.notion.com"}}}}}).SharedHosts()) != 0 {
		t.Error("a host with one connection should not be called shared")
	}
}
