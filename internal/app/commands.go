package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// afterRestart is the part of a thread a turn reads after `!restart`: everything after the
// message that asked, which is dropped too. Found by position rather than by comparing
// timestamps, because a Slack ts and a Teams message id are not the same shape and the message is
// in the thread either way. If it has since been deleted, what came later still sorts after it
// within one platform, so the comparison is the fallback.
func afterRestart(thread []ThreadMsg, sess *Session) []ThreadMsg {
	if sess == nil || sess.RestartTS == "" {
		return thread
	}
	for i, m := range thread {
		if m.TS == sess.RestartTS {
			return thread[i+1:]
		}
	}
	var after []ThreadMsg
	for _, m := range thread {
		if m.TS > sess.RestartTS {
			after = append(after, m)
		}
	}
	return after
}

// Bang commands are handled before anything reaches the model.
// Returns (handled, reply).
func (a *Agent) command(ctx context.Context, c *Call, text string) (bool, string) {
	text = unwrapCode(strings.TrimSpace(text))
	if !strings.HasPrefix(text, "!") {
		return false, ""
	}
	fields := strings.Fields(text)
	cmd := strings.ToLower(fields[0])
	arg := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	switch cmd {
	case "!help":
		return true, helpText()
	case "!notes", "!note":
		return true, a.notesCommand(ctx, c, arg)
	case "!connect":
		return true, a.connectCommand(ctx, c)
	case "!personal_instructions", "!personal":
		return true, a.personalInstructionsCommand(ctx, c, arg)
	case "!access":
		return true, a.accessCommand(ctx, c, arg)
	case "!whoami":
		return true, a.whoami(ctx, c)
	case "!restart":
		// The cut is this very message. Without one there is nothing to cut at — the reply would
		// promise a fresh start and the next turn would read the whole thread again — so say so.
		if c.MessageTS == "" {
			return true, "I couldn't tell where in the thread you said that, so nothing was reset."
		}
		if err := a.store.RestartSession(ctx, c.TeamID, c.Channel, c.ThreadTS, c.Kind, c.MessageTS); err != nil {
			return true, "couldn't restart: " + err.Error()
		}
		return true, "Fresh start. I'll only look at messages from here on."
	case "!stop":
		n := a.stopThread(c.OrgID, c.TeamID, c.Channel, c.ThreadTS, c.UserID)
		if n == 0 {
			return true, "Nothing of mine is running in this thread."
		}
		return true, "" // the run itself says it was stopped, in the reply it was writing
	case "!jobs":
		return true, a.jobsText(ctx, c.OrgID, c.Channel)
	case "!job":
		f := strings.Fields(arg)
		if len(f) == 2 && f[0] == "cancel" {
			id, _ := strconv.ParseInt(f[1], 10, 64)
			j, _ := a.store.Job(ctx, c.OrgID, id)
			if j == nil || a.jobs == nil {
				return true, "No such job."
			}
			if j.Channel != c.Channel {
				return true, "That job belongs to another channel."
			}
			if !a.jobs.cancel(ctx, j, c.UserID, "user") {
				return true, fmt.Sprintf("Fix job #%d is not running.", id)
			}
			return true, fmt.Sprintf("Cancelling fix job #%d.", id)
		}
		return true, "Usage: `!job cancel <id>`"
	case "!mute":
		// A mute reaches exactly one thread — the session is keyed by (team, channel, thread) —
		// and the reply says so, because the first thing anyone assumes about a bot going quiet
		// is that it has gone quiet everywhere. Somebody who wants the whole channel silent
		// should hear that this is not it, here, rather than discover it at the next mention:
		// there is deliberately no channel-wide mute, and removing the bot from the channel is
		// the answer at that scope.
		if err := a.store.SetSessionField(ctx, c.TeamID, c.Channel, c.ThreadTS, "muted", 1); err != nil {
			return true, "Couldn't mute: " + err.Error()
		}
		return true, "Muted — in *this thread only*. I won't reply here until someone says `!unmute`; " +
			"mention me anywhere else and I'll answer as usual. To quiet a whole channel, remove me from it in Slack."
	case "!unmute":
		if err := a.store.SetSessionField(ctx, c.TeamID, c.Channel, c.ThreadTS, "muted", 0); err != nil {
			return true, "Couldn't unmute: " + err.Error()
		}
		return true, "Unmuted — I'm listening in this thread again."
	case "!model":
		// "advanced" is what the console and the Configure page call it; "heavy" is the
		// stored value and the word people already typed, so both reach the same model.
		if strings.EqualFold(arg, "advanced") {
			arg = "heavy"
		}
		// Neither answer names the id behind the tier. The footer stopped saying which model
		// answered, and a confirmation that prints the id would put it back in the channel by
		// another door — for a reader who never asked and cannot act on it. The exception is a
		// model somebody named themselves: echoing it back is quoting their own message.
		if arg == "" || arg == "default" {
			a.store.SetSessionField(ctx, c.TeamID, c.Channel, c.ThreadTS, "model", "")
			return true, "Back to the basic model for this thread."
		}
		st := a.settings.Get(ctx, c.OrgID)
		if arg == "heavy" {
			if st.HeavyModel == "" {
				return true, "No advanced model is configured. Set one in the console under Settings."
			}
		} else if !routineModelAllowed(st, arg) {
			// The same list the Configure page and routines pick from. Without this, `!model
			// <anything>` set the thread to any string a member typed, so a costly model id
			// billed every later turn in the thread until someone reset it.
			return true, "That isn't a model this workspace offers. Use `!model` for the basic model, `!model advanced`, or one of the models an admin has offered to channels under Settings."
		}
		a.store.SetSessionField(ctx, c.TeamID, c.Channel, c.ThreadTS, "model", arg)
		if arg == "heavy" {
			return true, "This thread now uses the advanced model."
		}
		return true, "This thread now uses `" + arg + "`."
	case "!memory", "!memories":
		mems, _ := a.store.Memories(ctx, c.OrgID, channelMemoryScope(c.TeamID, c.Channel), teamMemoryScope(c.TeamID))
		if len(mems) == 0 {
			return true, "Nothing remembered yet. Say *remember for this channel: …*"
		}
		var b strings.Builder
		b.WriteString("*Memories*\n")
		for _, m := range mems {
			scope := "workspace"
			if strings.HasPrefix(m.Scope, "channel:") {
				scope = "channel"
			}
			fmt.Fprintf(&b, "• _%s_ %s\n", scope, m.Text)
		}
		return true, b.String()
	case "!forget":
		if arg == "" {
			return true, "Usage: `!forget <words the memory contains>`"
		}
		n, _ := a.store.ForgetMemory(ctx, c.OrgID, channelMemoryScope(c.TeamID, c.Channel), arg)
		if a.isPublic(ctx, c) {
			k, _ := a.store.ForgetMemory(ctx, c.OrgID, teamMemoryScope(c.TeamID), arg)
			n += k
		}
		return true, fmt.Sprintf("Forgot %d memor(y/ies).", n)
	case "!routines":
		return true, "*Routines*\n" + a.routinesText(ctx, c.OrgID, c.Channel)
	case "!routine":
		f := strings.Fields(arg)
		if len(f) == 2 && (f[0] == "off" || f[0] == "on") {
			id, _ := strconv.ParseInt(f[1], 10, 64)
			n, _ := a.store.SetRoutineEnabledInChannel(ctx, c.OrgID, c.Channel, id, f[0] == "on")
			if n == 0 {
				return true, "No such routine in this channel."
			}
			return true, fmt.Sprintf("Routine #%d %s.", id, map[string]string{"on": "enabled", "off": "disabled"}[f[0]])
		}
		return true, "Usage: `!routine off <id>` or `!routine on <id>`"
	case "!ingest":
		rep, err := a.indexer.IngestOrg(ctx, c.OrgID)
		if err != nil {
			return true, "Ingest failed: " + err.Error()
		}
		msg := fmt.Sprintf("Indexed %d docs / %d chunks in %s (%d embedded, %d unchanged, %d removed).", rep.Docs, rep.Chunks, rep.Took.Round(1e8), rep.Embedded, rep.Unchanged, rep.Deleted)
		if len(rep.Errors) > 0 {
			msg += "\nErrors:\n• " + strings.Join(rep.Errors, "\n• ")
		}
		return true, msg
	case "!docs":
		d, ch, _ := a.store.DocStats(ctx, c.OrgID)
		return true, fmt.Sprintf("%d documents, %d chunks indexed from `%s`.", d, ch, a.cfg.DocsDir)
	case "!usage":
		rows, _ := a.store.UsageByChannel(ctx, c.OrgID)
		total, _ := a.store.MonthSpend(ctx, c.OrgID, "", "")
		var b strings.Builder
		st := a.settings.Get(ctx, c.OrgID)
		fmt.Fprintf(&b, "*This month:* $%.4f of $%.2f budget", total, st.EffectiveBudget())
		if st.Plan == PlanFree {
			if e := a.cfg.SupportEmail; e != "" {
				fmt.Fprintf(&b, " (free plan; to raise it, email %s)", e)
			} else {
				b.WriteString(" (free plan; the budget is fixed here)")
			}
		}
		b.WriteString("\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "• <#%s>: %d calls, %d in / %d out tokens, $%.4f\n", r.Channel, r.Turns, r.In, r.Out, r.Cost)
		}
		return true, b.String()
	}
	return false, ""
}

