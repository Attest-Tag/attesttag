#!/usr/bin/env bash
# Deploy attest_tag to ECS Fargate behind an Application Load Balancer, with documents and the
# SQLite replica in S3. Idempotent: safe to rerun, and rerunning is how you ship a new image.
#
#   DOMAIN=bot.example.com ./deploy/aws/fargate.sh
#
# Requires: awscli v2 authenticated at an account, docker, python3, and a domain you can add a
# CNAME to. Reads secrets from $ENV_FILE (default .env — the file deploy/local/bootstrap.sh writes).
#
# ── Why Fargate and not App Runner ──
#
# App Runner is the obvious Cloud Run analogue and it is the wrong one. It throttles an
# instance's CPU whenever it is not serving a request, and offers no way to turn that off. This
# bot does its real work *between* requests — the Slack inbox dispatcher, the routine scheduler,
# the ingest loop, Drive sync — so on App Runner it would ack Slack's event inside three seconds
# and then never do the work. That reads as the bot ignoring people, not as a misconfiguration,
# which is the worst kind of wrong. A Fargate task's vCPU is its own for as long as the task
# runs. That is the whole reason this script is longer than an App Runner one would be.
#
# ── What it creates ──
#
#   S3 bucket                 documents, and the SQLite replica under the same prefix
#   IAM user + access key     the app signs S3 requests with static keys; it has no
#                             instance-metadata credential path (internal/app/docs_s3.go)
#   Secrets Manager secret    one JSON document, every secret from $ENV_FILE
#   ECR repository            the image, mirrored from ghcr.io or built from this checkout
#   ACM certificate           for $DOMAIN, validated by a CNAME you add
#   ALB + target group        the public https origin Slack needs
#   ECS cluster + service     exactly one task, always on
#
# ── The one-writer rule ──
#
# SQLite has one writer. The service runs desiredCount=1, and the deployment is configured so
# the old task stops before the new one starts (minimumHealthyPercent=0). That costs a few
# seconds of downtime per deploy and is the correct trade: two tasks against one replica is not
# a faster deployment, it is a corrupted one. Set DATABASE_URL to a Postgres and the ceiling
# comes off — see deploy/docs/storage.md.
#
# Never put the database on EFS. EFS is NFS, SQLite must not be run on NFS, and the corruption
# is quiet and noticed much later. Documents on EFS would be fine; they are ordinary file I/O.
set -euo pipefail
cd "$(dirname "$0")/../.."

: "${AWS_REGION:=${AWS_DEFAULT_REGION:-us-east-1}}"
: "${SERVICE:=attesttag}"
: "${CLUSTER:=attesttag}"
: "${ENV_FILE:=.env}"
: "${CPU:=1024}"
: "${MEMORY:=2048}"
: "${IMAGE_SOURCE:=ghcr}"          # ghcr = mirror the published image; build = build this checkout
: "${GHCR_IMAGE:=ghcr.io/attest-tag/attesttag:latest}"
export AWS_REGION AWS_DEFAULT_REGION="$AWS_REGION"

# python3 reads and writes every JSON document below. Checked with the rest, because missing it
# used to surface halfway through, after the bucket, the IAM user and its key had been created.
for BIN in aws docker python3; do
  command -v "$BIN" >/dev/null 2>&1 || { echo "$BIN is not on PATH" >&2; exit 1; }
done
[ -f "$ENV_FILE" ] || { echo "$ENV_FILE missing — run ./deploy/local/bootstrap.sh first, or set ENV_FILE" >&2; exit 1; }
[ -n "${DOMAIN:-}" ] || { echo "DOMAIN is required: the public https host Slack will deliver to. Slack will not accept an http redirect URL or deliver to an ALB's own *.elb.amazonaws.com name over http." >&2; exit 1; }

ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
: "${BUCKET:=attesttag-data-$ACCOUNT}"
echo "▸ account=$ACCOUNT region=$AWS_REGION service=$SERVICE domain=$DOMAIN bucket=s3://$BUCKET"

# envval strips the quotes godotenv strips when the app reads a dotenv file locally, so the
# deployed value is the same string that was tested with. The last line with a value wins: the
# template holds an empty line for every key, and reading the first one let that empty line hide
# a value appended below it — by hand, or by CREATE_DATABASE=1 further down.
envval() {
  grep -E "^$1=." "$ENV_FILE" | tail -n 1 | cut -d= -f2- | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/"
}

