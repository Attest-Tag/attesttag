package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// A routine either posts every run, or only the runs that clear its own bar. Quiet is not a
// weaker version of always: a run that may end with nothing to say must not announce itself
// before it has looked, so it takes a different path through runRoutine entirely.
const (
	notifyAlways     = "always"
	notifyWhenNeeded = "when_needed"
)

// joinNote puts two notes for the run log in one line, either of which may be empty.
func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " — " + b
}

// routineWhere names where a routine runs, for a sentence somebody reads. A channel is a link;
// a direct message is not one — <#D…> renders as a dead reference — so it is named in words.
func routineWhere(channel string) string {
	if isDirectConversation(channel) {
		return "a direct message"
	}
	return "<#" + channel + ">"
}

// notifyMode narrows anything stored or submitted to the two modes the scheduler understands.
// An unknown value reads as "always": the mode that talks is the safe one to fall back to.
func notifyMode(s string) string {
	if s == notifyWhenNeeded {
		return notifyWhenNeeded
	}
	return notifyAlways
}

// What a run is recorded as. "quiet" is a success — the routine ran, looked, and decided there
// was nothing worth a person's attention. "skipped" is the budget gate, which is not a fault.
const (
	runPosted  = "posted"
	runQuiet   = "quiet"
	runFailed  = "failed"
	runSkipped = "skipped"
)

// What a scheduled run is allowed to be, whatever an organisation sets. The floor is one round
// and two minutes, which is the smallest thing still worth waking the model for; the ceiling on
// rounds is the run ceiling rather than a reply's, and the clock stops at an hour because a
// routine holds a container's attention for as long as it runs and the next one is already due.
const (
	minRoutineRounds, maxRoutineRounds   = 1, maxRunRounds
	minRoutineMinutes, maxRoutineMinutes = 2, 60
	// What a deployment that has never said otherwise gives one run. Sixty rounds was the shape
	// of the work as first written — read a system, cross-check it, write the result somewhere
	// else — but runs kept spending all sixty on the reading and landing on the write-up with
	// the writing-back undone, which is the half that mattered. Two hundred is enough to finish
	// the work rather than only to survey it.
	defaultRoutineRounds = 200
	// The clock has to be able to hold the rounds or the number above is a fiction: a run landed
	// by the wall at round ninety was never given two hundred. Thirty is where two hundred
	// rounds land in practice — the run this was sized from spent six minutes on sixty, and a
	// transcript that keeps growing makes the later calls slower than the early ones — and a
	// deployment that still sees the clock arrive first can raise it to the hour in Settings.
	defaultRoutineMinutes = 30
)

// routineBudget is what one run is given: rounds, and the clock to spend them in. A routine
// carries its own rather than the channel's reply budget for the same reason an investigation
// does — nobody is waiting on it, and the work it was written for is several times a
// conversation's. The channel's "tool rounds per reply" is about replies and is left alone.
// Zero is "never set", not "as little as possible": the defaults are applied here rather than
// only where settings are loaded, so a Settings value assembled anywhere else — a test, a stored
// row written before these existed — still gives a run the budget the product promises.
func routineBudget(st Settings) (rounds int, wall time.Duration) {
	rounds = min(max(cmp.Or(st.RoutineRounds, defaultRoutineRounds), minRoutineRounds), maxRoutineRounds)
	mins := min(max(cmp.Or(st.RoutineMinutes, defaultRoutineMinutes), minRoutineMinutes), maxRoutineMinutes)
	return rounds, time.Duration(mins) * time.Minute
}

// skippedError is the budget gate stopping a run before it starts. It still reads as an error to
// everything that handled one before — the creator is still told — but the log can tell a
// ceiling apart from a fault.
type skippedError struct{ why string }

func (e skippedError) Error() string { return "skipped: " + e.why }

// quietDecisionTools are how a quiet run says what it decided. Asking for the decision in the
// answer text did not work: a model that has decided to stay quiet writes "…10 or fewer, so no
// report posted", and prose explaining that nothing will be posted is exactly the noise the mode
// exists to remove. A tool call is a signal rather than a sentence, so there is nothing to parse
// and nothing to misread.
func (a *Agent) quietDecisionTools() []Tool {
	return []Tool{{
		Name: "report_now",
		Desc: "Post a report to the channel. Use this ONLY when what you found meets the bar you were given. The text you pass is what people read — write it for them, not about your decision.",
		Params: schema(map[string]any{
			"report": str("The report itself, as the channel should read it"),
		}, "report"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Report string }
			json.Unmarshal(args, &p)
			if strings.TrimSpace(p.Report) == "" {
				return "", fmt.Errorf("report is empty; pass the text the channel should read")
			}
			c.quietDecided, c.quietReport = true, p.Report
			return "Recorded — this will be posted. Say nothing further; reply with the single word: done.", nil
		},
	}, {
		Name: "stay_quiet",
		Desc: "Post nothing to the channel. Use this when what you found does not meet the bar. This is the right answer most of the time — a routine that speaks every run is the thing this mode exists to prevent.",
		Params: schema(map[string]any{
			"found": str("One line on what you found, for the run log. Nobody is notified; this is not a message to the channel."),
		}, "found"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Found string }
			json.Unmarshal(args, &p)
			c.quietDecided, c.quietReport = true, ""
			c.quietNote = oneLine(truncate(strings.TrimSpace(p.Found), 300))
			return "Recorded — nothing will be posted. Say nothing further; reply with the single word: done.", nil
		},
	}}
}

// maxRoutineDMs is what one run may send. A routine is written once and runs forever, so the
// mistake to bound is not malice but a loop: a prompt that says "DM the owner of each" against a
// query that quietly starts returning four hundred rows.
const maxRoutineDMs = 20

