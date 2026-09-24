#!/bin/zsh
# Deploy attest_tag to Cloud Run with Litestream → GCS for the SQLite DB and a GCS volume for docs.
# Idempotent: safe to rerun. Requires: gcloud auth login, a project, and an env file in the repo
# root holding the deployed secrets: ENV_FILE if set, else .env.prod when there is one (the
# maintainer's), else .env (the one deploy/local/bootstrap.sh writes). .env.testing is the
# local-testing copy and is never deployed.
#
#   PROJECT=<attest-project-id> REGION=us-central1 BUCKET=attesttag-data ./deploy/gcp/cloudrun.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
: "${PROJECT:=$(gcloud config get-value project 2>/dev/null)}"
: "${REGION:=us-central1}"
: "${SERVICE:=attesttag}"
: "${BUCKET:=attesttag-data-$PROJECT}"
SA="attesttag-run@$PROJECT.iam.gserviceaccount.com"
if [ -z "${ENV_FILE:-}" ]; then
  ENV_FILE=.env
  [ -f .env.prod ] && ENV_FILE=.env.prod
fi
[ -f "$ENV_FILE" ] || { echo "$ENV_FILE missing — run ./deploy/local/bootstrap.sh first, or set ENV_FILE" >&2; exit 1; }

# envval strips the surrounding quotes godotenv strips when the app reads a dotenv file
# locally, so the deployed value is the same string the developer tested with. The last line with
# a value wins: the template holds an empty line for every key, and reading the first one let
# that empty line hide a value appended below it — which is how the binary itself adds a
# MASTER_KEY it generated.
envval() {
  grep -E "^$1=." "$ENV_FILE" | tail -n 1 | cut -d= -f2- | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/"
}

# HOSTED marks the maintainer's own service (app.attesttag.com), the only deployment the policy
# defaults further down belong to: open signup, a public support address, the attesttag.com site,
# a $5 free-plan cap, BYOK gated to enterprise. It is on only when asked for by name, HOSTED=1 in
# the shell or in the env file, and the maintainer's .env.prod says so. It used to follow from the
# file being called .env.prod, which turned a copy of this repository, deployed with the file name
# this script asks for, into a public service anyone could sign up to. Without it a deployment is
# one organisation: signup is first-run, no support address is set, the console talks to its own
# origin, and the free budget and key policy take the binary's self-host defaults. Anything set
# explicitly in the env file wins either way. Only 1 is on, because HOSTED=0 used to read as on.
HOSTED="${HOSTED:-$(envval HOSTED || true)}"
[ "$HOSTED" = 1 ] || HOSTED=

# The hosted policy is a public one — anyone may sign up, and users' support mail goes to
# attesttag.com — so it is never applied silently.
if [ -n "$HOSTED" ]; then
  echo "▸ HOSTED policy ON: open signup · support@attesttag.com · https://attesttag.com · \$5 free-plan cap · own model key gated to Enterprise"
fi

echo "▸ project=$PROJECT region=$REGION service=$SERVICE bucket=gs://$BUCKET"
gcloud services enable run.googleapis.com cloudbuild.googleapis.com secretmanager.googleapis.com \
  artifactregistry.googleapis.com storage.googleapis.com --project "$PROJECT" >/dev/null

# Bucket: docs/ (mounted read-only into the service) + litestream/ (DB replica)
if ! gcloud storage buckets describe "gs://$BUCKET" --project "$PROJECT" >/dev/null 2>&1; then
  gcloud storage buckets create "gs://$BUCKET" --project "$PROJECT" --location "$REGION" \
    --uniform-bucket-level-access --public-access-prevention
