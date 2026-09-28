# Fix jobs

Say *fix this and raise a PR* in a thread where the bot has already worked out what is wrong,
and it calls `start_fix_job` with a brief: what to change, the evidence it gathered (log lines,
stack traces, ticket, the recent tool results from the thread), and what done looks like. The
bot never runs code itself. The brief is held like any other write, with a Confirm card that
states the repository, the base branch, the change, and the rule the worker runs under: it may
push one new branch and open one draft pull request, and never merges or pushes to the base
branch. With `worker_allow_rules` on, an allow rule may start one without Confirm — never on a turn a
forwarded email started, where the job always waits for a named approver. Before anybody is asked,
a job is refused when another is still running in the thread, when `worker_max_jobs` are already
running, when the account's monthly budget, the channel's, or its credit has less left than
`worker_job_budget_usd`, or when there is no model credential it may hand the worker (below).
Confirm starts a **job** in a separate container:

### Worker platforms

`WORKER_MODE=workers` is the whole setting. The bot works out which platform it is on from the
environment that platform gives it, and says which at boot:

| resolves to | the container is | set up by |
|---|---|---|
| `k8s` | a `batch/v1` Job in the bot's own namespace | `helm --set worker.enabled=true` |
| `ecs` | a Fargate task | `deploy/aws/worker.sh` |
| `aca` | an Azure Container Apps job execution | `deploy/azure/worker.sh` |
| `cloudrun` | a Cloud Run Job execution | `deploy/gcp/worker.sh` |
| `docker` | a container on the host's Docker daemon | `deploy/local/worker.yml` |

Naming one of those directly pins it instead, for somewhere detection would be wrong.
`WORKER_MODE=local` is the development mode: a subprocess of this binary, `attesttag worker`.

