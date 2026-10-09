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
the job with the reason. It is used for jobs only while the key form's **Fix jobs** switch is on.
It cannot be capped per job at the provider; the worker's model proxy stops the engine at the job's
budget instead, from what each call cost (below), and the bot still cancels a job at 1.25× it.

### What a job costs

A coding agent sends its whole conversation with every turn, so a job of a hundred turns reads
millions of input tokens — mostly the same text again. A provider that caches that prefix charges a
fraction for it, and that is most of the difference between an expensive job and a long one.

**The model proxy.** The engine never talks to the provider. The worker starts a small proxy on its
own loopback, and the engine is configured with that address and a token good for that port for the
life of the job — so the real model key never enters the sandbox where the repository's code and
the engine's shell commands run. The proxy adds three things to every call on OpenRouter: the job's
`session_id`, which keeps the job on the provider endpoint holding its cache instead of balancing
turns across every provider of the model; the providers named in `worker_providers`, in order; and a
request for usage accounting, so each answer carries what it cost, how much of the prompt came from
cache and who served it. That is the bill, on any key — the shared one included — and it is reported
call by call, so the job's cost is current while it runs. When it reaches the job's budget the proxy
refuses further calls and stops the engine, and what the engine had changed is committed and opened
as a draft with a note saying why it stopped. The job's log ends the run with one line: calls, the
share served from cache, the money and the providers (`model calls: 6 · 125,467 in (83% from cache)
/ 2,452 out · $0.0547 as billed · served by Mistral 6`).

The cached share is recorded on the job and shown wherever its cost is: the Slack report
("9.8M in (90% cached) / 74k out"), the console's job page, the reply on a pull request a fix was
pushed to, and the **Cost** table that ends a pull request the job opens — turns, input, cached
input, output, and an estimate at the model's list price, since the pull request is written before
the bill is settled. On an endpoint that reports no cost the bot prices the tokens from its
catalogue, cached ones at the cache-read rate.

### Engines and long jobs

Two coding agents run inside the worker, chosen under Settings → Workers → Coding agent: **Qwen Code**
and **pi**, a smaller agent with seven tools and a short system prompt. Both get the same brief, the
same proxy and the same turn cap. Both are told to compact: the conversation is summarised once it
nears a 160,000-token window, rather than the model's own — on a model with a million-token window
that would never happen, and every file read early in a long job would ride along in every later
turn. Qwen Code also runs without its sub-agent, web and memory tools, and cuts a tool's output at
6,000 characters.

The brief starts the engine where the work is. Files the request names — a path, the tail of one in a
stack trace, a file name, or a component's name that matches a file — are looked up in the clone and
listed first, and the engine is told how to spend turns: several reads in one turn, grep before
whole files, a file read once, and installed dependencies left alone.

A repository's Node processes get a V8 heap of about 60% of the container's memory
(`NODE_OPTIONS=--max-old-space-size`, unless the worker's own environment sets `NODE_OPTIONS`): Node
sizes its default from the machine, and a large front end's production build ran out of it with
most of the container unused.

To compare engines, models or providers on the same work, `evals/fixjobs` holds small repositories
with a task each and hidden tests the engine never sees; see [evals/README.md](../evals/README.md).

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
before and after, with an honest note on top when one fails or did not finish, or none could be
found — and whether a failing one passed before the change, failed worse after it, or was failing
already. Every pull request is a draft, and no setting changes that; on a repository under
[code review](code-review.md) it is reviewed like anybody's once somebody marks it ready for review.
Build output and lockfiles the install step generated are never committed. Progress comes back as
events; one checklist message in the thread
(`○ clone → ○ set up and check → ○ fix → ○ build and test → ○ pull request`) is edited as they
arrive, and the result is posted with the PR link, the diff and the log as files. When the model
provider refuses a call, the pull request, its commit, the thread and an answer on GitHub give a
plain reason (credit or key limit reached, rate limited, or the provider's status code). The
provider's own message names the account and key the call was made on, so it stays on the job in
the console and in the server log. *stop* in the
thread, `!job cancel <id>` or the console's Cancel end a job; the worker learns on its next event
and pushes nothing further. A job that goes quiet for five minutes is marked stale, checked against
the platform that started it, and given up on after fifteen more; jobs survive a bot restart
because their rows and the checklist message live in the database.

### Jobs asked for on a pull request

A job can also be asked for on GitHub, from a [code review](code-review.md#fixing-a-finding-on-the-pull-request)
finding: a ticked box on the finding's comment, or `@<app> fix` in its thread or on the
conversation, by somebody GitHub says can push to the repository. There is no Confirm card — the
person asking on the pull request is the one who could have pushed the change themselves — and no
chat thread: the job answers on the pull request and is followed in the console's *Jobs*, where it
reads *Asked for on GitHub*. Its brief is the findings as the review stored them: what is wrong,
where, the code they quote, the change a finding suggested, and the asker's own words.

What it does differently from a job asked for in chat:

- it clones the pull request's own branch, and its change is one more commit on top of it — no new
  branch, no pull request of its own. It pushes only to that branch, never to the one the pull
  request merges into, and only with its commit built on the head it started from. It never forces:
  when somebody pushed to the branch meanwhile, its commit is replayed once onto theirs and pushed
  again, with a note that its checks ran before the replay; a replay that conflicts pushes nothing.
- it pushes as the App, through the installation the review came through, whatever stored
  connection the repository has — that connection still lends its recipe and its worker image.
- a change that breaks a check that passed before it is not pushed. In chat such a change is still
  opened as a draft, which waits for somebody to look; on a branch somebody is working on it would
  be in their way first. The diff goes back with the answer instead.
- it runs inside the life of an installation token, like any job on an App connection.

Its cost is logged where the review's is, under the pull request.

### The sandbox user

**The repository's code never runs as the worker.** In every container mode the worker drops to an
unprivileged sandbox user (`WORKER_SANDBOX_UID`, 10002 in the image) for everything the repository
decides — its install, build and tests, and the engine — and that user cannot read the job token,
the environment the repository token travels in, or the model key, which stays with the worker's
model proxy. Every git command after the clone is handed over
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
directory, what they said before the change and how long they took — so it knows the checks the
harness will grade it with instead of going looking for them. It checks its change with the
narrowest run that covers it, such as the tests for the files it changed or the linter on just
those files, and runs a whole command itself only when it passed before the change in under two
minutes; the harness runs every one after it stops either way. A command the sandbox stops is not
worked around: the agent says in its summary what it could not check.

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
thread, together with what would fix it. Nor is a gate that could not finish: a suite or a build
the worker's sandbox stopped partway — killed, most often for want of memory, which the container's
own count of out-of-memory kills says when it can be read — is reported as *did not finish
(killed: out of memory)*, never as a failure, and the pull request says it did not check the
change. The coding agent is told the same of a check that was killed before its change, and to
check the change with a narrower run, such as only the tests for the files it touches.

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
