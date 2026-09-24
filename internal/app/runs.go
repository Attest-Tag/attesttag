package app

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A turn runs for as long as the model keeps calling tools, so people need a way to call one
// off: `!stop`, or a bare "stop" in the thread. Every Run registers here; stopping one cancels
// its context, which unwinds the model call and whatever tool is in flight, and the turn then
// closes its stream with a note instead of failing with an error.

type runHandle struct {
	id int64
	// The tenancy of the turn. A channel id is not unique across Slack — a Slack Connect
	// channel carries the same id in both workspaces — so matching on channel and thread alone
	// let a "!stop" in one organisation cancel another organisation's run.
	orgID    int64
	teamID   string
	channel  string
	threadTS string
	cancel   context.CancelFunc
	started  time.Time
	// A run in the digging lane. It is registered like any other so it can be stopped from the
	// thread, but it is deliberately not counted against the in-flight cap: the cap exists to
	// stop one organisation's burst taking the process, and a lane with a pool of its own is
	// already bounded. Counting them there would let one ten-minute question refuse every
	// one-line question in the workspace for as long as it ran.
	deep bool

	mu      sync.Mutex
	done    bool
	stopped bool
	by      string // user who stopped it
}

func (h *runHandle) stoppedBy() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.by, h.stopped
}

// stoppedBy reports whether this turn was called off, and by whom.
func (c *Call) stoppedBy() (string, bool) {
	if c.run == nil {
		return "", false
	}
	return c.run.stoppedBy()
}

// beginRun registers a turn and returns the context it runs under: cancelling that context is
// what stopping a run does. The returned func must be deferred; it deregisters the run.
func (a *Agent) beginRun(ctx context.Context, c *Call) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	h := &runHandle{id: a.runSeq.Add(1), orgID: c.OrgID, teamID: c.TeamID, channel: c.Channel, threadTS: c.ThreadTS,
		cancel: cancel, started: time.Now(), deep: c.MaxRounds > 0}
	a.runsMu.Lock()
	if a.runs == nil {
		a.runs = map[int64]*runHandle{}
	}
	a.runs[h.id] = h
	a.runsMu.Unlock()
	c.run = h
	// A stop asked for an hour ago must not kill the next thing somebody says in this thread.
	// Cleared as the turn begins rather than when the stop is served, because the instance that
	// served it is not necessarily the one that will run what comes next.
	if a.store != nil {
		a.store.ClearStopRequest(ctx, c.TeamID, c.Channel, c.ThreadTS)
	}
	return ctx, func() { a.endRun(h) }
}

// detached keeps a context's values but not its cancellation, for the Slack calls that have to
// land once a run is over — or once it has been stopped, which cancelled its context.
func detached(ctx context.Context) context.Context {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	go func() { <-ctx.Done(); cancel() }()
	return ctx
}

// endRun deregisters a finished (or stopped) run.
func (a *Agent) endRun(h *runHandle) {
	h.mu.Lock()
	h.done = true
	h.mu.Unlock()
	a.runsMu.Lock()
	delete(a.runs, h.id)
	a.runsMu.Unlock()
	h.cancel()
}

// StopRuns calls off every turn in flight in a thread and reports how many it stopped.
func (a *Agent) StopRuns(orgID int64, teamID, channel, threadTS, by string) int {
	return a.stopMatching(by, func(h *runHandle) bool {
		return h.orgID == orgID && h.teamID == teamID && h.channel == channel && h.threadTS == threadTS
	})
}

func (a *Agent) stopMatching(by string, match func(*runHandle) bool) int {
	a.runsMu.Lock()
	var hs []*runHandle
	for _, h := range a.runs {
		if match(h) {
			hs = append(hs, h)
		}
	}
	a.runsMu.Unlock()
	n := 0
	for _, h := range hs {
		h.mu.Lock()
		if !h.done && !h.stopped {
			h.stopped, h.by = true, by
			n++
		}
		h.mu.Unlock()
		h.cancel() // the turn unwinds and reports the stop in the thread
	}
	return n
}

