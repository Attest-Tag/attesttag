package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The digging lane.
//
// A reply is sized for a conversation: the person is watching the thread while its clock runs,
// and everything else in the workspace is queued behind the same in-flight cap. Some questions are
// not that shape — "what is causing these failures", asked under an alert — and answering one
// means pulling logs, narrowing, cross-checking, and going back for more. Run as a reply it hit
// the cap holding everything it had found and said so; run with the cap simply raised, one such
// question could occupy every slot the organisation had.
//
// So it runs here instead: queued in the database, picked up by a small pool of its own, given a
// budget of its own, and answered in the thread it came from. The lane's runs are excluded from
// the interactive in-flight cap on purpose — a question that takes ten minutes must never be the
// reason a one-line question elsewhere is refused.

const (
	// What the lane is allowed to be, whatever an organisation sets. The floor is the reply
	// budget — anything less and there was no reason to leave the turn.
	minInvestigationRounds, maxInvestigationRounds   = 12, maxRoundsCeiling
	minInvestigationMinutes, maxInvestigationMinutes = 2, 60
)

// investigationBudget is what one run is given: rounds, and the clock to spend them in.
func investigationBudget(st Settings) (rounds int, wall time.Duration) {
	rounds = min(max(st.InvestigationRounds, minInvestigationRounds), maxInvestigationRounds)
	mins := min(max(st.InvestigationMinutes, minInvestigationMinutes), maxInvestigationMinutes)
	return rounds, time.Duration(mins) * time.Minute
}

// ---- the tool ----

// wantDiggingRe is the ask that might be worth handing over: a question about a cause, a
// failure, or which records are involved in one. It is the same kind of read as forcedTool and
// mentionsConn — what the person said decides which tools the turn carries — and it is only
// ever a trigger for offering the tool, never for using it: whether the question actually needs
// the lane is the model's call, and refusing it costs nothing but a round.
var wantDiggingRe = regexp.MustCompile(`(?i)\b(root ?cause|why (is|are|isn't|aren't|did|didn't|does|doesn't|do|don't|was|wasn't|were|weren't|has|have)|` +
	`what('s| is| are|'re)? (the )?(cause|reason|causing|going on|happening|happened|wrong)|went wrong|goes wrong|` +
	`investigate|look into|looking into|dig into|get to the bottom|diagnose|troubleshoot|trace|track down|figure out|find out|` +
	`fail(ing|ed|ures?)|erroring|errors?|broke(n)?|breaking|regress(ion|ed)|degrad(ed|ing|ation)|outage|incident|timing out|timeouts?|` +
	`stopped working|not working|keeps? (failing|crashing|erroring)|` +
	`which (customers?|clients?|users?|tenants?|accounts?|documents?|files?|records?|jobs?|requests?|orgs?))\b`)

// wantsDigging decides whether this turn is offered start_investigation.
func (a *Agent) wantsDigging(ctx context.Context, c *Call) bool {
	if a.store == nil || c.MaxRounds > 0 || c.offline() || c.Kind == "routine" || c.NoTools {
		return false
	}
	if !wantDiggingRe.MatchString(c.Text) {
		return false
	}
	return a.settings.Get(ctx, c.OrgID).Investigations
}

