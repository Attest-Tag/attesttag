#!/usr/bin/env bash
# Deploy attest_tag to Azure Container Apps. Idempotent: safe to rerun, and rerunning is how you
# ship a new image.
#
#   DOMAIN=bot.example.com ./deploy/azure/containerapps.sh
#
# Requires: the az CLI logged in (`az login`) with a subscription selected, and python3. Reads
# secrets from $ENV_FILE (default .env — the file deploy/local/bootstrap.sh writes).
#
# ── Why Container Apps works here and App Runner does not ──
#
# This bot does its real work *between* requests: the Slack inbox dispatcher, the routine
# scheduler, the ingest loop, Drive sync. A platform that freezes an idle container's CPU acks
# Slack's event in three seconds and then never does the work. On Container Apps "idle" is a
# billing state, not a throttle — Azure's own rule is that a replica counts as idle only while
# it uses less than 0.01 vCPU, so a replica doing background work simply gets billed at the
# active rate and keeps its CPU. minReplicas: 1 below is what stops it scaling to zero.
#
# ── Storage, which is the one thing Azure makes awkward ──
#
# Azure Blob has no S3-compatible API and Azure Files is SMB, which SQLite must not be run on
# any more than NFS. So there are exactly two shapes that work, and this script picks whichever
# one $ENV_FILE describes:
#
#   DOCS_S3_URL set      SQLite on the replica's own disk, streamed to that bucket and restored
#                        on boot; documents in the same bucket. The bucket comes from Cloudflare
#                        R2, Backblaze B2, Wasabi or a MinIO you run. One replica.
#
#   DATABASE_URL set     Rows in Azure Database for PostgreSQL, documents on an Azure Files
#                        share mounted at /app/docs. Entirely within Azure, because documents
#                        are ordinary file I/O and do not care that the share is SMB. This is
#                        the shape that can run more than one replica.
#
# Setting both is fine and means Postgres plus a bucket: the binary prefers DATABASE_URL over
# DB_PATH and turns the SQLite replica off by itself (internal/app/config.go).
set -euo pipefail
cd "$(dirname "$0")/../.."

: "${LOCATION:=eastus}"
: "${RESOURCE_GROUP:=attesttag}"
: "${APP:=attesttag}"
: "${ENV_NAME:=attesttag-env}"
: "${ENV_FILE:=.env}"
: "${IMAGE:=ghcr.io/attest-tag/attesttag:latest}"
: "${CPU:=1.0}"
: "${MEMORY:=2.0Gi}"
: "${REPLICAS:=1}"

command -v az >/dev/null 2>&1 || { echo "the az CLI is not on PATH — https://learn.microsoft.com/cli/azure/install-azure-cli" >&2; exit 1; }
# python3 writes the app spec and the role below. Checked up front, because missing it used to
# surface only at the spec, after the resource group, the environment and — with
# CREATE_DATABASE=1 — a Postgres that bills by the month had been created.
command -v python3 >/dev/null 2>&1 || { echo "python3 is not on PATH" >&2; exit 1; }
[ -f "$ENV_FILE" ] || { echo "$ENV_FILE missing — run ./deploy/local/bootstrap.sh first, or set ENV_FILE" >&2; exit 1; }

# The last line with a value wins, here and in the spec writer's own envval below: the template
# holds an empty line for every key, and reading the first one let that empty line hide a value
# appended below it — by hand, or by CREATE_DATABASE=1 further down.
envval() {
  grep -E "^$1=." "$ENV_FILE" | tail -n 1 | cut -d= -f2- | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/"
}

