package app

import (
	"context"
	"testing"
)

// A workspace's platform decides which client it gets, and a platform this build does not know
// gets none. Guessing — building a Slack client for a row that is not a Slack workspace — would
// point that workspace's messages, and whatever token was at hand, somewhere they do not belong.
func TestTheRegistryBuildsTheTransportAWorkspacesPlatformNeeds(t *testing.T) {
	fixedMasterKey(t)
	st := testStore(t)
	sealer, err := NewSealer()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	enc, err := sealer.Seal([]byte("xoxb-test"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTeam(ctx, &Team{TeamID: "T1", OrgID: 1, Name: "Acme", BotUserID: "UBOT"}, enc); err != nil {
		t.Fatal(err)
	}
	reg := NewChatRegistry(st, sealer)

	sl, err := reg.For(ctx, "T1")
	if err != nil {
		t.Fatalf("a Slack workspace: %v", err)
	}
	if sl.Platform != platformSlack {
		t.Errorf("platform = %q, want %q: a Slack install saved without saying so is still Slack", sl.Platform, platformSlack)
	}
	if _, err := sl.slackAPI(); err != nil {
		t.Errorf("a Slack workspace came back without a Slack client: %v", err)
	}

	if _, err := st.db.ExecContext(ctx, `insert into teams (team_id, org_id, name, status, platform)
		values ('msteams:tenant-1', 1, 'Contoso', 'active', 'msteams')`); err != nil {
		t.Fatal(err)
	}
	if sl, err := reg.For(ctx, "msteams:tenant-1"); err == nil {
		t.Fatalf("built a client (platform %q) for a platform this build cannot talk to", sl.Platform)
	}
	if _, err := reg.Token(ctx, "msteams:tenant-1"); err == nil {
		t.Fatal("handed out a bot token for a workspace that is not on Slack")
	}
}