// investigationTool is start_investigation. It is offered to a turn that could hand work over,
// which is any ordinary reply — and never to a run that is already the lane, which would let one
// investigation queue the next forever.
func (a *Agent) investigationTool(c *Call) Tool {
	return Tool{
		Name: "start_investigation",
		Desc: "Hand a question that needs real digging to the background investigator: it works in this thread on a budget of its own, in the background — so nobody waits on it and it holds none of the slots replies need — survives a restart, and posts what it finds here when it is done. Reach for it when answering properly means several rounds of gathering and checking evidence: tracing an alert to its cause, reading logs across a window, reconciling numbers between systems. Give it the question and everything you already know, verbatim — the alert text, the numbers, the service, the time window — because it starts from your brief, not from your reasoning. Then tell the person, in one short line, that you are on it. Do not use this for anything you can answer in a couple of tool calls, and never for a change to anything: it only reads.",
		Params: schema(map[string]any{
			"question": str("The question to answer, in one or two full sentences"),
			"brief":    str("Everything you already know that bears on it: the alert or message that started this, figures, service and environment names, the time window, and anything you have already ruled out. Quote verbatim where you can."),
			"plan":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The steps you would take, one per item, if you have a view on where to start"},
		}, "question"),
		Run: func(ctx context.Context, c *Call, args json.RawMessage) (string, error) {
			var p struct {
				Question string   `json:"question"`
				Brief    string   `json:"brief"`
				Plan     []string `json:"plan"`
			}
			json.Unmarshal(args, &p)
			p.Question, p.Brief = strings.TrimSpace(p.Question), strings.TrimSpace(p.Brief)
			if p.Question == "" {
				return "", errors.New("say what the investigation is to find out")
			}
			st := a.settings.Get(ctx, c.OrgID)
			if err := a.investigationPrecheck(ctx, c, st); err != nil {
				return "", err
			}
			rounds, wall := investigationBudget(st)
			brief := truncate(redact(p.Brief), 6000)
			if steps := cleanList(p.Plan, 12, 300); len(steps) > 0 {
				brief = strings.TrimSpace(brief + "\n\nWhere the reply turn would have started:\n- " + strings.Join(steps, "\n- "))
			}
			id, err := a.store.EnqueueInvestigation(ctx, &Investigation{
				OrgID: c.OrgID, TeamID: c.TeamID, Channel: c.Channel, ThreadTS: c.ThreadTS,
				Requester: c.UserID, Question: truncate(redact(p.Question), 2000), Brief: brief,
				Rounds: rounds, Minutes: int(wall / time.Minute),
			})
			if err != nil {
				return "", fmt.Errorf("could not queue the investigation: %w", err)
			}
			slog.Info("investigation queued", "id", id, "org", c.OrgID, "channel", c.Channel, "thread", c.ThreadTS, "rounds", rounds, "wall", wall)
			return fmt.Sprintf("Investigation #%d is queued and will work in this thread with %d tool rounds and up to %s. "+
				"Reply now with one short line saying you are looking into it — do not answer the question itself, do not guess at the cause, "+
				"and do not list what you are about to do: the investigation posts its own answer here when it has one.", id, rounds, wall), nil
		},
	}
}

// investigationPrecheck is everything that can refuse a run before it is queued.
func (a *Agent) investigationPrecheck(ctx context.Context, c *Call, st Settings) error {
	if !st.Investigations {
		return errors.New("the background investigator is switched off for this account; answer with the tool budget you have")
	}
	if c.MaxRounds > 0 {
		return errors.New("this is already the investigation; keep going rather than queueing another")
	}
	if c.Preview {
		return errors.New("a console preview has no thread to post an investigation into; answer with the budget you have")
	}
	if c.Silent {
		return errors.New("a quiet run has no thread to post an investigation into")
	}
	open, err := a.store.OpenInvestigationsInThread(ctx, c.OrgID, c.TeamID, c.Channel, c.ThreadTS)
	if err != nil {
		return errors.New("the investigation queue is unavailable")
	}
	if len(open) > 0 {
		return fmt.Errorf("investigation #%d is already working in this thread; wait for it, or say stop", open[0].ID)
	}
	if n := a.store.CountOpenInvestigations(ctx, c.OrgID); n >= st.InvestigationMaxOpen {
		return fmt.Errorf("%d investigations are already queued or running (the limit is %d); wait for one to finish", n, st.InvestigationMaxOpen)
	}
	if ok, why := a.budgetOK(ctx, c.OrgID, c.TeamID); !ok {
		return errors.New(why)
	}
	return nil
}

// ---- the lane ----

// RunInvestigations drains the queue for as long as the process lives. The pool is small and
// process-wide: these runs are minutes long, and the point of the lane is that they are the only
// thing waiting on each other.
func (a *Agent) RunInvestigations(ctx context.Context) {
	n := a.cfg.InvestigationWorkers
	if n <= 0 {
		slog.Info("the investigation lane is off (INVESTIGATION_WORKERS=0)")
		return
	}
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if !a.nextInvestigation(ctx) {
					select {
					case <-ctx.Done():
						return
					case <-time.After(2 * time.Second):
					}
				}
			}
		}()
	}
	// Runs whose attempts are spent are nobody's work any more, and the thread that asked is
	// still waiting. Closing them out is this loop's only job.
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-tick.C:
			a.closeAbandonedInvestigations(ctx)
		}
	}
}

