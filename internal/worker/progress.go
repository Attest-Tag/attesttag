package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"attesttag/internal/app"
)

// Reporter is the event pipe to the bot: ordered, sequenced, scrubbed, size-capped, sent from
// one goroutine so the pipeline never blocks on the network. A heartbeat goes out when nothing
// else has for a while, and a cancel in any reply ends the job's context.

var errCancelled = errors.New("cancelled by the bot")

type Reporter struct {
	client *Client
	scrub  *Scrubber
	cancel context.CancelCauseFunc

	mu      sync.Mutex
	seq     int64
	queue   []app.JobEvent
	usage   app.JobUsage
	wake    chan struct{}
	stopped bool

	cancelled atomic.Bool
	reason    atomic.Value // string
	lastSend  atomic.Int64 // unix seconds
	inflight  atomic.Bool  // a batch has left the queue but not yet reached the bot
	drained   chan struct{}
}

func newReporter(client *Client, scrub *Scrubber, cancel context.CancelCauseFunc) *Reporter {
	r := &Reporter{client: client, scrub: scrub, cancel: cancel, wake: make(chan struct{}, 1), drained: make(chan struct{}, 1)}
	r.reason.Store("")
	return r
}

func (r *Reporter) push(e app.JobEvent) {
	r.mu.Lock()
	r.seq++
	e.Seq = r.seq
	e.At = time.Now().UTC().Format(time.RFC3339)
	if e.Message != "" {
		e.Message = cut(r.scrub.Clean(e.Message), app.JobEventMaxBytes-64)
	}
	if len(r.queue) < 512 {
		r.queue = append(r.queue, e)
	}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Reporter) Phase(phase, status, text string) {
	slog.Info("phase", "phase", phase, "status", status, "text", cut(text, 200))
	r.push(app.JobEvent{Kind: app.JobKindPhase, Phase: phase, Status: status, Message: text})
}
func (r *Reporter) Log(text string) {
	slog.Info(cut(text, 500))
	r.push(app.JobEvent{Kind: app.JobKindLog, Message: text})
}
func (r *Reporter) Tests(text string) { r.push(app.JobEvent{Kind: app.JobKindTests, Message: text}) }
func (r *Reporter) Warn(text string) {
	slog.Warn(cut(text, 500))
	r.push(app.JobEvent{Kind: app.JobKindWarn, Message: text})
}

// Usage records a delta of model spend and tells the bot.
func (r *Reporter) Usage(u app.JobUsage) {
	if u.In == 0 && u.Out == 0 && u.CostUSD == 0 {
		return
	}
	r.mu.Lock()
	r.usage.In += u.In
	r.usage.Out += u.Out
	r.usage.CostUSD += u.CostUSD
	r.mu.Unlock()
	r.push(app.JobEvent{Kind: app.JobKindUsage, Usage: &u})
}

func (r *Reporter) Total() app.JobUsage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.usage
}

// NextSeq is the sequence number the result should carry.
func (r *Reporter) NextSeq() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return r.seq
}

func (r *Reporter) Cancelled() (bool, string) {
	return r.cancelled.Load(), r.reason.Load().(string)
}

func (r *Reporter) take() []app.JobEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.queue)
	if n > app.JobEventsBatchMax {
		n = app.JobEventsBatchMax
	}
	batch := append([]app.JobEvent{}, r.queue[:n]...)
	r.queue = r.queue[n:]
	return batch
}

func (r *Reporter) pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queue)
}

// run sends batches as they queue up and a heartbeat every 20 s of silence. It keeps going
// after the job context is cancelled (the final events still matter) until Stop.
func (r *Reporter) run(ctx context.Context) {
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.wake:
		case <-tick.C:
			if time.Since(time.Unix(r.lastSend.Load(), 0)) >= 20*time.Second {
				r.push(app.JobEvent{Kind: app.JobKindHeartbeat})
			}
		}
		for r.pending() > 0 {
			r.inflight.Store(true)
			batch := r.take()
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
			resp, err := r.client.Events(sctx, batch)
			cancel()
			r.inflight.Store(false)
			r.lastSend.Store(time.Now().Unix())
			if err != nil {
				slog.Warn("events not delivered", "count", len(batch), "err", err)
				if isFatal(err) { // the bot no longer wants them (job finished or token revoked)
					r.mu.Lock()
					r.stopped = true
					r.queue = nil
					r.mu.Unlock()
				}
				continue
			}
			if resp.Cancel && !r.cancelled.Load() {
				r.cancelled.Store(true)
				r.reason.Store(resp.Reason)
				slog.Warn("cancel requested by the bot", "reason", resp.Reason)
				r.cancel(errCancelled)
			}
		}
		select {
		case r.drained <- struct{}{}:
		default:
		}
		r.mu.Lock()
		stopped := r.stopped
		r.mu.Unlock()
		if stopped {
			return
		}
	}
}

// Flush waits until queued and in-flight events are sent or the timeout passes, so the result
// never overtakes the last progress line.
func (r *Reporter) Flush(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for (r.pending() > 0 || r.inflight.Load()) && time.Now().Before(deadline) {
		select {
		case r.wake <- struct{}{}:
		default:
		}
		select {
		case <-r.drained:
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Stop ends the sender after a final flush.
func (r *Reporter) Stop() {
	r.Flush(10 * time.Second)
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