// routineDMTool lets a scheduled run speak to one person. Without it a routine speaks exactly
// once, by finishing — whatever it found is a single post in a single channel. That is right for
// a digest and wrong for work that is per-item: a run that researches eleven leads has eleven
// things to say, each worth reading on its own, and stapling them into one message is the
// version nobody reads to the end.
//
// Scheduled runs only. Every tool definition is re-sent on every round of every turn that
// carries it, and a person in a thread who wants somebody told can tell them.
func (a *Agent) routineDMTool() Tool {
	return Tool{
		Name: "send_dm",
		Desc: "Send a direct message to one person in Slack, as this run's own message. Use it when what you found belongs to a particular person rather than to the channel — one DM per item, not one DM with everything in it. Slack markdown: *bold*, _italic_, <url|text>. At most " + strconv.Itoa(maxRoutineDMs) + " per run.",
		Params: schema(map[string]any{
			"user": str(`Slack member id like U0123ABC, or "me" for whoever this routine belongs to`),
			"text": str("The message, written for the person who will read it"),
		}, "user", "text"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ User, Text string }
			json.Unmarshal(args, &p)
			p.User, p.Text = strings.TrimSpace(p.User), strings.TrimSpace(p.Text)
			if p.Text == "" {
				return "", fmt.Errorf("text is empty; pass the message the person should read")
			}
			if strings.EqualFold(p.User, "me") || p.User == "" {
				p.User = c.UserID
			}
			// Slack member ids start U (people) or W (Enterprise Grid). A channel id here would
			// post the message somewhere everyone can read it, which is the one mistake this
			// tool must not make quietly.
			if !strings.HasPrefix(p.User, "U") && !strings.HasPrefix(p.User, "W") {
				return "", fmt.Errorf("%q is not a Slack member id; those start with U (or W). A channel id is not a person", p.User)
			}
			if c.dmsSent >= maxRoutineDMs {
				return "", fmt.Errorf("this run has already sent %d direct messages, which is the limit. Say what is left unsent in your report rather than trying again", maxRoutineDMs)
			}
			if c.SL == nil {
				return "", fmt.Errorf("no Slack connection for this run")
			}
			ts, err := c.SL.PostMarkdown(ctx, p.User, "", p.Text, "")
			if err != nil {
				return "", fmt.Errorf("could not DM %s: %w", p.User, err)
			}
			c.dmsSent++
			slog.Info("routine dm", "channel", c.Channel, "to", p.User, "sent", c.dmsSent)
			return fmt.Sprintf("Sent (message %d of at most %d this run, ts %s). Do not send it again.", c.dmsSent, maxRoutineDMs, ts), nil
		},
	}
}

// maxRoutinePosts is what one run may add to its own thread. Higher than the DM cap because the
// cost is a different kind: twenty DMs are twenty interruptions, where twenty replies are one
// thread somebody scrolls past in a second — and a run that researches twenty leads has twenty
// things to say.
const maxRoutinePosts = 40

// routineThreadTool lets a scheduled run say more than one thing where it was announced. A
// routine's answer is posted for it when the run ends, and that one post is all it had: work
// that is per-item came out as everything stapled together, which is the version nobody reads
// to the end. send_dm was the first answer to that and it is the wrong one for anything the
// channel owns — a brief about a lead belongs where the team can find it next week, not in one
// person's DMs — so a prompt that says "one reply per lead, do not DM anyone" had no tool that
// could do as it asked, and the run reached for the only tool that sent anything at all.
//
// Scheduled runs only, and only where there is a thread: a quiet run and a preview never posted
// the header this writes under, and a reply addressed to a message id that was never a message
// goes nowhere. In a conversation, a person who wants something said can say it.
func (a *Agent) routineThreadTool() Tool {
	return Tool{
		Name: "post_to_thread",
		Desc: "Post a message in this run's own thread, under the message that announced the run. Use it when what you found is per-item and each item is worth reading on its own — one post per item, not one post with everything in it. Everyone who can see the channel can read these, so this is where anything the team owns belongs; send_dm is for what belongs to one person. Your final answer is posted for you when the run ends: finish with what is left to say about the run as a whole, not with a copy of what you posted here. Slack markdown: *bold*, _italic_, <url|text>. At most " + strconv.Itoa(maxRoutinePosts) + " per run.",
		Params: schema(map[string]any{
			"text": str("The message, written for the channel that will read it"),
		}, "text"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ Text string }
			json.Unmarshal(args, &p)
			p.Text = strings.TrimSpace(p.Text)
			if p.Text == "" {
				return "", fmt.Errorf("text is empty; pass the message the channel should read")
			}
			if c.postsSent >= maxRoutinePosts {
				return "", fmt.Errorf("this run has already posted %d messages in the thread, which is the limit. Say what is left unposted in your report rather than trying again", maxRoutinePosts)
			}
			if c.SL == nil || c.ThreadTS == "" {
				return "", fmt.Errorf("no thread for this run to post in")
			}
			ts, err := c.SL.PostMarkdown(ctx, c.Channel, c.ThreadTS, defuseBroadcasts(p.Text), "")
			if err != nil {
				return "", fmt.Errorf("could not post to the thread: %w", err)
			}
			c.postsSent++
			slog.Info("routine thread post", "channel", c.Channel, "thread", c.ThreadTS, "sent", c.postsSent)
			return fmt.Sprintf("Posted (message %d of at most %d this run, ts %s). Do not post it again.", c.postsSent, maxRoutinePosts, ts), nil
		},
	}
}

// broadcasts takes the sting out of @channel, @here and @everyone and leaves the rest of the
// markdown — links, mentions of one person — exactly as written. A routine posts on a schedule,
// forever, so the one mistake that cannot be taken back is the one that notifies everybody every
// single time it is made; it has happened here once already, from a prompt that carried
// <!channel> into a post. Escaping the whole message is the blunt version of this and costs
// every link in it.
var broadcasts = strings.NewReplacer(
	"<!channel|@channel>", "@channel", "<!channel>", "@channel",
	"<!here|@here>", "@here", "<!here>", "@here",
	"<!everyone|@everyone>", "@everyone", "<!everyone>", "@everyone",
)