# MASTER_KEY seals every stored credential, and the image refuses to start without one
# (REQUIRE_MASTER_KEY in the Dockerfile), so a task deployed without it only crash-loops. Caught
# here instead, before a build, a push and a rolled task.
[ -n "$(envval MASTER_KEY)" ] || { echo "MASTER_KEY is missing from $ENV_FILE. Generate one with: openssl rand -base64 32" >&2; exit 1; }
[ -n "$(envval SLACK_SIGNING_SECRET || true)" ] || { echo "SLACK_SIGNING_SECRET is missing from $ENV_FILE. Copy it from Slack → Basic Information → App Credentials." >&2; exit 1; }
# The OAuth pair an install runs on. Without it the service refuses to start, so catching it
# here saves a build, a push and a rolled task.
for K in SLACK_CLIENT_ID SLACK_CLIENT_SECRET; do
  [ -n "$(envval "$K" || true)" ] || { echo "$K is missing from $ENV_FILE. Connecting a workspace is an OAuth install and the service will not start without it: Slack → Basic Information → App Credentials, and register <base>/slack/oauth/callback as a redirect URL." >&2; exit 1; }
done
[ -n "$(envval RESEND_API_KEY || true)" ] || echo "▸ warning: RESEND_API_KEY is unset — verification, reset and invitation links will be logged rather than emailed. The console offers them to copy by hand, so this is usable; it is not silent."

# ── Network. The default VPC's public subnets, because a bot that talks to Slack, OpenRouter and
# S3 needs egress and nothing needs to reach it except the load balancer. Override VPC_ID and
# SUBNETS to place it in a VPC of your own; the task needs a public IP or a NAT either way, or
# it cannot pull its own image.
if [ -z "${VPC_ID:-}" ]; then
  VPC_ID="$(aws ec2 describe-vpcs --filters Name=is-default,Values=true --query 'Vpcs[0].VpcId' --output text)"
  [ "$VPC_ID" != "None" ] || { echo "no default VPC in $AWS_REGION — set VPC_ID and SUBNETS (two public subnets in different availability zones; an ALB requires two)" >&2; exit 1; }
fi
if [ -z "${SUBNETS:-}" ]; then
  SUBNETS="$(aws ec2 describe-subnets --filters "Name=vpc-id,Values=$VPC_ID" "Name=map-public-ip-on-launch,Values=true" \
    --query 'Subnets[].SubnetId' --output text | tr '\t' ',')"
fi
SUBNET_COUNT=$(echo "$SUBNETS" | tr ',' '\n' | grep -c .)
[ "$SUBNET_COUNT" -ge 2 ] || { echo "found $SUBNET_COUNT public subnet(s) in $VPC_ID; an ALB needs two in different availability zones. Set SUBNETS=subnet-a,subnet-b" >&2; exit 1; }
echo "▸ vpc=$VPC_ID subnets=$SUBNETS"

# ── Security groups. The load balancer takes 80 and 443 from the internet; the task takes 8080
# from the load balancer and from nowhere else.
sg_id() { aws ec2 describe-security-groups --filters "Name=vpc-id,Values=$VPC_ID" "Name=group-name,Values=$1" --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null; }
ensure_sg() {
  local NAME="$1" DESC="$2" ID
  ID="$(sg_id "$NAME")"
  if [ -z "$ID" ] || [ "$ID" = "None" ]; then
    ID="$(aws ec2 create-security-group --group-name "$NAME" --description "$DESC" --vpc-id "$VPC_ID" --query GroupId --output text)"
  fi
  printf '%s' "$ID"
}
ALB_SG="$(ensure_sg "$SERVICE-alb" "attest_tag load balancer")"
TASK_SG="$(ensure_sg "$SERVICE-task" "attest_tag task")"
for PORT in 80 443; do
  aws ec2 authorize-security-group-ingress --group-id "$ALB_SG" --protocol tcp --port "$PORT" --cidr 0.0.0.0/0 >/dev/null 2>&1 || true
done
aws ec2 authorize-security-group-ingress --group-id "$TASK_SG" --protocol tcp --port 8080 --source-group "$ALB_SG" >/dev/null 2>&1 || true

# ── S3. Documents live under docs/, and the SQLite replica lands beside them at
# docs/litestream/attesttag.db — under the prefix rather than at the bucket root, so two
# deployments sharing a bucket cannot land two databases on one key.
if ! aws s3api head-bucket --bucket "$BUCKET" >/dev/null 2>&1; then
  if [ "$AWS_REGION" = "us-east-1" ]; then
    aws s3api create-bucket --bucket "$BUCKET" >/dev/null
  else
    aws s3api create-bucket --bucket "$BUCKET" --create-bucket-configuration "LocationConstraint=$AWS_REGION" >/dev/null
  fi
  aws s3api put-public-access-block --bucket "$BUCKET" --public-access-block-configuration \
    BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true >/dev/null
  echo "▸ created s3://$BUCKET"
fi

