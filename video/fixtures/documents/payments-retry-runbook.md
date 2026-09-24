# Payments retry runbook

## Scope

Applies to the payments client on the checkout path, and nowhere else. The
on-call for payments owns this page; change it in a pull request, not in place.

## Retry budget

- Every request gets a total retry budget of 2.5 seconds, measured from the
  first attempt. There is no fixed retry count: attempts continue while the
  budget allows, and stop when it is spent.
- The budget is bounded from the caller's side, which is the side that cares.
  Do not raise it in production without a ticket — a bigger budget against a
  downstream that is already struggling is how a brownout becomes an outage.
- Backoff is exponential with jitter: 100 ms base, doubling, capped at 800 ms,
  with up to 50% random jitter on every wait so retries from many callers do
  not line up.

## When the budget is exhausted

- Do not fail closed. Put the charge on the delayed-charge queue and return a
  pending result to the caller. A delayed charge is recoverable; a dropped
  charge is a support ticket.
- The queue worker retries with the same idempotency key, so a charge that did
  in fact succeed downstream is never taken twice.
- Reconciliation has to know: every queued charge is written to the
  reconciliation ledger as pending, and the nightly reconciliation job clears
  it once the downstream confirms. Until the reconciliation change ships,
  exhausted-budget charges are queued and paged, so a person watches the queue.

## Monitoring

- Page when more than 0.5% of checkout calls exhaust their budget over five
  minutes.
- Page when the oldest item on the delayed-charge queue is over ten minutes old.
- Dashboards: attempts per request, budget exhaustion rate, queue depth and age.

## Deploys

- Deploys go out on Tuesdays only. Never on a Friday.
- Ship behind the flag payments.retry_budget and ramp 10%, 50%, 100% over the
  day, watching the exhaustion rate at each step before the next.

## Rollback

1. Turn payments.retry_budget off. The client falls back to the previous
   fixed-count behaviour at once; nothing needs redeploying.
2. Drain the delayed-charge queue before the flag goes back on, or the queued
   charges are retried under the old rules.
3. Note the rollback in the incident channel: the time, and the exhaustion
   rate that triggered it.
4. If the rollback happened after the nightly reconciliation window, run the
   reconciliation job by hand.