# MASTER_KEY seals every stored credential, and the image refuses to start without one
# (REQUIRE_MASTER_KEY in the Dockerfile), so a revision deployed without it only crash-loops.
[ -n "$(envval MASTER_KEY)" ] || { echo "MASTER_KEY is missing from $ENV_FILE. Generate one with: openssl rand -base64 32" >&2; exit 1; }
[ -n "$(envval SLACK_SIGNING_SECRET || true)" ] || { echo "SLACK_SIGNING_SECRET is missing from $ENV_FILE. Copy it from Slack → Basic Information → App Credentials." >&2; exit 1; }
# The OAuth pair an install runs on. Without it the service refuses to start, so a revision
# deployed without it would just crash-loop.
for K in SLACK_CLIENT_ID SLACK_CLIENT_SECRET; do
  [ -n "$(envval "$K" || true)" ] || { echo "$K is missing from $ENV_FILE. Connecting a workspace is an OAuth install and the service will not start without it: Slack → Basic Information → App Credentials, and register <base>/slack/oauth/callback as a redirect URL." >&2; exit 1; }
done
[ -n "$(envval RESEND_API_KEY || true)" ] || echo "▸ warning: RESEND_API_KEY is unset — verification, reset and invitation links will be logged rather than emailed. The console offers them to copy by hand, so this is usable; it is not silent."

DOCS_URL="$(envval DOCS_S3_URL || true)"
DB_URL="$(envval DATABASE_URL || true)"
if [ -z "$DOCS_URL" ] && [ -z "$DB_URL" ] && [ "${CREATE_DATABASE:-0}" != "1" ]; then
  cat >&2 <<'WHY'
Neither DOCS_S3_URL nor DATABASE_URL is set in the env file, and on Container Apps that
combination has nowhere durable to put anything: the replica's disk is wiped on every restart,
Azure Blob speaks no S3 API, and Azure Files is SMB, which SQLite must not be run on.

Pick one and put it in the env file:

  DOCS_S3_URL=s3://bucket/docs?endpoint=https://acct.r2.cloudflarestorage.com&region=auto
  DOCS_S3_KEY_ID=…
  DOCS_S3_SECRET=…
      SQLite, streamed to that bucket and restored on boot; documents in the same bucket.
      Any S3-compatible store: Cloudflare R2, Backblaze B2, Wasabi, a MinIO you run.

  DATABASE_URL=postgres://user:pass@server.postgres.database.azure.com:5432/attesttag?sslmode=require
      Rows in Azure Database for PostgreSQL; this script then creates an Azure Files share for
      the documents and mounts it. Entirely within Azure.

  CREATE_DATABASE=1
      Create the Postgres for you: a Flexible Server in this resource group, with the DSN
      written back into the env file. It lists what it will bill you for and asks first.

deploy/docs/storage.md has both recipes in full.
WHY
  exit 1
fi

echo "▸ resource group=$RESOURCE_GROUP location=$LOCATION app=$APP replicas=$REPLICAS"
az provider register --namespace Microsoft.App --wait >/dev/null 2>&1 || true
az provider register --namespace Microsoft.OperationalInsights --wait >/dev/null 2>&1 || true
az extension add --name containerapp --upgrade --allow-preview false >/dev/null 2>&1 || true

az group show --name "$RESOURCE_GROUP" >/dev/null 2>&1 || \
  az group create --name "$RESOURCE_GROUP" --location "$LOCATION" >/dev/null
az containerapp env show --name "$ENV_NAME" --resource-group "$RESOURCE_GROUP" >/dev/null 2>&1 || \
  az containerapp env create --name "$ENV_NAME" --resource-group "$RESOURCE_GROUP" --location "$LOCATION" >/dev/null
ENV_ID="$(az containerapp env show --name "$ENV_NAME" --resource-group "$RESOURCE_GROUP" --query id -o tsv)"

