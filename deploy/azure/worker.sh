#!/usr/bin/env bash
# Build the fix-job worker image and create the Container Apps job the bot runs it as.
# Rerun to ship a new image. Idempotent, like containerapps.sh.
#
#   ./deploy/azure/worker.sh
#
# Then deploy the bot with WORKER_MODE=workers; containerapps.sh turns on the app's managed identity
# and grants it the right to start these jobs and nothing else. Run this first — the bot cannot
# start a job that does not exist, and containerapps.sh says so rather than deploying something
# that fails on the first fix request.
#
# ── What it creates ──
#
#   container registry        attesttag<hash>, Basic tier; the images are built inside it
#   user-assigned identity    attesttag-worker-pull, with AcrPull on that registry
#   Container Apps job        attesttag-worker (2 vCPU / 4 GiB), manual trigger, one replica
#   and attesttag-worker-jvm  the heavy image, unless WORKER_JVM=0
#
# ── Why the image is built in the registry ──
#
# `az acr build` builds server-side. That is not a convenience here: Container Apps runs X86_64,
# and an image built on an Apple Silicon laptop starts and dies with an exec format error that
# reads like a broken worker rather than a wrong architecture. Building in the registry removes
# the question, and removes the need for a local docker at all.
#
# ── What the worker may do ──
#
# The job carries one identity, and it is there only to pull its own image: AcrPull on this
# registry, nothing else. Everything the worker actually uses — the spec, the repository token,
# the model key — arrives over the claim call it makes back to the bot. The alternative, the
# registry's admin password stored as a job secret, would be one more credential to rotate and
# would be readable by anyone with read access to the job.
set -euo pipefail
cd "$(dirname "$0")/../.."

: "${LOCATION:=eastus}"
: "${RESOURCE_GROUP:=attesttag}"
: "${ENV_NAME:=attesttag-env}"
: "${ENV_FILE:=.env}"
: "${JOB:=attesttag-worker}"
: "${JVM_JOB:=attesttag-worker-jvm}"
: "${CPU:=2.0}"
: "${MEMORY:=4.0Gi}"
: "${JVM_CPU:=4.0}"
: "${JVM_MEMORY:=8.0Gi}"
# The heavy worker (JDK, Maven, Gradle, .NET). WORKER_JVM=0 skips it, which is the right choice
# for a deployment with no JVM or .NET repositories: it is a large image to build and store.
: "${WORKER_JVM:=1}"
# Container Apps caps a job replica at this; the worker keeps its own 55-minute ceiling inside it.
: "${REPLICA_TIMEOUT:=3600}"

command -v az >/dev/null 2>&1 || { echo "the az CLI is not on PATH" >&2; exit 1; }
az extension add --name containerapp --upgrade --allow-preview false >/dev/null 2>&1 || true
az provider register --namespace Microsoft.App --wait >/dev/null 2>&1 || true
az provider register --namespace Microsoft.ContainerRegistry --wait >/dev/null 2>&1 || true

SUBSCRIPTION="$(az account show --query id -o tsv)"
TAG="$(git rev-parse --short=12 HEAD 2>/dev/null || date +%Y%m%d%H%M%S)"
# A registry name is globally unique and alphanumeric only, so it is derived from the
# subscription id the same way containerapps.sh derives the storage account name.
: "${ACR:=attesttag$(echo "$SUBSCRIPTION" | tr -d '-' | cut -c1-12)}"

az group show --name "$RESOURCE_GROUP" >/dev/null 2>&1 || \
  az group create --name "$RESOURCE_GROUP" --location "$LOCATION" >/dev/null
az containerapp env show --name "$ENV_NAME" --resource-group "$RESOURCE_GROUP" >/dev/null 2>&1 || \
  az containerapp env create --name "$ENV_NAME" --resource-group "$RESOURCE_GROUP" --location "$LOCATION" >/dev/null

az acr show --name "$ACR" --resource-group "$RESOURCE_GROUP" >/dev/null 2>&1 || \
  az acr create --name "$ACR" --resource-group "$RESOURCE_GROUP" --location "$LOCATION" --sku Basic >/dev/null
ACR_ID="$(az acr show --name "$ACR" --resource-group "$RESOURCE_GROUP" --query id -o tsv)"
SERVER="$ACR.azurecr.io"

echo "▸ building $SERVER/$JOB:$TAG in the registry"
az acr build --registry "$ACR" --resource-group "$RESOURCE_GROUP" --platform linux/amd64 \
  --image "$JOB:$TAG" --file Dockerfile.worker . >/dev/null