func defuseBroadcasts(s string) string { return broadcasts.Replace(s) }

// quietVerdict is what a silent turn decided: whether to post, the text to post, and the note
// for the log. A decision made with the tools is taken as it stands; only a run that ignored
// them falls back to reading its prose.
func (c *Call) quietVerdict() (report bool, body, note string) {
	if c.quietDecided {
		return c.quietReport != "", c.quietReport, c.quietNote
	}
	report, note = parseRoutineVerdict(c.FinalText)
	return report, c.FinalText, note
}

// parseRoutineVerdict reads a quiet run's answer. NOTHING on the first line means it found
// nothing worth reporting, and whatever follows is its note for the log. The parse is
// deliberately forgiving: "Nothing to report — all six VMs under 80%" is a decision, not a
// report, and a model that phrases it that way should not end up posting it to the channel.
func parseRoutineVerdict(text string) (report bool, note string) {
	t := strings.TrimSpace(text)
	if t == "" {
		return false, ""
	}
	first, rest, _ := strings.Cut(t, "\n")
	head := strings.TrimSpace(first)
	if !strings.HasPrefix(strings.ToUpper(head), "NOTHING") {
		// A model that decided to stay quiet often says so in prose instead of using the
		// signal — "…10 or fewer, so no report posted". That sentence is a decision, not a
		// report, and posting it is the noise the mode exists to remove.
		if narratesNoReport(t) {
			return false, oneLine(truncate(t, 300))
		}
		return true, ""
	}
	after := strings.TrimSpace(head[len("NOTHING"):])
	switch {
	case after == "":
		// The verdict on its own, as instructed. Whatever is under it is the note.
	case len(after) >= 9 && strings.EqualFold(after[:9], "to report"):
		// "Nothing to report" is the one phrasing where the word still means the verdict
		// rather than opening a sentence about something.
		after = strings.TrimSpace(after[9:])
	case !strings.ContainsRune(":-–—,.;", []rune(after)[0]):
		// "Nothing is wrong with web-1, but web-3 is at 94%" is a report that happens to start
		// with the word, and it is the one thing this must never swallow. Only a bare verdict,
		// or one followed by punctuation, counts as a decision to stay quiet.
		return true, ""
	}
	note = strings.TrimSpace(strings.TrimLeft(after, ":-–—,.; "))
	if note == "" {
		note = strings.TrimSpace(rest)
	}
	return false, oneLine(truncate(note, 300))
}

// noReportPhrases are the ways a model announces it is not reporting. Each states the decision
// outright, so a real report cannot contain one without contradicting itself. This is a fallback
// for a run that ignored the decision tools, deliberately narrow: the cost of reading a genuine
// finding as silence is higher than the cost of one stray post, and every quiet run's full text
// is in the console either way.
var noReportPhrases = []string{
	"no report posted", "no report was posted", "no report is posted", "not posting a report",
	"no report needed", "no report necessary", "nothing to report", "not reporting",
	"so no report", "no message posted", "no message was posted", "staying quiet",
}

