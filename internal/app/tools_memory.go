package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Memory sharing rule (same as Claude Tag): public-channel memories are visible workspace-wide,
// private-channel and DM memories stay in that conversation.
// Scope strings carry the workspace: "team:T…" is everything public in one Slack workspace,
// "channel:T…/C…" is one conversation. Without the team a channel id from one workspace could
// recall another's memories, since Slack only guarantees channel ids unique within a team.
func (a *Agent) memoryScope(ctx context.Context, c *Call, wantTeam bool) string {
	if wantTeam && a.isPublic(ctx, c) {
		return teamMemoryScope(c.TeamID)
	}
	return channelMemoryScope(c.TeamID, c.Channel)
}

// The two scope strings, in one place: every read has to spell them exactly as the write did.
func teamMemoryScope(teamID string) string { return "team:" + teamID }

func channelMemoryScope(teamID, channel string) string {
	return "channel:" + teamID + "/" + channel
}

// isPublic asks Slack rather than reading the channel id: a public channel converted to
// private keeps its "C" prefix, and guessing from the prefix used to promote a private
// channel's memories to workspace scope, where every public channel could recall them.
func (a *Agent) isPublic(ctx context.Context, c *Call) bool {
	if c.SL == nil {
		return false
	}
	return !c.SL.IsPrivateConversation(ctx, c.Channel)
}

func (a *Agent) registerMemoryTools() {
	a.register(Tool{
		Name: "remember",
		Desc: "Save a short fact for later turns. Use when someone says 'remember that…'. scope 'channel' (default) keeps it to this channel; 'workspace' shares it everywhere (public channels only).",
		Params: schema(map[string]any{
			"text":  str("The fact to remember, one sentence"),
			"scope": map[string]any{"type": "string", "enum": []string{"channel", "workspace"}},
		}, "text"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Text, Scope string }
			json.Unmarshal(args, &p)
			if strings.TrimSpace(p.Text) == "" {
				return "", fmt.Errorf("text is required")
			}
			scope := a.memoryScope(ctx, c, p.Scope == "workspace")
			if err := a.store.AddMemory(ctx, c.OrgID, c.TeamID, scope, strings.TrimSpace(p.Text), c.UserID); err != nil {
				return "", err
			}
			// The link that edits it follows the answer, out of band; see postMemoryLink.
			c.memoryTouched = true
			return "saved to " + scope, nil
		},
	})
	a.register(Tool{
		Name:   "recall",
		Desc:   "List everything remembered for this channel and the workspace.",
		Params: schema(map[string]any{}),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			mems, err := a.store.Memories(ctx, c.OrgID, channelMemoryScope(c.TeamID, c.Channel), teamMemoryScope(c.TeamID))
			if err != nil {
				return "", err
			}
			if len(mems) == 0 {
				return "nothing remembered yet", nil
			}
			var b strings.Builder
			for _, m := range mems {
				fmt.Fprintf(&b, "- (%s, by %s, %s) %s\n", m.Scope, c.SL.UserName(ctx, m.CreatedBy), m.At[:10], m.Text)
			}
			return b.String(), nil
		},
	})
	a.register(Tool{
		Name:   "forget",
		Desc:   "Delete remembered facts containing the given words (channel scope, or workspace scope if asked).",
		Params: schema(map[string]any{"contains": str("Words the memory contains"), "scope": map[string]any{"type": "string", "enum": []string{"channel", "workspace"}}}, "contains"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Contains, Scope string }
			json.Unmarshal(args, &p)
			if strings.TrimSpace(p.Contains) == "" {
				// remember checks its text the same way. The store refuses this too, but the
				// model is told here, in words it can act on, rather than through an error.
				return "", fmt.Errorf("say which words the memory contains")
			}
			n, err := a.store.ForgetMemory(ctx, c.OrgID, a.memoryScope(ctx, c, p.Scope == "workspace"), p.Contains)
			if err != nil {
				return "", err
			}
			c.memoryTouched = c.memoryTouched || n > 0
			return fmt.Sprintf("forgot %d memor(y/ies)", n), nil
		},
	})
}

// teamOfMemoryScope reads the workspace back out of a scope string, for the console's "add a
// memory" form, which posts the scope it was shown rather than a separate team field.
func teamOfMemoryScope(scope string) string {
	if rest, ok := strings.CutPrefix(scope, "team:"); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(scope, "channel:"); ok {
		if team, _, found := strings.Cut(rest, "/"); found {
			return team
		}
	}
	return ""
}
