# attest_tag on Kubernetes

```bash
helm install attest-tag ./deploy/helm/attest-tag \
  --namespace attest-tag --create-namespace \
  --set ingress.host=bot.example.com \
  --set secrets.MASTER_KEY="$MASTER_KEY" \
  --set secrets.SLACK_SIGNING_SECRET=... \
  --set secrets.SLACK_CLIENT_ID=... \
  --set secrets.SLACK_CLIENT_SECRET=... \
  --set secrets.OPENROUTER_API_KEY=...
```

`helm install` is the whole ceremony. There is no "one click" beyond that on Kubernetes and
nobody expects one. `$MASTER_KEY` is a key you made first — `openssl rand -base64 32` — and
backed up somewhere other than the cluster: it seals every stored credential, and generating one
inside the command line leaves the only copy in Helm's release history.

Each release also publishes this chart, pinned to that release's images, so from the first
release on the path can be the registry instead of a checkout:

```bash
helm install attest-tag oci://ghcr.io/attest-tag/charts/attest-tag --version <x.y.z> ...
```

## Before you run it

**Until the first release there is no image to pull.** The chart runs
`ghcr.io/attest-tag/attesttag` at its `appVersion`, `latest`, and nothing has been published there
yet. Build this checkout for your nodes' architecture, push it somewhere the cluster can pull
from, and add `--set image.repository=<registry>/attesttag --set image.tag=<tag>` to the install
(and `imagePullSecrets` if the registry is private):

```bash
docker buildx build --platform linux/amd64 -t <registry>/attesttag:<tag> --push .
```

**The ingress host must be reachable over https**, and must match `ADMIN_BASE_URL` — the chart
derives one from the other so they cannot disagree. Slack will not deliver events to anything
else, and there is no Socket Mode fallback in this bot. The chart creates an `Ingress`, so the
cluster needs an ingress controller, and the Ingress takes its certificate from a TLS Secret
named `<release>-attest-tag-tls` (or `ingress.tls.secretName`). Either let cert-manager make it
— put the issuer annotation in `ingress.annotations` — or create that Secret yourself; without
it the controller serves its own default certificate, which Slack refuses.

**`MASTER_KEY` seals every stored credential.** The image refuses to start without one, and the
chart refuses to render without `secrets.MASTER_KEY` for the same reason — unless the key is in
an `existingSecret`, which the chart cannot read. Back it up outside the cluster. Losing the
cluster and the key together means reconnecting every Slack workspace and every connection by
hand.

## Two shapes, and the chart works out which

**Local (the default).** A StatefulSet, one replica, two ReadWriteOnce volumes. Nothing to
operate and nothing to pay for. The database volume must be a **real block device** — any
default storage class over EBS, Persistent Disk, Azure Disk, Longhorn or local-path is fine;
**EFS and Azure Files are not**, because SQLite on a network filesystem corrupts quietly and is
noticed later.

**Bring your own, and then you can scale:**

```bash
helm install attest-tag ./deploy/helm/attest-tag \
  --namespace attest-tag --create-namespace \
  --set ingress.host=bot.example.com \
  --set secrets.MASTER_KEY="$MASTER_KEY" \
  --set secrets.SLACK_SIGNING_SECRET=... --set secrets.SLACK_CLIENT_ID=... \
  --set secrets.SLACK_CLIENT_SECRET=... --set secrets.OPENROUTER_API_KEY=... \
  --set database.url="postgres://user:pass@host:5432/attesttag?sslmode=require" \
  --set documents.s3.url="s3://my-bucket/docs?region=eu-west-2" \
  --set documents.s3.keyId=... --set documents.s3.secret=... \
  --set replicaCount=3
```

No volumes are created and the chart renders a **Deployment** instead of a StatefulSet. What
makes several replicas safe is not the absence of volumes: the routine scheduler claims each
routine on its row, the loops that must run once sit behind leader leases, and the rate limits
count in the database.

Ask for more than one replica while anything is still local and it **refuses to render**,
naming which half is local and why a second writer would corrupt it rather than scale it.

