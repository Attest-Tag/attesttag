# AWS

ECS Fargate behind an Application Load Balancer, with documents and the SQLite replica in S3.

```bash
DOMAIN=bot.example.com ./deploy/aws/fargate.sh
```

Needs the AWS CLI v2 authenticated at an account, `docker`, `python3`, and a domain you can add a
CNAME to — and `openssl` for `CREATE_DATABASE=1`. Reads secrets from `.env` (`ENV_FILE`
overrides) — run [`../local/bootstrap.sh`](../local/bootstrap.sh) first if you have not.
Idempotent: rerunning it is how you ship a new image.

By default it mirrors the published image into ECR, and until the first release there is none:
add `IMAGE_SOURCE=build`, which builds this checkout for `linux/amd64` instead.

> **Not run against a live account routinely, unlike [`../gcp/`](../gcp/README.md),** which the
> hosted service deploys with. This one was written against the AWS API and reviewed, and
> [`../test/`](../test/README.md) runs it against fake CLIs, so read what it is about to create
> before the first run. Every step is idempotent and re-entrant, so a failure part-way through is
> fixed by fixing the cause and rerunning.

## Why not App Runner

App Runner is the obvious Cloud Run analogue and it is the wrong one. It throttles an instance's
CPU whenever it is not serving a request, and offers no way to turn that off.

This bot does its real work *between* requests — the Slack inbox dispatcher, the routine
scheduler, the ingest loop, Drive sync. On App Runner it would ack Slack's event inside three
seconds and then never do the work, which reads as the bot ignoring people rather than as a
misconfiguration. A Fargate task's vCPU is its own for as long as the task runs. The ALB is the
price of that, and it is most of the bill here.

## What it creates

| | |
|---|---|
| S3 bucket | `attesttag-data-<account>`, or the one `BUCKET` names. Documents under `docs/`, the SQLite replica beside them |
| IAM user + access key | The app signs S3 requests with static keys and has no instance-role path, so a task role would not be read. The user's policy covers this one bucket prefix and nothing else |
| Secrets Manager | `attesttag/s3-key`, holding that key, and `attesttag/env`, one JSON document holding the secrets the script knows from the env file plus the S3 secret. ECS reads individual keys out of the second |
| ECR repository | The image, mirrored from ghcr.io — or built from this checkout with `IMAGE_SOURCE=build`, the only choice until the first release |
| ACM certificate | For `$DOMAIN`, validated by a CNAME the script prints |
| ALB + target group | 443 forwards, 80 redirects. Health check `/health` |
| ECS cluster + service | One task, always on |

It uses the account's **default VPC** and its public subnets. Set `VPC_ID` and `SUBNETS` to place
it somewhere else; the task needs a public IP or a NAT either way, or it cannot pull its own
image.

## What reaches the task

The script sets `DOCS_S3_URL` and the key itself, pointing at its own bucket, and
`ADMIN_BASE_URL` to `https://$DOMAIN`; any `DOCS_S3_*` or `ADMIN_BASE_URL` in the env file is
not read. From the env file, these go into Secrets Manager: `SLACK_SIGNING_SECRET`,
`SLACK_CLIENT_ID`, `SLACK_CLIENT_SECRET`, `OPENROUTER_API_KEY`, `LLM_API_KEY`, `MASTER_KEY`,
`MASTER_KEY_PREVIOUS`, `RESEND_API_KEY`, `OPENROUTER_PROVISIONING_KEY`, `WORKER_LLM_API_KEY`,
`WORKER_ENGINE_API_KEY`, `HEALTH_SECRET`, `OPERATOR_SECRET`, the GitHub App's private key and
client secret, `DATABASE_URL` and `MSTEAMS_APP_PASSWORD`. And these travel as plain variables:
`TZ_NAME`, `ALLOWED_EMAIL_DOMAINS`, `MAIL_FROM`, `SIGNUP_MODE`, `LLM_BASE_URL`, `LLM_MODEL`,
`HEAVY_MODEL`, `EMBED_MODEL`, `ORG_MODEL_KEYS`, `LOG_LEVEL`, the GitHub App's id, slug and client
id, the other four `MSTEAMS_*` settings and, with the worker on, the `WORKER_*` settings — the
same list [`../azure/`](../azure/README.md) passes.

Nothing else in the file reaches the task — not `PUBLIC_ORIGIN_HOSTS` or the `LIMIT_*`
overrides; any other setting needs a line in the script's environment block.

## The first run takes two passes

ACM will not issue until you have added the validation CNAME, and nothing in the script can add
it for you. So the first run gets as far as the certificate — by then it has made the security
groups, the bucket, the IAM user and its key, both secrets, the ECR repository and the pushed
image, and the RDS instance too with `CREATE_DATABASE=1` — then prints the record and exits
non-zero. Add the record and rerun: the load balancer, the roles, the task definition and the
service come on that pass.

Afterwards, point `$DOMAIN` at the load balancer — a CNAME to the printed `*.elb.amazonaws.com`
name, or an A/ALIAS in Route 53.

## One writer

`desiredCount=1`, and the deployment runs `minimumHealthyPercent=0` with `maximumPercent=100`,
so the old task stops before the new one starts. That costs a few seconds of downtime on every
deploy and is the correct trade: two tasks against one SQLite replica is not a faster deployment,
it is a corrupted one.