fi
# Seed docs/ from the local folder only if the bucket has none yet, and only files git tracks —
# never a local docs/org-*/ corpus a developer happens to have here (those are gitignored). Left
# unfiltered, such a folder would become some organisation's documents by its number in a fresh
# deployment, since folders are keyed by the serial org id.
if [ -z "$(gcloud storage ls "gs://$BUCKET/docs/" 2>/dev/null)" ]; then
  git ls-files -z docs 2>/dev/null | while IFS= read -r -d '' f; do
    gcloud storage cp "$f" "gs://$BUCKET/$f" 2>/dev/null || true
  done
fi

# Service account with access to the bucket and the secrets only.
if ! gcloud iam service-accounts describe "$SA" --project "$PROJECT" >/dev/null 2>&1; then
  gcloud iam service-accounts create attesttag-run --project "$PROJECT" --display-name "attest_tag Cloud Run"
fi
gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" --member "serviceAccount:$SA" \
  --role roles/storage.objectAdmin --project "$PROJECT" >/dev/null

# Secrets: read from $ENV_FILE, never echoed. Each key becomes a Secret Manager secret.

# MASTER_KEY unlocks every stored connection credential. Deploying without it used to be
# silent: the service would mint a throwaway key on each cold start and quietly lose the lot.
if [ -z "$(envval MASTER_KEY)" ]; then
  echo "MASTER_KEY is missing from $ENV_FILE. Generate one with: openssl rand -base64 32" >&2
  exit 1
fi

if [ -z "$(envval SLACK_SIGNING_SECRET || true)" ]; then
  echo "SLACK_SIGNING_SECRET is missing from $ENV_FILE. Copy it from Slack Basic Information → App Credentials before deploying HTTP delivery." >&2
  exit 1
fi

# The OAuth pair an install runs on. Refused here rather than warned about at startup: a
# deployment without it comes up healthy and then cannot attach a workspace at all, which is a
# thing to find out before the deploy rather than three screens into the console.
for K in SLACK_CLIENT_ID SLACK_CLIENT_SECRET; do
  if [ -z "$(envval "$K" || true)" ]; then
    echo "$K is missing from $ENV_FILE. Connecting a workspace is an OAuth install, so the service cannot start without it: copy it from Slack Basic Information → App Credentials, and register <base>/slack/oauth/callback as a redirect URL on the app." >&2
    exit 1
  fi
done

# Mail is optional: the service starts without it and logs the verification, reset and
# invitation links instead of sending them, so the console can offer them to copy by hand.
# Worth saying out loud at deploy time, because nobody reads a startup log on purpose.
if [ -z "$(envval RESEND_API_KEY || true)" ]; then
  echo "▸ warning: RESEND_API_KEY is unset — deploying without email. Verification links, password resets and invitations will be logged rather than delivered; add the key from resend.com to $ENV_FILE and redeploy to turn mail on." >&2
elif [ -z "$(envval MAIL_FROM || true)" ]; then
  echo "▸ warning: RESEND_API_KEY is set but MAIL_FROM is unset — no mail will be sent. Set MAIL_FROM to an address on a domain verified at resend.com/domains." >&2
fi

if [ "$(envval WORKER_MODE || true)" != "off" ] && [ -n "$(envval WORKER_MODE || true)" ] && [ -z "$(envval OPENROUTER_PROVISIONING_KEY || true)" ]; then
  echo "WARNING: WORKER_MODE is on but OPENROUTER_PROVISIONING_KEY is missing from $ENV_FILE: fix jobs run on the shared OPENROUTER_API_KEY with no per-job spend cap. Create one at https://openrouter.ai/settings/provisioning-keys to give each job its own capped key." >&2
fi

SECRETS=""
for KEY in SLACK_SIGNING_SECRET OPENROUTER_API_KEY MASTER_KEY MASTER_KEY_PREVIOUS SLACK_CLIENT_ID SLACK_CLIENT_SECRET RESEND_API_KEY \
           OPENROUTER_PROVISIONING_KEY WORKER_LLM_API_KEY WORKER_ENGINE_API_KEY HEALTH_SECRET OPERATOR_SECRET \
           STRIPE_SECRET_KEY STRIPE_WEBHOOK_SECRET \
           GITHUB_APP_PRIVATE_KEY_B64 GITHUB_APP_CLIENT_SECRET DATABASE_URL MSTEAMS_APP_PASSWORD; do
  VAL="$(envval "$KEY" || true)"
  [ -z "$VAL" ] && continue
  NAME="attesttag-$(echo "$KEY" | tr 'A-Z_' 'a-z-')"
  if gcloud secrets describe "$NAME" --project "$PROJECT" >/dev/null 2>&1; then
    CUR="$(gcloud secrets versions access latest --secret "$NAME" --project "$PROJECT" 2>/dev/null || true)"
    if [ "$CUR" != "$VAL" ]; then
      printf '%s' "$VAL" | gcloud secrets versions add "$NAME" --project "$PROJECT" --data-file=- >/dev/null
    fi
  else
    printf '%s' "$VAL" | gcloud secrets create "$NAME" --project "$PROJECT" --replication-policy automatic --data-file=- >/dev/null
  fi
  gcloud secrets add-iam-policy-binding "$NAME" --project "$PROJECT" --member "serviceAccount:$SA" \
    --role roles/secretmanager.secretAccessor >/dev/null
  SECRETS="${SECRETS:+$SECRETS,}${KEY}=${NAME}:latest"
done

# Non-secret settings travel as env vars, one addenv each. Add LLM_MODEL etc. that way if you
# override them.
#
# They are joined by $D rather than by gcloud's comma. STRIPE_SIZES and WORKER_JOB_NAMES are
# comma-separated lists, and a comma inside a value split --set-env-vars there: only the first size
# reached the service, the others arrived as variables of their own, and the heavy worker job could
# not be routed to at all. gcloud takes another delimiter named in a ^…^ prefix (gcloud topic
# escaping), so every value travels as the env file has it. A value holding $D is refused rather
# than cut in two.
D='|'
ENVS=""
addenv() {
  case "$2" in
    *"$D"*) echo "$1 contains '$D', which separates the settings this script passes to Cloud Run. Remove it from $1 in $ENV_FILE or the shell." >&2; exit 1 ;;
  esac
  ENVS="${ENVS:+$ENVS$D}$1=$2"
}