// nextInvestigation claims one run and sees it through. It reports whether there was work, so an
// idle worker knows to wait rather than spin.
func (a *Agent) nextInvestigation(ctx context.Context) bool {
	v, err := a.store.claimInvestigation(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("investigation claim", "err", err)
		}
		return false
	}
	if v == nil {
		return false
	}
	if v.Attempts > 1 {
		// The lease it held ran out, which means the container running it went away. Everything
		// it had found went with it: the run starts again, and the thread will show both the
		// message it had begun streaming and the finished answer.
		slog.Warn("investigation resumed after an interrupted run", "id", v.ID, "attempt", v.Attempts)
	}
	// The lease belongs to this worker only while it is renewed. Losing it means another
	// container has taken the run, and two containers answering one question is worse than
	// none — so the run is cancelled rather than raced.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopHeartbeat := a.holdInvestigation(runCtx, v.ID, cancel)
	status, usage, err := a.runInvestigation(runCtx, v)
	stopHeartbeat()
	// Not runCtx: it is cancelled by the defer above, and during shutdown so is everything
	// derived from the signal. The outcome still has to be written.
	done, doneCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer doneCancel()
	why := ""
	if err != nil {
		why = err.Error()
	}
	if status == "retry" {
		// Nothing was decided, so nothing is recorded as decided: the row goes back to the queue
		// with the attempt it just used, and gives up for good once those are spent.
		if err := a.store.releaseInvestigation(done, v.ID, why, usage); err != nil {
			slog.Warn("investigation not released", "id", v.ID, "err", err)
		}
		slog.Warn("investigation deferred", "id", v.ID, "attempt", v.Attempts, "err", why)
		return true
	}
	if err := a.store.finishInvestigation(done, v.ID, status, why, usage); err != nil {
		slog.Warn("investigation not closed out", "id", v.ID, "err", err)
	}
	slog.Info("investigation finished", "id", v.ID, "status", status, "in", usage.In, "out", usage.Out,
		"cost_usd", fmt.Sprintf("%.5f", usage.CostUSD), "err", why)
	return true
}

// holdInvestigation renews the run's lease until the returned func is called. A renewal that
// fails means the row is no longer ours, and the run is cancelled.
func (a *Agent) holdInvestigation(ctx context.Context, id int64, lost context.CancelFunc) func() {
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(investigationLease / 3)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err := a.store.touchInvestigation(tctx, id)
			cancel()
			if err != nil {
				slog.Warn("investigation lease lost", "id", id, "err", err)
				lost()
				return
			}
		}
	}()
	return func() { once.Do(func() { close(stop) }) }
}

// runInvestigation is the run itself: one turn in the thread, with the lane's budget instead of
// the channel's, and the brief in front of it.
func (a *Agent) runInvestigation(ctx context.Context, v *Investigation) (status string, u Usage, err error) {
	sl, err := a.slacks.For(ctx, v.TeamID)
	if err != nil {
		// Infrastructure, not an answer: the workspace may be mid-reinstall or the database
		// briefly unreachable. The run goes back on the queue rather than failing the question.
		return "retry", u, fmt.Errorf("workspace unavailable: %w", err)
	}
	// Budget is checked again here, not only when it was queued: a run can sit in the queue
	// behind others, and the account may have spent the rest of its month in between.
	if ok, why := a.budgetOK(ctx, v.OrgID, v.TeamID); !ok {
		sl.PostText(ctx, v.Channel, v.ThreadTS, "I couldn't run that investigation: "+why+".")
		return "failed", u, errors.New(why)
	}
	sess, err := a.store.GetSession(ctx, v.TeamID, v.Channel, v.ThreadTS)
	if err != nil {
		return "retry", u, fmt.Errorf("session: %w", err)
	}
	if sess == nil {
		if sess, err = a.store.EnsureSession(ctx, v.TeamID, v.Channel, v.ThreadTS, "channel", ""); err != nil {
			return "retry", u, fmt.Errorf("session: %w", err)
		}
	}
	rounds, wall := v.Rounds, time.Duration(v.Minutes)*time.Minute
	if rounds <= 0 || wall <= 0 { // a row from before the budget was stored, or a hand-written one
		st := a.settings.Get(ctx, v.OrgID)
		rounds, wall = investigationBudget(st)
	}
	requester := v.Requester
	if requester == "" {
		requester = sl.BotUserID
	}
	c := &Call{TeamID: v.TeamID, OrgID: v.OrgID, SL: sl, Channel: v.Channel, ThreadTS: v.ThreadTS, UserID: requester,
		Text: v.Question, Kind: sess.Kind, Session: sess, Brief: investigationBrief(v, rounds, wall),
		MaxRounds: rounds, Wall: wall, Streamer: sl.NewStreamer(v.Channel, v.ThreadTS, requester)}
	slog.Info("investigation running", "id", v.ID, "channel", v.Channel, "thread", v.ThreadTS, "rounds", rounds, "wall", wall)
	err = a.Run(ctx, c)
	u = c.usage
	status, err = investigationOutcome(ctx, c, err)
	if status == "failed" && !c.Streamer.Started() {
		// The thread has been waiting minutes for this, so it is told — a run that fails in
		// silence looks exactly like one that is still going.
		sl.PostText(detached(ctx), v.Channel, v.ThreadTS, "I couldn't finish looking into that: "+truncate(err.Error(), 300))
	}
	return status, u, err
}

