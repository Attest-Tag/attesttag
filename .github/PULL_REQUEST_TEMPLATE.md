<!--
A sentence on what changed, then a paragraph on what was wrong before and why this is the fix.
CONTRIBUTING.md has an example of the shape. A one-line message is fine for a one-line change.
-->

## Checks

- [ ] `make test` passes (CI does not run on pull requests yet, so this is on you)
- [ ] It also passes against Postgres (`TEST_DATABASE_URL`), if this touches SQL
- [ ] `cd ui && npx tsc --noEmit` passes, if this touches the console
- [ ] A bug fix comes with a test that fails without it
- [ ] A page in `guide/` that describes the behaviour changed with it