# ── A managed Postgres, if you ask for one. The same option as deploy/aws/fargate.sh, and off
# for the same reason: it bills monthly, and a deployment pointed at a bucket does not need it.
# The DSN is appended to $ENV_FILE, which is what makes a rerun idempotent — DATABASE_URL is
# simply set the next time — and is the only copy of the password you will be able to read.
if [ "${CREATE_DATABASE:-0}" = "1" ] && [ -z "$DB_URL" ]; then
  SUBSCRIPTION="$(az account show --query id -o tsv)"
  # A Flexible Server name becomes <name>.postgres.database.azure.com, so it is globally unique
  # and derived from the subscription the same way the storage account name is.
  PG="${PG_SERVER:-$APP-pg-$(echo "$SUBSCRIPTION" | tr -d '-' | cut -c1-10)}"
  PG_TIER="${PG_TIER:-Burstable}"
  PG_SKU="${PG_SKU:-Standard_B1ms}"
  PG_STORAGE="${PG_STORAGE:-32}"
  PG_VERSION="${PG_VERSION:-16}"

  cat <<BILL

CREATE_DATABASE=1 will create these billable resources in subscription $SUBSCRIPTION:

  Azure Database for PostgreSQL Flexible Server
    $PG — $PG_TIER $PG_SKU, ${PG_STORAGE} GiB, version $PG_VERSION, in $RESOURCE_GROUP
    reachable from Azure services only; no public firewall rule is added

A Standard_B1ms is in the region of fifteen dollars a month with storage; check the current
price for $LOCATION rather than trusting this line. Leaving CREATE_DATABASE unset and setting
DOCS_S3_URL instead keeps the rows in SQLite streamed to a bucket, which needs no server.