// finishStopped closes a turn someone called off: the stream ends with a note instead of an
// error, the thinking status clears, and anything the turn was holding for confirmation is
// dropped — nobody should be asked to approve a write from a run they just stopped.
func (a *Agent) finishStopped(ctx context.Context, c *Call, by string) {
	// Nobody should be asked to approve a write from a run that was just called off — which goes
	// for the approver's DM as much as the in-thread card, so drop what was held for them too.
	c.pendingID, c.pendingGrants = 0, nil
	if a.store != nil {
		a.store.DiscardPendingWrites(ctx, c.TeamID, c.Channel, c.ThreadTS)
		if c.usage.In > 0 || c.usage.Out > 0 {
			a.store.LogUsageBy(ctx, c.OrgID, c.TeamID, c.Channel, c.ThreadTS, c.UserID, c.model, c.usage)
		}
	}
	if c.SL == nil {
		return
	}
	c.SL.SetStatus(ctx, c.Channel, c.ThreadTS, "")
	note := "\n\n_Stopped._"
	if by != "" {
		note = fmt.Sprintf("\n\n_Stopped by <@%s>._", by)
	}
	if _, err := c.Streamer.Stop(ctx, note, a.footer(ctx, c, c.model, c.usage)); err != nil {
		slog.Warn("closing a stopped run", "channel", c.Channel, "err", err)
	}
	var ran time.Duration
	if c.run != nil {
		ran = time.Since(c.run.started).Round(time.Second)
	}
	slog.Info("run stopped", "channel", c.Channel, "thread", c.ThreadTS, "by", by, "after", ran,
		"in", c.usage.In, "out", c.usage.Out)
}

// ---- asking for a stop in the thread ----

// stopWordRe matches a message that is nothing but "stop": anything longer is a question, not
// an interruption. It only counts while something is actually running (see stopRequested), so
// "cancel" still means the pending-write answer the rest of the time.
var stopWordRe = regexp.MustCompile(`(?i)^(stop|stop it|stop please|please stop|cancel|cancel that|abort|halt|never ?mind)[.!]*$`)

// stopRequested calls off the thread's running turns when someone types a bare "stop".
func (b *Bot) stopRequested(orgID int64, teamID, channel, threadTS, user, text string) bool {
	if !stopWordRe.MatchString(strings.TrimSpace(text)) {
		return false
	}
	return b.agent.stopThread(orgID, teamID, channel, threadTS, user) > 0
}

// stopThread is everything "stop" means in a thread, for the bare word and for `!stop` alike:
// they are the same request, and when `!stop` had its own shorter version it left a turn running on
// another instance, and a queued investigation to start minutes later, after it had said stopped.
// It returns how many things it stopped here; the request written for other instances is not
// counted, because nothing here can see whether one picked it up.
func (a *Agent) stopThread(orgID int64, teamID, channel, threadTS, user string) int {
	n := a.StopRuns(orgID, teamID, channel, threadTS, user)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// StopRuns only reaches the turns registered in this process. The turn somebody is asking
	// to stop may be running on another instance, which would otherwise go on answering a
	// question it has already been told to drop. The request is written to the session row and
	// the running instance reads it between tool rounds. Recorded even when this instance did
	// stop something: the same thread may have work in flight on both.
	a.store.RequestStop(ctx, teamID, channel, threadTS)
	if a.jobs != nil { // a fix job running in this thread stops too
		n += a.jobs.CancelInThread(ctx, orgID, teamID, channel, threadTS, user)
	}
	// An investigation that has not started yet is stopped in the queue: cancelling the turns in
	// flight would otherwise be followed, minutes later, by the run nobody wanted starting.
	n += a.store.StopQueuedInvestigations(ctx, orgID, teamID, channel, threadTS, user)
	return n
}

// inFlight counts the turns one organisation has running right now, for the per-organisation
// cap in allowed().
func (a *Agent) inFlight(orgID int64) int {
	a.runsMu.Lock()
	defer a.runsMu.Unlock()
	n := 0
	for _, h := range a.runs {
		if h.orgID == orgID && !h.deep {
			n++
		}
	}
	return n
}
