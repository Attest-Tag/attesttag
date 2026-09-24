# Where to run it

Three things decide whether a platform works, and they are the same three everywhere:

1. **A public HTTPS origin.** Slack will not deliver events to localhost and will not accept an
   http redirect URL. There is no Socket Mode fallback in this codebase.
2. **Always-on CPU.** The Slack inbox dispatcher, the routine scheduler, the ingest loop and
   Drive sync run *between* requests. A platform that freezes an idle container will ack Slack's
   event in three seconds and then never do the work — which looks like the bot ignoring people.
   Pin a warm instance everywhere.
3. **Somewhere for two pieces of state.** A database and a folder of documents. Each is a local
   volume by default and each can be something you bring — see
   [`storage.md`](storage.md), which is the detail behind every row below.

Everything else is a container, so the choice is mostly about which of your bills it lands on.
Nothing has been released yet, so no image is published to pull: until the first release, every
platform runs an image built from this checkout, and each folder's README says how.

| | Database | Documents | More than one instance |
|---|---|---|---|
| **A** container + a bucket | SQLite on the instance's own disk, **replicated to the bucket** and restored on boot | the same bucket | no — SQLite has one writer |
| **B** container + a bucket + a managed Postgres | Postgres | the bucket | yes |
| **C** one VM, `docker compose` | SQLite on a volume, or Postgres in the same compose project | a volume, or MinIO in the same project | no |

**A** is the cheaper answer and it is not a lesser one: it is what the hosted service ran on for
months. **B** is what you move to when one instance is not enough, and it is two environment
variables away. **C** is the answer when you would rather operate one machine than five services.

---

## Google Cloud

**A — Cloud Run + GCS, SQLite replicated.** This is what [`cloudrun.sh`](../gcp/cloudrun.sh) builds
by default; the hosted service ran it for months and moved to shape B on 2026-09-14:

```bash
ENV_FILE=.env PROJECT=<your-project> REGION=us-central1 ./deploy/gcp/cloudrun.sh
```

Pass `ENV_FILE=.env`, so that a stray `.env.prod` — the name of the hosted service's own file,
which the script prefers when there is one — is never the file deployed. The hosted service's
public policy, open signup included, comes on only with `HOSTED=1`, which a self-host never sets;
without it the deployment is one organisation ([`gcp/README.md`](../gcp/README.md)).

