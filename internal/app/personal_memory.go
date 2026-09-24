package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Personal memory: notes that belong to one person rather than to a channel or a workspace.
//
// The organisation's memory answers "what does this room know". This answers "what did I ask you
// to hold for me", and the difference is that the second one has an owner who is the only reader.
// Everything here is arranged around that single sentence: the notes live in their own table so
// no organisation-wide query can reach them, the tools that touch them exist only on a turn with
// an owner, and the prompt carries them only where the answer cannot be read by anybody else.

// personalKey is whose notes this turn may read and write, and false when there is nobody.
//
// The obvious implementations are both wrong, which is why this is a function and not a field.
// c.UserID is a real Slack id on turns nobody is present for: routines.go runs a routine as
// r.CreatedBy and playground.go falls back to the bot itself. c.Kind is inherited -- an
// investigation takes sess.Kind, so a background run started from a channel question arrives
// looking exactly like a person asking in that channel. Both would pass a check written against
// them, so the Call says so instead, and says nothing by default.
func (c *Call) personalKey() (personalKey, bool) {
	switch {
	case !c.HumanTurn:
		// Every other lane is opted out by staying quiet rather than by being listed here.
		return personalKey{}, false
	case c.OrgID == 0 || c.TeamID == "" || c.UserID == "":
		// oauth_user.go's rule, for the same reason: no requester means nobody, not everybody.
		return personalKey{}, false
	case c.offline():
		// A preview is of a channel's settings and a quiet run has no asker. Neither is a person,
		// and a preview's answer is returned over HTTP to whoever opened the console.
		return personalKey{}, false
	case c.SL != nil && c.UserID == c.SL.BotUserID:
		// Four different fallbacks land on the bot's own id, including the selftest path. Without
		// this the bot would own one bucket that every unattributed turn in the workspace shares.
		return personalKey{}, false
	}
	return personalKey{orgID: c.OrgID, teamID: c.TeamID, owner: c.UserID}, true
}

// personalMemoryTools are assembled per turn rather than registered once, because who may use
// them is a property of the turn and not of the agent. registerMemoryTools puts remember, recall
// and forget into a.tools for every call there will ever be, and toolsFor then deletes them where
// they must not appear -- a denylist, which works until somebody adds a lane and forgets it.
// These go the other way: they exist only where personalKey found an owner, so a routine, a
// preview, a quiet run and any transport added later get nothing, and there is no list to be
// forgotten from. It is the rule plan-email.md argues for, applied before there is an email lane.
//
// has says whether this person has written anything yet. Somebody who never uses the feature
// carries one definition rather than three, on every round of every turn, forever.
func (a *Agent) personalMemoryTools(k personalKey, has bool) []Tool {
	tools := []Tool{{
		Name: "remember_personal",
		Desc: "Save a note that belongs to the person asking and to nobody else: their own to-do, " +
			"reminder, or a detail about them. Only they ever see it -- not this channel, not their " +
			"teammates, not a scheduled routine. Use it for \"remind me\", \"note for myself\", " +
			"\"my to-do\", \"remember for me\", \"I need to\". For something the whole channel should " +
			"know, use remember instead.",
		Params: schema(map[string]any{
			"note": str("The note, one sentence, in their own words"),
		}, "note"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			// Re-checked here and not only in toolsFor: that deletion shapes what the model is
			// offered, and runTool resolves a name the model returns, which is not the same thing.
			k, ok := c.personalKey()
			if !ok {
				return "", errNoOwner
			}
			var p struct {
				Note string `json:"note"`
			}
			json.Unmarshal(args, &p)
			if strings.TrimSpace(p.Note) == "" {
				return "", fmt.Errorf("note is required")
			}
			if _, err := a.store.AddPersonalMemory(ctx, k, p.Note); err != nil {
				return "", err
			}
			// The link to edit it is DMed after the answer; see postMemoryLink.
			c.personalMemoryTouched = true
			return "saved privately for them; only they can recall it, in any channel", nil
		},
	}}
	if !has {
		return tools
	}
	return append(tools,
		Tool{
			Name: "recall_personal",
			Desc: "List the asking person's own private notes, with the id of each. You can never " +
				"read anyone else's. Call it when they ask what they have to do, what they noted, or " +
				"when answering them depends on something only they told you.",
			Params: schema(map[string]any{}),
			Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
				k, ok := c.personalKey()
				if !ok {
					return "", errNoOwner
				}
				notes, err := a.store.PersonalMemories(ctx, k)
				if err != nil {
					return "", err
				}
				if len(notes) == 0 {
					return "they have no private notes yet", nil
				}
				var b strings.Builder
				for _, n := range notes {
					fmt.Fprintf(&b, "- [%d] (%s) %s\n", n.ID, n.CreatedAt[:10], n.Text)
				}
				if c.Kind != "dm" {
					// Said on the turns where it matters rather than in the shared prompt, which
					// everyone in the channel pays for on every round whether or not it applies.
					b.WriteString("\nThese are that person's own private notes and this is not a " +
						"direct message. Use them to answer what they asked; do not list them or " +
						"bring up one they did not ask about, because everyone here reads the reply.")
				}
				return b.String(), nil
			},
		},
		Tool{
			Name: "forget_personal",
			Desc: "Delete one of the asking person's own notes, by the id recall_personal gave it. " +
				"Call recall_personal first if you do not have the id.",
			Params: schema(map[string]any{
				"id": num("The id of the note to delete, from recall_personal"),
			}, "id"),
			Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
				k, ok := c.personalKey()
				if !ok {
					return "", errNoOwner
				}
				var p struct {
					ID int64 `json:"id"`
				}
				json.Unmarshal(args, &p)
				if p.ID <= 0 {
					return "", fmt.Errorf("id is required; call recall_personal to get it")
				}
				found, err := a.store.DeletePersonalMemory(ctx, k, p.ID)
				if err != nil {
					return "", err
				}
				if !found {
					return "they have no note with that id", nil
				}
				c.personalMemoryTouched = true
				return "deleted", nil
			},
		})
}

