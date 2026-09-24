# Google Cloud

Cloud Run plus a GCS bucket. The hosted service runs on Cloud Run and deploys with these
scripts, so they are the ones most often run against a real account — with a caveat: since it
moved to Cloud SQL on 2026-09-14, only `cloudrun.sh`'s Postgres branch (shape B below) runs in
production, and the SQLite branch the script defaults to has not run there since.

```bash
ENV_FILE=.env PROJECT=<your-project> REGION=us-central1 ./deploy/gcp/cloudrun.sh
```

Needs the `gcloud` CLI logged in, and zsh: both scripts here start `#!/bin/zsh`. Idempotent:
rerunning it is how you ship a new revision. It reads its env file from the repository root,
copies the secrets it knows into Secret Manager, and deploys with `--min-instances 1
--no-cpu-throttling --max-instances 1` — the always-on-CPU rule and the one-writer rule in one
line.

| | |
|---|---|
| `cloudrun.sh` | The bot and console. Start here |
| `worker.sh` | The fix-job worker images and their Cloud Run Jobs — [below](#the-fix-job-worker), and [what it is](../docs/platforms.md#the-fix-job-worker) |
| `cloudbuild.worker.yaml` | Supporting config for `worker.sh` |

`make deploy` and `make worker-deploy` are the same two with `REGION` already set, so a self-host
runs `ENV_FILE=.env make deploy`.

## Why `ENV_FILE=.env`

`cloudrun.sh` reads `ENV_FILE` when it is set, else `.env.prod` when the checkout has one — the
maintainer's file for the hosted service — else `.env`. Passing `ENV_FILE=.env`, the file
`bootstrap.sh` writes, makes sure a stray `.env.prod` is never the one deployed.

`HOSTED=1`, in the shell or in the env file, is what turns on the hosted service's public policy,
and it is the maintainer's switch: a self-host never sets it. With it, the script fills in open
signup, `SUPPORT_EMAIL=support@attesttag.com`, `SITE_URL=https://attesttag.com`, a $5 free-plan
cap and `ORG_MODEL_KEYS=enterprise` wherever the env file does not set them itself, and prints
`HOSTED policy ON`. Without it the deployment is one organisation: the script passes
`SIGNUP_MODE=first-run` and says so — the first account creates the organisation, and everyone
after that joins by invitation — sets no support address, leaves the console talking only to
its own origin, and keeps the free budget and the model-key policy at the self-host defaults. A
value set in the env file wins either way.

## What reaches the service

Only what the script names. These go to Secret Manager: `SLACK_SIGNING_SECRET`,
`SLACK_CLIENT_ID`, `SLACK_CLIENT_SECRET`, `OPENROUTER_API_KEY`, `MASTER_KEY`,
`MASTER_KEY_PREVIOUS`, `RESEND_API_KEY`, `OPENROUTER_PROVISIONING_KEY`, `WORKER_LLM_API_KEY`,
`WORKER_ENGINE_API_KEY`, `HEALTH_SECRET`, `OPERATOR_SECRET`, the two Stripe keys, the GitHub
App's private key and client secret, `DATABASE_URL` and `MSTEAMS_APP_PASSWORD`. These travel as
plain variables: `MAIL_FROM`, `ALLOWED_EMAIL_DOMAINS`, `ADMIN_BASE_URL`, `TZ_NAME`, the GitHub
App's id, slug and client id, the other four `MSTEAMS_*` settings, `SIGNUP_MODE`,
`SUPPORT_EMAIL`, `SITE_URL`, `ORG_MODEL_KEYS`, `FREE_PLAN_BUDGET_USD`, the two `PLATFORM_*`
ceilings, `STRIPE_SIZES`, the four `BILLING_*` settings, `WORKER_MODE`, `WORKER_JOB_NAMES`, and
with the worker on `WORKER_SA_EMAIL`, `WORKER_CACHE_BUCKET` and `WORKER_CACHE`.

Nothing else in the file reaches the service — not `LLM_BASE_URL`, `LLM_MODEL`, `HEAVY_MODEL`,
`EMBED_MODEL`, `LLM_API_KEY`, `PUBLIC_ORIGIN_HOSTS`, `LOG_LEVEL` or the `LIMIT_*` overrides. So
the model key has to be `OPENROUTER_API_KEY`, and a model endpoint other than OpenRouter needs
its settings added in `cloudrun.sh`, one `addenv` line each, as the comment there says.
`gcloud run services update --update-env-vars` works too, until the next run of the script,
which sets every variable afresh.

The plain variables travel as one `--set-env-vars` joined by `|` rather than commas — gcloud's
`^|^` escape — so a comma-separated list such as `STRIPE_SIZES` or `WORKER_JOB_NAMES` arrives
whole, as the env file has it. A value that contains `|` is refused before anything deploys.

## The two shapes

**A — Cloud Run + GCS, SQLite replicated.** The default, and not a lesser one: it is what the
hosted service ran on for months, until 2026-09-14. One bucket holds the documents and the
database replica. Cloud Run's disk is wiped on every restart, so the SQLite file is restored from
that replica on boot and streamed back continuously. Deploys are safe because the new container
takes a **write lease** in the bucket before it touches the database, and the old one holds it
until it is done.

**B — Cloud Run + Cloud SQL + GCS.** Put `DATABASE_URL` in the env file with a
`host=/cloudsql/<project>:<region>:<instance>` socket in the DSN, and `cloudrun.sh` adds the
instance to the service, turns Litestream and the lease off, and sets `REQUIRE_POSTGRES=1`, so
that a `DATABASE_URL` that fails to reach the container is a refusal to start rather than a new,
empty SQLite file. The DSN must name a Cloud SQL socket — or `CLOUDSQL_INSTANCE` the instance —
or the script stops, so a Postgres outside Cloud SQL (Neon, Supabase) cannot be used this way.
It does not enable `sqladmin.googleapis.com`, and it does not grant its service account,
`attesttag-run@<project>.iam.gserviceaccount.com`, the `roles/cloudsql.client` role: do both
yourself (the account exists once the script has run once). Roughly $25/month more.
[`../docs/postgres-cutover.md`](../docs/postgres-cutover.md) is the runbook for moving a
database that already has rows in it.

## The fix-job worker

Optional and off by default. `worker.sh` builds the worker image on Cloud Build and deploys it as
the Cloud Run Job `attesttag-worker` (2 CPU, 4 GiB), under a service account of its own that is
granted nothing:

```bash
PROJECT=<your-project> REGION=us-central1 ./deploy/gcp/worker.sh
```

It reads no env file. Its grants go to the bot's service account, which it creates when
`cloudrun.sh` has not yet, so on a new project either script may run first. It ends by printing
the lines to put in the env file — `WORKER_MODE=workers`, and the `WORKER_JOB_NAMES` line below —
and the command that deploys the bot with them. The bot gets `roles/run.developer` on each job —
to start executions and read or cancel them — and `roles/iam.serviceAccountTokenCreator` on its
own account, to sign the dependency-cache URLs it hands a worker. With the worker on,
`cloudrun.sh` puts that cache in the service's own bucket on both shapes (`WORKER_CACHE_BUCKET`
in the env file names another, and `WORKER_CACHE=off` turns it off).

