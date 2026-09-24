package app

import (
	"context"
	"testing"
)

// An artifact is a file uploaded into the thread, and Teams takes no file from a bot, so
// create_artifact failed on every call there — after the model had written the whole artifact.
// guide/msteams.md says tools that cannot work on Teams are left out rather than offered to fail.
func TestTeamsIsNotOfferedArtifacts(t *testing.T) {
	st := testStore(t)
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, nil, newSettingsCache(st, Config{}))
	ctx := context.Background()
	offered := func(platform string) bool {
		c := &Call{OrgID: orgID, TeamID: "T1", Channel: "C1", ThreadTS: "1", Kind: "channel", Session: &Session{},
			SL: &Chat{TeamID: "T1", Platform: platform}, Access: &Access{}}
		_, ok := a.ensureTools(ctx, c)["create_artifact"]
		return ok
	}
	if !offered(platformSlack) {
		t.Fatal("Slack is not offered create_artifact either, so this proves nothing")
	}
	if offered(platformMSTeams) {
		t.Error("Teams is offered create_artifact, which cannot upload there")
	}
}
