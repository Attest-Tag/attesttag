# Building and running from source

The container images are the short way in ([`deploy/README.md`](../deploy/README.md)).
This is the long way: a Go toolchain, a Node toolchain, and the binary in your hands.

Requirements: Go 1.26 or newer, Node 22 (to build the console), and `pdftotext` from poppler
if you want PDF support (`brew install poppler` on macOS, `apt install poppler-utils` on Debian).
You also need a Slack app — [`slack-app.md`](slack-app.md) — a public HTTPS address for Slack to
reach it on ([`deploy/docs/https.md`](../deploy/docs/https.md)), and a model key.

## 1. Get a model key

An OpenRouter key works out of the box. Any other OpenAI-compatible endpoint works by setting
`LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODEL`, `HEAVY_MODEL` and `EMBED_MODEL` — the defaults of all
three models are OpenRouter ids. The provider must support streaming and tool calling for the chat
model, and `/v1/embeddings` for the embedding model.

## 2. Build and run

```bash
cp .env.example .env.testing  # fill in SLACK_SIGNING_SECRET, SLACK_CLIENT_ID/SECRET, OPENROUTER_API_KEY; set DB_PATH=testing.db
make build                    # builds the console (ui/out), then the Go binary that embeds it
./attesttag
```

The binary reads the dotenv file `ENV_FILE` names, else `.env.testing` when that file exists,
else `.env`; every `.env*` but `.env.example` is gitignored. Keep development in `.env.testing`,
with a Slack app of its own and its own `DB_PATH`, so a local run can never answer in a workspace
people rely on or write to the database a real deployment uses — nothing defaults to `testing.db`,
so set it. (`deploy/gcp/cloudrun.sh` reads `.env.prod` when there is one, which is the maintainer's
hosted service; a self-host deploys with `ENV_FILE=.env`. See [Deploy](deploy.md#the-env-file).)

Build the console before the binary, every time you change it: it is embedded with `//go:embed`,
and a tree that has never built it either fails to compile or, with only the page shell the
repository keeps in `ui/out`, builds a binary whose console does not load. `make build` does both;
`make ui` (or `cd ui && npm ci && npm run build`) is the console alone. Then:

- Point the Slack app's two Request URLs at your HTTPS address, and set `ADMIN_BASE_URL` to it.
- Open `/admin/` on that address, sign up, and name your organisation. The first sign-up founds
  it; after that people join by invitation. Connect the workspace on the Workspaces page.
- Invite the bot to a channel and mention it.
- `curl http://127.0.0.1:8080/health` answers `ok`. With `HEALTH_SECRET` set, send it as an
  `X-Health-Secret` header (or `?secret=`, which ends up in access logs) and it also reports the
  writer state, the workspace count and the month's spend.

On first run without a `MASTER_KEY`, a bare binary generates one and appends it to the dotenv
file it read. Back it up: it unlocks every stored connection credential. (The container image
refuses to start without one instead.)

### `make run` and `start_dev.sh`

`make run` starts the binary with `SELF_TEST=1` on port 8090 and `ADMIN_BASE_URL` pointing at
it, which is the setup the live evals expect. `./start_dev.sh` rebuilds the console (when
`ui/src` or `ui/package.json` changed) and the binary, stops any `attesttag` already listening on
the port, and runs it the same way except that it leaves `ADMIN_BASE_URL` unset. It reads
`.env.testing` — create that first, or it goes looking for a `.env.prod` to copy.
`PORT=8092 ./start_dev.sh` picks another port, and `FRESH_DB=1 ./start_dev.sh` deletes whichever
file `DB_PATH` names — `attesttag.db` when the env file sets none — so the run starts empty.

## Tests and evals

```bash
make test                                       # go vet + unit tests (everything except TestEvals)
make test-deploy                                # the AWS, Azure and Google Cloud deploy scripts against fake CLIs (python3, shellcheck)
make run                                        # start the bot with SELF_TEST=1 on :8090
./start_dev.sh                                  # same, but rebuilds the console and binary first
make evals                                      # live evals from evals/cases.json against it
```

- **Unit tests** cover chunking, redaction, thinking-tag stripping, forced-tool detection, host
  matching, PKCE, the MCP read-only heuristic, the footer, routine scheduling, artifact formats,
  allow-rule parsing, inherited scope access, repository parsing, and the `!whoami` report — and
  the guard tests, which fail the build on a whole class of mistake
  ([`CONTRIBUTING.md`](../CONTRIBUTING.md)). The fix-job tests run the bot side end to end (the
  tool holds a brief, Confirm dispatches through a fake dispatcher, the worker API's claim,
  events, diff and result with a fake Slack, the reconciler's stale and timeout verdicts) and
  the worker side offline: a real clone, edit, commit and push against a local bare repository,
  a fake bot and a fake GitHub, with the fake engine. They need `git` and `make` on `PATH`.
- **Postgres.** The same suite runs against Postgres when `TEST_DATABASE_URL` names an empty
  database (`createdb attesttag_test`, then
  `TEST_DATABASE_URL="postgres://$(whoami)@localhost:5432/attesttag_test?sslmode=disable" go test ./...`).
  Anything that touches SQL should pass on both; a new migration is two files under the same
  number, one per dialect.

### Self test, live tests, evals and CI

- **Self test** (`SELF_TEST=1`) makes the bot answer its own messages tagged `[selftest]`, so
  you can drive it with curl or the evals without a second Slack account.
- **Opt-in live tests** stay skipped unless you give them a key: `ALLOW_LIVE=1` runs the
  allow-rule checker against the configured model, `LLM_LIVE=1` sends one real completion
  (`LLM_LIVE_MODEL` to check the heavy model too), and `GITHUB_LIVE_TOKEN=…` walks the real
  repository listing.
- **Live evals** post each case in `evals/cases.json` to the eval channel, wait for the reply,
  then check the `tool_calls` table and the reply text against the expected tools and regex.
  The `connection_*` cases need a bundle with a bearer connection for `httpbin.org` attached
  to the eval channel and are skipped otherwise. See [`evals/README.md`](../evals/README.md).
- **CI** is two workflows, both started by hand (`workflow_dispatch`) for now and neither run on
  a push or a pull request: `ci.yml` runs the suite against SQLite and Postgres, type-checks and
  builds the console, and builds the image for both architectures; `evals.yml` runs the suite and
  the Go vulnerability scan. Run `make test` yourself before opening a pull request. Live evals
  run by hand on a staging host with a configured HTTPS delivery URL and an installed workspace;
  an ephemeral GitHub runner has neither that ingress nor the staging install database. See
  `evals/README.md`. Never point live evals at production.