func narratesNoReport(text string) bool {
	low := strings.ToLower(text)
	for _, p := range noReportPhrases {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func nextRun(spec, tz string, from time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad timezone %q", tz)
	}
	sched, err := cronParser.Parse(spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad cron %q: %w", spec, err)
	}
	return sched.Next(from.In(loc)), nil
}

// routineTooFrequent says whether a schedule fires more often than the floor. Every run is a
// model turn on the shared key and one of the scheduler's slots; a routine on `* * * * *`
// holds one of an organisation's slots permanently and eats into everybody's.
func routineTooFrequent(cronSpec, tz string) bool {
	first, err := nextRun(cronSpec, tz, time.Now())
	if err != nil {
		return false // an invalid schedule is refused elsewhere
	}
	second, err := nextRun(cronSpec, tz, first)
	if err != nil {
		return false
	}
	return second.Sub(first) < minRoutineInterval
}

func (a *Agent) registerRoutineTools() {
	a.register(Tool{
		Name: "create_routine",
		Desc: "Schedule a recurring task here, e.g. 'every weekday at 9am post a digest of #support'. Provide a 5-field cron expression (min hour dom month dow) and the prompt to run. Routines post where they were created — a channel, or this DM if that is where you are asked.",
		Params: schema(map[string]any{
			"cron":        str("5-field cron, e.g. '0 9 * * 1-5' for weekdays 09:00"),
			"prompt":      str("What to do each time, written as an instruction to the assistant"),
			"tz":          str("IANA timezone (optional, default " + a.cfg.Timezone + ")"),
			"notify":      str("'always' to post every run (default), or 'when_needed' to stay out of the channel unless notify_when is met"),
			"notify_when": str("With notify='when_needed': what makes a run worth posting, e.g. 'any VM is over 80% CPU'. Every run is recorded in the console either way."),
		}, "cron", "prompt"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct {
				Cron, Prompt, TZ, Notify string
				NotifyWhen               string `json:"notify_when"`
			}
			json.Unmarshal(args, &p)
			// A routine created here always starts with its writes held for a Confirm press. Whether
			// they may run unattended is not the model's to decide: the instruction can come from
			// content the model read — a document, a web page, a forwarded mail — and a routine that
			// both writes without asking and stays quiet is exactly what a planted instruction would
			// ask for. Turning auto-confirm on is a deliberate console action by a person the org
			// trusts, out of reach of anything the model was told.
			autoConfirm := false
			if p.TZ == "" {
				p.TZ = a.settings.Get(ctx, c.OrgID).Timezone
			}
			next, err := nextRun(p.Cron, p.TZ, time.Now())
			if err != nil {
				return "", err
			}
			if routineTooFrequent(p.Cron, p.TZ) {
				return "", fmt.Errorf("routines run at most every %s; pick a wider schedule", minRoutineInterval)
			}
			// The organisation and workspace are what make the routine findable afterwards: the
			// console lists by org, the scheduler resolves a Slack client by team. Without them
			// a routine created from Slack lands at org 0 and nobody — including the person who
			// asked for it — can list, edit or stop it again.
			id, err := a.store.AddRoutine(ctx, Routine{OrgID: c.OrgID, TeamID: c.TeamID,
				Channel: c.Channel, Cron: p.Cron, TZ: p.TZ, Prompt: p.Prompt,
				CreatedBy: c.UserID, NextRun: next.UTC().Format(time.DateTime),
				Notify: notifyMode(p.Notify), NotifyWhen: p.NotifyWhen, AutoConfirm: autoConfirm})
			if err != nil {
				return "", err
			}
			// Said out loud rather than left in a flag: the channel should know a routine now exists,
			// and that its writes will be held for a Confirm press each run until an admin chooses
			// otherwise in the console.
			held := ". Each write it makes will wait for a Confirm press; an admin can let it run writes unattended from the console"
			if notifyMode(p.Notify) == notifyWhenNeeded {
				return fmt.Sprintf("routine #%d created, posting here only when %s; next run %s (every run is recorded in the console either way)%s",
					id, cmp.Or(p.NotifyWhen, "there is something worth reporting"), next.Format("Mon 2 Jan 15:04 MST"), held), nil
			}
			return fmt.Sprintf("routine #%d created; next run %s%s", id, next.Format("Mon 2 Jan 15:04 MST"), held), nil
		},
	})
	a.register(Tool{
		Name:   "list_routines",
		Desc:   "List scheduled routines in this channel.",
		Params: schema(map[string]any{}),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			return a.routinesText(ctx, c.OrgID, c.Channel), nil
		},
	})
	a.register(Tool{
		Name:   "delete_routine",
		Desc:   "Disable a routine by id.",
		Params: schema(map[string]any{"id": num("Routine id")}, "id"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct{ ID int64 }
			json.Unmarshal(args, &p)
			n, err := a.store.SetRoutineEnabledInChannel(ctx, c.OrgID, c.Channel, p.ID, false)
			if err != nil {
				return "", err
			}
			if n == 0 {
				return "no such routine in this channel", nil
			}
			return fmt.Sprintf("routine #%d disabled", p.ID), nil
		},
	})
}

func (a *Agent) routinesText(ctx context.Context, orgID int64, channel string) string {
	rs, err := a.store.Routines(ctx, orgID, channel)
	if err != nil {
		return "error: " + err.Error()
	}
	var b strings.Builder
	for _, r := range rs {
		if !r.Enabled {
			continue
		}
		quiet := ""
		if r.Notify == notifyWhenNeeded {
			quiet = " [posts only when " + cmp.Or(r.NotifyWhen, "there is something to report") + "]"
		}
		fmt.Fprintf(&b, "- #%d `%s` (%s) next %s — %s%s\n", r.ID, r.Cron, r.TZ, r.NextRun, truncate(r.Prompt, 120), quiet)
	}
	if b.Len() == 0 {
		return "no routines in this channel"
	}
	return b.String()
}

// RunScheduler fires due routines every 30s. Each run posts a header message in the target
// channel and answers the prompt in that message's thread (a fresh session), so the output is
// a normal thread people can reply into.
func (a *Agent) RunScheduler(ctx context.Context) { a.runScheduler(ctx, schedulerTick) }

// runScheduler is RunScheduler with its clock as an argument, so a test can watch two ticks
// without waiting a minute for them.
func (a *Agent) runScheduler(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	// The gate outlives the tick that fills it, because the runs do. This loop used to wait for
	// everything it had started before looking at the clock again, which made both caps per-tick
	// numbers and — far worse — made the slowest routine in the deployment the one that decided
	// when anybody else's ran. A routine's wall is half an hour by default, so that was half an
	// hour of every other tenant's schedule missed by one tenant's run.
	gate := newRoutineGate(schedulerConcurrency, schedulerPerOrg)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if a.sweepAccess != nil {
			a.sweepAccess(ctx)
		}
		rs, err := a.store.DueRoutines(ctx)
		if err != nil {
			continue
		}
		nowUTC := time.Now().UTC().Format(time.DateTime)
		for _, r := range rs {
			if !r.Enabled || r.NextRun == "" || r.NextRun > nowUTC {
				continue
			}
			// A routine whose run is still going reads as due — next_run only moves when a run
			// finishes — and a second run beside the first is not what "due" means. runRoutineNow
			// is where that is actually decided, for every caller; asking here first keeps a long
			// run from spending a slot every thirty seconds to be turned away inside.
			if a.routineBusy(r) || !gate.claim(r.OrgID) {
				continue // still running, or the deployment is full: it is due again next tick
			}
			go func(r Routine) {
				defer gate.release(r.OrgID)
				a.runDueRoutine(ctx, r)
			}(r)
		}
	}
}

// How often the scheduler looks for work, how many routines run at once across the deployment,
// and the most any one organisation may hold. The last two are deliberately small: a routine is
// a full agent turn.
const (
	schedulerTick        = 30 * time.Second
	schedulerConcurrency = 8
	schedulerPerOrg      = 3
)

// routineGate is what bounds the runs now that nothing waits for them: slots across the whole
// deployment, and a count per organisation so one tenant cannot hold them all. Both are counted
// against runs in flight rather than against one tick's worth of dispatch.
type routineGate struct {
	mu        sync.Mutex
	perOrg    map[int64]int
	perOrgCap int
	slots     chan struct{}
}

