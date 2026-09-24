# Moving a running deployment from SQLite to Postgres

About ten minutes of downtime, at the quietest hour you have. Rehearse it twice against a copy
before you do it once against the real thing.

The hosted service made this move on 2026-09-14. The commands are for Cloud Run and
`deploy/gcp/cloudrun.sh` — run with `ENV_FILE=.env` on a self-host — and another platform does
the same steps with its own deploy command.

## What the freeze does, and the one thing it must not do

`MAINTENANCE=1` stops the deployment writing: the schedulers do not start, queued Slack
deliveries are not dispatched, documents are not ingested.

Slack events during the freeze are **acknowledged with 200 and dropped**, not refused. That is
not politeness. Slack disables event delivery to an app that fails more than about 95% of its
deliveries, and the only way back is a reinstall by every connected workspace — so a freeze that
answered 503 would turn ten quiet minutes into an outage that needs other people to fix it.
`TestMaintenanceAcknowledgesSlackRatherThanRefusing` pins that, and `url_verification` is still
answered so the Request URL can be re-verified mid-freeze if you need to.

Microsoft Teams activities are answered the same way, once their token checks out: 200, dropped,
and a log line (`TestMaintenanceDropsATeamsActivity`). Every one of them writes something — the
person, the conversation, the message log — to the database you are about to replace.

The events Slack and Teams send during the window are gone. At night that is a handful of
messages, and the log carries each of their ids.

## Before the day

1. Create the instance and an empty database. For Cloud SQL: enable `sqladmin.googleapis.com`,
   create the instance, the `attesttag` database and its user, and bind `roles/cloudsql.client`
   to the service account the bot runs as.
2. Have the DSN ready —
   `postgres://attesttag:<password>@/attesttag?host=/cloudsql/<project>:<region>:<instance>` —
   and keep it out of the env file until step 5. `cloudrun.sh` moves the service to Postgres on
   the first run that finds it there, so any deploy before then would make the empty database
   the live one.
3. **Rehearse.** Take a snapshot the way step 3 below does, import it into a throwaway database,
   run the suite against it, and then drop it and do it again:

   ```bash
   TEST_DATABASE_URL="postgres://…/rehearsal" go test ./internal/app
   ```

   The second rehearsal is the one that matters: the first tells you the recipe works, the
   second tells you *you* can follow it at two in the morning.

## The cutover

**1. Freeze.** Set `MAINTENANCE=1` on the running service, still on SQLite:

```bash
gcloud run services update attesttag --project <project> --region <region> \
  --update-env-vars MAINTENANCE=1
```

No deploy script passes `MAINTENANCE` through, and the next `cloudrun.sh` run replaces every
variable, which drops it. Writes stop; Slack is acked and dropped; Litestream keeps syncing.

**2. Wait 60 seconds**, then check the container is alive and the replica is current: `renewed_at`
in the lock object is still advancing, and the newest LTX file is younger than the deploy.

**3. Snapshot.** Copy the replica down and restore from the copy, never from the live replica
path. The bucket is `attesttag-data-<project>` unless `BUCKET` named another, and the
`litestream` CLI should be 0.5, the version in the image:

```bash
gcloud storage rsync -r gs://<bucket>/litestream/attesttag-v2.db ./replica/attesttag-v2.db
litestream restore -o snap.db "file://$PWD/replica/attesttag-v2.db"
```

**4. Import and verify.**

```bash
./attesttag pg-import --sqlite snap.db --to "$DATABASE_URL"
```

`./attesttag` is this checkout built with `make build`. Run it where the DSN reaches the
database: a `host=/cloudsql/…` DSN needs the Cloud SQL Auth Proxy serving that socket.

It refuses a target that is not empty, copies every table carrying the ids across, moves each
identity sequence past the largest id that arrived, and then **compares both databases row for
row and byte for byte** — a hash over every value of every row, computed the same way on each
side. Nine columns hold sealed credentials, and a byte lost in one of them is every credential
in that table gone, found later, by somebody whose bot has quietly stopped reaching anything.

**Any mismatch stops here.** Lift the freeze and you have lost nothing but the window:

```bash
gcloud run services update attesttag --project <project> --region <region> \
  --remove-env-vars MAINTENANCE
```

**5. Cut over.** Put the DSN in the env file and rerun `cloudrun.sh`. It adds the instance to
the service, drops `LITESTREAM_*` and `DB_PATH`, sets `REQUIRE_POSTGRES=1`, and — because it sets
every variable afresh — drops `MAINTENANCE` too. It boots in seconds: no lease to take, no
replica to restore.

**6. Smoke test**, in this order, because each one proves something the next depends on:

- `/health` answers.
- Sign in to the console with TOTP — sessions and the sealed TOTP secret both survived.
- `!whoami` in Slack — the sealed bot token decrypts and `auth.test` accepts it.
- One proxied connection call — a sealed connection secret decrypts.
- A routine "run now" — the scheduler writes.
- Mention the bot in a real channel and get a reply.

**7. Leave the Litestream replica alone for fourteen days.** Nothing writes to it any more.

**8. Rollback**, if it comes to that: take `DATABASE_URL` out of the env file and rerun
`cloudrun.sh`, which puts `LITESTREAM_*` and `DB_PATH` back and detaches the instance.
`DATABASE_URL` beats `DB_PATH` precisely so that this is one variable rather than a different
image. Rows written to Postgres since the cutover are lost, so this is a decision to take within
the day, not the week.

## Afterwards

On Postgres more than one instance is safe: the routine scheduler claims each routine on its
row, and the loops that must run once sit behind leader leases (`singleton.go`). Raise the
ceiling with `MAX_INSTANCES=<n>` on every `cloudrun.sh` run — the script reads it from the shell
alone and sets 1 otherwise — and ignore the warning it prints about instances past the lease
holder answering 503, which is about SQLite and does not apply here.
