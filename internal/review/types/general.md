# General review: the type that runs when nothing else is chosen.
#
# Format (see ParseType in types.go): a header of "key: value" lines, a purpose paragraph,
# then one rule per "- [P0|P1|P2] text" item, the bracket holding the most severe a finding
# citing the rule may be. Lines starting with "#" are comments.

key: general
name: General
strictness: medium
inline: P2
purpose: Find what will go wrong when this change runs: logic that does not do what the code
around it says it should, contracts with other code that it breaks, data it can lose or
corrupt, races, and new behaviour that nothing tests. Report a problem only when you can name
the input or the sequence of events that triggers it and what happens then. Leave style,
formatting and anything a compiler, type checker or linter already catches to those tools.

- [P0] Data loss or corruption on a reachable path: a write that drops, truncates or
  overwrites data the caller meant to keep, a delete or update whose condition can match more
  rows than intended, or a migration that discards columns or rows without carrying them over.
- [P0] A crash, hang or outage on a reachable path: a null or nil dereference, an index out
  of range, an unhandled exception or panic in a request handler or worker, unbounded
  recursion, a deadlock, or a loop that cannot end.
- [P1] Logic that does not do what its name, its comment, its callers or its tests say it
  does: an inverted condition, an off-by-one bound, the wrong variable or operator, a missing
  case, or an early return that skips work the function promises.
- [P1] A broken contract: a changed signature, return value, API response, database schema,
  event or message payload, config key or command-line flag whose other users, in this
  repository or a context repository, were not updated to match.
- [P1] An error that is swallowed, logged and ignored, or turned into success when the caller
  needs to know about it: a failed write reported as saved, a partial result returned as
  complete, a retry loop that hides the last failure.
- [P1] Concurrency: shared state read and written without the lock or atomic the rest of the
  code uses, a check-then-act race, background work that outlives its owner or is never
  awaited, or results applied out of order.
- [P1] Resources: a file, connection, transaction, lock, timer or subscription that is not
  closed or released on every path, the early returns and error paths included.
- [P1] Inputs the new code will receive and does not handle: empty collections, zero,
  negative numbers, missing optional fields, duplicates, time zones and daylight saving,
  non-ASCII text, very large inputs.
- [P1] Work that can run twice and is not safe to: a retried request, a redelivered webhook or
  queue message, a form submitted twice, two instances of a scheduled job running at once.
- [P1] Caching and state: a cache that is not invalidated when its source changes, a cache key
  that leaves out something the value depends on, or state kept in memory that a restart or a
  second instance would lose or split.
- [P2] New logic with branches no test exercises, where the code around it is tested. Name the
  case that is not covered rather than asking for "more tests".
- [P2] A test that cannot fail: it asserts nothing, asserts against the mock it set up, or
  catches the very error it is meant to check.
- [P2] Work that grows with the data where it need not: a query or remote call inside a loop,
  a whole table or file read into memory, quadratic work over a list that grows with users.
- [P2] A convention stated in the repository's instruction files or the team's instructions,
  when the change breaks it. Say where the convention is written.
- [P2] Dead or misleading code this change adds: a branch that can never run, a parameter that
  is never read, a flag that is always on, or a comment that now says the opposite of what
  the code does.
