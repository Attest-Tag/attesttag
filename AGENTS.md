# AGENTS.md

Instructions for coding agents — Claude Code, Codex, Cursor, Aider, Copilot CLI and anything
else that reads a repository and runs shell commands.

## If you were asked to install or deploy this

Follow [`guide/install-with-an-agent.md`](guide/install-with-an-agent.md). It is a runbook
written for you: preflight, secrets, the public origin, the Slack app, one deploy command per
platform, and how to verify. Do not improvise an install from the other documentation — the
ordering matters and the runbook is the only place it is written down.

The short version of the rules it gives you, because they are the ones worth failing loudly on:

- **Never print, echo, log or commit a secret.** `.env*` is gitignored. Write a value in by
  replacing its line in `.env` — the template already holds an empty line for each key, and the
  cloud deploy scripts read the first one they find, so an appended duplicate is ignored there —
  and never read it back to the user.
- **`MASTER_KEY` is unrecoverable.** It seals every stored credential. Never regenerate one for
  a deployment that has data, and stop until the user confirms they have backed it up.
- **Never create a billable resource** without showing the user the list and getting a yes.
- **Deploy only to the project or account the user named**, not whatever the CLI has selected.
- **`deploy/gcp/cloudrun.sh` and `deploy/plan.sh` read `.env.prod` when it exists**, which is the
  maintainer's hosted service, else `.env`. A self-host always passes `ENV_FILE=.env`, and never
  sets `HOSTED=1`: that switches on the hosted service's public policy, open signup among it.
  Without it every deployment is one organisation.

## If you were asked to change the code

```bash
make build   # the console (make ui), then the binary. The console is embedded with //go:embed:
             # a tree that has never built it either fails to compile or serves a blank console
make test    # go vet ./... && go test -skip TestEvals ./...
```

`make test` is the gate. `TestEvals` is skipped because it costs money and needs a running bot
(`make evals` runs it deliberately). CI (`.github/workflows/ci.yml`) runs it on every pull request, but run it yourself first;
anything that touches SQL should also pass against Postgres (`TEST_DATABASE_URL`, see
[`CONTRIBUTING.md`](CONTRIBUTING.md)).

Worth knowing before you touch things:

- **The guard tests are load-bearing.** `TestEveryPerOrgQueryIsScoped` is what makes open signup
  safe; `TestManifestAsksForTheScopesTheCodeAsksFor` keeps the Slack manifest honest against the
  code. If one fails, the test is right and the change is wrong until proven otherwise.
- **Changing a default in `internal/app/config.go` is not the whole change.** A default the
  hosted service relies on is also set in `deploy/gcp/cloudrun.sh`; changing one silently
  diverges the two. Check both.
- **An applied migration must never be edited.** `internal/app/migrations.go` checksums them and
  refuses to boot if one changed. Add a new migration instead — one file per dialect, under the
  same number, in `internal/app/migrations/sqlite/` and `internal/app/migrations/postgres/`.
- **The guide is part of the product.** `guide/*.md` is embedded into the binary and quoted by
  the console assistant, so a change to behaviour a page describes changes that page too.
- **House style for comments and commits** is in [`CONTRIBUTING.md`](CONTRIBUTING.md) — comments
  say *why*, and commit subjects are sentences about behaviour, not labels.

## Where things are

| | |
|---|---|
| `cmd/`, `internal/app/` | the bot, the console API, storage, the proxy |
| `internal/worker/` | the fix-job worker that clones a repo and opens a PR |
| `internal/sandbox/` | the QuickJS-in-WebAssembly sandbox behind the `run_js` tool |
| `ui/` | the admin console (Next.js static export, embedded by `ui/embed.go`) |
| `deploy/` | one folder per platform: `local/`, `gcp/`, `aws/`, `azure/`, `helm/` |
| `guide/` | the documentation |
| `docs/` | **not** documentation — the bot's own document corpus, indexed and answered from |

[`guide/architecture.md`](guide/architecture.md) is the file-by-file map.
