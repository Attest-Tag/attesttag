package app

import (
	"context"
	"strings"
	"testing"
)

// `!connect` and `!personal_instructions` read the channel's connections, and a bang command
// never becomes a turn — so nothing had resolved them, and both answered every person with "I
// can't see this channel's connections right now" whatever the channel could reach. The Call a
// command gets is built the way incoming builds it: no Access.
func TestConnectCommandsSeeTheChannelsConnections(t *testing.T) {
	st, p, conn := userConnFixture(t)
	ctx := context.Background()
	sc, err := st.UpsertScope(ctx, orgID, "channel", "T1", "C1", "#general")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachBundle(ctx, orgID, sc.ID, conn.BundleID); err != nil {
		t.Fatal(err)
	}
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, NewResolver(st), p, newSettingsCache(st, Config{}))
	newCall := func() *Call {
		return &Call{OrgID: orgID, TeamID: "T1", SL: &Chat{TeamID: "T1"}, Channel: "C1", ThreadTS: "1",
			UserID: "UPRIYA", Kind: "channel", Session: &Session{}, HumanTurn: true}
	}

	for _, cmd := range []string{"!connect", "!personal_instructions"} {
		handled, reply := a.command(ctx, newCall(), cmd)
		if !handled {
			t.Fatalf("%s was not handled", cmd)
		}
		if strings.Contains(reply, "can't see this channel's connections") || !strings.Contains(reply, "Google") {
			t.Errorf("%s did not see the Google connection this channel has:\n%s", cmd, reply)
		}
	}

	// And setting instructions reaches the same connection, so the command does what it says.
	if _, reply := a.command(ctx, newCall(), "!personal_instructions only ever look at my inbox"); !strings.Contains(reply, "Saved for *Google*") {
		t.Errorf("setting instructions answered: %s", reply)
	}
}
