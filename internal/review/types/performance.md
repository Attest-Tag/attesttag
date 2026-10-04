# Performance review: what this change makes slower or dearer as the data and the traffic grow.
#
# Format: see general.md and ParseType in types.go.

key: performance
name: Performance
strictness: medium
inline: P2
purpose: Find where this change makes the system slower or more expensive as the data, the
traffic or the number of users grows: queries and remote calls repeated inside loops, work that
reads or builds everything when it needs a part, lists and loops with no upper bound, queries no
index serves, quadratic work on paths that run often, expensive work repeated where it could be
done once, and interfaces that redo work on every render. Report a problem only when you can say
what grows, where the code runs and roughly how the cost scales with it. Leave
micro-optimisations, and anything only a profiler could settle, to the team.

- [P1] A query, cache read or remote call made once per item inside a loop, a list being rendered
  or a serializer, where one batched query or call would do. Say what the loop runs over and how
  large it can get.
- [P1] Unbounded work on data that grows: a query with no limit or pagination, a whole table,
  file or response read into memory, a loop or retry with no upper bound, or a cache, queue or map
  that only ever grows.
- [P1; paths: **/migrations/**, **/migrate/**, **/*.sql, **/schema.*] Migrations: a foreign key or
  a column the code looks rows up by, added without an index; or an index built, a column added or
  a table rewritten in a way that locks a large table for as long as it runs.
- [P1] A new query whose filter, join or sort no index covers, on a table that grows with users or
  time, where the schema can be seen in this repository or a context repository.
- [P2] Quadratic or worse work on a path that runs often: nested loops over the same collection,
  a linear search or a membership check on a list inside a loop where a set or map would do, or
  sorting and copying inside a loop.
- [P2] Expensive work repeated where it could be done once: compiling a pattern, parsing
  configuration, building a client or a connection, or reading the same file or record on every
  call or request instead of once.
- [P2] Unnecessary re-renders in a user interface: a component handed a new object, array or
  function on every render, state or context that re-renders a whole tree for a small change, an
  effect whose dependencies change every time, or a long list rendered whole.
- [P2] Blocking work on a hot path: synchronous network, disk or CPU-heavy work inside a request
  handler, an event loop or the interface's main thread, where it could run in the background, be
  cached or be streamed.
- [P2] Reading or sending more than is used: every column or field selected when a few are read,
  related records loaded that nothing uses, or whole objects sent to a client that shows a summary.