# Source deploys drop .git (see .gcloudignore), so the commit is passed along. It is how to tell
# what is running: the service's own environment names it, and on SQLite so does the write lease,
# beside the revision holding the database. Neither !whoami nor /health reports it.
COMMIT="$(git rev-parse --short=12 HEAD 2>/dev/null || true)"
[ -n "$COMMIT" ] && ! git diff --quiet HEAD 2>/dev/null && COMMIT="$COMMIT-dirty"
# LITESTREAM_PATH names the replica this service restores from on a cold start. It moved to
# attesttag-v2.db when the multi-tenant schema landed: the old replica held a database created
# before organisations existed, and migrate() cannot add the composite keys that schema needs,
# so a container restoring it died on "no such column: org_id". Pointing at an unused path is
# how you ask for a fresh database — and why this must never quietly go back to the old one.
: "${LITESTREAM_PATH:=litestream/attesttag-v2.db}"
# The cron timezone for routines, and "current time" in prompts, for an organisation that has
# never set its own. Read from $ENV_FILE like every other policy knob: the binary's default is
# not this service's, and a change to one in config.go must not quietly become a change to the
# other. A new organisation takes the zone its founder signed up from regardless.
TZ="${TZ_NAME:-$(envval TZ_NAME || true)}"
[ -z "$TZ" ] && TZ=America/New_York

addenv GIT_COMMIT "${COMMIT:-unknown}"
addenv DOCS_DIR /mnt/bucket/docs
addenv DOCS_BUCKET "$BUCKET"
addenv DOCS_PREFIX docs
addenv TZ_NAME "$TZ"

