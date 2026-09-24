package app

// Who finishes a delivery.
//
// The dispatcher used to mark a delivery done the moment b.route returned — but route only starts
// the turn, which then runs detached for up to four minutes. So the inbox recorded the work as
// complete before it had begun, and a shutdown in that window lost the message outright: Slack had
// already had its 200 so it would not retry, and the row said done so the dispatcher would not
// either.
//
// A delivery is therefore handed to whatever asynchronous work it starts. That work adopts it,
// keeps its lease alive while it runs, and closes it out at the end. Work that never starts —
// a bot's own message, a duplicate, an event nobody handles — leaves the delivery unadopted and
// the dispatcher closes it as before.

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type deliveryCtxKey struct{}

// deliveryHandoff is one claimed delivery, and the right to complete it.
type deliveryHandoff struct {
	store *Store
	d     *slackDelivery

	mu      sync.Mutex
	adopted bool
	stop    chan struct{}
	once    sync.Once
}

func withDelivery(ctx context.Context, h *deliveryHandoff) context.Context {
	return context.WithValue(ctx, deliveryCtxKey{}, h)
}

func deliveryFrom(ctx context.Context) *deliveryHandoff {
	h, _ := ctx.Value(deliveryCtxKey{}).(*deliveryHandoff)
	return h
}

// deliveryOwner names the delivery a piece of work came from, for the dedup in SeenEvent. Empty
// when there is none, which is what every non-inbox caller gets.
func deliveryOwner(ctx context.Context) string {
	if h := deliveryFrom(ctx); h != nil {
		return h.d.Key
	}
	return ""
}

// adoptDelivery transfers responsibility for finishing the delivery in ctx to the caller, which
// must call the returned func exactly once when its work ends. It is safe on a context with no
// delivery, and safe to call twice — only the first adoption takes effect, so one delivery can
// never be finished by two goroutines.
func adoptDelivery(ctx context.Context) func(error) {
	h := deliveryFrom(ctx)
	if h == nil {
		return func(error) {}
	}
	h.mu.Lock()
	if h.adopted {
		h.mu.Unlock()
		return func(error) {}
	}
	h.adopted, h.stop = true, make(chan struct{})
	h.mu.Unlock()
	go h.keepLeased()
	return h.finish
}

// owned reports whether some asynchronous work took the delivery, in which case the dispatcher
// must not close it.
func (h *deliveryHandoff) owned() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.adopted
}

// keepLeased holds the claim while the work runs. It renews on its own context: the dispatch
// context is cancelled as soon as the dispatcher returns, which is the whole point of the handoff.
func (h *deliveryHandoff) keepLeased() {
	t := time.NewTicker(slackDeliveryLease / 3)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		h.mu.Lock()
		err := h.store.touchSlackDelivery(ctx, h.d)
		h.mu.Unlock()
		cancel()
		if err != nil {
			// Either the row is gone or another dispatcher has it. Stop renewing; the work
			// continues, but it no longer owns the receipt.
			slog.Warn("Slack delivery lease not renewed", "team", h.d.Team, "err", err)
			return
		}
	}
}

// finish closes the delivery out. A nil error means the work is done and nothing should retry it;
// an error leaves the row to its normal retry, or dead-letters it once the attempts are spent.
func (h *deliveryHandoff) finish(workErr error) {
	h.once.Do(func() {
		close(h.stop)
		// Not the dispatch context: by now it is long cancelled, and during shutdown so is
		// everything derived from the signal. The receipt still has to be written.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.mu.Lock()
		defer h.mu.Unlock()
		if workErr != nil {
			if err := h.store.failSlackDelivery(ctx, h.d, workErr.Error()); err != nil {
				slog.Warn("Slack delivery failure record", "err", err)
			}
			return
		}
		if err := h.store.finishSlackDelivery(ctx, h.d); err != nil {
			slog.Warn("Slack delivery completion", "err", err)
		}
	})
}
