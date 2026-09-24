package app

import (
	"context"
	"testing"
)

// `!stop` and a bare "stop" are one request. `!stop` had its own shorter version that stopped the
// turns in this process and any fix job, and said so — while a turn running on another instance
// carried on, and an investigation queued in the thread started minutes later.
func TestBangStopDoesWhatStopDoes(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	a := NewAgent(Config{Timezone: "UTC"}, nil, nil, st, nil, nil, nil, newSettingsCache(st, Config{}))
	const thread = "1700000000.000100"
	if _, err := st.EnsureSession(ctx, "T1", "C1", thread, "channel", ""); err != nil {
		t.Fatal(err)
	}
	id, err := st.EnqueueInvestigation(ctx, &Investigation{OrgID: orgID, TeamID: "T1", Channel: "C1",
		ThreadTS: thread, Requester: "U1", Question: "why is tier-2 failing?", Rounds: 40, Minutes: 12})
	if err != nil {
		t.Fatal(err)
	}

	c := &Call{OrgID: orgID, TeamID: "T1", Channel: "C1", ThreadTS: thread, UserID: "U9", Kind: "channel",
		Session: &Session{}, HumanTurn: true}
	if _, reply := a.command(ctx, c, "!stop"); reply == "Nothing of mine is running in this thread." {
		t.Error("!stop said nothing was running while an investigation was queued in the thread")
	}
	var status string
	st.db.QueryRowContext(ctx, `select status from investigations where id=?`, id).Scan(&status)
	if status != "stopped" {
		t.Errorf("the queued investigation is %q after !stop, and would start after the person said stop", status)
	}
	if st.StopRequestedAt(ctx, "T1", "C1", thread) == "" {
		t.Error("!stop wrote no stop request, so a turn running on another instance carries on")
	}
}