# Where the rows live. DATABASE_URL beats DB_PATH in the binary, so moving to Postgres is one
# variable — but the Litestream pair has to come off in the same deploy, or the container takes a
# write lease and restores a replica that nothing will read again (deploy/docs/postgres-cutover.md).
# Unset, this is the SQLite deployment it has always been, and rollback is removing the variable.
DBURL="$(envval DATABASE_URL || true)"
if [ -n "$DBURL" ]; then
  # Cloud Run mounts the instance's Unix socket itself; the connection name is already in the
  # DSN (host=/cloudsql/<project>:<region>:<instance>), so there is nothing to keep in step by
  # hand. CLOUDSQL_INSTANCE overrides it for a DSN written some other way.
  CONN="${CLOUDSQL_INSTANCE:-$(printf '%s' "$DBURL" | sed -n 's|.*host=/cloudsql/\([^&]*\).*|\1|p')}"
  if [ -z "$CONN" ]; then
    echo "DATABASE_URL is set but names no Cloud SQL socket: expected host=/cloudsql/<project>:<region>:<instance> in the DSN, or CLOUDSQL_INSTANCE set." >&2
    exit 1
  fi
  SQLARG="--add-cloudsql-instances=$CONN"
  # And having said Postgres, say it in a way the container cannot fall out of. Without this the
  # binary treats a missing DATABASE_URL as "SQLite at DB_PATH" and opens a new empty file on a
  # disk that dies with the revision — a secret that fails to resolve would look like a working
  # deployment with everybody's rows gone rather than like a failure.
  addenv REQUIRE_POSTGRES 1
  echo "▸ database: Postgres at $CONN — SQLite, Litestream and the write lease are off"
else
  SQLARG="--clear-cloudsql-instances"
  addenv LITESTREAM_BUCKET "$BUCKET"
  addenv LITESTREAM_PATH "$LITESTREAM_PATH"
  addenv DB_PATH /data/attesttag.db
fi

# ALLOWED_EMAIL_DOMAINS is the deployment's default list of the email domains the bot answers in
# chat, for an organisation that keeps none of its own (Settings → Security). Guests and Slack
# Connect members are a separate gate, closed unless an organisation opens it, so an empty list
# lets in every member and nobody else. The console has no domain gate: signup is bounded by
# SIGNUP_MODE below, by verification and by limits.
FROM="$(envval MAIL_FROM || true)"
[ -n "$FROM" ] && addenv MAIL_FROM "$FROM"
USERS="$(envval ALLOWED_EMAIL_DOMAINS || true)"
if [ -n "$USERS" ]; then
  addenv ALLOWED_EMAIL_DOMAINS "$USERS"
else
  echo "▸ warning: ALLOWED_EMAIL_DOMAINS is unset — an organisation with no email-domain list of its own lets every member of its workspace use the bot. Guests and Slack Connect members are refused either way, unless the organisation allows them under Settings → Security"
fi

# The service is public at the edge: the console has its own login, and the /setup and
# /configure pages must be reachable by non-admins. The bot learns its public origin
# (https://app.attesttag.com) from the console traffic itself and keeps it in the database
# (internal/app/public_origin.go), so nothing is set here. ADMIN_BASE_URL from the shell or
# $ENV_FILE pins it instead, for a host the admins never sign in on.
BASE="${ADMIN_BASE_URL:-$(envval ADMIN_BASE_URL || true)}"
[ -n "$BASE" ] && addenv ADMIN_BASE_URL "${BASE%/}"

# The GitHub App this deployment installs as (internal/app/github_app.go). The id and slug are
# public — the slug is in the install URL people click — so only the key is a secret, and it
# ships base64 because envval reads one line and a PEM is twenty-eight of them.
for KEY in GITHUB_APP_ID GITHUB_APP_SLUG GITHUB_APP_CLIENT_ID; do
  VAL="$(envval "$KEY" || true)"
  [ -n "$VAL" ] && addenv "$KEY" "$VAL"
done