// investigationOutcome reads what became of a run. The distinction that matters is between a
// question that was answered badly and one that was never answered at all: a container going
// away mid-run — a deploy, a lost write lease — decides nothing, so it is not recorded as a
// decision. The row stays claimable and the next container starts it again, which is the case
// the queue exists for. A stop is a person's decision and is final.
func investigationOutcome(ctx context.Context, c *Call, err error) (string, error) {
	if by, stopped := c.stoppedBy(); stopped {
		return "stopped", fmt.Errorf("stopped by %s", by)
	}
	if err == nil {
		return "done", nil
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return "retry", err
	}
	return "failed", err
}

// investigationBrief is what the lane puts in front of the model, after the thread it is working
// in. The thread carries the question as it was asked; this says what the run is and what an
// answer to it has to contain.
func investigationBrief(v *Investigation, rounds int, wall time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the background investigation this thread asked for, and you are answering now — nobody is waiting on a further handover.\n\n")
	fmt.Fprintf(&b, "The question: %s\n", v.Question)
	if v.Brief != "" {
		fmt.Fprintf(&b, "\nWhat the reply turn already had:\n%s\n", v.Brief)
	}
	fmt.Fprintf(&b, "\nYou have %d tool rounds and about %s. Use them: gather the evidence yourself rather than reasoning from what is above, "+
		"and check anything that decides the answer against a second source where you can.\n", rounds, wall)
	b.WriteString("\nPost one answer in this thread when you have it: what the cause is, the evidence it rests on (quote the log lines, ids and figures you actually saw), " +
		"and what you could not establish. If the evidence does not settle it, say so plainly and give what it does settle — a narrowed answer with its gaps named is worth more here than a confident one. " +
		"Name specific records, ids or documents where the question asks for them, and never invent one to fill the shape of an answer.")
	return b.String()
}

// closeAbandonedInvestigations retires runs that used their attempts without finishing, and tells
// the thread. A question the bot took on and then dropped in silence is the one failure of this
// design that a person cannot see for themselves.
func (a *Agent) closeAbandonedInvestigations(ctx context.Context) {
	vs, err := a.store.abandonedInvestigations(ctx)
	if err != nil || len(vs) == 0 {
		return
	}
	for _, v := range vs {
		why := fmt.Sprintf("gave up after %d interrupted attempts", v.Attempts)
		if err := a.store.finishInvestigation(ctx, v.ID, "failed", why, Usage{}); err != nil {
			slog.Warn("abandoned investigation not closed", "id", v.ID, "err", err)
			continue
		}
		slog.Error("investigation abandoned", "id", v.ID, "org", v.OrgID, "channel", v.Channel, "attempts", v.Attempts)
		if sl, err := a.slacks.For(ctx, v.TeamID); err == nil {
			sl.PostText(ctx, v.Channel, v.ThreadTS,
				"I had to give up looking into that one — the run was interrupted "+fmt.Sprint(v.Attempts)+" times. Ask again and I'll start over.")
		}
		a.alert(ctx, v.OrgID, fmt.Sprintf("investigation:%d", v.ID),
			fmt.Sprintf(":warning: Investigation #%d in <#%s> was abandoned after %d interrupted attempts.", v.ID, v.Channel, v.Attempts))
	}
}
