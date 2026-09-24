# Storage: bring your own, run it in the box, or keep it local

> Choosing a platform rather than a storage shape? [`platforms.md`](platforms.md) has the two
> or three ways each cloud can run this, and what each costs you in moving parts.

attest_tag keeps two things: a **database** and a folder of **documents**. Each has an answer
that needs nothing configured and an answer you bring, and you pick per deployment rather than
per platform.

| | Default — nothing to configure | Bring your own |
|---|---|---|
| Database | SQLite on a volume | `DATABASE_URL` → any Postgres |
| Documents | a folder on a volume | `DOCS_S3_URL` → any S3-compatible bucket |

**For a single deployment, use the default.** One container, one volume, nothing to operate,
and no monthly bill for a managed database. That is not a demo mode — it is what the hosted
service ran on for months.

Bring your own when you want **more than one replica**, or when you are on a platform that
gives a container **no persistent disk** (Cloudflare Containers) or whose **persistent volumes
are NFS or SMB** (EFS on Fargate, Azure Files on Container Apps), which SQLite must not be run
on. Their own task disk suits SQLite, but it is wiped on every restart.

Set both and there is no local state at all: the workload becomes stateless and can be scaled.
Set one, and you are still pinned to a single replica by the other.

### Naming a bucket also replicates the database

There is a third answer for the database in that table, and it arrives without being asked for:
**give the documents a bucket and the SQLite file is streamed into it too**, second by second,
and restored from it when a container starts with an empty disk. No extra variable, and no
Postgres.

| the database is | durability | run it when |
|---|---|---|
| **SQLite on a volume** (default) | as good as the disk, and whatever you back it up with | one box you keep |
| **SQLite + a bucket** (`DOCS_S3_URL`) | continuously replicated off the box; a restart restores it | one instance whose disk you do not trust, or a platform that wipes it |
| **Postgres** (`DATABASE_URL`) | the database server's own | more than one instance |

The middle row is still **exactly one writer** — replication is not clustering, and SQLite
allows one writer wherever the bytes end up. What makes that safe is a lock object beside the
replica, described below. Set `DATABASE_URL` as well and the replica is simply not used: the
rows are in Postgres, and the bot says so at startup.

---

## Docker

**Local, which is the answer for one box:**

```bash
./deploy/local/bootstrap.sh
docker compose --profile quicktunnel up -d
```

Until the first release there is no image to pull, so build one first, as
[`../local/README.md`](../local/README.md) shows.

Two named volumes, `data` and `docs`. Snapshot the disk underneath, or stop the bot and copy the
volume — a copy of a SQLite file taken while it is being written can be inconsistent:

```bash
docker compose stop attesttag
docker run --rm -v attest-tag_data:/d -v "$PWD":/b alpine tar czf /b/data.tgz /d
docker compose start attesttag
```

**Postgres and MinIO in the same compose project**, if you want them without an account
anywhere. Each is a profile and a second compose file, because a compose file cannot say "set
this variable only under that profile":

```bash
# a Postgres beside the bot
docker compose -f docker-compose.yml -f deploy/local/postgres.yml --profile postgres up -d

# an S3 bucket beside the bot: MinIO, and the documents and the database replica go into it
docker compose -f docker-compose.yml -f deploy/local/minio.yml --profile minio up -d

# both
docker compose -f docker-compose.yml -f deploy/local/postgres.yml -f deploy/local/minio.yml \
  --profile postgres --profile minio up -d
```

`bootstrap.sh` generates a password for each, neither is reachable from outside the machine
(MinIO is published on loopback, Postgres not at all), and each gets a named volume. A one-shot
`minio-init` container creates the bucket before the bot starts — it is the bot's own image
running `attesttag bucket-init`, which also proves the credentials can write, read and delete,
and says whether the store enforces the conditional writes the write lease needs.

Tired of the flags? Compose reads these from `.env` for itself, and then `docker compose up -d`
is the whole command:

```bash
COMPOSE_FILE=docker-compose.yml:deploy/local/postgres.yml:deploy/local/minio.yml
COMPOSE_PROFILES=postgres,minio,quicktunnel
```

**Your own Postgres and bucket** — put them in `.env` and change nothing else:

```bash
DATABASE_URL=postgres://user:pass@host:5432/attesttag?sslmode=require
DOCS_S3_URL=s3://my-bucket/docs?endpoint=https://acct.r2.cloudflarestorage.com&region=auto
DOCS_S3_KEY_ID=...
DOCS_S3_SECRET=...
```

## Kubernetes