# ── The S3 credential. The app signs its own requests with SigV4 and reads the key from
# DOCS_S3_KEY_ID/DOCS_S3_SECRET; there is no instance-role path, so a task role would not be
# read. That is why this creates an IAM user whose only permission is this one bucket prefix.
# The key is created once and kept in Secrets Manager: a rerun must not mint a second one.
S3_USER="attesttag-s3"
S3_KEY_SECRET="attesttag/s3-key"
if ! aws iam get-user --user-name "$S3_USER" >/dev/null 2>&1; then
  aws iam create-user --user-name "$S3_USER" >/dev/null
fi
aws iam put-user-policy --user-name "$S3_USER" --policy-name bucket-only --policy-document "$(cat <<JSON
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:ListBucket"],"Resource":"arn:aws:s3:::$BUCKET","Condition":{"StringLike":{"s3:prefix":["docs/*","docs"]}}},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":"arn:aws:s3:::$BUCKET/docs/*"}]}
JSON
)" >/dev/null

if ! aws secretsmanager describe-secret --secret-id "$S3_KEY_SECRET" >/dev/null 2>&1; then
  echo "▸ minting an access key for $S3_USER (once — it is kept in Secrets Manager)"
  KEY_JSON="$(aws iam create-access-key --user-name "$S3_USER" --query 'AccessKey.{id:AccessKeyId,secret:SecretAccessKey}' --output json)"
  aws secretsmanager create-secret --name "$S3_KEY_SECRET" --secret-string "$KEY_JSON" >/dev/null
fi
S3_KEY_ID="$(aws secretsmanager get-secret-value --secret-id "$S3_KEY_SECRET" --query SecretString --output text | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"

# ── A managed Postgres, if you ask for one. Off by default, because the deployment this script
# builds runs on SQLite replicated to the bucket, and for a single instance that is the right
# shape and costs cents. CREATE_DATABASE=1 creates an RDS instance instead.
#
# The DSN is appended to $ENV_FILE rather than kept only in Secrets Manager, for two reasons:
# it is what makes a rerun idempotent — DATABASE_URL is simply set the next time — and a
# password that exists only inside an AWS secret is one you cannot use to connect with psql or
# to move the deployment somewhere else.
if [ "${CREATE_DATABASE:-0}" = "1" ] && [ -z "$(envval DATABASE_URL || true)" ]; then
  DB_ID="${DB_INSTANCE:-$SERVICE-pg}"
  DB_CLASS="${DB_INSTANCE_CLASS:-db.t4g.micro}"
  DB_STORAGE="${DB_ALLOCATED_STORAGE:-20}"

  # Creating something that bills monthly is not something to do because a variable was set in
  # the wrong shell, so it is listed and confirmed. ASSUME_YES=1 is for a scripted install.
  cat <<BILL

CREATE_DATABASE=1 will create these billable resources in account $ACCOUNT, region $AWS_REGION:

  RDS PostgreSQL    $DB_ID — $DB_CLASS, ${DB_STORAGE} GiB gp3, encrypted, 7 days of backups
  DB subnet group   $DB_ID-subnets, across $SUBNETS
  security group    $DB_ID, reachable on 5432 from the $SERVICE-task group and nothing else

A db.t4g.micro is in the region of twenty dollars a month with storage and backups; check the
current price for $AWS_REGION rather than trusting this line. Leaving CREATE_DATABASE unset
keeps the rows in SQLite replicated to s3://$BUCKET, which costs cents and needs no server.

BILL
  if [ "${ASSUME_YES:-0}" != "1" ]; then
    [ -t 0 ] || { echo "stdin is not a terminal and ASSUME_YES is unset — refusing to create billable resources that nobody confirmed" >&2; exit 1; }
    printf 'Create them? [y/N] '
    read -r REPLY
    case "$REPLY" in [yY]*) ;; *) echo "Nothing was created."; exit 1 ;; esac
  fi

  DB_SG="$(ensure_sg "$DB_ID" "attest_tag database")"
  # The database is reachable from the task and from nothing else — not from the load balancer,
  # not from the internet. --no-publicly-accessible below is the other half of that.
  aws ec2 authorize-security-group-ingress --group-id "$DB_SG" --protocol tcp --port 5432 \
    --source-group "$TASK_SG" >/dev/null 2>&1 || true
  aws rds describe-db-subnet-groups --db-subnet-group-name "$DB_ID-subnets" >/dev/null 2>&1 || \
    aws rds create-db-subnet-group --db-subnet-group-name "$DB_ID-subnets" \
      --db-subnet-group-description "attest_tag" --subnet-ids $(echo "$SUBNETS" | tr ',' ' ') >/dev/null

  if aws rds describe-db-instances --db-instance-identifier "$DB_ID" >/dev/null 2>&1; then
    # The instance is there but the DSN is not in $ENV_FILE, so the password is gone: it was
    # only ever written to that file. Resetting it is a decision for you, not for this script.
    echo "RDS instance $DB_ID already exists but DATABASE_URL is not in $ENV_FILE, so this script does not know its password. Put the DSN in $ENV_FILE by hand, or reset the password with: aws rds modify-db-instance --db-instance-identifier $DB_ID --master-user-password <new> --apply-immediately" >&2
    exit 1
  fi
  # The password goes into a DSN, so it is stripped of everything that would have to be escaped
  # in a URL. RDS rejects /, ", @ and spaces in a master password in any case.
  DB_PASS="$(openssl rand -base64 30 | tr -d '/+=@" ' | cut -c1-24)"
  aws rds create-db-instance --db-instance-identifier "$DB_ID" \
    --engine postgres --db-instance-class "$DB_CLASS" \
    --allocated-storage "$DB_STORAGE" --storage-type gp3 --storage-encrypted \
    --master-username attesttag --master-user-password "$DB_PASS" --db-name attesttag \
    --db-subnet-group-name "$DB_ID-subnets" --vpc-security-group-ids "$DB_SG" \
    --no-publicly-accessible --backup-retention-period 7 --no-multi-az >/dev/null
  echo "▸ creating RDS $DB_ID — five to ten minutes"
  aws rds wait db-instance-available --db-instance-identifier "$DB_ID"
  DB_HOST="$(aws rds describe-db-instances --db-instance-identifier "$DB_ID" \
    --query 'DBInstances[0].Endpoint.Address' --output text)"
  # Appended with a restrictive umask, and never echoed: the host is printed, the DSN is not.
  ( umask 077
    printf '\n# Written by deploy/aws/fargate.sh (CREATE_DATABASE=1) on %s\nDATABASE_URL=postgres://attesttag:%s@%s:5432/attesttag?sslmode=require\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$DB_PASS" "$DB_HOST" >> "$ENV_FILE" )
  echo "▸ database: RDS $DB_ID at $DB_HOST — DATABASE_URL written to $ENV_FILE"
  echo "▸ back up $ENV_FILE: it now holds the only copy of that password, as well as MASTER_KEY"
fi

# ── Application secrets. One Secrets Manager document holding every secret key present in
# $ENV_FILE; ECS reads individual JSON keys out of it, so this is one secret rather than
# fifteen, and rotating any of them is a rerun of this script.
APP_SECRET="attesttag/env"
SECRET_KEYS=""
SECRET_JSON="{"
for KEY in SLACK_SIGNING_SECRET OPENROUTER_API_KEY LLM_API_KEY MASTER_KEY MASTER_KEY_PREVIOUS SLACK_CLIENT_ID \
           SLACK_CLIENT_SECRET RESEND_API_KEY OPENROUTER_PROVISIONING_KEY WORKER_LLM_API_KEY WORKER_ENGINE_API_KEY \
           HEALTH_SECRET OPERATOR_SECRET GITHUB_APP_PRIVATE_KEY_B64 GITHUB_APP_CLIENT_SECRET DATABASE_URL \
           MSTEAMS_APP_PASSWORD; do
  VAL="$(envval "$KEY" || true)"
  [ -z "$VAL" ] && continue
  ESC="$(printf '%s' "$VAL" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')"
  SECRET_JSON="$SECRET_JSON${SECRET_KEYS:+,}\"$KEY\":$ESC"
  SECRET_KEYS="${SECRET_KEYS:+$SECRET_KEYS }$KEY"
done
# The S3 secret travels in the same document, so the task definition names one secret ARN.
S3_SECRET_VAL="$(aws secretsmanager get-secret-value --secret-id "$S3_KEY_SECRET" --query SecretString --output text | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["secret"]))')"
SECRET_JSON="$SECRET_JSON,\"DOCS_S3_SECRET\":$S3_SECRET_VAL}"
SECRET_KEYS="$SECRET_KEYS DOCS_S3_SECRET"

if aws secretsmanager describe-secret --secret-id "$APP_SECRET" >/dev/null 2>&1; then
  aws secretsmanager put-secret-value --secret-id "$APP_SECRET" --secret-string "$SECRET_JSON" >/dev/null
else
  aws secretsmanager create-secret --name "$APP_SECRET" --secret-string "$SECRET_JSON" >/dev/null
fi
APP_SECRET_ARN="$(aws secretsmanager describe-secret --secret-id "$APP_SECRET" --query ARN --output text)"
S3_KEY_SECRET_ARN="$(aws secretsmanager describe-secret --secret-id "$S3_KEY_SECRET" --query ARN --output text)"

# ── The image. Fargate can pull from a public registry, but only through a NAT or a public IP
# and without the retry behaviour ECR gives you, so the published image is mirrored into ECR.
# IMAGE_SOURCE=build builds this checkout instead, which is what you want when you have changed
# something. Either way the tag is immutable per deploy, so a rollback is a previous tag.
aws ecr describe-repositories --repository-names "$SERVICE" >/dev/null 2>&1 || \
  aws ecr create-repository --repository-name "$SERVICE" --image-scanning-configuration scanOnPush=true >/dev/null
REGISTRY="$ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com"
TAG="$(git rev-parse --short=12 HEAD 2>/dev/null || date +%Y%m%d%H%M%S)"
IMAGE="$REGISTRY/$SERVICE:$TAG"
aws ecr get-login-password | docker login --username AWS --password-stdin "$REGISTRY" >/dev/null

if [ "$IMAGE_SOURCE" = "build" ]; then
  echo "▸ building this checkout for linux/amd64"
  docker build --platform linux/amd64 --build-arg "GIT_COMMIT=$TAG" -t "$IMAGE" .
else
  echo "▸ mirroring $GHCR_IMAGE"
  docker pull --platform linux/amd64 "$GHCR_IMAGE"
  docker tag "$GHCR_IMAGE" "$IMAGE"
fi
docker push "$IMAGE" >/dev/null
echo "▸ image=$IMAGE"

# ── Certificate. Requested once; validated by a CNAME you add at your DNS provider. The script
# waits a bounded time and then tells you what is outstanding rather than hanging, because the
# record is yours to add and nothing here can do it for you.
CERT_ARN="${ACM_CERT_ARN:-$(aws acm list-certificates --query "CertificateSummaryList[?DomainName=='$DOMAIN'].CertificateArn | [0]" --output text 2>/dev/null || true)}"
if [ -z "$CERT_ARN" ] || [ "$CERT_ARN" = "None" ]; then
  CERT_ARN="$(aws acm request-certificate --domain-name "$DOMAIN" --validation-method DNS --query CertificateArn --output text)"
  echo "▸ requested a certificate for $DOMAIN"
  sleep 5
fi
CERT_STATUS="$(aws acm describe-certificate --certificate-arn "$CERT_ARN" --query Certificate.Status --output text)"
if [ "$CERT_STATUS" != "ISSUED" ]; then
  echo ""
  echo "The certificate for $DOMAIN is $CERT_STATUS. Add this CNAME at your DNS provider:"
  aws acm describe-certificate --certificate-arn "$CERT_ARN" \
    --query 'Certificate.DomainValidationOptions[0].ResourceRecord.{Name:Name,Value:Value}' --output table
  echo "Then rerun this script. Everything above is already created; it will pick up from here."
  exit 1
fi

# ── Load balancer, target group, listeners. The health check is /health, which the gate serves
# before authentication (internal/app/gate.go). Deregistration is quick because there is only
# ever one task and a slow drain just lengthens the deploy gap.
ALB_ARN="$(aws elbv2 describe-load-balancers --names "$SERVICE" --query 'LoadBalancers[0].LoadBalancerArn' --output text 2>/dev/null || true)"
if [ -z "$ALB_ARN" ] || [ "$ALB_ARN" = "None" ]; then
  ALB_ARN="$(aws elbv2 create-load-balancer --name "$SERVICE" --type application --scheme internet-facing \
    --subnets $(echo "$SUBNETS" | tr ',' ' ') --security-groups "$ALB_SG" \
    --query 'LoadBalancers[0].LoadBalancerArn' --output text)"
fi
ALB_DNS="$(aws elbv2 describe-load-balancers --load-balancer-arns "$ALB_ARN" --query 'LoadBalancers[0].DNSName' --output text)"

TG_ARN="$(aws elbv2 describe-target-groups --names "$SERVICE" --query 'TargetGroups[0].TargetGroupArn' --output text 2>/dev/null || true)"
if [ -z "$TG_ARN" ] || [ "$TG_ARN" = "None" ]; then
  TG_ARN="$(aws elbv2 create-target-group --name "$SERVICE" --protocol HTTP --port 8080 --vpc-id "$VPC_ID" \
    --target-type ip --health-check-path /health --health-check-interval-seconds 30 \
    --healthy-threshold-count 2 --unhealthy-threshold-count 3 \
    --query 'TargetGroups[0].TargetGroupArn' --output text)"
  aws elbv2 modify-target-group-attributes --target-group-arn "$TG_ARN" \
    --attributes Key=deregistration_delay.timeout_seconds,Value=15 >/dev/null
fi

listener_for() { aws elbv2 describe-listeners --load-balancer-arn "$ALB_ARN" --query "Listeners[?Port==\`$1\`].ListenerArn | [0]" --output text 2>/dev/null; }
if [ "$(listener_for 443)" = "None" ] || [ -z "$(listener_for 443)" ]; then
  aws elbv2 create-listener --load-balancer-arn "$ALB_ARN" --protocol HTTPS --port 443 \
    --certificates "CertificateArn=$CERT_ARN" --ssl-policy ELBSecurityPolicy-TLS13-1-2-2021-06 \
    --default-actions "Type=forward,TargetGroupArn=$TG_ARN" >/dev/null
fi
# Port 80 redirects rather than serves: the console sets Secure cookies and Slack's redirect URL
# must be https, so there is nothing correct to answer on http.
if [ "$(listener_for 80)" = "None" ] || [ -z "$(listener_for 80)" ]; then
  aws elbv2 create-listener --load-balancer-arn "$ALB_ARN" --protocol HTTP --port 80 \
    --default-actions 'Type=redirect,RedirectConfig={Protocol=HTTPS,Port=443,StatusCode=HTTP_301}' >/dev/null
fi

# ── Roles. The execution role is what pulls the image and reads the secrets; the task role is
# what the running container may do, and the answer is nothing — its S3 access comes from the
# static key above, not from the role.
TRUST='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ecs-tasks.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
EXEC_ROLE="$SERVICE-exec"
if ! aws iam get-role --role-name "$EXEC_ROLE" >/dev/null 2>&1; then
  aws iam create-role --role-name "$EXEC_ROLE" --assume-role-policy-document "$TRUST" >/dev/null
  aws iam attach-role-policy --role-name "$EXEC_ROLE" \
    --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy >/dev/null
fi
aws iam put-role-policy --role-name "$EXEC_ROLE" --policy-name read-secrets --policy-document \
  "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"secretsmanager:GetSecretValue\",\"Resource\":[\"$APP_SECRET_ARN\",\"$S3_KEY_SECRET_ARN\"]}]}" >/dev/null
EXEC_ROLE_ARN="$(aws iam get-role --role-name "$EXEC_ROLE" --query Role.Arn --output text)"

aws logs create-log-group --log-group-name "/ecs/$SERVICE" >/dev/null 2>&1 || true
aws logs put-retention-policy --log-group-name "/ecs/$SERVICE" --retention-in-days 30 >/dev/null 2>&1 || true

# ── Environment. Everything that is not a secret. DOCS_S3_URL is what makes this deployment
# survive losing its task: documents go to the bucket, and naming it also streams the SQLite
# database there and restores it when a task starts with an empty disk
# (internal/app/replica_target.go). With DATABASE_URL set the rows are in Postgres instead and
# the bucket holds only documents — the binary prefers DATABASE_URL over DB_PATH.
TZ="$(envval TZ_NAME || true)"; TZ="${TZ:-UTC}"
DOMAINS="$(envval ALLOWED_EMAIL_DOMAINS | tr ',;' '  ' || true)"
[ -n "$DOMAINS" ] || echo "▸ warning: ALLOWED_EMAIL_DOMAINS is unset — an organisation with no email-domain list of its own lets every member of its workspace use the bot. Guests and Slack Connect members are refused either way, unless the organisation allows them under Settings → Security"

ENV_JSON="$(python3 - "$BUCKET" "$AWS_REGION" "$S3_KEY_ID" "$DOMAIN" "$TZ" "$DOMAINS" "$TAG" "$(envval MAIL_FROM || true)" "$(envval SIGNUP_MODE || true)" <<'PY'
import json, sys
bucket, region, key_id, domain, tz, domains, commit, mail_from, signup = sys.argv[1:10]
env = {
    "DOCS_S3_URL": f"s3://{bucket}/docs?region={region}",
    "DOCS_S3_KEY_ID": key_id,
    "ADMIN_BASE_URL": f"https://{domain}",
    "TZ_NAME": tz,
    "GIT_COMMIT": commit,
    "PORT": "8080",
}
if domains: env["ALLOWED_EMAIL_DOMAINS"] = domains
if mail_from: env["MAIL_FROM"] = mail_from
if signup: env["SIGNUP_MODE"] = signup
print(json.dumps([{"name": k, "value": v} for k, v in env.items()]))
PY
)"
# ── Plain settings passed on as the env file has them, the same list containerapps.sh passes:
# a model endpoint other than OpenRouter (its key, LLM_API_KEY, travels with the secrets above),
# the GitHub App's public half (its private key and client secret are secrets above; without the
# id and slug no App can be set up on this deployment), and Microsoft Teams (guide/msteams.md),
# whose app id and tenant id are in the package every tenant installs, so only its password is a
# secret. MSTEAMS_APP_TYPE left unset means SingleTenant.
PLAIN_ARGS=()
for KEY in LLM_BASE_URL LLM_MODEL HEAVY_MODEL EMBED_MODEL ORG_MODEL_KEYS LOG_LEVEL \
           GITHUB_APP_ID GITHUB_APP_SLUG GITHUB_APP_CLIENT_ID \
           MSTEAMS_APP_ID MSTEAMS_TENANT_ID MSTEAMS_APP_TYPE MSTEAMS_SIGNIN; do
  PLAIN_ARGS+=("$KEY=$(envval "$KEY" || true)")
done
ENV_JSON="$(python3 -c '
import json, sys
env = json.loads(sys.argv[1])
for pair in sys.argv[2:]:
    name, _, value = pair.partition("=")
    if value:
        env.append({"name": name, "value": value})
print(json.dumps(env))
' "$ENV_JSON" "${PLAIN_ARGS[@]}")"
# ── The fix-job worker, if deploy/aws/worker.sh has been run. WORKER_MODE comes from the shell
# or $ENV_FILE and is off by default, so the "fix this and raise a PR" tool is not offered until
# the worker task definition exists. The cluster, subnets and security group default to the ones
# this script already resolved, which is what worker.sh puts the worker in too.
WM="${WORKER_MODE:-$(envval WORKER_MODE || true)}"
WM="${WM:-off}"
# `workers` and `ecs` both land here: the first works the platform out from the task-role
# endpoint every ECS task is given, and the second pins it.
if [ "$WM" = "workers" ] || [ "$WM" = "ecs" ]; then
  WJOB="${WORKER_JOB_NAME:-$(envval WORKER_JOB_NAME || true)}"; WJOB="${WJOB:-attesttag-worker}"
  if ! aws ecs describe-task-definition --task-definition "$WJOB" >/dev/null 2>&1; then
    echo "▸ warning: the fix worker is on ($WM) but the task definition $WJOB does not exist yet; run deploy/aws/worker.sh" >&2
  fi
  # The bot signs its own RunTask calls, so it needs a task role — the first AWS permission the
  # running container has ever needed. worker.sh creates it; without it the tool is offered and
  # then fails on the first request with an access-denied nobody will connect to this script.
  BOT_TASK_ROLE_ARN="$(aws iam get-role --role-name "$SERVICE-task" --query Role.Arn --output text 2>/dev/null || true)"
  if [ -z "$BOT_TASK_ROLE_ARN" ] || [ "$BOT_TASK_ROLE_ARN" = "None" ]; then
    echo "▸ warning: no $SERVICE-task role — run deploy/aws/worker.sh, which creates it and scopes it to the worker definitions" >&2
    BOT_TASK_ROLE_ARN=""
  fi
  WSUB="${WORKER_ECS_SUBNETS:-$(envval WORKER_ECS_SUBNETS || true)}"; WSUB="${WSUB:-$SUBNETS}"
  WSG="${WORKER_ECS_SECURITY_GROUPS:-$(envval WORKER_ECS_SECURITY_GROUPS || true)}"
  if [ -z "$WSG" ]; then
    WSG="$(aws ec2 describe-security-groups --filters "Name=vpc-id,Values=$VPC_ID" "Name=group-name,Values=$WJOB" \
      --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null || true)"
    [ "$WSG" = "None" ] && WSG=""
  fi
  # Exported rather than passed positionally: seven more arguments on the block above would be
  # seven more places to get the order wrong, and these are already environment variables.
  export WORKER_MODE="$WM" WORKER_REGION="$AWS_REGION" WORKER_JOB_NAME="$WJOB"
  export WORKER_ECS_CLUSTER="${WORKER_ECS_CLUSTER:-$CLUSTER}"
  export WORKER_ECS_SUBNETS="$WSUB" WORKER_ECS_SECURITY_GROUPS="$WSG"
  export WORKER_JOB_NAMES="${WORKER_JOB_NAMES:-$(envval WORKER_JOB_NAMES || true)}"
  [ -n "$WSG" ] || echo "▸ warning: no worker security group found; the task will use the VPC default, which may not allow egress" >&2
  ENV_JSON="$(python3 -c '
import json, os, sys
env = json.loads(sys.argv[1])
for name in ("WORKER_MODE", "WORKER_REGION", "WORKER_ECS_CLUSTER", "WORKER_ECS_SUBNETS",
             "WORKER_ECS_SECURITY_GROUPS", "WORKER_JOB_NAME", "WORKER_JOB_NAMES"):
    if os.environ.get(name):
        env.append({"name": name, "value": os.environ[name]})
print(json.dumps(env))
' "$ENV_JSON")"
fi

SECRETS_JSON="$(python3 - "$APP_SECRET_ARN" "$SECRET_KEYS" <<'PY'
import json, sys
arn, keys = sys.argv[1], sys.argv[2].split()
print(json.dumps([{"name": k, "valueFrom": f"{arn}:{k}::"} for k in keys]))
PY
)"

# stopTimeout is 45 rather than the default 30: the write lease wants about nine seconds to be
# released cleanly on the way down, and a task killed before it lets go leaves the next one
# waiting out the lease's expiry instead of starting (internal/app/lease.go).
TASKDEF_ARN="$(aws ecs register-task-definition --cli-input-json "$(python3 - "$SERVICE" "$CPU" "$MEMORY" "$EXEC_ROLE_ARN" "$IMAGE" "$AWS_REGION" "$ENV_JSON" "$SECRETS_JSON" "${BOT_TASK_ROLE_ARN:-}" <<'PY'
import json, sys
family, cpu, mem, exec_role, image, region, env, secrets, task_role = sys.argv[1:10]
print(json.dumps({
    "family": family,
    "networkMode": "awsvpc",
    "requiresCompatibilities": ["FARGATE"],
    "cpu": cpu, "memory": mem,
    "executionRoleArn": exec_role,
    # No task role at all unless the fix-job worker is on: the bot signs its S3 requests with a
    # static key and has never needed an AWS identity of its own. worker.sh creates the role and
    # scopes it to starting the worker definitions, and nothing else.
    **({"taskRoleArn": task_role} if task_role else {}),
    "runtimePlatform": {"cpuArchitecture": "X86_64", "operatingSystemFamily": "LINUX"},
    "containerDefinitions": [{
        "name": family, "image": image, "essential": True,
        "portMappings": [{"containerPort": 8080, "protocol": "tcp"}],
        "environment": json.loads(env),
        "secrets": json.loads(secrets),
        "stopTimeout": 45,
        "logConfiguration": {"logDriver": "awslogs", "options": {
            "awslogs-group": f"/ecs/{family}", "awslogs-region": region, "awslogs-stream-prefix": "ecs"}},
    }],
}))
PY
)" --query 'taskDefinition.taskDefinitionArn' --output text)"
echo "▸ task definition=$TASKDEF_ARN"

# ── Cluster and service. minimumHealthyPercent=0 with maximumPercent=100 is the single-writer
# rule expressed to ECS: the old task is stopped before the new one starts, so there is never a
# moment with two. It buys a short gap on every deploy. The alternative buys a corrupt database.
aws ecs describe-clusters --clusters "$CLUSTER" --query 'clusters[0].status' --output text 2>/dev/null | grep -q ACTIVE || \
  aws ecs create-cluster --cluster-name "$CLUSTER" >/dev/null
NET="awsvpcConfiguration={subnets=[$SUBNETS],securityGroups=[$TASK_SG],assignPublicIp=ENABLED}"
if aws ecs describe-services --cluster "$CLUSTER" --services "$SERVICE" --query 'services[0].status' --output text 2>/dev/null | grep -q ACTIVE; then
  aws ecs update-service --cluster "$CLUSTER" --service "$SERVICE" --task-definition "$TASKDEF_ARN" \
    --desired-count 1 --network-configuration "$NET" \
    --deployment-configuration "minimumHealthyPercent=0,maximumPercent=100" >/dev/null
else
  aws ecs create-service --cluster "$CLUSTER" --service-name "$SERVICE" --task-definition "$TASKDEF_ARN" \
    --desired-count 1 --launch-type FARGATE --network-configuration "$NET" \
    --deployment-configuration "minimumHealthyPercent=0,maximumPercent=100" \
    --health-check-grace-period-seconds 120 \
    --load-balancers "targetGroupArn=$TG_ARN,containerName=$SERVICE,containerPort=8080" >/dev/null
fi

cat <<NEXT

Deployed. One task, always on, in cluster $CLUSTER.

  1. Point $DOMAIN at the load balancer, as a CNAME (or an A/ALIAS in Route 53):

       $DOMAIN  CNAME  $ALB_DNS

  2. Wait for the service to settle, then check it:

       aws ecs wait services-stable --cluster $CLUSTER --services $SERVICE
       curl -fsS https://$DOMAIN/health

  3. Make the Slack app point at it:

       BASE_URL=https://$DOMAIN ./deploy/slack/manifest.sh

     Paste the output at api.slack.com/apps -> Create New App -> From a manifest, then copy the
     Signing Secret, Client ID and Client Secret into $ENV_FILE and rerun this script.

  4. Open https://$DOMAIN and sign up. The first sign-up founds the deployment; everybody after
     it arrives by invitation.

Logs:      aws logs tail /ecs/$SERVICE --follow
Redeploy:  rerun this script — it registers a new task definition and rolls the one task.

Back up the MASTER_KEY line in $ENV_FILE somewhere other than this machine and this account.
It seals every stored credential. The task reads its copy from the Secrets Manager secret
$APP_SECRET, which is the only other one: delete that secret or lose the account, and this file
is all that is left.
NEXT