func newRoutineGate(concurrency, perOrgCap int) *routineGate {
	return &routineGate{perOrg: map[int64]int{}, perOrgCap: perOrgCap, slots: make(chan struct{}, concurrency)}
}

// claim takes a slot for one organisation, or says there is none. It never blocks: a routine
// turned away is still due, and the next tick is thirty seconds off, which is a better place to
// wait than in a goroutine holding a row from a sweep that is already stale.
func (g *routineGate) claim(orgID int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.perOrg[orgID] >= g.perOrgCap {
		return false
	}
	select {
	case g.slots <- struct{}{}:
	default:
		return false
	}
	g.perOrg[orgID]++
	return true
}

func (g *routineGate) release(orgID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.perOrg[orgID]--; g.perOrg[orgID] <= 0 {
		delete(g.perOrg, orgID)
	}
	<-g.slots
}

// One run of a routine at a time, across the whole deployment.
//
// This used to be a map in the agent, which was right for one process and wrong for two: both
// schedulers find the same routine due — next_run is only advanced when a run finishes — both
// claim it, and the channel gets the same answer twice from what its members were told was one
// scheduled job. The claim is on the row every scheduler already read.
//
// The lease also improves the single-instance case it replaces. A process that died mid-run
// left its map entry with it and the routine ran again on the next boot; a lease expires, so a
// hung run blocks its routine for routineLease and no longer.
const (
	routineLease = 60 * time.Second
	routineRenew = 20 * time.Second
)

// routineBusy is the scheduler asking in advance, and it costs no query: DueRoutines already
// read the row. Being told "no" a moment before it becomes true costs nothing — claimRoutine is
// what two callers actually race on.
func (a *Agent) routineBusy(r Routine) bool { return r.RunLease > time.Now().UnixNano() }

// claimRoutine takes the run, or reports that somebody else has it. The returned token is the
// lease value written; every renewal and the release carry it, so an instance that stalled long
// enough to lose the routine finds out rather than releasing somebody else's claim.
func (a *Agent) claimRoutine(ctx context.Context, orgID, id int64) (int64, bool) {
	until := time.Now().Add(routineLease).UnixNano()
	res, err := a.store.db.ExecContext(ctx,
		`update routines set run_lease=?, run_holder=? where org_id=? and id=? and run_lease<=?`,
		until, instanceID(), orgID, id, time.Now().UnixNano())
	if err != nil {
		slog.Warn("could not claim routine", "id", id, "err", err)
		return 0, false
	}
	n, _ := res.RowsAffected()
	return until, n == 1
}

// renewRoutineClaim keeps a long run's claim alive. A run that outlives routineLease without
// this would have its routine picked up beside it by the next scheduler tick.
func (a *Agent) renewRoutineClaim(ctx context.Context, orgID, id, token int64) (int64, bool) {
	until := time.Now().Add(routineLease).UnixNano()
	res, err := a.store.db.ExecContext(ctx,
		`update routines set run_lease=? where org_id=? and id=? and run_lease=?`, until, orgID, id, token)
	if err != nil {
		return token, true // a hiccup is not proof of loss; the next tick decides
	}
	n, _ := res.RowsAffected()
	return until, n == 1
}

func (a *Agent) releaseRoutine(ctx context.Context, orgID, id, token int64) {
	a.store.db.ExecContext(ctx, `update routines set run_lease=0, run_holder='' where org_id=? and id=? and run_lease=?`, orgID, id, token)
}

// runDueRoutine runs one routine and records the outcome. Split out of the scheduler loop so it
// can be a goroutine without the loop variable and the error handling tangling together.
func (a *Agent) runDueRoutine(ctx context.Context, r Routine) {
	next, err := nextRun(r.Cron, r.TZ, time.Now())
	nextStr := ""
	if err == nil {
		nextStr = next.UTC().Format(time.DateTime)
	}
	a.runRoutineNow(ctx, r, nextStr)
}