Which one it is changes nothing below: every platform starts the same image with the same four
variables, and the worker cannot tell the difference beyond the value it was handed. On
`cloudrun`, `ecs`, `aca` and `k8s` the bot refuses to start a job until its own public origin is
https, because the worker calls back to it with the job's token in a header.
[`deploy/docs/platforms.md#the-fix-job-worker`](../deploy/docs/platforms.md#the-fix-job-worker)
has what each platform grants the bot and the worker.

### On the organisation's own model key

An organisation on its own model key ([plans.md](plans.md#its-own-model-key)) runs its jobs on that
key and its endpoint: nothing is minted on the deployment's provisioning account, and a job keeps
the key it was dispatched on — removing the key, or bringing one, between dispatch and claim fails
the job with the reason. The key sits in the job's environment while the repository's own code
runs, so it is used for jobs only while the key form's **Fix jobs** switch is on. It cannot be
capped per job; the worker reports the tokens it used and the bot prices them from the catalogue,
which is what the 1.25× budget cancel reads. The worker meters a key's spend itself only when the
key was minted for that job — a shared key's running total is every other caller's spend too.

### A job from claim to draft pull request

The container's environment holds only the job id, the bot's URL, the mode, and a token good for
that one job — every call the worker makes back to the bot carries it, it expires with the job's
timeout plus fifteen minutes, and it is revoked two minutes after the job ends. The worker claims
the spec, the repository token and a model key back over `POST /api/worker/jobs/{id}/claim`, then
clones the base branch (token in an HTTP header, never in argv, the remote URL or on disk; tags,
submodules and LFS objects come with it), resolves the recipe of every package the brief points
at, installs their dependencies, builds and tests each, runs the engine, builds and tests again,
commits, pushes a new branch named by the
organisation's convention — `bugfix/fix-12-retry-storm-attest_tag` by default: a prefix picked by
the kind of change, `fix-<id>-<slug>`, then the suffix that marks it as the bot's (see
`worker_branch_prefix` in [configuration](configuration.md#console-settings)) — and opens a
**draft** pull request whose body carries the brief, the evidence, the files changed and every gate
before and after, with an honest note on top when one still fails or none could be found. Every
pull request is a draft, and no setting changes that. Build output and lockfiles the install step
generated are never committed. Progress comes back as events; one checklist message in the thread
(`○ clone → ○ set up and check → ○ fix → ○ build and test → ○ pull request`) is edited as they
arrive, and the result is posted with the PR link, the diff and the log as files. *stop* in the
thread, `!job cancel <id>` or the console's Cancel end a job; the worker learns on its next event
and pushes nothing further. A job that goes quiet for five minutes is marked stale, checked against
the platform that started it, and given up on after fifteen more; jobs survive a bot restart
because their rows and the checklist message live in the database.

### The sandbox user

**The repository's code never runs as the worker.** In every container mode the worker drops to an
unprivileged sandbox user (`WORKER_SANDBOX_UID`, 10002 in the image) for everything the repository
decides — its install, build and tests, and the engine — and that user cannot read the job token or
the environment the repository token travels in. Every git command after the clone is handed over
(stage, diff, commit, push) runs as that same user, with hooks, `core.fsmonitor`, credential
helpers, commit signing and external diff drivers pinned off on the command line, so a
`.git/config` the repository's own code rewrote cannot run anything as root. `WORKER_MODE=local`
is the exception, being a developer's subprocess: there the repository's code runs as the bot's own
user.

### Any repository, any language

What a job runs is a **recipe**, and three things can decide it, in this order:

1. **`.attest/recipe.yaml`, committed in the repository.** The people who own the code are the
   ones who know how it is built, and this is how they say so:

   ```yaml
   workdir: services/api        # for a monorepo; omit for the root
   tools: { go: "1.25", node: "22" }
   setup: [go mod download]
   build: go build ./...
   test: go test -race ./...
   lint: golangci-lint run
   services: [postgres:16]      # what the suite needs running
   ```

   Every command is a program and its arguments — pipes, redirection and chaining are refused
   rather than quietly run, so a file a contributor commits is not a shell in the worker.
2. **The recipe on the repository connection**, set by an admin in the console. What the first
   job on a repository worked out is remembered there too, but only to route later jobs to the
   right worker image — it never decides what a later job runs, since that job may be about
   another package.
3. **Detection**, which reads the marker files in the clone: Go, Rust, Python (uv, Poetry,
   Pipenv, requirements), Node (npm, pnpm, Yarn, Bun), Maven, Gradle, .NET, Ruby, PHP, Elixir,
   Dart, Swift, CMake and a plain Makefile. It picks the package directory nearest the first file
   the brief pointed at, and checks the other packages the brief points into as well (below);
   where one directory declares several ecosystems, each contributes its install step and the
   most specific one names the recipe. An authored recipe is merged over the detected one, so
   correcting the test command does not cost you the rest.

Whichever decided it, the resolved recipe reaches the engine's prompt — the exact commands, the
directory, and what they said before the change — so it runs the same checks the harness will
grade it with instead of going looking for them.

#### Monorepos and more than one package

A job checks every package its brief points at, up to three. The first file's package is the
primary; each other package the brief names a file in is set up and checked the same way — its
own toolchains, its own install, its own build and tests before and after the change — within the
time the engine can spare, and one that no longer fits is named as not checked, with the reason.
A package the change touched that the brief never named is listed as *changed, not checked* in the
pull request, the thread and the console, so its silence is never read as a pass. The other
packages are always detected: a `.attest/recipe.yaml` or a console recipe speaks for the package
it names. Documentation, examples and tooling folders (`docs/`, `examples/`, `scripts/`, …) are
never checked as packages of their own.

Folders can pin different versions of the same tool — `web/` on Node 20 beside `tools/` on
Node 22, one service on Python 3.9 and another on 3.12 — and every command, the checks' and the
engine's, runs the version pinned where it runs. A pin nothing can serve is said, never swapped
in silence: a folder pinning a Python older than 3.8, which no longer exists in any form the
worker can obtain, runs on 3.8 and the pull request and the engine are told so; any other
unobtainable version runs on the image's own, and says that.

#### Toolchains and the dependency cache

Toolchains come from two places. The worker image bakes in Python, Node, Go and Rust (and a
second, heavier image adds a JDK, Maven, Gradle and the .NET SDK, which the bot routes to per
repository with `WORKER_JOB_NAMES` — once the repository's ecosystem is known, from an admin's
recipe or from the first job, which runs on the base image). Everything else, and every *other
version* of those, is fetched per job by [mise](https://mise.jdx.dev) from what each folder of the
repository pins — `.tool-versions`, `mise.toml`, `.nvmrc`, `.python-version`, `.java-version`,
`.ruby-version` — and mise's shims pick the right one wherever a command runs. Go and Rust switch
per folder on their own (`GOTOOLCHAIN=auto` from `go.mod`, rustup from `rust-toolchain.toml`). A
range in a manifest (`requires-python`, `engines.node`, a Maven release) counts as its floor, the
oldest version it allows. Fetched toolchains and every package-manager store land in a **dependency cache**
kept per organisation, repository and base branch in a Google Cloud Storage bucket
(`WORKER_CACHE_BUCKET`, else `LITESTREAM_BUCKET`), which the bot hands the worker as two
short-lived signed URLs with the claim, so the container holds no storage credential and the bot
proxies no bytes. `WORKER_CACHE=off` turns it off; on any other storage, without a bucket, or
without permission to sign, jobs simply install from cold.

A gate that cannot run is never reported as a gate that failed. A missing toolchain, a suite that
needs a database the worker cannot start, a monorepo where nothing said which package, a package
the change touched outside the brief — each comes back as itself, in the pull request and in the
thread, together with what would fix it.

#### The coding agent and its model key

The coding agent (Settings → Workers → **Coding agent**, `worker_engine`) is
[Qwen Code](https://github.com/QwenLM/qwen-code), run headlessly on the worker model set there —
empty means the advanced model, then the default — with caps on turns, tool calls and wall time,
and only the model key in its environment, so a `git push` from inside it cannot authenticate.
`worker_engine=fake` makes one placeholder edit without a model, for CI and dry runs.

Which model key the worker gets is decided before anybody is asked to confirm:

- **With `OPENROUTER_PROVISIONING_KEY`**, every job gets its own OpenRouter key capped at the job
  budget, destroyed when the job ends, and the exact spend is read back afterwards. A key is never
  minted without a positive cap, and `worker_job_budget_usd` must be between $0.10 and $100.
- **Without it**, the worker gets the shared key — `WORKER_LLM_API_KEY`, else the deployment's own —
  with no per-job cap. That is refused where `SIGNUP_MODE=open`: the key sits in a container running
  a stranger's repository code, so an open deployment needs a provisioning key or the organisation's
  own key.
- **On the organisation's own key**, as above, and only while its **Fix jobs** switch is on.

With none of them the tool refuses and says which variable to set. The cost lands in `usage` for
the channel, so budgets and `!usage` include it. The console's Jobs page lists every job with its
status, PR, cost, events, diff and log. Each plan size is sold with a number of jobs a month
(`sizeJobLimits` in `config.go`, shown on Settings → Billing against this month's count); that is a
printed figure, not a gate — `worker_max_jobs`, `worker_job_budget_usd` and the budgets above are
the limits that stop anything.