// personalMemoryTool reports whether a tool name is one of the three. It decides two things: the
// activity card's wording, and whether the arguments and result are written to tool_calls at all.
func personalMemoryTool(name string) bool {
	switch name {
	case "remember_personal", "recall_personal", "forget_personal":
		return true
	}
	return false
}

// notesCommand is the fast path to a person's own notes, the way !personal_instructions is the
// fast path to the Personal tab. `!note <text>` adds one; `!notes` sends the list.
//
// The list goes by DM even when it is asked for in a channel, and the reply in the channel says
// only that it was sent. command()'s return value is posted where it was typed, and a command
// whose output must never reach the channel does not belong in a mechanism whose only output is
// the channel. Four characters is too few to stand between somebody and reading their private
// list out loud to the room.
func (a *Agent) notesCommand(ctx context.Context, c *Call, arg string) string {
	k, ok := c.personalKey()
	if !ok {
		return "I can't tell who is asking here, so I won't open anybody's notes."
	}
	if arg = strings.TrimSpace(arg); arg != "" {
		if _, err := a.store.AddPersonalMemory(ctx, k, arg); err != nil {
			return "I couldn't save that: " + err.Error()
		}
		c.personalMemoryTouched = true
		return "Saved, privately to you."
	}
	notes, err := a.store.PersonalMemories(ctx, k)
	if err != nil {
		return "I couldn't read your notes: " + err.Error()
	}
	if len(notes) == 0 {
		return "You have no private notes yet. Say *remember for me: …*, or `!note <text>`."
	}
	var b strings.Builder
	b.WriteString("*Your notes* — private to you\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "• %s\n", n.Text)
	}
	link := a.personalMemoryURL(ctx, c.OrgID, k.teamID, c.Channel, k.owner)
	if link != "" {
		b.WriteString("\n<" + link + "|Edit or delete them> — no sign-in needed. The link is yours alone.")
	}
	if c.Kind == "dm" {
		return b.String()
	}
	if c.SL == nil {
		return "I can't send you a DM from here."
	}
	if _, err := c.SL.PostMarkdown(ctx, k.owner, "", b.String(), ""); err != nil {
		return "I couldn't DM you your notes: " + err.Error()
	}
	return "Sent them to you in a DM — they're private, so not here."
}