// runRoutineNow runs one routine and records what happened, whatever set it off. nextRun is the
// schedule to advance to, which a run started by hand leaves where it was: asking for a run now
// should not move the next one.
func (a *Agent) runRoutineNow(ctx context.Context, r Routine, nextRun string) {
	// One run of a routine at a time, whoever asked for it. next_run is only advanced when a run
	// finishes, so a run that outlives its own next slot leaves the routine looking due and the
	// scheduler would start a second one beside it; "run now" from the console and the API never
	// went past the scheduler at all. Deciding it here rather than up there is what makes it true
	// of all three.
	// The claim is bookkeeping about this deployment, not part of the run's own work, so it
	// does not travel on the run's context. A turn cancelled by a shutdown or its own wall
	// clock still has to take the claim, fail, and leave the row that says it failed — that
	// row is the only trace a scheduled run leaves.
	book := context.WithoutCancel(ctx)
	token, got := a.claimRoutine(book, r.OrgID, r.ID)
	if !got {
		slog.Info("routine is already running, this trigger is dropped", "id", r.ID)
		return
	}
	defer a.releaseRoutine(book, r.OrgID, r.ID, token)
	// Renew underneath the run. A routine may spend minutes on tool calls, and a lease that
	// expired while it worked would let the next scheduler tick start a second one beside it.
	renewed := make(chan struct{})
	defer close(renewed)
	go func() {
		tick := time.NewTicker(routineRenew)
		defer tick.Stop()
		for {
			select {
			case <-renewed:
				return
			case <-tick.C:
				next, ok := a.renewRoutineClaim(ctx, r.OrgID, r.ID, token)
				if !ok {
					return // somebody else has it; the run finishes and releases nothing
				}
				token = next
			}
		}
	}()
	// A routine belongs to one workspace. Resolve its client first, so a routine in a
	// workspace that was disconnected fails with a reason instead of posting nowhere.
	started := time.Now()
	sl, slErr := a.slacks.For(ctx, r.TeamID)
	out, runErr := routineOutcome{}, slErr
	if slErr == nil {
		out, runErr = a.runRoutine(ctx, sl, r)
	}
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
		out.Status = runFailed
		var skipped skippedError
		if errors.As(runErr, &skipped) {
			out.Status = runSkipped
		}
		slog.Error("routine failed", "id", r.ID, "err", runErr)
		a.alert(ctx, orgOfChat(sl), fmt.Sprintf("routine:%d", r.ID), fmt.Sprintf(":warning: Routine #%d in %s failed: %s", r.ID, routineWhere(r.Channel), truncate(msg, 200)))
		// A quiet routine says nothing in the channel, so a broken one would otherwise fail
		// silently for as long as nobody thought to look. The creator is told either way.
		if r.CreatedBy != "" && sl != nil {
			sl.PostText(ctx, r.CreatedBy, "", fmt.Sprintf("Routine #%d in %s failed: %s", r.ID, routineWhere(r.Channel), msg))
		}
	}
	// The run's context may be dead by the time there is something to record — the wall clock
	// ran out, or the container was told to stop — and this row is the only trace a scheduled run
	// leaves anywhere. It is written on a context of its own so that a run which was cut short is
	// recorded as having been cut short, rather than disappearing along with its deadline.
	wctx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	a.store.UpdateRoutineRun(wctx, r.OrgID, r.ID, nextRun, msg, out.Status)
	a.store.AddRoutineRun(wctx, RoutineRun{
		OrgID: r.OrgID, RoutineID: r.ID, TeamID: r.TeamID, Channel: r.Channel,
		ThreadTS: out.ThreadTS, Status: out.Status, Reason: out.Reason,
		Output: truncate(out.Output, routineOutputKept), Error: msg,
		TokensIn: out.Usage.In, TokensOut: out.Usage.Out, CostUSD: out.Usage.CostUSD,
		StartedAt: started.UTC().Format(time.DateTime), FinishedAt: now(), MS: int(time.Since(started).Milliseconds()),
	})
}

// routineChannelTeam resolves the channel a routine is being moved to into the workspace it
// belongs to. The channel has to be one the bot is in — a routine posts on a schedule with
// nobody watching, so a channel it cannot post to is a run that fails every morning — and a
// Slack Connect channel carries the same id in every workspace it is shared into, so when the
// id alone is ambiguous the caller has to say which.
func routineChannelTeam(ctx context.Context, st *Store, orgID int64, channel, teamID string) (string, error) {
	scopes, err := st.Scopes(ctx, orgID)
	if err != nil {
		return "", err
	}
	var teams []string
	for _, sc := range scopes {
		if sc.Kind == "channel" && sc.SlackID == channel && (teamID == "" || sc.TeamID == teamID) && !slices.Contains(teams, sc.TeamID) {
			teams = append(teams, sc.TeamID)
		}
	}
	switch len(teams) {
	case 0:
		return "", fmt.Errorf("%q is not a channel the bot is in: invite it there and refresh its workspace's channels on the Workspaces page first", channel)
	case 1:
		return teams[0], nil
	}
	return "", fmt.Errorf("%q is shared into %d workspaces; say which one with teamId", channel, len(teams))
}

// routineByID reads one routine as it stands, for a handler that has to compare before with
// after. The store lists rather than fetching one, which is no loss here: a routine list is
// short by construction (maxRoutinesPerOrg) and this runs on an edit, not on a schedule.
func (b *Bot) routineByID(ctx context.Context, orgID, id int64) *Routine {
	rs, err := b.store.Routines(ctx, orgID, "")
	if err != nil {
		return nil
	}
	for _, r := range rs {
		if r.ID == id {
			return &r
		}
	}
	return nil
}

// tellRoutineChangedHands lets the person a routine used to run as know that it no longer does.
// Nothing else would tell them: the routine goes on running, on their schedule, in their
// channel, under the name they gave it, and the only thing that moved is whose accounts it
// reaches — so from where they sit a silent handover looks exactly like nothing having
// happened. Best effort, like every other notice the scheduler sends.
func (b *Bot) tellRoutineChangedHands(ctx context.Context, was Routine, now string) {
	sl, err := b.slacks.For(ctx, was.TeamID)
	if err != nil || sl == nil {
		return
	}
	who := "Somebody working in the console"
	if now != "" {
		who = "<@" + now + ">"
	}
	sl.PostText(ctx, was.CreatedBy, "", fmt.Sprintf(
		"%s changed what routine #%d in %s does, so it now runs as them rather than as you. "+
			"It can no longer reach the accounts you connected. Edit it yourself to take it back.",
		who, was.ID, routineWhere(was.Channel)))
}

// routineStepsField reads the steps of a routine write. A console textarea sends the JSON
// text of an array and an API client sends the array itself; both mean the same thing, and
// parseRoutineSteps is still what decides whether either is valid. ok is false when the field
// was absent, which is not the same as an empty list — one leaves the steps alone, the other
// clears them.
func routineStepsField(raw json.RawMessage) (steps string, ok bool, err error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "", false, nil
	}
	if strings.HasPrefix(s, `"`) {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", false, err
		}
		s = text
	}
	return s, true, nil
}

// routineOutputKept caps what one run may put in the log. A run that returns a whole file's
// worth of text should not be able to make the table unreadable, or unloadable.
const routineOutputKept = 20_000

// routineOutcome is what a run leaves behind for the log. ThreadTS is the run key everything
// else about the run was recorded under — usage, tool_calls, turns and its session — which for
// a quiet run is synthetic, because there is no Slack message to key on.
type routineOutcome struct {
	ThreadTS string
	Status   string
	Reason   string
	Output   string
	Usage    Usage
}