One bucket holds the documents and the database replica. Cloud Run's disk is wiped on every
restart, so the SQLite file is restored from that replica on boot and streamed back continuously.
The script sets `--min-instances 1 --no-cpu-throttling --max-instances 1`, which is the
always-on CPU rule and the one-writer rule in one line. Deploys are safe because the new
container takes a **write lease** in the bucket before it touches the database, and the old one
holds it until it is done ([`storage.md`](storage.md#the-database-in-a-bucket)).

Roughly: an always-on Cloud Run instance plus a bucket, and no database bill.

**B — Cloud Run + Cloud SQL + GCS.** Add a managed Postgres and the instance ceiling comes off:

```bash
gcloud sql instances create attesttag --database-version=POSTGRES_17 --tier=db-g1-small …
# then put its socket in the env file and rerun cloudrun.sh:
# DATABASE_URL=postgres://attesttag:<password>@/attesttag?host=/cloudsql/<project>:<region>:attesttag
```

`cloudrun.sh` reads the instance out of the DSN and adds it to the service itself. It does not
enable the Cloud SQL Admin API or grant the service account `roles/cloudsql.client`, so do both
first ([`gcp/README.md`](../gcp/README.md)). `DATABASE_URL` wins over `DB_PATH`, so this is one
variable rather than a different build, and [`postgres-cutover.md`](postgres-cutover.md) is the
runbook for moving a database that already has data in it. Roughly $25/month more for the
instance.

## AWS

**ECS Fargate behind an ALB**, which is what [`aws/fargate.sh`](../aws/fargate.sh) builds:

```bash
DOMAIN=bot.example.com ./deploy/aws/fargate.sh
```

It mirrors the published image into ECR, so until the first release add `IMAGE_SOURCE=build`,
which builds this checkout instead.

**Not App Runner**, though it looks like the obvious Cloud Run analogue. App Runner throttles an
instance's CPU whenever it is not serving a request and gives you no way to turn that off — so
rule 2 above fails, and it fails in the shape that is hardest to diagnose: Slack's event is
acked inside three seconds and the work never happens. A Fargate task's vCPU is its own for as
long as the task runs. The ALB is the price of that, and it is most of the AWS bill here.

**A — Fargate + S3, SQLite replicated.** A task's own ephemeral storage is a local block device,
which is what SQLite needs; what makes it safe to lose is that `DOCS_S3_URL` puts the documents
in S3 *and* streams the database there, restoring it when a task starts with an empty disk.
Exactly one task — the script sets `minimumHealthyPercent=0` so that a deploy stops the old task
before starting the new one, rather than briefly running two writers.

The script sets the bucket up itself: it creates `attesttag-data-<account>` (`BUCKET=` names
another) and an IAM user whose key reaches only that bucket's `docs/` prefix, and hands the task
`DOCS_S3_URL` and that key. Any `DOCS_S3_*` in your env file is not read. The app signs its own
S3 requests and has no instance-role credential path, which is why it is a user and a key rather
than the task role.

**Never put the database on EFS.** EFS is NFS, SQLite must not be run on NFS, and the corruption
it causes is quiet and noticed later. Documents on EFS are fine; they are ordinary file I/O.

**B — the same plus RDS Postgres.** Put `DATABASE_URL` in the env file and the task in the same
VPC as the instance. The binary prefers `DATABASE_URL` over `DB_PATH` and turns the SQLite
replica off by itself, so the bucket then holds only documents. Every run of `fargate.sh` sets
the task count back to one, so raise it after each deploy:
`aws ecs update-service --cluster attesttag --service attesttag --desired-count <n>`.

**C — an EC2 instance with `docker compose`**, which is the bottom of the page.

## Azure

**Container Apps**, which is what [`azure/containerapps.sh`](../azure/containerapps.sh) builds:

```bash
DOMAIN=bot.example.com ./deploy/azure/containerapps.sh
```

It runs the published image, so until the first release `IMAGE=` has to name one built from this
checkout — and one that can be pulled without credentials, because the app is given none
([`azure/README.md`](../azure/README.md)).

Container Apps passes rule 2, which is worth saying because the wording suggests otherwise: an
"idle" replica there is a *billing* state, not a throttle. Azure's own rule is that a replica
counts as idle only while it uses less than 0.01 vCPU, so one doing background work is simply
billed at the active rate and keeps its CPU. `minReplicas: 1` is what stops it scaling to zero.
Ingress comes with a managed certificate on an `azurecontainerapps.io` name, so the https origin
needs no work at all.

Storage is the part Azure makes awkward: **Azure Blob has no S3-compatible API**, and Azure
Files is SMB, which SQLite must not be run on any more than NFS. Two shapes survive that, and
the script picks whichever one the env file describes:

**A — Container Apps + a bucket elsewhere, SQLite replicated.** `DOCS_S3_URL` at Cloudflare R2,
Backblaze B2, Wasabi or a MinIO you run, with `DOCS_S3_KEY_ID` and `DOCS_S3_SECRET`. The replica
and the documents go to that bucket; the container's own disk holds the live database. Exactly
one replica.

**B — Container Apps + Azure Database for PostgreSQL + Azure Files.** `DATABASE_URL` at the
flexible server, and the documents on a file share the script creates and mounts at `/app/docs`
— documents are ordinary file I/O and do not care that the share is SMB, which is exactly why
this shape is allowed and putting the *database* there is not. This one is entirely within
Azure and can run more than one replica.

If neither is acceptable, **C** is the honest answer on Azure: one VM with everything in it.

## Anywhere else, and the simplest thing that works

**One VM and `docker compose`** — Hetzner, DigitalOcean, Lightsail, EC2, an Azure VM, a machine
under a desk. It is one container, one volume, and a tunnel or a proxy for HTTPS:

```bash
./deploy/local/bootstrap.sh
docker compose --profile quicktunnel up -d
```

Want the rest of the stack in the same box rather than an account somewhere? Both are profiles:

```bash
docker compose -f docker-compose.yml -f deploy/local/postgres.yml -f deploy/local/minio.yml \
  --profile postgres --profile minio --profile caddy up -d
```

The `caddy` profile needs `ATTEST_DOMAIN` in `.env` first, and takes an optional
`ATTEST_ACME_EMAIL` ([`https.md`](https.md)). That gives you Postgres and an S3 bucket (MinIO)
beside the bot, with generated passwords, on loopback, backed by named volumes. Nothing to sign
up for, and nothing leaves the machine — which also means **the backups are yours**: snapshot
the volumes or the disk.

## Kubernetes

The chart is the same three shapes, chosen by values rather than by platform — storage it runs
for you, storage you bring, or a single stateful pod. [`deploy/helm/README.md`](../helm/README.md)
has the commands; the chart refuses to render a combination that would corrupt something, and
says which two values disagree.

---

## The fix-job worker

Optional, off everywhere by default, and the one feature with a per-platform piece beyond "run
the container". When it is on, "fix this and raise a PR" appears as a tool in Slack: the bot
starts a **second container** that clones a repository, works on it, and opens a draft pull
request.

Two things are worth knowing before turning it on.

**A worker runs the code of whatever repository it is pointed at** — its build, its tests, its
install scripts. That is the point of it, and it means the blast radius is the repositories you
connect. It is why the worker is a container of its own rather than a goroutine in the bot.

**The worker holds no secrets.** The launch carries four variables — a job id, the bot's URL, a
token for that one job and the mode — and nothing else. The token answers only for its own job:
it expires at the job's timeout plus fifteen minutes, and the bot forgets it about two minutes
after the job ends. The spec, the repository token and the model key all arrive over the claim
call the worker makes back to the bot, so the job definition that anyone with read access can see
in your cloud console never contains a credential.

### `WORKER_MODE=workers`, everywhere

```bash
WORKER_MODE=workers
```

That is the switch. There are two questions here — should jobs run at all, and where — and only
the first is usually yours to answer: a deployment already knows which platform it is on,
because the platform tells every container it starts. `workers` reads that and says which it
found at boot.

On most platforms it is the only line you write, and the deploy script, the chart or the compose
overlay fills in the rest. Azure is the exception: `WORKER_AZURE_SUBSCRIPTION` and
`WORKER_AZURE_RESOURCE_GROUP` have to be in the env file too, because `containerapps.sh` reads
them from there and nowhere else, and the bot cannot start a job without them.
[`azure/worker.sh`](../azure/worker.sh) prints both.

| | detected from | What the bot starts | Set it up with |
|---|---|---|---|
| Kubernetes | `KUBERNETES_SERVICE_HOST` | a `batch/v1` Job in its own namespace | `--set worker.enabled=true` |
| AWS | `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` | a Fargate task | [`aws/worker.sh`](../aws/worker.sh) |
| Azure | `CONTAINER_APP_NAME` | a Container Apps job execution | [`azure/worker.sh`](../azure/worker.sh) |
| Google Cloud | `K_SERVICE` | a Cloud Run Job execution | [`gcp/worker.sh`](../gcp/worker.sh) |
| `docker compose` | a Docker socket | a container on the host's Docker daemon | [`local/worker.yml`](../local/worker.yml) |

Every one of those is a variable the platform sets on its own containers and one the matching
dispatcher already depends on, so nothing is inferred from a coincidence. Only the Docker socket
is a filesystem check, which is why it is tried last.

**Naming the platform pins it** — `cloudrun`, `ecs`, `aca`, `k8s`, `docker` — which is the escape
hatch for somewhere detection would be wrong, and the reason no env file written before `workers`
had to change. `local` is the development mode: this binary again, as a subprocess.

Told to run workers and unable to tell where it is, the bot says so at boot and leaves the tool
switched off. It does not pick a default: a job started on the wrong thing an hour later is a far
worse outcome than a refusal you read immediately.

On the three clouds a `worker.sh` builds the worker images, creates the jobs or task
definitions, and prints the `WORKER_*` settings for your env file. Run it **before** the bot's
own deploy script, since the bot cannot start a job that does not exist — on Google Cloud it
creates the bot's service account itself when the bot has never been deployed. `cloudrun.sh`,
`fargate.sh` and `containerapps.sh` each warn rather than deploying a tool that fails on its
first use.

There are two images: [`Dockerfile.worker`](../../Dockerfile.worker) and the heavier
`Dockerfile.worker.jvm`, which adds a JDK, Maven, Gradle and the .NET SDK. The three `worker.sh`
scripts build both from this checkout, and `WORKER_JVM=0` skips the heavy one. Kubernetes and
Docker pull the published `ghcr.io/attest-tag/attesttag-worker` instead, which exists only once
a release is cut — until then build it yourself, as [`local/README.md`](../local/README.md)
shows. The published worker images are `linux/amd64` only, and so are the ones the three scripts
build; on Kubernetes the bot pins its worker pods to amd64 nodes for that reason, and
`WORKER_K8S_ARCH` (the chart's `worker.arch`) moves the pin or, set to `any`, lifts it for an
image built for more.

`WORKER_JOB_NAMES=java=…,java-gradle=…,dotnet=…` routes an ecosystem to the heavy image — job or
task names on the clouds, image references on Kubernetes and Docker.

**A bot deploy never touches a worker image.** After every upgrade, rerun your platform's
`worker.sh`, which rebuilds the images from the checkout. On Docker, `docker pull` the worker
image, because the bot pulls it only when it is missing; on Kubernetes, move `worker.image.tag`
with `image.tag`.

### What each platform grants

The shape is the same everywhere and it is worth checking after you run these: **the worker's
own identity is granted nothing**, and the bot's is scoped to starting that one job.

| platform | The bot may | The worker may |
|---|---|---|
| `cloudrun` | `roles/run.developer` on the worker jobs; `roles/iam.serviceAccountTokenCreator` on its own account, to sign dependency-cache URLs | nothing |
| `ecs` | `RunTask` on the worker definitions, and `DescribeTasks` and `StopTask`, all conditioned on one cluster; `PassRole` for the two worker roles only | nothing |
| `aca` | a custom role with four actions — read a job, start it, read and stop executions — scoped to each job | pull its own image, nothing else |
| `k8s` | a namespaced `Role` over `batch/jobs`: create, get, list, watch, delete | nothing, and no token is mounted |
| `docker` | **everything on the host** | nothing |

That last row is not a gap in the table. Docker's API cannot be scoped to "start this one job",
so resolving to `docker` means mounting the Docker socket into the bot, and access to the Docker
socket is root on the host. On a machine that is yours and runs nothing else that is a reasonable
trade — it is the only way a compose deployment can run containers at all — and on a host shared
with anything you would not hand over it is not. It is an overlay file you add deliberately
rather than a profile that might get switched on by accident, and the bot logs the trade on every
boot in that mode.

### The per-job spend cap

Set `OPENROUTER_PROVISIONING_KEY` and each job gets its own OpenRouter key with its own ceiling,
destroyed when the job ends. Without it every job spends the shared `OPENROUTER_API_KEY` with no
per-job cap, inside a container running somebody's repository. `cloudrun.sh` warns about this
when `WORKER_MODE` is set in the env file (the other scripts do not), and the console's
Settings → Workers tab says which of the two is in force. With `SIGNUP_MODE=open`, a job on the
shared key is refused outright, because a stranger's repository code could read it.

---

## What every one of them still needs

- **`MASTER_KEY`, backed up somewhere else.** It seals every stored credential. No database and
  no bucket helps if it is lost: the rows survive and nothing can read them.
- **A backup that is yours.** A replica follows every write, including the destructive ones. It
  is what a lost disk needs, not what a mistake made last Tuesday needs.
- **One writer**, unless the rows are in Postgres. Replication is durability, not clustering.