[`../docs/storage.md`](../docs/storage.md) covers where to get a managed Postgres or a bucket.

## Secrets

`--set` puts secrets into Helm's release history in plain text, which is fine for trying this
out and not fine for anything else. For real use, manage a `Secret` yourself — External
Secrets, Sealed Secrets, SOPS — with the same keys, and point the chart at it:

```yaml
existingSecret: attest-tag-secrets
```

The chart then creates no Secret of its own and reads every value from yours. The keys it
expects are `MASTER_KEY`, `SLACK_SIGNING_SECRET`, `SLACK_CLIENT_ID`, `SLACK_CLIENT_SECRET`,
`OPENROUTER_API_KEY`, and — if you brought storage — `DATABASE_URL`, `DOCS_S3_URL`,
`DOCS_S3_KEY_ID` and `DOCS_S3_SECRET`, and — if the bot is on Microsoft Teams too —
`MSTEAMS_APP_PASSWORD`. If instead the chart runs its own Postgres or MinIO, add
`POSTGRES_PASSWORD` and `MINIO_ROOT_PASSWORD` to your Secret: the chart cannot generate a
password into a Secret it does not own, and it will say so rather than start something that
cannot log in.

Made by hand from the `.env` that `bootstrap.sh` writes, with its values unquoted, that is:

```bash
kubectl create namespace attest-tag
kubectl create secret generic attest-tag-secrets -n attest-tag \
  --from-env-file=<(grep -E '^(MASTER_KEY|SLACK_SIGNING_SECRET|SLACK_CLIENT_ID|SLACK_CLIENT_SECRET|OPENROUTER_API_KEY)=.' .env)
helm install attest-tag ./deploy/helm/attest-tag -n attest-tag \
  --set existingSecret=attest-tag-secrets --set ingress.host=bot.example.com
```

Two things are then yours to get right, because the chart cannot see inside your Secret. It
checks for none of the keys, so a missing one is a pod that will not start rather than an
install that is refused. And it still picks its shape from `database.url` and
`documents.s3.url` in values, not from your Secret: with your storage only in the Secret, it
renders the one-replica StatefulSet with volumes. For the Deployment, set both values to a
placeholder such as `in-secret`. The pod never reads them when `existingSecret` is set, so the
real `DATABASE_URL` and `DOCS_S3_*` keys must be in the Secret. A missing `DATABASE_URL` is a pod
that refuses to start — the chart sets `REQUIRE_POSTGRES=1` whenever it has been told the rows
are in Postgres — but missing `DOCS_S3_*` keys put each pod's documents on a disk of its own that
disappears with it.

## Storage it runs for you

Two values and the release brings its own database and its own bucket, each a StatefulSet with
one replica and one volume:

```bash
helm install attest-tag ./deploy/helm/attest-tag \
  --namespace attest-tag --create-namespace \
  --set ingress.host=bot.example.com \
  --set secrets.MASTER_KEY="$MASTER_KEY" \
  --set secrets.SLACK_SIGNING_SECRET=... --set secrets.SLACK_CLIENT_ID=... \
  --set secrets.SLACK_CLIENT_SECRET=... --set secrets.OPENROUTER_API_KEY=... \
  --set postgres.enabled=true --set minio.enabled=true \
  --set replicaCount=2
```

The bot then keeps nothing locally, so it renders as a Deployment and `replicaCount` may be
raised. Passwords are generated on install, kept in the release's Secret, and read back from it
on upgrade — an upgrade that regenerated them would lock the release out of its own database.

Turn on one and not the other and you get the half-and-half shape: `minio.enabled` alone leaves
the database as SQLite on a volume, replicated into that MinIO and restored from it on boot,
still one replica. `postgres.enabled` alone leaves the documents on a volume, also one replica.

This is not high availability — one Postgres, one MinIO, one volume each — and nothing backs
them up. For anything you would be sad to lose, `database.url` and `documents.s3.url` point the
same chart at a managed Postgres and a real bucket, which is the shape the hosted service runs.

## The fix-job worker