func (a *Agent) runRoutine(ctx context.Context, sl *Chat, r Routine) (routineOutcome, error) {
	set := a.settings.Get(ctx, sl.OrgID)
	rounds, wall := routineBudget(set)
	// The whole run gets the wall; the digging gets it less the clock the write-up is given, so
	// a run that spends every round still has somewhere to say what it found. Under a budget too
	// small to split, the two share it and the write-up takes its chances.
	dig := wall - answerWall
	if dig < wall/2 {
		dig = wall / 2
	}
	ctx, cancel := context.WithTimeout(ctx, wall)
	defer cancel()
	var out routineOutcome
	if ok, why := a.budgetOK(ctx, sl.OrgID, r.TeamID); !ok {
		return out, skippedError{why}
	}
	// The prompt is somebody's text and it is posted before the model has said anything, so it
	// is escaped like any other quoted text. Unescaped, a routine whose prompt held <!channel>
	// pinged everyone in the channel on every single run — the schedule made it recurring, and
	// nothing the model did could take it back.
	// The pinned steps, if it has any. Parsed before anything is posted: a routine whose steps
	// no longer read as steps has a mistake in it, and the run should say so rather than post a
	// header and then fail underneath it.
	steps, err := parseRoutineSteps(r.Steps)
	if err != nil {
		return out, err
	}
	header := fmt.Sprintf(":alarm_clock: *Routine #%d* — %s", r.ID, escapeMrkdwn(preview(oneLine(r.Prompt), 160)))
	quiet := r.Notify == notifyWhenNeeded
	// A routine whose steps are the whole of it: they run, their output is the post, and the
	// model is never called. Nothing to stream and nothing to announce — the run is over in the
	// time one HTTP request takes — so it posts once, at the end, if it posts at all.
	raw := len(steps) > 0 && routineFinish(r.Finish) == finishRaw

	// A run that may end with nothing to say must not announce itself before it has looked, so
	// the quiet path posts no header and keys its session on a synthetic id instead of a message
	// ts: unique per run, so its tool budget is its own, and deliberately not ts-shaped, so
	// nothing downstream tries to resolve it against Slack.
	rootTS := fmt.Sprintf("routine:%d:%d", r.ID, time.Now().UnixNano())
	if !quiet && !raw {
		var err error
		if rootTS, err = sl.PostMarkdown(ctx, r.Channel, "", header+"\n\n_working…_", ""); err != nil {
			return out, err
		}
	}
	out.ThreadTS = rootTS
	// The session is this run's alone, so the model the routine was set to ride on it.
	sess, err := a.store.EnsureSession(ctx, r.TeamID, r.Channel, rootTS, "routine", routineSessionModel(set, r.Model))
	if err != nil {
		return out, err
	}
	a.store.AddTurn(ctx, r.TeamID, r.Channel, rootTS, "user", r.CreatedBy, r.Prompt, rootTS, 0, 0)
	userID := r.CreatedBy
	if userID == "" {
		userID = sl.BotUserID
	}
	c := &Call{TeamID: r.TeamID, OrgID: sl.OrgID, SL: sl, Channel: r.Channel, ThreadTS: rootTS, UserID: userID, Text: r.Prompt, Kind: "routine", Session: sess,
		MaxRounds: rounds, Wall: dig, autoConfirm: r.AutoConfirm,
		Silent: quiet || raw, PostWhen: r.NotifyWhen, Streamer: sl.NewStreamer(r.Channel, rootTS, userID)}
	if quiet || raw {
		// Nothing posted a header, so there is no thread to write into: the activity cards a
		// live streamer sends would be addressed to a message id that does not exist.
		c.Streamer = sl.NewSilentStreamer(r.Channel, rootTS, userID)
	}
	if len(steps) > 0 {
		loc, lerr := time.LoadLocation(r.TZ)
		if lerr != nil {
			loc = time.UTC
		}
		results := a.runRoutineSteps(ctx, c, steps, loc)
		if raw {
			return a.finishRawRoutine(ctx, sl, r, header, quiet, results, out)
		}
		// The model gets the output instead of the round it would have spent asking for it.
		// With finish "answer" that is all it gets: the work is done, so the tools go too, and
		// what is left is one call that writes the result up.
		c.Result = stepsPrelude(results, routineFinish(r.Finish) == finishAnswer)
		if routineFinish(r.Finish) == finishAnswer {
			c.Fixed, c.NoTools = true, true
		}
	}
	err = a.Run(ctx, c)
	out.Output, out.Usage = c.FinalText, c.usage
	// Anything the run was not allowed to do. It still wrote a report and the report may read
	// perfectly well, which is the danger: without this the run is recorded green and the write
	// that never happened is nowhere anybody would look.
	heldNote := ""
	if n := len(c.silentHeld); n > 0 {
		heldNote = fmt.Sprintf("%d step(s) needed a person's OK and did not run: %s", n, truncate(strings.Join(c.silentHeld, "; "), 400))
		if r.CreatedBy != "" {
			sl.PostText(detached(ctx), r.CreatedBy, "", fmt.Sprintf("Routine #%d in %s ran, but %s\nIf it should run unattended, set the routine's Writes to Run without asking in the console (Routines, then edit it), "+
				"or set that connection's Writes to Automatic, or add an allow rule covering it.",
				r.ID, routineWhere(r.Channel), heldNote))
		}
	}
	if c.writesRun > 0 {
		// A routine that changes things unattended should not be readable only as "posted".
		heldNote = joinNote(fmt.Sprintf("%d write(s) ran without asking", c.writesRun), heldNote)
	}
	out.Reason = heldNote
	if err != nil {
		return out, err
	}
	if !quiet {
		if c.FinalText != "" {
			// Claude-Tag style: the header message is edited with the outcome — and the answer
			// ends up there or in the thread, never in both.
			sl.settleAnswer(ctx, r.Channel, rootTS, header, c.FinalText, c.AnswerTS, set.LongAnswerChars)
		}
		out.Status = runPosted
		return out, nil
	}
	// The decision it signalled, if it used the tools; otherwise read from what it wrote.
	report, body, note := c.quietVerdict()
	out.Reason = joinNote(heldNote, note)
	if body != "" {
		out.Output = body
	}
	if !report {
		out.Status = runQuiet
		return out, nil
	}
	// It cleared its own bar, so it speaks — once, as a single message. There is no header to
	// edit and no thread to fill, because nothing was posted while it worked.
	if _, err := sl.PostMarkdown(ctx, r.Channel, "", header+"\n\n"+body, ""); err != nil {
		return out, err
	}
	out.Status = runPosted
	return out, nil
}

