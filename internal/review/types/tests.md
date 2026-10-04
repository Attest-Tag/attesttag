# Tests review: whether the change is tested, and whether its tests could catch anything.
#
# Format: see general.md and ParseType in types.go.

key: tests
name: Tests
strictness: medium
inline: P2
purpose: Find where the tests in this change would let a bug through: new or changed behaviour
that no test exercises, tests that pass whatever the code does, assertions that were removed or
weakened, cases that are only ever tried with input that succeeds, and tests whose result
depends on the clock, the order things run in or the machine they run on. Judge a test by what
would make it fail. Name the case that is not covered, or the change to the code a test would not
notice, rather than asking for more tests in general. Leave coverage figures, test style and
naming to the team's own tools.

- [P2] New or changed branches with no test that reaches them, where the code around them is
  tested: a new condition, an error path, a fallback or an early return. Name the input that
  would take the branch.
- [P1] A test or an assertion removed, skipped or weakened while the behaviour it checked is still
  in the code: an exact comparison loosened to a partial one, a specific error accepted as any
  error, a check deleted or a test marked skip. Say what can now break without failing anything.
- [P1] An expected value edited to match what the code returns now, where the old value was the
  right one by the function's name, its comment, its callers or the rest of its tests: the test
  was changed to agree with a bug.
- [P2] A test that cannot fail: it asserts nothing, asserts on the value it has just set up or on
  its own mock, compares a value with itself, or catches and ignores the error it is meant to check.
- [P2] Missing negative cases: the change adds validation, a permission check, a limit or error
  handling, and only input that succeeds is tested. Name the input that should be refused.
- [P2] A test whose result depends on time, order or environment: the current date or time zone,
  a sleep used to wait for other work, the iteration order of a map or set, concurrent work
  finishing in a particular order, random values without a fixed seed, or the network.
- [P2] Tests that share state: one test relying on data, files, globals or environment variables
  that another test left behind, or changing them without putting them back, so the result
  depends on which tests ran first.
- [P2] A mock or fake that no longer matches what it stands in for: the change altered the real
  function's signature, errors or response shape, and the test still passes against the old one.
- [P2] Test data that cannot show the bug: one element where the code treats several differently,
  values that are all equal, or zero and empty strings where the code under test treats a boundary
  differently.
