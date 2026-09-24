#!/bin/zsh
# Build the fix-job worker image and create or update the Cloud Run Job the bot runs it as.
# Rerun to ship a new image. Idempotent, like cloudrun.sh.
#
#   PROJECT=<your-gcp-project> REGION=us-central1 ./deploy/gcp/worker.sh
#
# Then put WORKER_MODE=workers in the env file and deploy the bot (cloudrun.sh passes WORKER_*
# through; the container resolves `workers` to cloudrun from K_SERVICE, and WORKER_MODE=cloudrun
# pins it). Either script may run first on a new project.
set -euo pipefail
cd "$(dirname "$0")/../.."

PROJECT=${PROJECT:-$(gcloud config get-value project 2>/dev/null)}
REGION=${REGION:-us-central1}
JOB=${WORKER_JOB_NAME:-attesttag-worker}
BOT_SA=attesttag-run@$PROJECT.iam.gserviceaccount.com
JOB_SA=attesttag-worker@$PROJECT.iam.gserviceaccount.com
AR_REPO=attesttag
COMMIT=$(git rev-parse --short=12 HEAD 2>/dev/null || echo dev)
IMAGE=$REGION-docker.pkg.dev/$PROJECT/$AR_REPO/worker:$COMMIT
# The heavy worker (JDK, Maven, Gradle, .NET). WORKER_JVM=0 skips it, which is the right choice
# for a deployment with no JVM or .NET repositories: it is a large image to build and store.
WORKER_JVM=${WORKER_JVM:-1}
JVM_JOB=${WORKER_JVM_JOB_NAME:-attesttag-worker-jvm}
JVM_IMAGE=""
[[ "$WORKER_JVM" == "1" ]] && JVM_IMAGE=$REGION-docker.pkg.dev/$PROJECT/$AR_REPO/worker-jvm:$COMMIT

gcloud services enable run.googleapis.com cloudbuild.googleapis.com artifactregistry.googleapis.com --project "$PROJECT"
gcloud artifacts repositories describe "$AR_REPO" --location "$REGION" --project "$PROJECT" >/dev/null 2>&1 || \
  gcloud artifacts repositories create "$AR_REPO" --repository-format docker --location "$REGION" --project "$PROJECT"

echo "building $IMAGE"
gcloud builds submit --config deploy/gcp/cloudbuild.worker.yaml \
  --substitutions "_IMAGE=$IMAGE,_JVM_IMAGE=$JVM_IMAGE" --project "$PROJECT" .

# The job's own account needs nothing: no secrets, no buckets, no APIs. Everything the worker
# uses comes over the claim call, and a compromised job can reach nothing else in the project.
gcloud iam service-accounts describe "$JOB_SA" --project "$PROJECT" >/dev/null 2>&1 || \
  gcloud iam service-accounts create attesttag-worker --display-name "attest_tag fix-job worker" --project "$PROJECT"

# Every grant below goes to the bot's account, which cloudrun.sh creates — and IAM refuses a
# binding for an account that does not exist, so on a new project where this ran first the script
# stopped at the first grant. Made here the same way instead; cloudrun.sh finds it and goes on.
gcloud iam service-accounts describe "$BOT_SA" --project "$PROJECT" >/dev/null 2>&1 || \
  gcloud iam service-accounts create attesttag-run --display-name "attest_tag Cloud Run" --project "$PROJECT"

# One task, no retries (a retried task would find its job already claimed), an hour ceiling that
# the per-job timeout (worker_timeout_minutes, at most 60) stays inside. The writable layer is
# memory-backed on Cloud Run, so a clone plus caches counts against --memory.
gcloud run jobs deploy "$JOB" --image "$IMAGE" --region "$REGION" --project "$PROJECT" \
  --service-account "$JOB_SA" --tasks 1 --max-retries 0 --task-timeout "${TASK_TIMEOUT:-3600s}" \
  --cpu "${CPU:-2}" --memory "${MEMORY:-4Gi}" \
  --set-env-vars "WORKER_MODE=cloudrun,GIT_COMMIT=$COMMIT,WORKER_MAX_WALL=55m" \
  --labels app=attesttag

# The heavy worker is the same job, deployed a second time from the heavier image. The bot picks
# between them per repository (WORKER_JOB_NAMES below), so both must exist before it can route.
if [[ -n "$JVM_IMAGE" ]]; then
  gcloud run jobs deploy "$JVM_JOB" --image "$JVM_IMAGE" --region "$REGION" --project "$PROJECT" \
    --service-account "$JOB_SA" --tasks 1 --max-retries 0 --task-timeout "${TASK_TIMEOUT:-3600s}" \
    --cpu "${JVM_CPU:-4}" --memory "${JVM_MEMORY:-8Gi}" \
    --set-env-vars "WORKER_MODE=cloudrun,GIT_COMMIT=$COMMIT,WORKER_MAX_WALL=55m" \
    --labels app=attesttag
  gcloud run jobs add-iam-policy-binding "$JVM_JOB" --region "$REGION" --project "$PROJECT" \
    --member "serviceAccount:$BOT_SA" --role roles/run.developer >/dev/null
fi

# The dependency cache lives in the replica bucket as signed URLs the bot hands the worker with
# its claim (jobs_cache.go). Signing with the metadata server's credentials means the bot needs
# to be allowed to sign as itself; without this the cache is simply off and jobs install cold.
gcloud iam service-accounts add-iam-policy-binding "$BOT_SA" --project "$PROJECT" \
  --member "serviceAccount:$BOT_SA" --role roles/iam.serviceAccountTokenCreator >/dev/null || \
  echo "note: could not grant the bot permission to sign cache URLs; jobs will install from cold"

# The bot starts executions and reads or cancels them: run.developer on this job only
# (run.invoker lacks executions.get and executions.cancel).
gcloud run jobs add-iam-policy-binding "$JOB" --region "$REGION" --project "$PROJECT" \
  --member "serviceAccount:$BOT_SA" --role roles/run.developer >/dev/null

echo
echo "worker job $JOB → $IMAGE"
[[ -n "$JVM_IMAGE" ]] && echo "heavy worker  $JVM_JOB → $JVM_IMAGE"
# In the env file rather than on the command line: cloudrun.sh sets every variable afresh on each
# run, so a WORKER_MODE given only in the shell switched the worker off again at the next deploy.
# The deploy line names the env file and the region, because the bot looks for these jobs in the
# region it is deployed to, and a self-host's env file is .env, never a stray .env.prod.
echo
echo "Put these lines in the env file the bot deploys from:"
echo
echo "  WORKER_MODE=workers"       # or cloudrun, to pin it rather than let the container work it out
[[ -n "$JVM_IMAGE" ]] && echo "  WORKER_JOB_NAMES=java=$JVM_JOB,java-gradle=$JVM_JOB,java-maven=$JVM_JOB,dotnet=$JVM_JOB"
echo
echo "then: ENV_FILE=${ENV_FILE:-.env} PROJECT=$PROJECT REGION=$REGION ./deploy/gcp/cloudrun.sh"
echo
echo "Executions: gcloud run jobs executions list --job $JOB --region $REGION --project $PROJECT"