Optional and off by default. `--set worker.enabled=true` lets the bot create a `batch/v1` Job per
"fix this and raise a PR" request, running the worker image against a repository you have
connected:

```bash
helm upgrade attest-tag ./deploy/helm/attest-tag -n attest-tag --reuse-values \
  --set worker.enabled=true
```

`--reuse-values` keeps what the install set, the secrets included; without it the upgrade starts
again from the chart's defaults and refuses to render. The Job runs
`ghcr.io/attest-tag/attesttag-worker`, which is published only once a release is cut and only
for `linux/amd64` — so the bot pins each worker pod to amd64 nodes (`kubernetes.io/arch`), and a
cluster with no amd64 node needs an image of its own. `worker.arch` moves the pin, and `any` lifts
it for an image built for every architecture you run. Until the first release, build
[`Dockerfile.worker`](../../Dockerfile.worker) for `linux/amd64`, push it, and set
`worker.image.repository` and `worker.image.tag` to it.

Nothing else to install: the bot talks to the API server with its own mounted service account
token, and the chart adds the RBAC for it.

| | |
|---|---|
| `Role` + `RoleBinding` | `create`, `get`, `list`, `watch`, `delete` on `batch/jobs` — **namespaced**, bound to this release's service account. There is no `ClusterRole`: a bot that can create Jobs anywhere in the cluster is a different risk from one that can create them beside itself |
| `ServiceAccount` | the worker's own, granted nothing, with `automountServiceAccountToken: false` — set here *and* on the pod spec the bot writes, because either one alone is an upgrade away from silently flipping back |

`delete` is there because that is what cancelling a fix job is: the Job goes and, with background
propagation, the pod goes with it. Without it, Cancel in the console would report success and
leave the worker running to completion.

`worker.namespace` must be this release's own namespace — a `Role` created anywhere else would
land in the wrong place, and the chart refuses to render any other value rather than applying
it, whatever you have created there yourself. Workers run beside the bot.

Route heavier ecosystems to a second image with `worker.jobNames`, and size each job with
`worker.resources.cpu` and `worker.resources.memory`. The list is comma-separated and `--set`
splits on commas, so put it in a values file and pass that with `-f`:

```yaml
worker:
  jobNames: "java=<registry>/attesttag-worker-jvm:<tag>,java-gradle=<registry>/attesttag-worker-jvm:<tag>,dotnet=<registry>/attesttag-worker-jvm:<tag>"
```

[`../docs/platforms.md#the-fix-job-worker`](../docs/platforms.md#the-fix-job-worker) is what this
is and why it is a separate container.

## Probes

Worth knowing when a pod is Running but not Ready:

- **liveness is `/health`**, which answers from the moment the process is listening, before
  the database is open. It only asks whether the process is alive.
- **readiness and startup are `/admin/`**, which returns 503 until the store is ready and 503
  again while draining. That is exactly the readiness semantics, so no extra endpoint was
  needed — and a 200 also proves the console was really embedded in the binary.

A pod stuck in `Running`, not `Ready`, is almost always still opening the database. The
startup probe allows five minutes before giving up.

## Checking it

The chart names everything `<release>-attest-tag`, so for the release above that is
`attest-tag-attest-tag`, and it is a StatefulSet while anything is local and a Deployment once
nothing is. The notes `helm install` prints give the exact command.

```bash
kubectl rollout status statefulset/attest-tag-attest-tag -n attest-tag   # or deployment/…
kubectl port-forward -n attest-tag svc/attest-tag-attest-tag 8080:8080
curl -fsS localhost:8080/health          # ok
```

Then generate the Slack app definition with the host you configured:

```bash
BASE_URL=https://bot.example.com ./deploy/slack/manifest.sh
```

## Values

`values.yaml` is commented throughout; the ones that matter are `image.tag` (pin the moving
minor, e.g. `"1.4"`, once releases exist, so a restart brings fixes and never a surprise — and
pin `worker.image.tag` with it, because it does not follow), `ingress.host`,
`persistence.*.storageClass`, and `existingSecret`.
