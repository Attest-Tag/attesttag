package app

import (
	"slices"
	"testing"
)

// Teams is optional, and all of it absent is fine. Half of it is refused at startup: a deployment
// with an app id and no secret would come up healthy and fail on the first message a tenant sent.
func TestAHalfConfiguredTeamsIsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"absent", Config{}, true},
		{"whole, single tenant", Config{MSTeamsAppID: "a", MSTeamsAppPassword: "p", MSTeamsTenantID: "t", MSTeamsAppType: msteamsSingleTenant}, true},
		{"whole, multi tenant", Config{MSTeamsAppID: "a", MSTeamsAppPassword: "p", MSTeamsAppType: msteamsMultiTenant}, true},
		{"no secret", Config{MSTeamsAppID: "a", MSTeamsTenantID: "t", MSTeamsAppType: msteamsSingleTenant}, false},
		{"single tenant with no tenant", Config{MSTeamsAppID: "a", MSTeamsAppPassword: "p", MSTeamsAppType: msteamsSingleTenant}, false},
		{"a secret and no app", Config{MSTeamsAppPassword: "p"}, false},
		{"an app type Azure does not have", Config{MSTeamsAppID: "a", MSTeamsAppPassword: "p", MSTeamsTenantID: "t", MSTeamsAppType: "Tenantless"}, false},
	} {
		if got := c.cfg.msteamsMisconfigured() == ""; got != c.ok {
			t.Errorf("%s: accepted = %v, want %v (%s)", c.name, got, c.ok, c.cfg.msteamsMisconfigured())
		}
	}
	if (Config{}).msteamsConfigured() {
		t.Error("a deployment with no Teams settings reads as having Teams")
	}
}

// An approver list takes a Teams member by their Entra object id, however it was pasted, and keeps
// it lower case — upper-cased, it would never match the person Teams says it is.
func TestAnApproverListTakesATeamsMember(t *testing.T) {
	ids, emails, bad := parseApprovers("<@8F3B1C2D-0000-4000-8000-00000000000A|Ana>, U0123ABCD; ana@contoso.com")
	if !slices.Contains(ids, "8f3b1c2d-0000-4000-8000-00000000000a") || !slices.Contains(ids, "U0123ABCD") {
		t.Errorf("ids = %v", ids)
	}
	if len(emails) != 1 || len(bad) != 0 {
		t.Errorf("emails %v, bad %v", emails, bad)
	}
	if got := mentionedUsers("ask <@8f3b1c2d-0000-4000-8000-00000000000a> and <@U0123ABCD>"); len(got) != 2 {
		t.Errorf("mentions read = %v, want both people", got)
	}
}