// finishRawRoutine ends a run that never called the model: its steps returned, and their output
// is the post. A quiet one keeps its bar, read the only way a run with no model can read it —
// every step worked, so there is nothing to say; a step failed, so there is. That is the cheap
// health check people actually want from a schedule: silence while it is fine, the error the
// moment it is not, and not one token either way.
func (a *Agent) finishRawRoutine(ctx context.Context, sl *Chat, r Routine, header string, quiet bool, results []stepResult, out routineOutcome) (routineOutcome, error) {
	out.Output = stepsRawText(results)
	if ok := stepsOK(results); quiet && ok {
		out.Status, out.Reason = runQuiet, fmt.Sprintf("%d step(s) ran, nothing failed", len(results))
		return out, nil
	} else if quiet {
		out.Reason = "a step failed"
	}
	if _, err := sl.PostMarkdown(ctx, r.Channel, "", header+"\n\n"+out.Output, ""); err != nil {
		return out, err
	}
	out.Status = runPosted
	return out, nil
}

// budgetOK checks the account's monthly cap, and then this workspace's own if it has one.
// The message names which cap was hit: with several workspaces on one account, "the monthly
// budget is used up" reads as your own problem when it may be someone else's spending.
func (a *Agent) budgetOK(ctx context.Context, orgID int64, teamID string) (bool, string) {
	// The effective budget is the organisation's own, under the operator's ceiling: a tenant
	// setting its budget to zero used to switch the check off entirely, on a key the operator
	// pays for. And when spend cannot be read, the answer is no — an accounting error must not
	// read as "nothing spent".
	st := a.settings.Get(ctx, orgID)
	// An organisation whose own model key cannot be used stops here, before anything is spent
	// working out an answer there is nowhere to send (model_keys.go). The reason is the whole
	// answer: nothing else below can help it.
	if err := st.OwnKey.refusal(); err != nil {
		return false, err.Error()
	}
	// Credit first, because it is the harder of the two and therefore the more specific answer.
	// A monthly budget is the customer's own guard rail and an admin can raise it in the console;
	// credit is money, and the only thing that moves it is a payment. Telling somebody their
	// budget is spent when what has actually run out is the balance sends them to a field that
	// will not help.
	//
	// The balance is read here rather than taken off st, which is a cache: it changes on every
	// turn, and a stale copy of it is a window of spending money that is not there. An account
	// with no billing row never gets this far — neither flag is set for a free account or a
	// self-host.
	//
	// Two pockets, one figure. What is spendable is this month's plan allowance plus prepaid
	// credit, and a turn does not care which it comes out of — chargeCredit spends the allowance
	// first because that is the one that expires. AllowanceActive is why the cached flags are
	// tested rather than just CreditEnforced: a subscriber who has never topped up can still be
	// metered, against the credit their plan includes.
	// None of it applies to an organisation on its own key: nothing it spends is drawn from credit.
	ownKey := st.OwnKey.Active()
	if st.BillingUnknown && !ownKey {
		return false, "credit accounting is unavailable, so I've stopped here for now"
	}
	if (st.CreditEnforced || st.AllowanceActive) && !ownKey {
		bal, metered, err := a.store.SpendableCredit(ctx, orgID)
		if err != nil {
			return false, "credit accounting is unavailable, so I've stopped here for now"
		}
		if metered && bal <= -maxOverdraftMicros {
			why := fmt.Sprintf("this account's credit is spent (%s)", creditAmount(bal))
			if u := publicBaseURL(ctx, a.store, a.cfg); u != "" {
				why += ". Top it up at " + u + "/admin/settings/?tab=billing and I'll pick up where I left off"
			} else {
				why += "; an admin can top it up under Settings → Billing"
			}
			return false, why
		}
	}
	if budget := st.EffectiveBudget(); budget > 0 {
		spent, err := budgetSpend(ctx, a.store, orgID, st)
		if err != nil {
			return false, "spend accounting is unavailable, so I've stopped here for now"
		}
		if spent >= budget {
			why := fmt.Sprintf("the account's monthly budget of $%.2f is used up ($%.2f spent across every connected workspace)", budget, spent)
			// A free account cannot raise it in the console, so the refusal says where it can.
			if hint := st.raiseBudgetHint(); hint != "" {
				why += ". " + hint
			}
			return false, why
		}
	}
	if teamID == "" {
		return true, ""
	}
	sc, err := a.store.TeamScope(ctx, orgID, teamID)
	if err != nil || sc == nil || sc.MonthlyBudgetUSD <= 0 {
		return true, ""
	}
	spent, err := a.store.MonthSpend(ctx, orgID, teamID, "")
	if err != nil {
		return false, "spend accounting is unavailable, so I've stopped here for now"
	}
	if spent >= sc.MonthlyBudgetUSD {
		return false, fmt.Sprintf("this workspace's monthly budget of $%.2f is used up ($%.2f spent)", sc.MonthlyBudgetUSD, spent)
	}
	return true, ""
}