BILL
  if [ "${ASSUME_YES:-0}" != "1" ]; then
    [ -t 0 ] || { echo "stdin is not a terminal and ASSUME_YES is unset — refusing to create billable resources that nobody confirmed" >&2; exit 1; }
    printf 'Create them? [y/N] '
    read -r REPLY
    case "$REPLY" in [yY]*) ;; *) echo "Nothing was created."; exit 1 ;; esac
  fi

  az provider register --namespace Microsoft.DBforPostgreSQL --wait >/dev/null 2>&1 || true
  if az postgres flexible-server show --name "$PG" --resource-group "$RESOURCE_GROUP" >/dev/null 2>&1; then
    # The server is there but the DSN is not in the env file, so its password is gone — it was
    # only ever written there. Resetting it is your decision, not this script's.
    echo "Flexible Server $PG already exists but DATABASE_URL is not in $ENV_FILE, so this script does not know its password. Put the DSN in $ENV_FILE by hand, or reset it with: az postgres flexible-server update --name $PG --resource-group $RESOURCE_GROUP --admin-password <new>" >&2
    exit 1
  fi
  # Stripped of everything that would need escaping in a URL; Azure rejects some of these in an
  # admin password in any case.
  PG_PASS="$(openssl rand -base64 30 | tr -d '/+=@" ' | cut -c1-24)"
  # --public-access 0.0.0.0 reads like "open to the internet" and is the opposite: it is Azure's
  # spelling for the AllowAllAzureServicesAndResourcesWithinAzureIps rule and adds no rule for
  # any address outside Azure. Container Apps reaches it from inside Azure, so that is enough,
  # and it avoids putting the whole environment in a VNet for one dependency.
  echo "▸ creating Flexible Server $PG — five to ten minutes"
  az postgres flexible-server create --name "$PG" --resource-group "$RESOURCE_GROUP" \
    --location "$LOCATION" --tier "$PG_TIER" --sku-name "$PG_SKU" \
    --storage-size "$PG_STORAGE" --version "$PG_VERSION" \
    --admin-user attesttag --admin-password "$PG_PASS" \
    --database-name attesttag --public-access 0.0.0.0 --yes >/dev/null
  DB_URL="postgres://attesttag:$PG_PASS@$PG.postgres.database.azure.com:5432/attesttag?sslmode=require"
  ( umask 077
    printf '\n# Written by deploy/azure/containerapps.sh (CREATE_DATABASE=1) on %s\nDATABASE_URL=%s\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$DB_URL" >> "$ENV_FILE" )
  echo "▸ database: $PG.postgres.database.azure.com — DATABASE_URL written to $ENV_FILE"
  echo "▸ back up $ENV_FILE: it now holds the only copy of that password, as well as MASTER_KEY"
fi

if [ "$REPLICAS" != "1" ] && [ -z "$DB_URL" ]; then
  echo "REPLICAS=$REPLICAS with no DATABASE_URL: SQLite has one writer, and a second replica would be refused the write lease and answer 503 rather than serve traffic. Set DATABASE_URL, or leave REPLICAS at 1." >&2
  exit 1
fi


# ── Documents on Azure Files, when the rows are in Postgres. The share is SMB, which is fine
# for documents and would be wrong for the database — that is the whole reason this branch
# exists only alongside DATABASE_URL.
STORAGE_NAME=""
if [ -z "$DOCS_URL" ]; then
  SA="${STORAGE_ACCOUNT:-attesttag$(az account show --query id -o tsv | tr -d '-' | cut -c1-12)}"
  SHARE=attesttag-docs
  STORAGE_NAME=docs
  az storage account show --name "$SA" --resource-group "$RESOURCE_GROUP" >/dev/null 2>&1 || \
    az storage account create --name "$SA" --resource-group "$RESOURCE_GROUP" --location "$LOCATION" \
      --sku Standard_LRS --kind StorageV2 --min-tls-version TLS1_2 --allow-blob-public-access false >/dev/null
  SA_KEY="$(az storage account keys list --account-name "$SA" --resource-group "$RESOURCE_GROUP" --query '[0].value' -o tsv)"
  az storage share-rm show --storage-account "$SA" --resource-group "$RESOURCE_GROUP" --name "$SHARE" >/dev/null 2>&1 || \
    az storage share-rm create --storage-account "$SA" --resource-group "$RESOURCE_GROUP" --name "$SHARE" --quota 100 >/dev/null
  az containerapp env storage set --name "$ENV_NAME" --resource-group "$RESOURCE_GROUP" \
    --storage-name "$STORAGE_NAME" --azure-file-account-name "$SA" --azure-file-account-key "$SA_KEY" \
    --azure-file-share-name "$SHARE" --access-mode ReadWrite >/dev/null
  echo "▸ documents on Azure Files: $SA/$SHARE mounted at /app/docs"
else
  echo "▸ documents in the bucket named by DOCS_S3_URL"
fi

# ── The spec. Written as one YAML document rather than assembled from forty flags, because
# volumes, secrets, scale and ingress have to agree with each other and a half-applied update
# is how you get a replica with no documents. Secret values live in this file, so it is created
# mode 600 and removed on the way out however this script ends.
SPEC="$(mktemp -t attesttag-spec)"
trap 'rm -f "$SPEC"' EXIT INT TERM
chmod 600 "$SPEC"

DOMAINS="$(envval ALLOWED_EMAIL_DOMAINS | tr ',;' '  ' || true)"
[ -n "$DOMAINS" ] || echo "▸ warning: ALLOWED_EMAIL_DOMAINS is unset — an organisation with no email-domain list of its own lets every member of its workspace use the bot. Guests and Slack Connect members are refused either way, unless the organisation allows them under Settings → Security"

# A new revision on every run. Container Apps makes one only when the template changes, so a rerun
# on the same tag — :latest, after a release moved it — changed nothing and kept the old image, and
# a rotated secret, which is not part of the template, never reached a running replica. The suffix
# is the change: lower-case, starting with a letter, the time and a random tail so that no two
# runs share one.
REVISION="r$(date -u +%y%m%d-%H%M%S)-$RANDOM"

python3 - "$SPEC" "$LOCATION" "$ENV_ID" "$IMAGE" "$APP" "$CPU" "$MEMORY" "$REPLICAS" "$STORAGE_NAME" "$ENV_FILE" "${DOMAIN:-}" "$DOMAINS" "$REVISION" <<'PY'
import os, re, sys

spec, location, env_id, image, app, cpu, memory, replicas, storage, env_file, domain, domains, revision = sys.argv[1:14]

def envval(key):
    # The last line with a value, like the shell's envval above.
    found = ""
    with open(env_file) as fh:
        for line in fh:
            if line.startswith(key + "=") and line.rstrip("\n") != key + "=":
                v = line.split("=", 1)[1].strip()
                if len(v) > 1 and v[0] == v[-1] and v[0] in "\"'":
                    v = v[1:-1]
                found = v
    return found

# Everything the container reads as a secret. A Container Apps secret name is lowercase
# alphanumeric and dashes, so MASTER_KEY is stored as master-key and referenced by that name.
SECRET_KEYS = [
    "SLACK_SIGNING_SECRET", "OPENROUTER_API_KEY", "LLM_API_KEY", "MASTER_KEY", "MASTER_KEY_PREVIOUS",
    "SLACK_CLIENT_ID", "SLACK_CLIENT_SECRET", "RESEND_API_KEY", "OPENROUTER_PROVISIONING_KEY",
    "WORKER_LLM_API_KEY", "WORKER_ENGINE_API_KEY", "HEALTH_SECRET", "OPERATOR_SECRET",
    "GITHUB_APP_PRIVATE_KEY_B64", "GITHUB_APP_CLIENT_SECRET", "DATABASE_URL",
    "DOCS_S3_KEY_ID", "DOCS_S3_SECRET", "MSTEAMS_APP_PASSWORD",
]
# Plain settings. DOCS_S3_URL names a bucket and carries no credential, so it is not a secret.
PLAIN_KEYS = ["DOCS_S3_URL", "LLM_BASE_URL", "LLM_MODEL", "HEAVY_MODEL", "EMBED_MODEL",
              "MAIL_FROM", "SIGNUP_MODE", "ORG_MODEL_KEYS", "TZ_NAME", "LOG_LEVEL",
              # The GitHub App's public half: the id, and the slug in the install URL people
              # click. Its private key and client secret are listed above.
              "GITHUB_APP_ID", "GITHUB_APP_SLUG", "GITHUB_APP_CLIENT_ID",
              # Microsoft Teams (guide/msteams.md). The ids are in the app package every tenant
              # installs, so they are not secrets; MSTEAMS_APP_PASSWORD is, and is listed above.
              "MSTEAMS_APP_ID", "MSTEAMS_TENANT_ID", "MSTEAMS_APP_TYPE", "MSTEAMS_SIGNIN",
              # The fix-job worker (deploy/azure/worker.sh). WORKER_MODE is off unless it is
              # set, so none of the rest means anything until the jobs exist.
              "WORKER_MODE", "WORKER_AZURE_SUBSCRIPTION", "WORKER_AZURE_RESOURCE_GROUP",
              "WORKER_JOB_NAME", "WORKER_JOB_NAMES"]

secrets, env = [], []
for key in SECRET_KEYS:
    val = envval(key)
    if not val:
        continue
    name = key.lower().replace("_", "-")
    secrets.append({"name": name, "value": val})
    env.append({"name": key, "secretRef": name})
for key in PLAIN_KEYS:
    val = envval(key)
    if val:
        env.append({"name": key, "value": val})
if domains:
    env.append({"name": "ALLOWED_EMAIL_DOMAINS", "value": domains})
if domain:
    # The origin every password-reset, verification and invitation link is built from. Unset,
    # the bot learns it from the first authenticated request's Host header and keeps it — which
    # is right for a tunnel whose name changes and wrong for a host you already know.
    env.append({"name": "ADMIN_BASE_URL", "value": "https://" + domain.rstrip("/")})
env.append({"name": "PORT", "value": "8080"})

container = {
    "name": app,
    "image": image,
    "resources": {"cpu": float(cpu), "memory": memory},
    "env": env,
    # /health is served before authentication (internal/app/gate.go). The startup probe is
    # generous because a cold replica restores the SQLite database from the bucket first.
    "probes": [
        {"type": "Liveness", "httpGet": {"path": "/health", "port": 8080}, "periodSeconds": 30},
        {"type": "Startup", "httpGet": {"path": "/health", "port": 8080},
         "periodSeconds": 10, "failureThreshold": 30},
    ],
}
template = {"containers": [container],
            "revisionSuffix": revision,
            "scale": {"minReplicas": int(replicas), "maxReplicas": int(replicas)}}
if storage:
    container["volumeMounts"] = [{"volumeName": "docs", "mountPath": "/app/docs"}]
    template["volumes"] = [{"name": "docs", "storageType": "AzureFile", "storageName": storage}]

doc = {
    "location": location,
    "type": "Microsoft.App/containerApps",
    # A system-assigned identity only when the fix-job worker is on. Until then the app needs no
    # Azure identity at all — its storage credential is a key in $ENV_FILE — and creating one
    # that is granted nothing would only be something to wonder about later.
    **({"identity": {"type": "SystemAssigned"}} if envval("WORKER_MODE") in ("workers", "aca") else {}),
    "properties": {
        "managedEnvironmentId": env_id,
        "configuration": {
            "activeRevisionsMode": "Single",
            "secrets": secrets,
            "ingress": {"external": True, "targetPort": 8080, "transport": "auto",
                        "allowInsecure": False},
        },
        "template": template,
    },
}

# A small YAML writer, so that nothing here depends on PyYAML being installed alongside the az
# CLI. Every scalar is single-quoted with internal quotes doubled, which is YAML's own escape
# and leaves base64, URLs and passwords alone.
def scalar(v):
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, (int, float)):
        return repr(v)
    return "'" + str(v).replace("'", "''") + "'"

