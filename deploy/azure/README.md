# Azure

Container Apps, with the storage question answered one of two ways.

```bash
DOMAIN=bot.example.com ./deploy/azure/containerapps.sh
```

Needs the `az` CLI logged in with a subscription selected, and `python3` — plus `openssl` for
`CREATE_DATABASE=1`. Reads secrets from `.env` (`ENV_FILE` overrides) — run
[`../local/bootstrap.sh`](../local/bootstrap.sh) first if you have not. Idempotent: rerunning it
is how you ship a new image. Every run starts a new revision, with a suffix of its own, so the
same tag again — `:latest` after a release has moved it — is pulled afresh, and a secret changed
in the env file reaches the replicas too; Container Apps would otherwise make a revision only when
the template changed. `DOMAIN` is optional here, because ingress comes with a managed certificate
on an `azurecontainerapps.io` name.

It runs `ghcr.io/attest-tag/attesttag:latest` unless `IMAGE` names another, and the app spec
carries no registry credential, so the image must be one that can be pulled anonymously. Until
the first release nothing is published there: build this checkout for `linux/amd64`, push it to
a public repository of your own, and pass that as `IMAGE`:

```bash
docker buildx build --platform linux/amd64 -t <registry>/<you>/attesttag:<tag> --push .
IMAGE=<registry>/<you>/attesttag:<tag> DOMAIN=bot.example.com ./deploy/azure/containerapps.sh
```

> **Not run against a live subscription routinely, unlike [`../gcp/`](../gcp/README.md),** which
> the hosted service deploys with. This one was written against the Azure API and reviewed, and
> [`../test/`](../test/README.md) runs it against fake CLIs, so read what it is about to create
> before the first run. It builds one full app spec and applies it in a single call, so a
> failure leaves the previous revision serving rather than a half-updated one.

## Container Apps passes the always-on-CPU rule

Worth saying, because the wording suggests otherwise. An "idle" replica on Container Apps is a
*billing* state, not a throttle: Azure's rule is that a replica counts as idle only while it is
using less than 0.01 vCPU and serving no requests. A replica running the routine scheduler and
the Slack dispatcher is simply billed at the active rate and keeps its CPU. `minReplicas: 1` is
what stops it scaling to zero, and the script sets it.

This is the difference between Container Apps and AWS App Runner, which throttles idle CPU
outright — see [`../aws/README.md`](../aws/README.md).

## Storage: pick one, and the script follows

Azure is the awkward one. **Azure Blob has no S3-compatible API**, and Azure Files is SMB, which
SQLite must not be run on any more than NFS. Two shapes survive that, and the script picks
whichever your env file describes. With neither set it refuses and says this, rather than
deploying something that loses its documents on the first restart.

**A — a bucket from elsewhere, SQLite replicated.**

```bash
DOCS_S3_URL=s3://bucket/docs?endpoint=https://acct.r2.cloudflarestorage.com&region=auto
DOCS_S3_KEY_ID=…
DOCS_S3_SECRET=…
```

Cloudflare R2, Backblaze B2, Wasabi, or a MinIO you run. The replica and the documents go to
that bucket; the replica's own disk holds the live database, and a cold start restores it. One
replica. All three lines are needed: the script checks only that `DOCS_S3_URL` is set, and the
container exits at startup without the key pair.

**B — Postgres and Azure Files, entirely within Azure.**

```bash
DATABASE_URL=postgres://user:pass@server.postgres.database.azure.com:5432/attesttag?sslmode=require
```

The script then creates a storage account and a file share and mounts it at `/app/docs`.
Documents are ordinary file I/O and do not care that the share is SMB — which is exactly why
this is allowed and putting the *database* there is not. This shape can run more than one
replica: `REPLICAS=2 ./deploy/azure/containerapps.sh`.

**C — have the script create the Postgres.** If you want shape B and do not already have a
server, `CREATE_DATABASE=1` makes one:

```bash
CREATE_DATABASE=1 ./deploy/azure/containerapps.sh
```

It lists what it is about to bill you for and waits for a yes — `ASSUME_YES=1` for a scripted
install, and with no terminal and no `ASSUME_YES` it refuses before creating the server (the
resource group and the Container Apps environment come earlier, and are made by then). You
get an Azure Database for PostgreSQL Flexible Server (`Burstable Standard_B1ms`, 32 GiB, version
16 — override with `PG_SKU`, `PG_TIER`, `PG_STORAGE`, `PG_VERSION`, `PG_SERVER`) in the same
resource group, and the script carries on into shape B with the file share for documents.

The server takes **no public firewall rule**. `--public-access 0.0.0.0` in the script reads like
the opposite of that and is not: it is Azure's spelling for
*AllowAllAzureServicesAndResourcesWithinAzureIps*, which is how Container Apps reaches it without
putting the whole environment in a VNet for one dependency.

