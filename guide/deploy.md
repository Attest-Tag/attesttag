# Deploy: running it yourself

[`deploy/README.md`](../deploy/README.md) is the place to start — it has a folder per platform
([`local/`](../deploy/local/README.md), [`gcp/`](../deploy/gcp/README.md),
[`aws/`](../deploy/aws/README.md), [`azure/`](../deploy/azure/README.md),
[`helm/`](../deploy/helm/README.md)) and each one's README covers that platform end to end.
[Installing it with a coding agent](install-with-an-agent.md) hands the whole thing to Claude
Code, Codex or any harness that runs shell commands. This page is what every one of them has in
common, and the ways of running it that have no folder of their own: a Mac running it unattended,
a plain `docker run`, and the detail of Cloud Run.

## What every deployment needs

- **A public HTTPS origin.** Slack will not deliver events to localhost and will not accept an
  http redirect URL, and there is no Socket Mode fallback.
  [`deploy/docs/https.md`](../deploy/docs/https.md) has three ways to get one.
- **Always-on CPU.** The Slack inbox dispatcher, the routine scheduler, the document ingest and
  Drive sync all run *between* requests. A platform that scales to zero or throttles an idle
  container acknowledges Slack's event and then never does the work, which looks like a bot
  ignoring people.
- **What the process cannot start without:** `SLACK_SIGNING_SECRET`, `SLACK_CLIENT_ID`,
  `SLACK_CLIENT_SECRET` and a model key (`OPENROUTER_API_KEY`, or `LLM_API_KEY` for another
  endpoint) — a Teams-only deployment included — and, in the container image, `MASTER_KEY`.
  Missing any of them, it exits and says which.
- **`MASTER_KEY`, backed up somewhere else.** It seals every stored credential. Lose it and every
  workspace, connection and stored key has to be entered again; nothing can recover it.
- **`ADMIN_BASE_URL`, set to that origin.** Every link the bot builds — reply footers, setup links,
  mail — every sign-in redirect, and the address MCP clients are told to sign in at come from it.
  Unset, the bot learns it from the first signed-in console request on a host that is not loopback
  and keeps it, and a Slack or Microsoft sign-in started on any other address is sent there first.
  That is right for a service with one name and wrong for one whose name changes, like a quick
  tunnel.

## The env file

A self-host keeps its settings in `.env`, which `deploy/local/bootstrap.sh` writes from
[`deploy/env/selfhost.env.example`](../deploy/env/selfhost.env.example) with a fresh `MASTER_KEY`.
[Configuration](configuration.md) is every variable.

`deploy/gcp/cloudrun.sh` and `deploy/plan.sh` read `.env.prod` instead when there is one, which is
the maintainer's own file. The name changes nothing about what is deployed: the deployment is
**one organisation**, with `SIGNUP_MODE=first-run` (the first account creates the organisation
and everyone after it joins by invitation), unless the env file or the shell says `HOSTED=1`. That
turns on the **hosted service's policy** for whatever the file leaves unset: open signup,
`support@attesttag.com` as the support address, `https://attesttag.com` as the site, a $5
free-plan cap, and own model keys for the enterprise plan only. The script prints which one it is
deploying, and a `SIGNUP_MODE` the env file sets itself is deployed as it is, hosted or not. A
self-host passes `ENV_FILE=.env` and never sets `HOSTED`.

Write a secret into `.env` by replacing its line rather than appending one: the template already
holds an empty line for each key, and the cloud deploy scripts read the first one they find.

## The image

`ghcr.io/attest-tag/attesttag`, published by each release for `linux/amd64` and `linux/arm64`,
signed, with provenance and an SBOM; the fix-job worker images beside it are `linux/amd64` only.
Until the first release is tagged, build it from the checkout with `docker build -t attesttag-local .`.

The image runs as an unprivileged user (uid 10001), refuses to start without `MASTER_KEY` rather
than invent one it would lose, and sets its session cookies `Secure` on every origin — so the
console works over https, or on `localhost`, and nowhere else. `INSECURE_COOKIES=1` is for a test
on a LAN and nothing more.

## Mac, unattended (launchd)

```bash
make ui && ./deploy/local/install-launchd.sh
```

The script builds the binary (`go build`, so the console has to be built first) and installs a
per-user launchd service that starts at login, restarts on crash, and logs to `bot.log`. It reads
`.env.testing` if the checkout has one, else `.env` (the service sets no `ENV_FILE`); a bare
binary that finds no `MASTER_KEY` generates one and appends it to that file, so back that line
up. It listens on `:8080` on every interface — set `HEALTH_ADDR=127.0.0.1:8080` and let the
tunnel in front of it be the way in. Stop it with:

```bash
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.attesttag.bot.plist
```

## Plain `docker run`

For somewhere compose is not wanted:

```bash
docker run -d --name attesttag --restart unless-stopped --env-file .env \
  -v attesttag-data:/data -v attesttag-docs:/app/docs \
  -p 127.0.0.1:8080:8080 ghcr.io/attest-tag/attesttag
```

Named volumes rather than host folders, because the container's user has to be able to write
them; the port on loopback, because the way in is a tunnel or a proxy with https. With no bucket
configured the database lives only in the `/data` volume, with no replication and no write lease.
Set `DOCS_S3_URL` and it lives there too: the same bucket takes the documents and a continuous
replica of the database, and a container that starts with an empty `/data` restores it. See
[`deploy/docs/storage.md`](../deploy/docs/storage.md).

## Google Cloud Run

```bash
gcloud auth login
ENV_FILE=.env PROJECT=<your-gcp-project> REGION=us-central1 ./deploy/gcp/cloudrun.sh
```