Unless `WORKER_JVM=0`, it also builds `Dockerfile.worker.jvm`, deploys `attesttag-worker-jvm`
(4 CPU, 8 GiB) and prints a `WORKER_JOB_NAMES` line that routes JVM and .NET repositories to it.
Put that line in the env file and `cloudrun.sh` passes it on; `WORKER_JVM=0` skips building a job
you would not route to.

**A bot deploy never rebuilds the worker.** `cloudrun.sh` touches only the service, and each job
keeps the image `worker.sh` last built, tagged with its commit. Rerun `worker.sh` after every
upgrade, or fixes to the worker — security fixes included — never reach it.

## Things that have bitten

- **On SQLite, `MAX_INSTANCES` is a cost control, not a capacity dial.** Past the lease holder,
  every instance answers 503. The lease keeps a second one from corrupting the database; it
  cannot make it serve traffic. Raise it only on shape B, where there is no lease — and pass it
  on every run, because the script reads it from the shell alone and otherwise sets the ceiling
  back to 1.
- **`LITESTREAM_PATH` names the replica a cold start restores from.** Pointing it at an unused
  path is how you ask for a fresh database, which is why it must never quietly go back to an
  old one — a replica written before the multi-tenant schema will not migrate forward.
- **Source deploys drop `.git`**, so `cloudrun.sh` passes `GIT_COMMIT` explicitly, with `-dirty`
  on the end when the checkout had uncommitted changes. It is how you tell which commit is
  running, the first thing worth knowing about a bug report: the service's own environment
  names it (`gcloud run services describe attesttag --project <project> --region <region>`), and
  on shape A so does the write-lease object, beside the revision holding it
  (`gcloud storage cat gs://<bucket>/litestream/attesttag-v2.db.lock.json`).
