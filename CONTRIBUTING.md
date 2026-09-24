# Contributing

Thanks for looking. This is a small project and the bar for a useful contribution is lower
than you might think — a corrected sentence in the README is worth having.

## Getting it running

You need **Go 1.25.13** or newer, **Node 22**, and `pdftotext` (from poppler) on your PATH for
PDF indexing. Then:

```bash
make build        # make ui, then go build -o attesttag ./cmd/attesttag
```

The console first, always: it is a Next.js static export embedded into the binary with
`//go:embed`. With nothing in `ui/out`, `go build` fails with `pattern all:out: no matching files
found`, which is not obvious the first time; with only the page shell the repository keeps there
(`ui/out/index.html`, so that a fresh clone compiles), it builds a binary whose console does not
load. `make ui` on its own rebuilds just the console.

**Building the container needs more than the default Docker memory.** Linking the binary
takes over 2 GB, and Docker Desktop ships with a small VM — the symptom is
`compile: signal: killed` and `ResourceExhausted: cannot allocate memory`, which reads like a
broken Dockerfile and is not one. Give Docker 6 GB or more in its Resources settings. You do
not need this to work on the code: `go build` on the host is unaffected, and cross-compiling
for another architecture is just `GOOS=linux GOARCH=arm64 go build` — the binary is
`CGO_ENABLED=0` with pure-Go SQLite, so it needs no toolchain and no emulation.

To actually talk to Slack you need a Slack app of your own and a public HTTPS URL — Slack will
not deliver events to `localhost`. [`guide/slack-app.md`](guide/slack-app.md) covers both, and
`BASE_URL=https://<your address> ./deploy/slack/manifest.sh` prints the app definition to paste
in, filled in from [`deploy/slack/manifest.json`](deploy/slack/manifest.json).
[`guide/development.md`](guide/development.md) is the longer version of all of this.

## Tests

```bash
make test          # go vet + the suite, minus the live evals
go test ./...      # the same; TestEvals gates itself on an -eval flag
make test-deploy   # the AWS, Azure and Google Cloud deploy scripts against fake CLIs (python3, shellcheck)
```

The suite runs offline with no credentials. Tests that need a real service skip themselves
unless you set the variable that turns them on — `LLM_LIVE`, `GITHUB_LIVE_TOKEN`,
`LEASE_LIVE_BUCKET`, `ATTEST_TOOLCHAIN_LIVE` and friends. If a test needs the network to pass,
it is a broken test.

The same suite runs against Postgres when you point it at an empty database, and anything that
touches SQL should pass on both:

```bash
createdb attesttag_test
TEST_DATABASE_URL="postgres://$(whoami)@localhost:5432/attesttag_test?sslmode=disable" go test ./...
```

CI does not run on pull requests for now — the workflows are started by hand — so run these
yourself before you open one.

A schema change is a new migration, written twice under the same number:
`internal/app/migrations/sqlite/NNNN_name.sql` and `internal/app/migrations/postgres/NNNN_name.sql`.
A migration that has been applied is never edited — they are checksummed, and a deployment
refuses to boot on one that changed.

Some of the suite is made of **guard tests**, which fail the build on a class of mistake rather
than on a specific bug. `TestEveryPerOrgQueryIsScoped` reads every SQL literal in the package
and fails if a statement touching a tenant's table has no organisation in its predicate.
`TestEveryTableIsClassified` insists each table is accounted for. If one of these fails, it has
almost certainly found something real; read what it says before working around it.

## The house style

The code is commented more heavily than most, and in a particular way: comments say **why**,
not what. A comment that restates the line below it is noise, but the reason a timeout is 9
seconds, or why a lease is released before a shutdown completes, is the thing a reader cannot
recover from the code. Match the surrounding density.

The same goes for commit messages. They are prose — a sentence saying what changed, then a
paragraph or two on what was wrong before and why this is the fix. The subject is a sentence
about behaviour, in lower case, not a label like "fix proxy bug". For example:

```
a write to a credential-less domain waits for Confirm like any other write, where it went straight through because nothing of ours was being spent

needsConfirm answered false for any request matching a domain that has no connection, on the
reasoning that no credential was spent. The gate's promise is about writes, though: a POST, PUT
or DELETE to such a host still changes something on the far side. A domain write is now held
unless it is a read travelling as a POST, and reads still go straight through.
```

A one-line commit message is fine for a one-line change.

The pages in [`guide/`](guide/README.md) are embedded into the binary and quoted by the console's
assistant when somebody asks how to set something up, so a change to behaviour they describe
changes the page in the same pull request.

## Pull requests

Small and single-purpose travels fastest. Please:

- run `make test` and, if you touched `ui/`, `cd ui && npx tsc --noEmit`
- add a test when you fix a bug — the test is how the fix stays fixed
- say in the description what you saw go wrong, not only what you changed

For anything large, open an issue first and let's agree on the shape before you spend a weekend
on it. That is for your benefit, not the project's.

## Security

Do not open a public issue for a vulnerability. [`SECURITY.md`](SECURITY.md) says where to send
it.