# The Microsoft Teams bot (internal/app/msteams_auth.go): the app registration's id, the directory
# it lives in, and the Azure Bot's type. All public — they are in the app package every tenant
# uploads — so only the client secret travels in the --set-secrets list above. MSTEAMS_APP_TYPE is
# SingleTenant when unset, in config.go and so here: nothing is passed and the default holds.
for KEY in MSTEAMS_APP_ID MSTEAMS_TENANT_ID MSTEAMS_APP_TYPE MSTEAMS_SIGNIN; do
  VAL="$(envval "$KEY" || true)"
  [ -n "$VAL" ] && addenv "$KEY" "$VAL"
done

# The operator's ceilings (limits.go, routing.go): what one organisation may spend on the
# shared key in a month, and how many turns it may run at once. Tenant settings cannot lift
# them, which is the point — without the first one, a tenant that sets its own budget to 0
# has no limit on a key the operator pays for.
PB="${PLATFORM_MONTHLY_BUDGET_USD_PER_ORG:-$(envval PLATFORM_MONTHLY_BUDGET_USD_PER_ORG || true)}"
[ -n "$PB" ] && addenv PLATFORM_MONTHLY_BUDGET_USD_PER_ORG "$PB"
[ -z "$PB" ] && echo "▸ warning: PLATFORM_MONTHLY_BUDGET_USD_PER_ORG is unset — an organisation that sets its own budget to 0 has no spend limit"
PI="${PLATFORM_MAX_INFLIGHT_PER_ORG:-$(envval PLATFORM_MAX_INFLIGHT_PER_ORG || true)}"
[ -n "$PI" ] && addenv PLATFORM_MAX_INFLIGHT_PER_ORG "$PI"
# What a free-plan account may spend in a month (plans.go); only an account the operator has
# moved to pro gets the ceiling above instead. The figure is named here rather than left to the
# binary's default, because this is a policy of *this* service — signup is open and the key is
# the operator's — and the binary now defaults to 0 so that a self-host spending its own key is
# not capped at a number it never chose.
FB="${FREE_PLAN_BUDGET_USD:-$(envval FREE_PLAN_BUDGET_USD || true)}"
[ -z "$FB" ] && [ -n "$HOSTED" ] && FB=5
[ -n "$FB" ] && addenv FREE_PLAN_BUDGET_USD "$FB"
# Where a capped account is told to write. Named here for the same reason: a self-host that set
# no address must not point its own users at this service's inbox, so the binary defaults to
# empty and says nothing rather than naming somebody else.
SE="${SUPPORT_EMAIL:-$(envval SUPPORT_EMAIL || true)}"
[ -z "$SE" ] && [ -n "$HOSTED" ] && SE="support@attesttag.com"
[ -n "$SE" ] && addenv SUPPORT_EMAIL "$SE"
# Who may create an account (auth_password.go). The hosted service runs open registration and the
# tenant boundary is what holds the door, not the door — see the comment on SignupOpen and
# TestEveryPerOrgQueryIsScoped. Every other deployment is one organisation, and is told so here
# rather than left to the binary's first-run default: the policy is then on the service for anyone
# to read, and a default changed in config.go cannot quietly open a deployment up.
SM="${SIGNUP_MODE:-$(envval SIGNUP_MODE || true)}"
if [ -z "$SM" ]; then
  if [ -n "$HOSTED" ]; then SM=open; else SM=first-run; fi
fi
addenv SIGNUP_MODE "$SM"
case "$SM" in
  open) echo "▸ signup: open — anyone who can reach the console can create an organisation of their own" ;;
  first-run) echo "▸ signup: first-run — one organisation: the first account creates it, and everyone after that joins by invitation" ;;
  *) echo "▸ signup: $SM" ;;