// jobsText lists a channel's recent fix jobs for !jobs.
func (a *Agent) jobsText(ctx context.Context, orgID int64, channel string) string {
	jobs, _ := a.store.Jobs(ctx, orgID, JobFilter{Channel: channel, Limit: 10})
	if len(jobs) == 0 {
		return "No fix jobs in this channel yet."
	}
	var b strings.Builder
	b.WriteString("*Fix jobs*\n")
	for _, j := range jobs {
		line := fmt.Sprintf("• #%d `%s` — %s · %s", j.ID, j.Repo, j.Status, fmtCost(j.CostUSD))
		if j.PRURL != "" {
			line += " · <" + j.PRURL + "|PR>"
		}
		if j.Error != "" && (j.Status == JobFailed || j.Status == JobTimeout) {
			line += " · " + truncate(oneLine(j.Error), 80)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// unwrapCode takes a command out of the code span Slack wrapped it in.
//
// `!help` lists every command in backticks, and Slack's composer keeps that formatting through a
// copy-paste — so someone who reads the help and pastes what it showed sends `!mute`, backticks
// and all, and the bang is no longer the first character. That message used to fall through to
// the model, which had never been told the command exists and answered by inventing one: it said
// it could not mute itself while the flag it would have set sat one branch away. Only a span that
// opens the message is unwrapped, and only as far as its closing fence, so "`foo` is broken" is
// left for the model to read as the sentence it is.
func unwrapCode(text string) string {
	if !strings.HasPrefix(text, "`") {
		return text
	}
	fence := "`"
	if strings.HasPrefix(text, "```") {
		fence = "```"
	}
	rest := text[len(fence):]
	end := strings.Index(rest, fence)
	if end < 0 {
		return text
	}
	return strings.TrimSpace(rest[:end] + " " + rest[end+len(fence):])
}