Every install below carries the bot's five secrets: the chart refuses to render without the
Slack three and `OPENROUTER_API_KEY`, and the pod will not start without `MASTER_KEY` —
`$MASTER_KEY` below is one you generated with `openssl rand -base64 32` and backed up first.
[`../helm/README.md`](../helm/README.md) shows how to keep them in a Secret of your own instead,
and what to do about the image until the first release.

**Local** — the chart's default. A StatefulSet, one replica, two ReadWriteOnce volumes:

```bash
helm install attest-tag ./deploy/helm/attest-tag \
  --set ingress.host=bot.example.com \
  --set secrets.MASTER_KEY="$MASTER_KEY" \
  --set secrets.SLACK_SIGNING_SECRET=... --set secrets.SLACK_CLIENT_ID=... \
  --set secrets.SLACK_CLIENT_SECRET=... --set secrets.OPENROUTER_API_KEY=...
```

The database volume must be a **real block device**. Any default storage class backed by EBS,
Persistent Disk, Azure Disk, Longhorn or local-path is fine; EFS and Azure Files are not.

**Or let the chart run them**, which is the same trade as the compose profiles above — one
Postgres StatefulSet, one MinIO StatefulSet, one volume each, passwords generated into the
release's Secret and read back from it on upgrade:

```bash
helm install attest-tag ./deploy/helm/attest-tag \
  --set ingress.host=bot.example.com \
  --set secrets.MASTER_KEY="$MASTER_KEY" \
  --set secrets.SLACK_SIGNING_SECRET=... --set secrets.SLACK_CLIENT_ID=... \
  --set secrets.SLACK_CLIENT_SECRET=... --set secrets.OPENROUTER_API_KEY=... \
  --set postgres.enabled=true --set minio.enabled=true \
  --set replicaCount=2
```

The bot then keeps nothing locally, so the chart renders a Deployment and `replicaCount` may be
raised. It is not high availability — one of each, one volume each — and nothing backs them up.

**Bring your own, and then you can scale:**

```bash
helm install attest-tag ./deploy/helm/attest-tag \
  --set ingress.host=bot.example.com \
  --set secrets.MASTER_KEY="$MASTER_KEY" \
  --set secrets.SLACK_SIGNING_SECRET=... --set secrets.SLACK_CLIENT_ID=... \
  --set secrets.SLACK_CLIENT_SECRET=... --set secrets.OPENROUTER_API_KEY=... \
  --set database.url="postgres://user:pass@host:5432/attesttag?sslmode=require" \
  --set documents.s3.url="s3://my-bucket/docs?region=eu-west-2" \
  --set documents.s3.keyId=... --set documents.s3.secret=... \
  --set replicaCount=3
```

The chart notices: no volumes are created, and it renders a **Deployment** instead of a
StatefulSet. Ask for more than one replica while anything is still local and it refuses to
render, naming which half is local and why a second writer would corrupt it.

For anything real, keep the credentials in a `Secret` you manage — External Secrets, Sealed
Secrets, SOPS — and set `existingSecret` to its name, with the keys `MASTER_KEY`,
`SLACK_SIGNING_SECRET`, `SLACK_CLIENT_ID`, `SLACK_CLIENT_SECRET`, `OPENROUTER_API_KEY`,
`DATABASE_URL`, `DOCS_S3_URL`, `DOCS_S3_KEY_ID` and `DOCS_S3_SECRET`. The pod then reads all of
them from that Secret alone, but the chart still picks its shape from `database.url` and
`documents.s3.url` in values: leave them empty and it renders the one-replica StatefulSet with
volumes. Set both to a placeholder such as `in-secret` — with `existingSecret` the pod never
reads them — and it renders the Deployment. `--set` puts values into Helm's release history in
plain text.

---

## The database in a bucket