esac
# Which organisations may bring their own model key (model_keys.go). This service sells it on the
# enterprise plan. The binary defaults to all, because a deployment answering on its own key has
# no plan to gate it on and rotating that key from the console beats a redeploy.
OMK="${ORG_MODEL_KEYS:-$(envval ORG_MODEL_KEYS || true)}"
[ -z "$OMK" ] && [ -n "$HOSTED" ] && OMK=enterprise
[ -n "$OMK" ] && addenv ORG_MODEL_KEYS "$OMK"
# This service's public site. It is the one cross-origin the console's CSP allows, because the
# Get started page fetches its walkthrough library from there (headers.go), and it is the origin
# that page is handed on /api/me — one runtime setting, not a second one baked into the console
# at build time. A self-host sets nothing and its console then talks to its own origin and
# nothing else.
SU="${SITE_URL:-$(envval SITE_URL || true)}"
[ -z "$SU" ] && [ -n "$HOSTED" ] && SU="https://attesttag.com"
[ -n "$SU" ] && addenv SITE_URL "$SU"
[ -z "$(envval OPERATOR_SECRET || true)" ] && echo "▸ warning: OPERATOR_SECRET is unset — no way to move an account to the pro plan (/operator/plan and /api/operator/ are off)"
# Self-serve billing (billing.go). Named here rather than left to the binary because this is a
# policy of THIS service — it sells a plan on a key it pays for — and the binary defaults to
# selling nothing, so a self-host has no checkout button at all. The two keys are secrets and
# travel in the --set-secrets list above; the sizes and the limits are not.
SB="${STRIPE_SIZES:-$(envval STRIPE_SIZES || true)}"
[ -n "$SB" ] && addenv STRIPE_SIZES "$SB"
for KEY in BILLING_CURRENCY BILLING_TOPUP_MIN_USD BILLING_TOPUP_MAX_USD BILLING_LOW_BALANCE_USD; do
  VAL="$(envval "$KEY" || true)"
  [ -n "$VAL" ] && addenv "$KEY" "$VAL"
done
if [ -n "$(envval STRIPE_SECRET_KEY || true)" ]; then
  [ -z "$(envval STRIPE_WEBHOOK_SECRET || true)" ] && echo "▸ warning: STRIPE_SECRET_KEY is set but STRIPE_WEBHOOK_SECRET is not — billing switches itself off at startup: a service that can charge a card and cannot hear the result must not charge one"
  [ -z "$SB" ] && echo "▸ warning: STRIPE_SIZES is empty — credit top-ups will work and no plan can be subscribed to"
fi

# Fix worker (deploy/gcp/worker.sh creates the job). WORKER_MODE from the shell or $ENV_FILE; off by
# default, so the tool is not offered until the worker job exists.
WM="${WORKER_MODE:-$(envval WORKER_MODE || true)}"
WM="${WM:-off}"
addenv WORKER_MODE "$WM"
addenv WORKER_JOB_NAME "${WORKER_JOB_NAME:-attesttag-worker}"
addenv WORKER_REGION "$REGION"
addenv WORKER_PROJECT "$PROJECT"
# The heavy job worker.sh builds for JVM and .NET repositories is used only where this names it,
# as ecosystem=job pairs. It is a comma-separated list, which is why it could not be passed at all
# until the settings stopped being joined by commas.
WJN="${WORKER_JOB_NAMES:-$(envval WORKER_JOB_NAMES || true)}"
[ -n "$WJN" ] && addenv WORKER_JOB_NAMES "$WJN"
WSA="${WORKER_SA_EMAIL:-$(envval WORKER_SA_EMAIL || true)}"
# `workers` and `cloudrun` both land here: the first works the platform out from K_SERVICE,
# which Cloud Run sets on every container it starts, and the second pins it.
case "$WM" in workers|cloudrun) WORKER_HERE=1 ;; *) WORKER_HERE=0 ;; esac
if [ "$WORKER_HERE" = 1 ]; then
  addenv WORKER_SA_EMAIL "${WSA:-attesttag-worker@$PROJECT.iam.gserviceaccount.com}"
  # The dependency cache (jobs_cache.go) lives in the replica bucket unless told otherwise, and
  # on Postgres there is none — LITESTREAM_BUCKET is not set above — so the cache was off there
  # and every job installed from cold. Named here, it is this service's own bucket on either
  # shape, unless the env file names another; WORKER_CACHE=off still turns it off.
  WCB="${WORKER_CACHE_BUCKET:-$(envval WORKER_CACHE_BUCKET || true)}"
  addenv WORKER_CACHE_BUCKET "${WCB:-$BUCKET}"
  WCO="${WORKER_CACHE:-$(envval WORKER_CACHE || true)}"
  [ -n "$WCO" ] && addenv WORKER_CACHE "$WCO"
