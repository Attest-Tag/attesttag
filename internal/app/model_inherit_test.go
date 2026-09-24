package app

import (
	"context"
	"testing"
)

// The console offers a default model on the organisation's row, on each workspace and on each
// channel, and saves all three — but a turn read only the channel's, so a default chosen for a
// whole workspace was shown back to the admin who chose it and never answered on.
func TestADefaultModelIsInheritedFromTheWorkspaceAndTheOrganisation(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, nil,
		newSettingsCache(st, Config{Model: "base-model", HeavyModel: "strong-model"}))
	scope := func(kind, team, slackID string, model string) {
		t.Helper()
		sc, err := st.UpsertScope(ctx, orgID, kind, team, slackID, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateScope(ctx, orgID, sc.ID, "", model, sc.MemberEdits); err != nil {
			t.Fatal(err)
		}
	}
	choose := func(team, channel string) (string, string) {
		return a.chooseModel(ctx, &Call{OrgID: orgID, TeamID: team, Channel: channel})
	}

	scope("workspace", "", "", "org-model")
	if m, why := choose("T1", "C1"); m != "org-model" || why != "organisation default" {
		t.Errorf("with only the organisation's default set, got %q (%s)", m, why)
	}
	scope("team", "T1", "T1", "team-model")
	if m, why := choose("T1", "C1"); m != "team-model" || why != "workspace default" {
		t.Errorf("with the workspace's default set, got %q (%s)", m, why)
	}
	scope("channel", "T1", "C1", "heavy")
	if m, why := choose("T1", "C1"); m != "strong-model" || why != "channel default" {
		t.Errorf("with the channel on Advanced, got %q (%s)", m, why)
	}
	if m, _ := choose("T1", "C2"); m != "team-model" {
		t.Errorf("another channel in the workspace answers on %q, want the workspace's default", m)
	}
	if m, _ := choose("T2", "C9"); m != "org-model" {
		t.Errorf("a channel in another workspace answers on %q, want the organisation's default", m)
	}
	// The Configure page's Default choice is labelled with what choosing it gives.
	if got := inheritedDefault(ctx, st, a.settings.Get(ctx, orgID), orgID, "T1"); got != "team-model" {
		t.Errorf("a channel's Default in T1 is labelled %q, want the workspace's default", got)
	}
}