[Litestream](https://litestream.io) streams the SQLite file to object storage continuously and
restores it on boot when the local file is gone. It ran the hosted service for months against
GCS, until the move to Cloud SQL on 2026-09-14; the same thing works against anything speaking
S3.

**Where it goes.** Under the documents prefix, at `litestream/attesttag.db` — so a bucket shared
by two deployments keeps two databases apart the same way it keeps their documents apart. No
document can collide with it: documents live under `<prefix>/org-<id>/`.

**One writer, enforced.** Beside the replica is `…/attesttag.db.lock.json`, a lease one
container holds and renews. Nothing touches the database before that lease is held, the restore
least of all, because restoring while another container is still writing is how a stale snapshot
gets promoted over live data. This is what makes a rolling restart safe rather than lucky.

The lease is built out of conditional writes — `If-None-Match` and `If-Match`, which are how S3
says "only if this does not exist" and "only if it is still this version". AWS S3, R2 and
current MinIO have them; some compatible stores do not, and a store that *ignores* a
precondition rather than refusing it would hand one lease to two writers and tell both they hold
it. So the bot proves the precondition on a scratch key at startup instead of trusting the
provider's documentation. A store that fails the proof still gets replication, with this in the
log and no lease:

```
this object store does not honour conditional writes, so there is no write lease:
replication is on, and this deployment must run exactly one instance
```

**A different bucket for the database** — a backup account, say, separate from the documents:

```bash
LITESTREAM_S3_URL=s3://backups/attesttag/db.sqlite?endpoint=https://…&region=auto
LITESTREAM_S3_KEY_ID=...        # falls back to DOCS_S3_KEY_ID when unset
LITESTREAM_S3_SECRET=...
```

**Restoring.** Nothing to do: a container that starts with no database at `DB_PATH` restores the
newest replica before it opens anything. To read the replica somewhere else — a laptop, an
incident — point the `litestream restore` CLI at the same bucket and key.

**How much history it keeps** is Litestream's own affair — 0.5 compacts its change files on a
schedule of its own, and this does not override it. Treat the replica as a current copy rather
than an archive: it gives you the database as it was moments ago, which is what a lost disk
needs and not what a mistake made last Tuesday needs. That one still wants a backup you took.

---

## Getting a Postgres

Anything speaking Postgres 14 or newer works. The connection string is all this needs.

| Where | How | Roughly |
|---|---|---|
| **Neon**, **Supabase** | Create a project, copy the connection string. Free tiers exist and are enough to start | $0–20/mo |
| **AWS RDS** | `db.t4g.micro`, same VPC as the app | ~$12/mo |
| **Google Cloud SQL** | `db-g1-small`; put a `host=/cloudsql/…` `DATABASE_URL` in the env file and `cloudrun.sh` attaches the instance ([`../gcp/README.md`](../gcp/README.md)) | ~$25/mo |
| **Azure Database for PostgreSQL** | Flexible Server, B1ms | ~$16/mo |
| **DigitalOcean**, **Hetzner** | Managed Postgres, one click | ~$15/mo |
| **Your own** | `docker compose -f docker-compose.yml -f deploy/local/postgres.yml --profile postgres up -d`, or a Postgres you already run | — |

Append `?sslmode=require` for anything reached over a network you do not own. The schema is
created on first boot; no migration step to run by hand.

**Or let the deploy script make one:** `CREATE_DATABASE=1` on AWS and Azure creates the managed
Postgres, wires it up and appends the DSN to your env file. See [`../aws/README.md`](../aws/README.md)
and [`../azure/README.md`](../azure/README.md).

## Getting an S3 bucket

Any S3-compatible store. The documents backend asks each of them the same six ordinary
requests — list, get, put, delete, head and a server-side copy — and nothing provider-specific.

| Where | Endpoint | Region | Notes |
|---|---|---|---|
| **Cloudflare R2** | `https://<account>.r2.cloudflarestorage.com` | `auto` | No egress fees. Pairs well with Cloudflare Containers |
| **AWS S3** | omit — derived from the region | your region | |
| **MinIO** (self-hosted) | `http://minio:9000` | `us-east-1` | Runs beside the bot in compose or in-cluster |
| **DigitalOcean Spaces** | `https://<region>.digitaloceanspaces.com` | your region | |
| **Backblaze B2** | `https://s3.<region>.backblazeb2.com` | your region | Cheapest storage |
| **Wasabi**, **Ceph**, **Garage** | their endpoint | theirs | |

The bucket needs no public access and no website configuration — the bot reads and writes it
with a signed request, and nothing else ever touches it. A key with read and write on that one
bucket is all the permission it wants.

`region` is required by the signature even where the provider ignores it; R2 wants `auto`.

---

## Moving from local to your own

Documents: copy the folder into the bucket under the same layout — `docs/org-<id>/…` — and set
the three `DOCS_S3_*` variables. The next ingest picks them up. The database starts replicating
into the same bucket on the next restart, with nothing else to set.

Database: there is a tool and a runbook for it, because the sealed credentials have to arrive
byte for byte. See [`postgres-cutover.md`](postgres-cutover.md).

## What must be backed up either way

**`MASTER_KEY`.** It seals every stored credential — Slack bot tokens, connection secrets,
per-user OAuth tokens, TOTP secrets. Neither Postgres nor a bucket helps if it is lost: the rows
survive and nothing can read them. Keep it somewhere other than the machine it runs on.

And a replica is not a backup of your own making: it follows every write, including the
destructive ones, within seconds. A `pg_dump`, a volume snapshot, or a periodic copy of the
SQLite file is the thing that survives a mistake made an hour ago.