def emit(node, indent=0, out=None):
    pad = "  " * indent
    if isinstance(node, dict):
        for k, v in node.items():
            if isinstance(v, (dict, list)) and v:
                out.append(f"{pad}{k}:")
                emit(v, indent + 1, out)
            elif isinstance(v, (dict, list)):
                out.append(f"{pad}{k}: {{}}" if isinstance(v, dict) else f"{pad}{k}: []")
            else:
                out.append(f"{pad}{k}: {scalar(v)}")
    elif isinstance(node, list):
        for item in node:
            if isinstance(item, dict):
                lines = []
                emit(item, indent + 1, lines)
                lines[0] = pad + "- " + lines[0].lstrip()
                out.extend(lines)
            else:
                out.append(f"{pad}- {scalar(item)}")
    return out

lines = emit(doc, 0, [])
with open(spec, "w") as fh:
    fh.write("\n".join(lines) + "\n")
PY

if az containerapp show --name "$APP" --resource-group "$RESOURCE_GROUP" >/dev/null 2>&1; then
  az containerapp update --name "$APP" --resource-group "$RESOURCE_GROUP" --yaml "$SPEC" >/dev/null
else
  az containerapp create --name "$APP" --resource-group "$RESOURCE_GROUP" --yaml "$SPEC" >/dev/null