fi
if [ "$WORKER_HERE" = 1 ] && ! gcloud run jobs describe "${WORKER_JOB_NAME:-attesttag-worker}" --region "$REGION" --project "$PROJECT" >/dev/null 2>&1; then
  echo "▸ warning: the fix worker is on ($WM) but the job ${WORKER_JOB_NAME:-attesttag-worker} does not exist yet; run deploy/gcp/worker.sh"
fi

# Always-on CPU runs the HTTP inbox and schedulers after responses are sent.
#
# On SQLite, one writer is enforced by the bot itself, not by this flag: it takes a lease on
# gs://$BUCKET/${LITESTREAM_PATH}.lock.json before it will restore or open the database
# (internal/app/lease.go). --max-instances 1 is a cost control there, and a second instance would
# be inert rather than destructive -- but it would also be useless, because a container that does
# not hold the lease serves 503 and Cloud Run gives you no way to route around it. On Postgres
# there is no lease and every instance serves, so the warning is SQLite's alone.
MAX_INSTANCES="${MAX_INSTANCES:-1}"
if [ "$MAX_INSTANCES" != "1" ] && [ -z "$DBURL" ]; then
  echo "▸ warning: --max-instances $MAX_INSTANCES — on SQLite every instance past the lease holder answers 503."
  echo "           The lease keeps them from corrupting the database; it cannot make them serve traffic."
fi
# Memory is sized for the transcripts, not the binary. A turn holds its whole message array for
# the length of the run, and a routine may now spend two hundred rounds building one; eight of
# those can be in flight at once (schedulerConcurrency), and an instance that runs out is killed
# rather than slowed. 1Gi is a few cents a month at one always-on instance.
gcloud run deploy "$SERVICE" --source . --project "$PROJECT" --region "$REGION" \
  --service-account "$SA" \
  --min-instances 1 --max-instances "$MAX_INSTANCES" --no-cpu-throttling --concurrency 20 \
  --cpu 1 --memory 1Gi --timeout 300 \
  --set-env-vars "^$D^$ENVS" --set-secrets "$SECRETS" \
  "$SQLARG" \
  --add-volume "name=bucket,type=cloud-storage,bucket=$BUCKET,readonly=true" \
  --add-volume-mount "volume=bucket,mount-path=/mnt/bucket" \
  --allow-unauthenticated --ingress all \
  --labels app=attesttag

URL="$(gcloud run services describe "$SERVICE" --project "$PROJECT" --region "$REGION" --format 'value(status.url)')"
echo "▸ deployed: $URL"
# `logs tail` exists only under `gcloud beta`; the plain command printed here used to fail as an
# unknown command. `logs read` is GA and shows what has already happened.
echo "▸ logs:     gcloud beta run services logs tail $SERVICE --project $PROJECT --region $REGION"
echo "            gcloud run services logs read $SERVICE --project $PROJECT --region $REGION"
# The replica and the lease exist only on SQLite; on Postgres these pointed at nothing.
if [ -z "$DBURL" ]; then
  echo "▸ replica:  gcloud storage ls gs://$BUCKET/litestream/"
  echo "▸ writer:   gcloud storage cat gs://$BUCKET/$LITESTREAM_PATH.lock.json"
fi

echo "▸ Slack Events Request URL: $URL/slack/events"
echo "▸ Slack Interactivity Request URL: $URL/slack/interactions"
echo "▸ Configure those HTTPS URLs (or your canonical domain) in Slack, verify Events, and disable Socket Mode."