The DSN is appended to the env file, which is what makes a rerun idempotent — `DATABASE_URL` is
simply set the next time — and is the **only copy of that password you can read**. Back the file
up. The script reads the last `DATABASE_URL=` line with a value, so the appended DSN is the one
used whatever the template's line above it says. If the server exists and the env file has no
`DATABASE_URL`, the script stops and tells you to put the DSN back by hand or reset the password,
because it cannot invent one it never kept.

Setting both a bucket and a `DATABASE_URL` is fine and means Postgres plus a bucket. The binary
prefers `DATABASE_URL` over `DB_PATH` and turns the SQLite replica off by itself.

`REPLICAS` above 1 without a `DATABASE_URL` is refused. A second replica would be denied the
write lease and answer 503 rather than serve traffic — the lease stops it corrupting the
database, it cannot make it useful.

## What reaches the app

From the env file, these become Container Apps secrets: `SLACK_SIGNING_SECRET`,
`SLACK_CLIENT_ID`, `SLACK_CLIENT_SECRET`, `OPENROUTER_API_KEY`, `LLM_API_KEY`, `MASTER_KEY`,
`MASTER_KEY_PREVIOUS`, `RESEND_API_KEY`, `OPENROUTER_PROVISIONING_KEY`, `WORKER_LLM_API_KEY`,
`WORKER_ENGINE_API_KEY`, `HEALTH_SECRET`, `OPERATOR_SECRET`, the GitHub App's private key and
client secret, `DATABASE_URL`, `DOCS_S3_KEY_ID`, `DOCS_S3_SECRET` and `MSTEAMS_APP_PASSWORD`. And
these become plain settings: `DOCS_S3_URL`, `LLM_BASE_URL`, `LLM_MODEL`, `HEAVY_MODEL`,
`EMBED_MODEL`, `MAIL_FROM`, `SIGNUP_MODE`, `ORG_MODEL_KEYS`, `TZ_NAME`, `LOG_LEVEL`,
`ALLOWED_EMAIL_DOMAINS`, the GitHub App's id, slug and client id, the other four `MSTEAMS_*`
settings, and the five worker settings below.

Nothing else in the file reaches the app — not `ADMIN_BASE_URL`, which only `DOMAIN` sets (see
[A name of your own](#a-name-of-your-own)), and not `PUBLIC_ORIGIN_HOSTS` or the `LIMIT_*`
overrides.

## The fix-job worker

Optional and off by default. [`worker.sh`](worker.sh) builds the worker image **inside a
container registry** — `az acr build`, so no local `docker` and no chance of shipping an
arm64 image to an X86_64 runtime — and creates the Container Apps job the bot starts:

```bash
./deploy/azure/worker.sh        # run this before containerapps.sh
```

Run it first, with `WORKER_MODE=workers` in the env file — a replica finds `CONTAINER_APP_NAME`
in its own environment and resolves to `aca`; `WORKER_MODE=aca` pins it. `containerapps.sh` then
turns on the app's system-assigned managed identity and
grants it a **custom role with four actions** — read a job, start it, read its executions, stop
one — scoped to each worker job. Nothing built in is that narrow; the nearest is Container Apps
Contributor, which can also delete the jobs and rewrite their images.

The app's worker settings come from the env file and nowhere else: `WORKER_MODE`,
`WORKER_AZURE_SUBSCRIPTION`, `WORKER_AZURE_RESOURCE_GROUP`, `WORKER_JOB_NAME` and
`WORKER_JOB_NAMES`. `worker.sh` prints the lines to put there — they have to go in the file, not
the shell — and without the subscription and the resource group the bot cannot start a job.

`containerapps.sh` never rebuilds the worker images. Rerun `worker.sh` after every upgrade, or
the jobs keep the code they were last built from, security fixes included.

The job carries a user-assigned identity whose only permission is `AcrPull` on that registry, so
it can fetch its own image and do nothing else. User-assigned rather than system-assigned because
a system-assigned identity is created with its job, which leaves no moment at which `AcrPull` can
be granted before the first pull is attempted.

[`../docs/platforms.md#the-fix-job-worker`](../docs/platforms.md#the-fix-job-worker) is what this
is and why it is a separate container.

## A name of your own

The managed `azurecontainerapps.io` name is a working https origin and Slack will accept it.
For your own domain:

```bash
az containerapp hostname add --name attesttag --resource-group attesttag --hostname bot.example.com
az containerapp ssl upload   --name attesttag --resource-group attesttag --hostname bot.example.com …
```

Pass `DOMAIN` to the script as well, so `ADMIN_BASE_URL` is the name people actually use — it is
the origin every password-reset, verification and invitation link is built from. `DOMAIN` is the
only way the script sets it: an `ADMIN_BASE_URL` in the env file is not passed on.

## Tearing it down

```bash
az group delete --name attesttag
```

Which takes the storage account and the documents with it, and the registry and the worker jobs.
A server made with `CREATE_DATABASE=1` is in this resource group too, so it goes as well, rows
and all — take a `pg_dump` first. If the rows are in a Postgres or the documents in a bucket
outside this resource group, those survive — as does the `MASTER_KEY` in your env file, which
is the only thing able to read what is in them.