fi

# ── What the bot may do with the fix-job worker. The app has an identity only in aca mode
# (the spec above), and this is the whole of what that identity is allowed: read a job, start it,
# read its executions, stop one. Scoped to each worker job, so it cannot start anything else in
# the resource group.
#
# A custom role rather than a built-in one because nothing built in is this narrow — the nearest
# is Container Apps Contributor, which can also delete the jobs and rewrite their images.
# `workers` and `aca` both land here: the first works the platform out from CONTAINER_APP_NAME,
# which Container Apps sets on every replica, and the second pins it.
WM="$(envval WORKER_MODE || true)"
if [ "$WM" = "workers" ] || [ "$WM" = "aca" ]; then
  WJOB="${WORKER_JOB_NAME:-$(envval WORKER_JOB_NAME || true)}"; WJOB="${WJOB:-attesttag-worker}"
  PRINCIPAL="$(az containerapp show --name "$APP" --resource-group "$RESOURCE_GROUP" --query identity.principalId -o tsv 2>/dev/null || true)"
  if [ -z "$PRINCIPAL" ] || [ "$PRINCIPAL" = "None" ]; then
    echo "▸ warning: the app has no managed identity, so it cannot start worker jobs" >&2
  else
    SUB="$(az account show --query id -o tsv)"
    ROLE="attest_tag fix worker operator"
    if [ -z "$(az role definition list --name "$ROLE" --query '[0].name' -o tsv 2>/dev/null)" ]; then
      az role definition create --role-definition "$(python3 -c '