WORKER_IMAGE="$SERVER/$JOB:$TAG"
JVM_IMAGE=""
if [ "$WORKER_JVM" = "1" ]; then
  echo "▸ building $SERVER/$JVM_JOB:$TAG in the registry"
  az acr build --registry "$ACR" --resource-group "$RESOURCE_GROUP" --platform linux/amd64 \
    --image "$JVM_JOB:$TAG" --file Dockerfile.worker.jvm --build-arg "BASE=$WORKER_IMAGE" . >/dev/null
  JVM_IMAGE="$SERVER/$JVM_JOB:$TAG"
fi

# ── The pull identity. User-assigned rather than system-assigned so that it exists before the
# job does: a system-assigned identity is created with its job, which leaves no moment at which
# AcrPull can be granted before the first pull is attempted.
UAI="$JOB-pull"
az identity show --name "$UAI" --resource-group "$RESOURCE_GROUP" >/dev/null 2>&1 || \
  az identity create --name "$UAI" --resource-group "$RESOURCE_GROUP" --location "$LOCATION" >/dev/null
UAI_ID="$(az identity show --name "$UAI" --resource-group "$RESOURCE_GROUP" --query id -o tsv)"
UAI_PRINCIPAL="$(az identity show --name "$UAI" --resource-group "$RESOURCE_GROUP" --query principalId -o tsv)"
az role assignment create --assignee-object-id "$UAI_PRINCIPAL" --assignee-principal-type ServicePrincipal \
  --role AcrPull --scope "$ACR_ID" >/dev/null 2>&1 || true

# ── The jobs. Manual trigger, one replica, no retry: a retried replica would find its job
# already claimed and report a confusing failure over the top of the real one.
ensure_job() { # name image cpu memory
  # The container is named `worker`, which is what internal/app/jobs_aca.go looks for when it
  # overlays the launch environment. It falls back to the first container if the name differs,
  # so this is a readability choice rather than a contract.
  #
  # create and update take different flags — --environment and --trigger-type are create-only,
  # and passing them to update fails the whole script — so the two paths are written out rather
  # than assembled, which is the kind of cleverness that breaks a deploy six months from now.
  if az containerapp job show --name "$1" --resource-group "$RESOURCE_GROUP" >/dev/null 2>&1; then
    az containerapp job update --name "$1" --resource-group "$RESOURCE_GROUP" \
      --image "$2" --container-name worker --cpu "$3" --memory "$4" \
      --replica-timeout "$REPLICA_TIMEOUT" --replica-retry-limit 0 --parallelism 1 \
      --registry-server "$SERVER" --registry-identity "$UAI_ID" \
      --set-env-vars WORKER_MODE=aca WORKER_MAX_WALL=55m "GIT_COMMIT=$TAG" >/dev/null
  else
    az containerapp job create --name "$1" --resource-group "$RESOURCE_GROUP" \
      --environment "$ENV_NAME" --trigger-type Manual \
      --image "$2" --container-name worker --cpu "$3" --memory "$4" \
      --replica-timeout "$REPLICA_TIMEOUT" --replica-retry-limit 0 --parallelism 1 \
      --replica-completion-count 1 \
      --registry-server "$SERVER" --registry-identity "$UAI_ID" --mi-user-assigned "$UAI_ID" \
      --env-vars WORKER_MODE=aca WORKER_MAX_WALL=55m "GIT_COMMIT=$TAG" >/dev/null
  fi
}
ensure_job "$JOB" "$WORKER_IMAGE" "$CPU" "$MEMORY"
echo "▸ job $JOB → $WORKER_IMAGE"
if [ -n "$JVM_IMAGE" ]; then
  ensure_job "$JVM_JOB" "$JVM_IMAGE" "$JVM_CPU" "$JVM_MEMORY"
  echo "▸ job $JVM_JOB → $JVM_IMAGE"
fi

echo
# The file and only the file: containerapps.sh builds the app's environment from $ENV_FILE alone,
# so a WORKER_MODE exported in the shell would deploy a bot with the worker still off.
echo "Put these lines in $ENV_FILE — containerapps.sh takes the app's settings from that file, never from the shell:"
echo
echo "  WORKER_MODE=workers"       # or aca, to pin it rather than let the replica work it out
echo "  WORKER_AZURE_SUBSCRIPTION=$SUBSCRIPTION"
echo "  WORKER_AZURE_RESOURCE_GROUP=$RESOURCE_GROUP"
echo "  WORKER_JOB_NAME=$JOB"
[ -n "$JVM_IMAGE" ] && echo "  WORKER_JOB_NAMES=java=$JVM_JOB,java-gradle=$JVM_JOB,java-maven=$JVM_JOB,dotnet=$JVM_JOB"
echo
echo "then: ./deploy/azure/containerapps.sh   (it grants the app the right to start these jobs)"
echo
echo "Executions: az containerapp job execution list --name $JOB --resource-group $RESOURCE_GROUP -o table"
