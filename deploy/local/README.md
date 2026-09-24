# Docker, and one machine

The shortest way to a running deployment, and the right answer whenever you would rather
operate one machine than five services. A laptop, a VM at Hetzner or DigitalOcean, an EC2
instance, a box under a desk.

```bash
./deploy/local/bootstrap.sh
docker compose --profile quicktunnel up -d
docker compose logs quicktunnel      # the https URL to use as BASE_URL
```

`bootstrap.sh` writes `.env` from [`../env/selfhost.env.example`](../env/selfhost.env.example)
and generates `MASTER_KEY`. It refuses to overwrite an existing `.env`, which is not politeness:
a second run that generated a new key would leave every connected workspace and saved connection
undecryptable, with nothing to say so until something tried to use one.

Nothing has been released yet, so there is no published image to pull. Until the first release,
build one from this checkout before the first `up -d`, and put `ATTEST_IMAGE=attesttag-local`
in `.env` so that compose runs it:

```bash
docker build -t attesttag-local .
```

The first `up -d` starts the bot as well, and it restarts in a loop until `.env` has the three
Slack values and the model key. That is expected: the Slack app needs the tunnel's address
before it exists. Make the app, fill those in with `ADMIN_BASE_URL` set to the same address, and
run `docker compose --profile quicktunnel up -d` again: it recreates the bot with the new `.env`
and leaves the tunnel, and its name, alone.

`docker-compose.yml` is in the repository root, because that is where people look for it.

| | |
|---|---|
| `bootstrap.sh` | Writes `.env`, generates `MASTER_KEY` and the two in-box passwords |
| `postgres.yml` | A Postgres in the same compose project (`--profile postgres`) |
| `minio.yml` | An S3 bucket in the same compose project (`--profile minio`) |
| `Caddyfile` | For the `caddy` profile — a real certificate on a domain you point here. Needs `ATTEST_DOMAIN` from `.env`, and takes `ATTEST_ACME_EMAIL` as the certificate authority's contact address if you set one |
| `install-launchd.sh`, `com.attesttag.bot.plist` | Running the binary as a macOS LaunchAgent, for development |

## Everything in the box

Want the database and the bucket beside the bot rather than an account somewhere?

```bash
docker compose -f docker-compose.yml -f deploy/local/postgres.yml -f deploy/local/minio.yml \
  --profile postgres --profile minio --profile caddy up -d
```

The `caddy` profile needs `ATTEST_DOMAIN` in `.env` first; swap it for
`quicktunnel` or `tunnel` to get the address another way. Generated passwords, on loopback,
backed by named volumes. Nothing to sign up for and nothing leaves the machine — which also
means **the backups are yours**: snapshot the volumes or the disk. Set `COMPOSE_FILE` and
`COMPOSE_PROFILES` in `.env` and `docker compose up -d` becomes the whole command again; the
pair to uncomment is in the env file.

## HTTPS

Not optional — Slack will not deliver to localhost and will not accept an http redirect URL.
Three profiles cover it, and [`../docs/https.md`](../docs/https.md) explains the trade:

| | |
|---|---|
| `--profile quicktunnel` | A `trycloudflare.com` name, no account, two minutes. The hostname changes on every restart, so it is for trying this out |
| `--profile tunnel` | A named Cloudflare tunnel on a domain you own. Nothing listens on the public internet |
| `--profile caddy` | Ports 80 and 443 on this machine, with a certificate Caddy gets itself. Needs `ATTEST_DOMAIN` in `.env`; `ATTEST_ACME_EMAIL` is optional |

With a quick tunnel, set `ADMIN_BASE_URL` to its current address and run `docker compose up -d`
with the same flags as before — not `docker compose restart`, which restarts the tunnel as well
and does not reread `.env`. Each restart of the tunnel gives it a new name, and `ADMIN_BASE_URL`
and the Slack app's URLs have to follow it by hand: the bot does not learn a new host once it
has learned one, so links and Slack and Microsoft sign-ins keep going to the old name.
`PUBLIC_ORIGIN_HOSTS` is for a fixed set of names, not a changing one.

## The fix-job worker

Optional and off by default, and on this shape it is the one thing worth reading twice. Adding
[`worker.yml`](worker.yml) lets the bot start a container per fix request on this machine's
Docker daemon:

```bash
docker compose -f docker-compose.yml -f deploy/local/worker.yml up -d
```

It sets `WORKER_MODE=workers`, which resolves to `docker` from the socket it mounts. There is no
worker service in that file — nothing sits idle. It changes the bot's own service,
and the change is that **the Docker socket is mounted into it, which is root on this host**.
Everything else in the compose file the bot could already reach; the host was not on that list.

It needs one setting in `.env` first, and refuses to start without it: `ATTEST_DOCKER_GID`, the
id of the group that owns the socket. The bot runs as uid 10001, and on a Linux host the socket is
usually `root:docker` with mode 0660, so without that group every job fails to reach the daemon.
The id is whatever the host's `groupadd` handed out, so it is asked for rather than guessed. This
prints it as a container sees the socket, which is the view that counts — on Linux, Docker
Desktop and Colima alike:

```bash
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock alpine stat -c %g /var/run/docker.sock
```

The managed platforms do not need this. Cloud Run, ECS, Container Apps and Kubernetes each start
the worker through an API that can be scoped to "start this one job and nothing else". Docker's
API has no such scope — it is all of it or none. On a machine that is yours and runs nothing
else that is a reasonable trade, and on a host shared with anything you would not hand over it is
not.

It is an overlay you add deliberately rather than a profile you might switch on by accident, and
the bot logs the trade on every boot in that mode.

Two things the overlay does not do for you:

- **The image.** It runs `ghcr.io/attest-tag/attesttag-worker:latest` unless
  `ATTEST_WORKER_IMAGE` names another, and that image is published only once a release is cut,
  for `linux/amd64` only. Until then — and on an arm64 host (Apple silicon, Graviton) after it
  too — build it here and put `ATTEST_WORKER_IMAGE=attesttag-worker` in `.env`.
  `make worker-build` runs `docker build -f Dockerfile.worker -t attesttag-worker .`.
- **Upgrades.** The bot pulls the image only when the daemon does not have it, so after an
  upgrade run `docker pull ghcr.io/attest-tag/attesttag-worker:latest`, or `make worker-build`
  again. Upgrading the bot does not touch it.

[`../docs/platforms.md#the-fix-job-worker`](../docs/platforms.md#the-fix-job-worker) is what this
is on every other platform.

## One writer

SQLite, by default, on a Docker volume. That is a local block device and exactly one process
writes it, which is what SQLite needs. Do not move the database onto NFS or SMB — not an NFS
mount, not a CIFS share — the corruption it causes is quiet and noticed later. Documents are
ordinary file I/O and do not care.

`stop_grace_period` is 30 seconds because the write lease wants about nine to be released
cleanly, and Docker's default of ten leaves no room for the rest of the shutdown.
