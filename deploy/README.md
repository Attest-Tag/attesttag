# Deploying attest_tag

One folder per place you can run it. Pick yours and read that folder's README; everything
outside them is shared by all five.

| | |
|---|---|
| [`local/`](local/README.md) | **Start here.** Docker and `docker compose`, on a laptop or one VM. Also the macOS LaunchAgent |
| [`gcp/`](gcp/README.md) | Google Cloud Run, where the hosted service runs. Deploy with `ENV_FILE=.env`: the script prefers the hosted service's own env file whenever there is one |
| [`aws/`](aws/README.md) | ECS Fargate behind an ALB, with documents and the SQLite replica in S3 |
| [`azure/`](azure/README.md) | Azure Container Apps, with Postgres and Azure Files or a bucket elsewhere |
| [`helm/`](helm/README.md) | Kubernetes, anywhere. Cloud-agnostic, so it has a folder of its own rather than a place in a cloud's |

**Or hand it to a coding agent.** Point Claude Code, Codex or any harness at a clone of this
repository and say where you want it installed:
[`guide/install-with-an-agent.md`](../guide/install-with-an-agent.md).

The quickest path, from nothing:

```bash
./deploy/local/bootstrap.sh
docker compose --profile quicktunnel up -d
```

Until the first release there is no published image to pull: build one first with
`docker build -t attesttag-local .` and put `ATTEST_IMAGE=attesttag-local` in `.env`. After the
first `up -d` the bot restarts in a loop until `.env` has the Slack values and the model key,
which is expected at this point — the tunnel's address comes first, and then the Slack app that
points at it: [`guide/slack-app.md`](../guide/slack-app.md).

## Shared by every folder

| | |
|---|---|
| [`docs/platforms.md`](docs/platforms.md) | **Read this before choosing.** Which shape to run on each platform, and what each costs you in moving parts |
| [`docs/storage.md`](docs/storage.md) | Local by default, run in the box, or bring a Postgres and an S3 bucket — and where to get each |
| [`docs/https.md`](docs/https.md) | How to get a public HTTPS address, which is not optional |
| [`docs/platforms.md#the-fix-job-worker`](docs/platforms.md#the-fix-job-worker) | Turning on "fix this and raise a PR" — one `WORKER_MODE` and one script per platform |
| [`docs/postgres-cutover.md`](docs/postgres-cutover.md) | Moving a running deployment from SQLite to Postgres, with the freeze and the verification |
| [`env/selfhost.env.example`](env/selfhost.env.example) | The settings a self-host needs, in the order you need them. The root `.env.example` is the full catalogue |
| [`slack/manifest.json`](slack/manifest.json) | The Slack app definition. `TestManifestAsksForTheScopesTheCodeAsksFor` keeps its scopes honest against the code |
| [`slack/manifest.sh`](slack/manifest.sh) | Fills in your origin: `BASE_URL=https://host ./deploy/slack/manifest.sh` |
| [`test/`](test/README.md) | Run the AWS and Azure scripts, and the policy half of the Cloud Run one, against fake CLIs, with no account and no network. What it proves, and what only a real account can |
| [`plan.sh`](plan.sh) | Move an account between the free, pro and enterprise plans, grant credit, or find the account behind a support email. Talks to a *running* deployment over HTTP, so it works against any of the five. Needs zsh, curl and python3, `OPERATOR_SECRET` set on the server and handed to the script from the shell or its env file (`ENV_FILE`, else `.env.prod` when there is one, else `.env`), and the target in `BASE_URL`: `ENV_FILE=.env BASE_URL=https://bot.example.com ./deploy/plan.sh` |

`docker-compose.yml` is in the repository root, because that is where people look for it.

## The three things that decide whether a deployment works

**A public HTTPS origin.** Slack will not deliver to localhost and will not accept an http
redirect URL. [`docs/https.md`](docs/https.md) covers the three ways to get one; the quick
tunnel needs no account and takes two minutes.

**Always-on CPU.** The Slack inbox dispatcher, the routine scheduler, the ingest loop and Drive
sync all run *between* requests, not during them. A platform that scales to zero or throttles
the CPU of an idle container will ack Slack's event in three seconds and then never do the work
— which looks like the bot ignoring people rather than like a misconfiguration. Anywhere you
deploy this, pin a warm instance. This rule is the reason the AWS folder builds Fargate rather
than App Runner, which throttles idle CPU with no way to turn it off.

**`MASTER_KEY`, backed up somewhere else.** It seals every stored credential with AES-256-GCM:
Slack bot tokens, connection secrets, per-user OAuth tokens, TOTP secrets. Lose it and all of
them have to be reconnected by hand. `bootstrap.sh` generates one and then tells you to back it
up; that instruction is the important half.

## Storage

Two things are kept: a database and a folder of documents. Both default to a local volume, and
both can be something you bring — `DATABASE_URL` for any Postgres, `DOCS_S3_URL` for any
S3-compatible bucket. Naming that bucket also streams the SQLite database into it, so a single
instance survives losing its disk without a database server. **For a single deployment, use the
default**; bring your own when you want more than one replica or your platform wipes a
container's disk when it restarts.

One caveat that matters either way: **SQLite needs a local block device and exactly one writer.**
Not NFS, not SMB — so not EFS and not Azure Files. A Docker volume, an EBS disk, a Fargate task's
own ephemeral storage or a Kubernetes RWO PersistentVolumeClaim are all fine. Documents are
ordinary file I/O and do not care.

[`docs/storage.md`](docs/storage.md) has the per-environment recipes and where to get a managed
Postgres or a bucket.

## Somewhere none of these covers

The release workflow publishes the image as `ghcr.io/attest-tag/attesttag`, for `linux/amd64`
and `linux/arm64`, signed, with provenance and an SBOM, each time a `v*` tag is pushed — with the
Helm chart at `oci://ghcr.io/attest-tag/charts/attest-tag` and a GitHub release that names them.
None has been yet, so until the first release build it from this checkout
(`docker build -t attesttag-local .`). The fix-job worker images, `attesttag-worker` and
`attesttag-worker-jvm`, are published for `linux/amd64` only. Anywhere that runs a container and
can keep it warm will run this; the environment contract is [`env/selfhost.env.example`](env/selfhost.env.example)
and nothing about it is Docker-specific. Point it at a managed Postgres and a bucket and the
per-cloud folders stop mattering.