The script is written for zsh, and is idempotent: rerunning it is how you ship a new version. It
enables the APIs, creates the bucket (`attesttag-data-<project>` by default) and a
least-privilege service account, copies every secret it knows that the env file sets into Secret
Manager, builds the image from this checkout with Cloud Build, and deploys with one instance,
always on. It stops before deploying without `MASTER_KEY`, `SLACK_SIGNING_SECRET`,
`SLACK_CLIENT_ID` or `SLACK_CLIENT_SECRET`, and warns about the things a deployment can run without
(mail, an operator secret, a spend ceiling).

It has two shapes, chosen by one variable:

- **SQLite, replicated to the bucket** — the default. Cloud Run's disk is wiped on every restart,
  so the database is restored from the bucket on boot and streamed back continuously by
  [Litestream](https://litestream.io), behind a **write lease** in the same bucket: a new
  container will not restore or open the database until it holds the lease. Documents live in the
  bucket too, mounted read-only into the container; console uploads go through the bucket's API.
- **Cloud SQL Postgres** — put a `DATABASE_URL` naming the instance's socket
  (`host=/cloudsql/<project>:<region>:<instance>`) in the env file, and the script attaches the
  instance and turns Litestream and the lease off. This is what the hosted service has run on since
  September 2026. [`deploy/docs/postgres-cutover.md`](../deploy/docs/postgres-cutover.md) moves a
  database that already has rows in it.

The script forwards the settings it names, not the whole env file: secrets, the storage it set up,
mail, the public origin, the plan and billing policy, Teams, the GitHub App and the worker. Others —
`LLM_BASE_URL` and the model names among them — do not travel. Add them to the script's `ENVS`
line, or set them once with `gcloud run services update attesttag --update-env-vars …`, which a
later run of the script replaces.

### Cloud Run revisions, health, logs and cost

What to expect:

- The service is **public at the edge** (`--allow-unauthenticated`): the console has its own
  sign-in, Slack must reach `/slack/events` and `/slack/interactions` without a cookie (they are
  authenticated by Slack's signature instead), and `/setup/`, `/configure/` and `/connect/` are
  pages people reach from a link.
- A new revision starts before the old one stops. On SQLite, the new container answers `/health`
  straight away — which is what lets Cloud Run shift traffic and finally stop the old one — but
  serves 503 on everything else until the old container releases the write lease, usually a second
  or two. Slack retries events across that window; a button pressed in it has to be pressed again.
  On Postgres there is no lease and no gap.
- Health: `curl <service-url>/health` answers `ok` (Google's front end intercepts `/healthz`, so
  `/health` is the public one).
- Logs: `gcloud beta run services logs tail attesttag --project <project> --region <region>`, or
  `gcloud run services logs read` for what has already happened.
- Documents: upload them on the console's Documents page. Each organisation's are kept under
  `docs/org-<id>/` in the bucket; a file at the top of `docs/` belongs to nobody.
- Cost: the always-on instance (one vCPU and 1 GiB, CPU always allocated) is most of it; Cloud SQL
  adds its own; the bucket is pennies.

### The fix worker on Cloud Run Jobs

**The fix worker.** `PROJECT=… REGION=… ./deploy/gcp/worker.sh` (or `make worker-deploy`) — run
it before the bot's own deploy — builds `Dockerfile.worker` on Cloud Build (Debian slim, git, and the toolchains a repository's
tests reach for — Python 3.14 + uv, Node 22, Go and Rust — plus the Qwen Code CLI), and the heavier
`Dockerfile.worker.jvm` (a JDK, Maven, Gradle and the .NET SDK) unless `WORKER_JVM=0`. It creates
the Cloud Run Jobs `attesttag-worker` and `attesttag-worker-jvm` with one task, no retries and a
one-hour ceiling, gives them a service account with **no roles** (everything a worker uses comes
over the claim call), grants the bot's account `roles/run.developer` on those jobs only, and lets
the bot sign as itself, which is how it hands a worker the dependency cache's signed URLs. Then
deploy the bot with `WORKER_MODE=workers` in the env file; the script passes the job's name,
region, project and service account through, and warns if the job does not exist yet. Each job
runs as one execution
(`gcloud run jobs executions list --job attesttag-worker --region <region>`) whose launch carries
only a job id, the bot's URL, a token for that one job, and the mode; the Jobs page links to the
execution in the Cloud Console. Executions are billed per second while they run. The model spend
is what `worker_job_budget_usd` caps when `OPENROUTER_PROVISIONING_KEY` is set; without one, a job
spends the shared key uncapped, and a deployment with `SIGNUP_MODE=open` refuses to start one.

**A bot deploy never rebuilds the worker.** The worker image changes only when `worker.sh` runs,
so rerun it after every upgrade, or fixes to the worker never reach the jobs.

The same feature exists on every other platform, with `WORKER_MODE=workers` everywhere and one
platform piece each: `deploy/aws/worker.sh`, `deploy/azure/worker.sh`, the chart's
`worker.enabled`, or the `deploy/local/worker.yml` overlay. What each of them grants the bot and
the worker is in
[`deploy/docs/platforms.md#the-fix-job-worker`](../deploy/docs/platforms.md#the-fix-job-worker).

## Upgrading

Run the same deploy command again, or pull the new image and recreate the container. Migrations
apply themselves at boot, in order, and a migration that has already been applied is never edited
— a deployment refuses to boot on one that changed, which is the signal that something other than
a release touched it. The worker image is separate: rerun the platform's `worker.sh`, or on Docker
`docker pull ghcr.io/attest-tag/attesttag-worker`. `MASTER_KEY` never changes across an upgrade;
rotating it is its own procedure (`MASTER_KEY_PREVIOUS`, in [Configuration](configuration.md)).