import json, sys
role, scope = sys.argv[1], sys.argv[2]
print(json.dumps({
    "Name": role,
    "Description": "Start and stop attest_tag fix-job worker executions.",
    "Actions": ["Microsoft.App/jobs/read", "Microsoft.App/jobs/start/action",
                "Microsoft.App/jobs/executions/read", "Microsoft.App/jobs/executions/stop/action"],
    "AssignableScopes": [scope],
}))
' "$ROLE" "/subscriptions/$SUB/resourceGroups/$RESOURCE_GROUP")" >/dev/null
    fi
    # Both worker jobs, when the heavy one exists. A role assignment is idempotent in effect but
    # not in exit code, hence the `|| true`: rerunning this script must not fail on it.
    for J in "$WJOB" $(echo "${WORKER_JOB_NAMES:-$(envval WORKER_JOB_NAMES || true)}" | tr ',' '\n' | cut -d= -f2 | sort -u); do
      [ -n "$J" ] || continue
      JID="$(az containerapp job show --name "$J" --resource-group "$RESOURCE_GROUP" --query id -o tsv 2>/dev/null || true)"
      if [ -z "$JID" ]; then
        echo "▸ warning: the fix worker is on ($WM) but the job $J does not exist yet; run deploy/azure/worker.sh" >&2
        continue
      fi
      az role assignment create --assignee-object-id "$PRINCIPAL" --assignee-principal-type ServicePrincipal \
        --role "$ROLE" --scope "$JID" >/dev/null 2>&1 || true
      echo "▸ $APP may start job $J"
    done
  fi
fi

FQDN="$(az containerapp show --name "$APP" --resource-group "$RESOURCE_GROUP" --query properties.configuration.ingress.fqdn -o tsv)"
PUBLIC="${DOMAIN:-$FQDN}"

cat <<NEXT

Deployed. $REPLICAS replica, always on, at:

  https://$FQDN

Azure issues and renews the certificate for that name, so it is a working https origin with
nothing further to do. A name of your own is optional:

  az containerapp hostname add   --name $APP --resource-group $RESOURCE_GROUP --hostname $PUBLIC
  az containerapp ssl upload     --name $APP --resource-group $RESOURCE_GROUP --hostname $PUBLIC …

Next:

  1. Check it:   curl -fsS https://$FQDN/health

  2. Make the Slack app point at it:

       BASE_URL=https://$PUBLIC ./deploy/slack/manifest.sh

     Paste the output at api.slack.com/apps -> Create New App -> From a manifest, then copy the
     Signing Secret, Client ID and Client Secret into $ENV_FILE and rerun this script.

  3. Open https://$PUBLIC and sign up. The first sign-up founds the deployment; everybody after
     it arrives by invitation.

Logs:      az containerapp logs show --name $APP --resource-group $RESOURCE_GROUP --follow
Redeploy:  rerun this script — every run starts a new revision ($APP--$REVISION this time).
Remove:    az group delete --name $RESOURCE_GROUP

Back up the MASTER_KEY line in $ENV_FILE somewhere other than this machine and this
subscription. It seals every stored credential. The app keeps its copy as the Container Apps
secret master-key, which is the only other one, and it goes with the app: after the
az group delete above, this file is all that is left.
NEXT
