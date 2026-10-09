# Concurrency and state review: what breaks when things happen at once, or in another order.
#
# Format: see general.md and ParseType in types.go. Each "auto:" line is a regular expression
# (RE2); together they are one pattern over the changed lines and the changed paths. A review
# whose types a branch rule chose runs this type as well wherever the diff matches it.

key: concurrency
name: Concurrency and state
strictness: medium
inline: P2
auto: \b(?:async|await)\b|\bnew Promise\b|Promise\.(?:all|allSettled|race|any)\b|\.then\(|\bset(?:Timeout|Interval)\b|\bAbortController\b
auto: (?i:\b(?:debounce|throttle))|\buse(?:Effect|LayoutEffect|Ref|Callback|Query|Mutation)\b|\bcreateAsyncThunk\b
auto: \b(?:takeLatest|takeEvery|takeLeading|createLogic)\b|\bEventSource\b|(?i:web_?socket|pub_?sub)
auto: \basyncio\b|\b(?:gather|create_task|ensure_future|run_in_executor)\(|\b(?:R?Lock|Semaphore|BoundedSemaphore)\(
auto: \bthreading\b|\bmultiprocessing\b|\bconcurrent\.futures\b|\bcelery\b|\bsynchronized\b|Executor|\bCompletableFuture\b
auto: \bgo func\b|\bchan\b|\bsync\.|\b[Aa]tomic|(?i:mutex)|\bWaitGroup\b|\berrgroup\b|\bctx\.Done\(\)
auto: (?i:\b(?:worker|queue|retr(?:y|ies|ying)|backoff|(?:un)?subscri(?:be|ption)|saga))
purpose: Find what goes wrong when things happen at the same time, or in another order than the
code assumes: two requests in flight for the same thing, a response that arrives after the person
has moved on, a timer or a subscription that outlives what it was for, a lock that does not cover
what it guards, a task cancelled halfway, a retry that runs work twice, and a check whose answer is
stale by the time it is acted on. For each problem, name the two things that interleave, the order
that breaks it, and what the person or the data is left with. Code that runs one step after
another with nothing in between is not this review's business, and neither is style.

- [P1] A flag, spinner, "saving" or "dirty" marker shared by several requests, so the first to
  finish clears or sets it while a later request for the same thing is still pending, and the
  interface says saved or idle when it is not.
- [P1] Responses applied out of order: a later request can finish first and an earlier, slower
  response then overwrites newer data, because nothing ties a response to the request that is
  still current (a sequence number, the latest request's id, aborting the one before).
- [P1] A value read, or state written, after an await, a timer or a callback, when navigation, a
  switch of document or account, an unmount or a cancelled request may have changed or cleared it
  in between.
- [P1] A map, cache, queue, ref or store keyed by an id that is unique only within a parent (field
  12 of two documents, row 3 of two tables), used where entries of more than one parent can meet.
- [P1] Effects and refs: an effect that reads something missing from its dependencies, or starts a
  subscription, listener, timer or request it never cleans up; a callback closing over stale state
  or props; a ref holding state the interface should react to.
- [P1] Timers, debounce and throttle: delayed work dropped, or run against the wrong item, when the
  person navigates, closes the page or switches item before it fires; a pending flush not run, or
  run twice, on unmount or unload.
- [P1] Optimistic updates not rolled back when the request fails, rolled back over a newer change,
  or applied twice when the server's answer arrives as well.
- [P1] Polling replaced by pushed events (websockets, server-sent events, pub/sub): clients or
  states that stop getting updates, events that arrive during the first load or before a listener
  is attached and are lost or applied to stale data, a reconnect that misses or replays events.
- [P1] Action handlers, logics and sagas: every request handled where only the latest should be,
  or none cancelled; one that keeps running after the screen it served is gone; the result of an
  earlier request applied after a later one.
- [P0] Shared state read and written by more than one goroutine, thread, task or request without
  the lock or atomic the rest of the code uses, or a lock that guards the write and not the read,
  or the other way round.
- [P0] Deadlocks and lost wakeups: locks taken in a different order on two paths, a lock held across
  an await, a blocking call or a call back into code that takes it again, a channel operation
  nothing will ever match, a wait for a signal that was sent before the wait began.
- [P1] Cancellation: code that catches BaseException, CancelledError or everything and carries on,
  so a cancelled task never stops; a context or abort signal not passed to the work it should stop;
  cleanup skipped when a task is cancelled; a task or goroutine started with no way to stop it.
- [P1] Background work never awaited or joined: a task, promise or goroutine whose failure nobody
  sees, which outlives the request or the shutdown, or whose result is read before it is done.
- [P1] A retry around work that is not safe to run twice: it resets counters or state, sends a
  message, charges, or writes a row; or a retry on errors that are not transient, or with no limit
  or backoff.
- [P1] try, except and finally (or defer and recover) restructured so an exception type that used to
  end in one final state (failed, released, rolled back) now ends in another: marked done, left
  locked, left processing for ever. Say which exception and which state.
- [P1] Queues and workers: a message acknowledged before its work is done, or never on failure; a
  job two workers can claim at once; work that is not idempotent on a queue that may deliver twice;
  an order relied on that the queue does not guarantee.
- [P1] Check-then-act: a decision made from one read and acted on through another, such as allowing
  one record and then changing a record looked up again, or checking a name is free and then
  inserting it, with no transaction, lock, unique constraint or conditional write holding the two
  together.
- [P1] One thing resolved twice, once to check and once to use (an id, a path, a user, a record),
  where the two can disagree because something changed in between or because the two lookups follow
  different rules: case, tenant, deleted rows.
- [P2] Unbounded fan-out over input that grows (Promise.all, gather, a goroutine or task per item)
  with no limit on how many run at once; a semaphore, pool slot or connection released on the
  success path only.
- [P2] An await, lock, network call or channel operation with no timeout where the code around it
  has one, so one slow dependency holds a worker, a connection or a lock indefinitely.