`stopTimeout` is 45 seconds rather than the default 30, because the write lease wants about nine
to be released cleanly and a task killed before it lets go leaves the next one waiting out the
lease's expiry instead of starting.

**Never put the database on EFS.** EFS is NFS, SQLite must not be run on NFS, and the corruption
is quiet and noticed much later. Documents on EFS would be fine; they are ordinary file I/O.

## The fix-job worker

Optional and off by default. [`worker.sh`](worker.sh) builds the worker image, registers the task
definition the bot runs it as, and creates the IAM this needs:

```bash
./deploy/aws/worker.sh          # run this before fargate.sh
```

Run it first. `fargate.sh` then attaches the `attesttag-task` role it created — the bot's first
AWS permission of its own, since its S3 access comes from a static key — and passes the
`WORKER_*` settings through. With the worker on but no task definition, `fargate.sh` warns rather
than deploying a tool that fails on its first use. `WORKER_MODE=workers` is enough — a task
finds `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI` in its own environment and resolves to `ecs`;
`WORKER_MODE=ecs` pins it.

| | |
|---|---|
| ECR | `attesttag-worker`, and `attesttag-worker-jvm` unless `WORKER_JVM=0` |
| `attesttag-worker-exec` | pulls the image and writes logs |
| `attesttag-worker-task` | the worker's own identity, **granted nothing** |
| `attesttag-task` | the bot's: `RunTask` on these definitions, and `DescribeTasks` and `StopTask`, all conditioned on the cluster, and `PassRole` for the two roles above and nothing else |
| security group | `attesttag-worker`: egress only, no ingress rule ever added |

Each task gets 50 GiB of ephemeral storage rather than the 20 GiB default, because a clone plus a
cold dependency install exceeds 20 and running out of it reads as a failing build rather than as
a full disk. ECS has no per-task timeout, so the worker keeps its own 55-minute ceiling and the
bot's reconciler stops a task that outlives its job deadline.

`fargate.sh` never rebuilds the worker images. Rerun `worker.sh` after every upgrade, or the
worker keeps the code it was last built from, security fixes included.

[`../docs/platforms.md#the-fix-job-worker`](../docs/platforms.md#the-fix-job-worker) is what this
is and why it is a separate container.

## Lifting the one-task ceiling

Put a `DATABASE_URL` for an RDS Postgres in the env file and rerun. The binary prefers
`DATABASE_URL` over `DB_PATH` and turns the SQLite replica off by itself, so the bucket then
holds only documents. Put the task in the same VPC as the instance. Every run of `fargate.sh`
sets `desiredCount` back to 1, so raise it after each deploy:

```bash
aws ecs update-service --cluster attesttag --service attesttag --desired-count <n>
```

[`../docs/postgres-cutover.md`](../docs/postgres-cutover.md) is the runbook when the database
already has rows in it.

**Or have the script create it.** `CREATE_DATABASE=1` makes the RDS instance for you, in the
same VPC, and appends the DSN to the env file:

```bash
DOMAIN=bot.example.com CREATE_DATABASE=1 ./deploy/aws/fargate.sh
```

It lists what it is about to bill you for and waits for a yes — `ASSUME_YES=1` for a scripted
install, and with no terminal and no `ASSUME_YES` it refuses before creating the database. The
steps ahead of that question have run by then: the security groups, the bucket, and the IAM user
and its key.

| | |
|---|---|
| `attesttag-pg` | `db.t4g.micro`, 20 GiB gp3, encrypted, 7 days of backups, **not publicly accessible** |
| `attesttag-pg-subnets` | a DB subnet group across the same subnets as the task |
| `attesttag-pg` (SG) | reachable on 5432 from the `attesttag-task` group and nothing else |

Override with `DB_INSTANCE`, `DB_INSTANCE_CLASS` and `DB_ALLOCATED_STORAGE`.

The DSN is appended to the env file rather than kept only in Secrets Manager, and that matters
in both directions: it is what makes a rerun idempotent — `DATABASE_URL` is simply set the next
time — and it is the **only copy of that password you can read**. Back the file up. The script
reads the last `DATABASE_URL=` line with a value, so the appended DSN is the one used whatever the
template's line above it says. If the instance exists and the env file has no `DATABASE_URL`, the
script stops and tells you to put the DSN back by hand or reset the password, because it cannot
invent one it never kept.

## Roughly what it costs

An always-on Fargate task at 1 vCPU / 2 GB, plus an ALB — the load balancer is the larger half
and the reason the GCP shape is cheaper for one instance. S3 is pennies. Add an RDS instance
only when you actually need the second task.

## Tearing it down

```bash
aws ecs update-service --cluster attesttag --service attesttag --desired-count 0
aws ecs delete-service --cluster attesttag --service attesttag
aws elbv2 delete-load-balancer --load-balancer-arn <arn>
```

The bucket, the secrets and the IAM user are deliberately left: the bucket holds the documents
and the database, and Secrets Manager holds the `MASTER_KEY` that is the only thing able to read
them. Delete those by hand, once you are sure.

So is an RDS instance made with `CREATE_DATABASE=1`, which holds the rows and goes on billing
until you delete it. Keep a final snapshot when you do:

```bash
aws rds delete-db-instance --db-instance-identifier attesttag-pg \
  --final-db-snapshot-identifier attesttag-pg-final
```
