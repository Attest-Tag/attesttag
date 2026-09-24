package app

import (
	"context"
	"strings"
	"testing"
)

// A routine set to a model answers on it, and one left on the default answers on what any turn
// in its channel would. The pick rides the run's own session, so nothing else in the channel
// changes model because a routine did.
func TestRoutineAnswersOnTheModelItWasSet(t *testing.T) {
	cases := []struct{ name, model, heavy, want string }{
		{"default follows settings", "", "", "test"},
		{"an offered model", "m-cheap", "", "m-cheap"},
		{"advanced with one configured", "heavy", "m-big", "m-big"},
		{"advanced with none configured reads as default", "heavy", "", "test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _, llm, st, r := quietFixture(t, "all fine", Routine{Prompt: "check things", Model: tc.model})
			ctx := context.Background()
			if err := st.PutSettings(ctx, 1, map[string]string{"channel_models": "m-cheap", "heavy_model": tc.heavy}); err != nil {
				t.Fatal(err)
			}
			a.settings.Invalidate(1)
			if r.Model != tc.model {
				t.Fatalf("the routine was stored with model %q, want %q", r.Model, tc.model)
			}

			a.runRoutineNow(ctx, r, r.NextRun)

			if got := llm.model(); got != tc.want {
				t.Errorf("the run asked for model %q, want %q", got, tc.want)
			}
		})
	}
}

// The list a routine may pick from is the one the channel page uses: the default, Advanced
// where there is one, and the models Settings offers to channels. Anything else — including
// a model the console could set for a channel by name — is refused at the API.
func TestRoutineModelAllowed(t *testing.T) {
	st := Settings{HeavyModel: "m-big", ChannelModels: []string{"m-cheap"}}
	for m, want := range map[string]bool{"": true, "heavy": true, "m-cheap": true, "m-other": false, "M-CHEAP": false} {
		if got := routineModelAllowed(st, m); got != want {
			t.Errorf("routineModelAllowed(%q) = %v, want %v", m, got, want)
		}
	}
	if routineModelAllowed(Settings{}, "heavy") {
		t.Error("Advanced was allowed with no advanced model configured")
	}
}

// Editing a routine's model goes through the same list, and the stored value is what the
// editor sent, trimmed.
func TestRoutineModelIsStoredByTheEditor(t *testing.T) {
	_, _, _, st, r := quietFixture(t, "all fine", Routine{Prompt: "check things"})
	ctx := context.Background()
	m := " m-cheap "
	if err := st.UpdateRoutine(ctx, 1, r.ID, RoutinePatch{Model: &m}); err != nil {
		t.Fatal(err)
	}
	rs, err := st.Routines(ctx, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].Model != "m-cheap" {
		t.Errorf("stored model = %+v, want m-cheap", rs)
	}
}

// A routine made from Slack runs its writes without asking unless the person asking said
// otherwise: nobody is there to press Confirm when a schedule fires, so a held write would
// only expire. An explicit false is still honoured.
// TestCreateRoutineHoldsWritesForConfirm locks the rule that a routine made through the model never
// auto-confirms its writes — not even when the tool call asks it to. The instruction can come from
// content the model read, so whether a routine may write unattended is a console decision, out of
// reach of anything the model was told. auto_confirm in the args is ignored.
func TestCreateRoutineHoldsWritesForConfirm(t *testing.T) {
	a, _, _, st, _ := quietFixture(t, "ok", Routine{Prompt: "seed"})
	a.registerRoutineTools()
	ctx := context.Background()
	c := &Call{OrgID: 1, TeamID: "T1", Channel: "C1", UserID: "U1"}
	for _, args := range []string{
		`{"cron":"0 9 * * 1-5","prompt":"brief me","tz":"UTC"}`,
		`{"cron":"0 9 * * 1-5","prompt":"brief me","tz":"UTC","auto_confirm":true}`,
		`{"cron":"0 9 * * 1-5","prompt":"brief me","tz":"UTC","auto_confirm":false}`,
	} {
		out, err := a.tools["create_routine"].Run(ctx, c, []byte(args))
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		rs, err := st.Routines(ctx, 1, "C1")
		if err != nil {
			t.Fatal(err)
		}
		if got := rs[len(rs)-1]; got.AutoConfirm {
			t.Errorf("%s: stored AutoConfirm = true, want false — a model-made routine must hold its writes", args)
		}
		if !strings.Contains(out, "Confirm press") {
			t.Errorf("%s: the reply does not say writes are held: %q", args, out)
		}
	}
}

// A routine moved from the console lands in a channel the bot is in, and its workspace moves
// with it: a channel id means nothing outside one. A Slack Connect channel shared into two
// workspaces has to be named with its workspace.
func TestRoutineMovesToAChannelTheBotIsIn(t *testing.T) {
	_, _, _, st, r := quietFixture(t, "ok", Routine{Prompt: "brief me"})
	ctx := context.Background()
	if _, err := st.UpsertChannelScope(ctx, 1, "T1", "C2", "ops", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertChannelScope(ctx, 1, "T2", "C3", "shared", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertChannelScope(ctx, 1, "T1", "C3", "shared", false); err != nil {
		t.Fatal(err)
	}

	if team, err := routineChannelTeam(ctx, st, 1, "C2", ""); err != nil || team != "T1" {
		t.Errorf("C2 resolved to (%q, %v), want T1", team, err)
	}
	if _, err := routineChannelTeam(ctx, st, 1, "C9", ""); err == nil {
		t.Error("a channel the bot is not in was accepted")
	}
	if _, err := routineChannelTeam(ctx, st, 1, "C3", ""); err == nil || !strings.Contains(err.Error(), "teamId") {
		t.Errorf("a channel shared into two workspaces resolved without being told which: %v", err)
	}
	if team, err := routineChannelTeam(ctx, st, 1, "C3", "T2"); err != nil || team != "T2" {
		t.Errorf("C3 in T2 resolved to (%q, %v), want T2", team, err)
	}
	if _, err := routineChannelTeam(ctx, st, 2, "C2", ""); err == nil {
		t.Error("another organisation's channel was accepted")
	}

	ch, team := "C3", "T2"
	if err := st.UpdateRoutine(ctx, 1, r.ID, RoutinePatch{Channel: &ch, TeamID: &team}); err != nil {
		t.Fatal(err)
	}
	rs, err := st.Routines(ctx, 1, "C3")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].TeamID != "T2" || rs[0].Channel != "C3" {
		t.Errorf("after the move: %+v", rs)
	}
}
