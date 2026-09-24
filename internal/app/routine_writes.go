package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Writing a routine, for both doors that do it: the console's editor and the developer API (and
// through it, the MCP server). One definition of what a routine may be — a channel the bot is in,
// a schedule no tighter than the floor, a model somebody chose to pay for — so the two cannot
// drift apart on what they accept.

// routineDraft is a new routine as either door describes it.
type routineDraft struct {
	Channel, TeamID, Cron, TZ, Prompt string
	Notify, NotifyWhen, Model, Finish string
	Steps                             json.RawMessage
	AutoConfirm                       bool
	// CreatedBy is whose turn each run is: the Slack id of a console admin who signed in through
	// Slack, and empty — the bot itself — for anyone else, which includes every API caller.
	// Nothing an API request says can change that: no key and no MCP token is anybody's Slack
	// account, so neither can hand a routine somebody's personal connections.
	CreatedBy string
}

// addRoutine checks a new routine and stores it. An error is the caller's to fix, in words they
// can act on.
func (b *Bot) addRoutine(ctx context.Context, org int64, d routineDraft) (int64, error) {
	set := b.settings.Get(ctx, org)
	rt := Routine{
		OrgID: org, Channel: strings.TrimSpace(d.Channel),
		Cron: strings.TrimSpace(d.Cron), TZ: cmp.Or(strings.TrimSpace(d.TZ), set.Timezone),
		Prompt: strings.TrimSpace(d.Prompt), Notify: notifyMode(d.Notify),
		NotifyWhen: strings.TrimSpace(d.NotifyWhen), Model: strings.TrimSpace(d.Model),
		Finish: d.Finish, AutoConfirm: d.AutoConfirm, CreatedBy: d.CreatedBy,
	}
	if rt.Prompt == "" {
		return 0, errors.New("a routine needs a prompt: what it should do each time it runs")
	}
	if rt.Channel == "" {
		return 0, errors.New("a routine needs a channel to post in")
	}
	team, err := routineChannelTeam(ctx, b.store, org, rt.Channel, strings.TrimSpace(d.TeamID))
	if err != nil {
		return 0, err
	}
	rt.TeamID = team
	if !routineModelAllowed(set, rt.Model) {
		return 0, errRoutineModel(rt.Model)
	}
	next, err := nextRun(rt.Cron, rt.TZ, time.Now())
	if err != nil {
		return 0, err
	}
	if routineTooFrequent(rt.Cron, rt.TZ) {
		return 0, fmt.Errorf("routines run at most every %s; pick a wider schedule", minRoutineInterval)
	}
	rt.NextRun = next.UTC().Format(time.DateTime)
	steps, ok, err := routineStepsField(d.Steps)
	if err != nil {
		return 0, err
	}
	if ok {
		rt.Steps = steps
	}
	return b.store.AddRoutine(ctx, rt)
}

// routineEdit is a change to a routine as either door describes it. A nil field is left alone.
type routineEdit struct {
	Enabled                                             *bool
	Cron, TZ, Prompt, Notify, NotifyWhen, Finish, Model *string
	// Channel moves it; TeamID only when that channel id is shared into more than one workspace.
	Channel, TeamID *string
	Steps           json.RawMessage
	AutoConfirm     *bool
	// EditedBy is the editor's Slack id, empty for a password session or any API caller. It
	// decides whether the routine goes on running as the person who wrote it (changesWhatItDoes).
	EditedBy string
}

// editRoutine checks a change and applies it. wasRunningAs is the person the routine had been
// running as when this edit took it off them — they are told, since from here on it no longer
// reaches their accounts — and empty otherwise.
func (b *Bot) editRoutine(ctx context.Context, org, id int64, e routineEdit) (wasRunningAs string, err error) {
	patch := RoutinePatch{Cron: e.Cron, TZ: e.TZ, Prompt: e.Prompt, NotifyWhen: e.NotifyWhen, Finish: e.Finish,
		AutoConfirm: e.AutoConfirm, EditedBy: e.EditedBy}
	if e.Notify != nil {
		// Narrowed here as well as in the store: an unknown mode must not read as "quiet" and
		// take a routine out of its channel by accident.
		mode := notifyMode(*e.Notify)
		patch.Notify = &mode
	}
	if e.Model != nil {
		m := strings.TrimSpace(*e.Model)
		if !routineModelAllowed(b.settings.Get(ctx, org), m) {
			return "", errRoutineModel(m)
		}
		patch.Model = &m
	}
	if e.Channel != nil {
		ch, teamID := strings.TrimSpace(*e.Channel), ""
		if e.TeamID != nil {
			teamID = strings.TrimSpace(*e.TeamID)
		}
		team, err := routineChannelTeam(ctx, b.store, org, ch, teamID)
		if err != nil {
			return "", err
		}
		patch.Channel, patch.TeamID = &ch, &team
	}
	steps, ok, err := routineStepsField(e.Steps)
	if err != nil {
		return "", err
	}
	if ok {
		patch.Steps = &steps
	}
	// Everything is checked before anything is written, so a refused edit changes nothing —
	// pausing included.
	if e.Enabled != nil {
		b.store.SetRoutineEnabled(ctx, org, id, *e.Enabled)
	}
	if patch == (RoutinePatch{EditedBy: e.EditedBy}) {
		return "", nil
	}
	before := b.routineByID(ctx, org, id)
	if err := b.store.UpdateRoutine(ctx, org, id, patch); err != nil {
		return "", err
	}
	if before != nil && before.CreatedBy != "" && before.CreatedBy != patch.EditedBy && patch.changesWhatItDoes() {
		// Taking a routine over is not something to do quietly to somebody: from this moment
		// it stops reaching their accounts, and they are the only person who can put it back.
		go b.tellRoutineChangedHands(detached(ctx), *before, patch.EditedBy)
		return before.CreatedBy, nil
	}
	return "", nil
}

// findRoutineChannel reads the channel a developer API caller named — its id, or its name with or
// without the # — as one of the channels the bot is in. The console's editor offers a picker; a
// script or an MCP client has only what somebody typed, and "#eng" is what people type.
func (b *Bot) findRoutineChannel(ctx context.Context, org int64, named, teamID string) (channel, team string, err error) {
	named, teamID = strings.TrimSpace(named), strings.TrimSpace(teamID)
	scopes, err := b.store.Scopes(ctx, org)
	if err != nil {
		return "", "", err
	}
	want := strings.ToLower(strings.TrimPrefix(named, "#"))
	type hit struct{ channel, team string }
	var hits []hit
	for _, sc := range scopes {
		if sc.Kind != "channel" || (teamID != "" && sc.TeamID != teamID) {
			continue
		}
		if sc.SlackID == named || (want != "" && strings.ToLower(strings.TrimPrefix(sc.Name, "#")) == want) {
			if h := (hit{sc.SlackID, sc.TeamID}); !slices.Contains(hits, h) {
				hits = append(hits, h)
			}
		}
	}
	switch len(hits) {
	case 0:
		return "", "", fmt.Errorf("%q is not a channel the bot is in: GET /v1/scopes (list_scopes over MCP) lists the ones it is", named)
	case 1:
		return hits[0].channel, hits[0].team, nil
	}
	var teams []string
	for _, h := range hits {
		teams = append(teams, h.team)
	}
	return "", "", fmt.Errorf("%q names a channel in more than one workspace (%s); send team_id to say which", named, strings.Join(teams, ", "))
}
