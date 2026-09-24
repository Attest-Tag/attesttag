#!/usr/bin/env bash
# Build the fix-job worker image and register the ECS task definition the bot runs it as.
# Rerun to ship a new image. Idempotent, like fargate.sh.
#
#   ./deploy/aws/worker.sh
#
# Then deploy the bot with WORKER_MODE=workers; fargate.sh passes the WORKER_* settings through and
# attaches the task role this script creates. Run this first — the bot cannot start a task
# definition that does not exist yet, and fargate.sh says so rather than deploying something
# that fails on the first fix request.
#
# ── What it creates ──
#
#   ECR repository            attesttag-worker, and attesttag-worker-jvm unless WORKER_JVM=0
#   IAM role attesttag-worker-exec  pulls the image and writes logs; the usual execution role
#   IAM role attesttag-worker-task  the worker's own identity, granted nothing at all
#   IAM role attesttag-task         the *bot's* task role: RunTask/DescribeTasks/StopTask on
#                                   these definitions only, plus PassRole for the two above
#   security group            attesttag-worker: egress only, no ingress from anywhere
#   task definitions          attesttag-worker (2 vCPU/4 GB) and attesttag-worker-jvm (4/8)
#
# ── The shape of the permission ──
#
# The worker's own task role is granted nothing: no secrets, no buckets, no APIs. Everything it
# uses — the spec, the repository token, the model key — arrives over the claim call it makes
# back to the bot, so a compromised worker can reach nothing else in the account. What the bot
# may do is scoped the other way: RunTask is conditioned on the cluster, and PassRole is
# restricted to the two worker roles, so a bug in the bot cannot start an arbitrary task with an
# arbitrary role.
set -euo pipefail
cd "$(dirname "$0")/../.."

: "${AWS_REGION:=${AWS_DEFAULT_REGION:-us-east-1}}"
: "${SERVICE:=attesttag}"
: "${CLUSTER:=attesttag}"
: "${JOB:=attesttag-worker}"
: "${ENV_FILE:=.env}"
: "${CPU:=2048}"
: "${MEMORY:=4096}"
: "${JVM_CPU:=4096}"
: "${JVM_MEMORY:=8192}"
# The heavy worker (JDK, Maven, Gradle, .NET). WORKER_JVM=0 skips it, which is the right choice
# for a deployment with no JVM or .NET repositories: it is a large image to build and store.
: "${WORKER_JVM:=1}"
: "${JVM_JOB:=attesttag-worker-jvm}"
export AWS_REGION AWS_DEFAULT_REGION="$AWS_REGION"

for BIN in aws docker python3; do
  command -v "$BIN" >/dev/null 2>&1 || { echo "$BIN is not on PATH" >&2; exit 1; }
done

ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
REGISTRY="$ACCOUNT.dkr.ecr.$AWS_REGION.amazonaws.com"
TAG="$(git rev-parse --short=12 HEAD 2>/dev/null || date +%Y%m%d%H%M%S)"

# ── Network. The same VPC and public subnets fargate.sh puts the bot in, because a worker needs
# egress — GitHub, the package registries, the bot's own public URL — and nothing needs to reach
# it. Override VPC_ID and SUBNETS to place it somewhere else; either way the task needs a public
# IP or a NAT, or it will sit in PROVISIONING until it times out.
if [ -z "${VPC_ID:-}" ]; then
  VPC_ID="$(aws ec2 describe-vpcs --filters Name=is-default,Values=true --query 'Vpcs[0].VpcId' --output text)"
  [ "$VPC_ID" != "None" ] || { echo "no default VPC in $AWS_REGION — set VPC_ID and SUBNETS" >&2; exit 1; }
fi
if [ -z "${SUBNETS:-}" ]; then
  SUBNETS="$(aws ec2 describe-subnets --filters "Name=vpc-id,Values=$VPC_ID" "Name=map-public-ip-on-launch,Values=true" \
    --query 'Subnets[].SubnetId' --output text | tr '\t' ',')"
fi
[ -n "$SUBNETS" ] || { echo "no public subnets in $VPC_ID — set SUBNETS=subnet-a,subnet-b" >&2; exit 1; }

WORKER_SG="$(aws ec2 describe-security-groups --filters "Name=vpc-id,Values=$VPC_ID" "Name=group-name,Values=$JOB" \
  --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null || true)"
if [ -z "$WORKER_SG" ] || [ "$WORKER_SG" = "None" ]; then
  # No ingress rule is ever added: a worker makes outbound calls and accepts nothing. The default
  # egress-to-anywhere rule a new group carries is what it needs and all it needs.
  WORKER_SG="$(aws ec2 create-security-group --group-name "$JOB" --description "attest_tag fix worker (egress only)" \
    --vpc-id "$VPC_ID" --query GroupId --output text)"
fi
echo "▸ vpc=$VPC_ID subnets=$SUBNETS sg=$WORKER_SG"

# ── Images. Built here and pushed to ECR; Fargate cannot pull from a private registry without a
# credential, and ECR is the one it already has through the execution role.
aws ecr get-login-password | docker login --username AWS --password-stdin "$REGISTRY" >/dev/null
build_push() { # repo dockerfile [build-arg]
  aws ecr describe-repositories --repository-names "$1" >/dev/null 2>&1 || \
    aws ecr create-repository --repository-name "$1" --image-scanning-configuration scanOnPush=true >/dev/null
  echo "▸ building $REGISTRY/$1:$TAG"
  # --platform because Fargate's X86_64 runtime cannot run an image built on an Apple Silicon
  # laptop, and the failure is a task that stops immediately with an exec format error.
  docker build --platform linux/amd64 -f "$2" ${3:+--build-arg "$3"} -t "$REGISTRY/$1:$TAG" . >/dev/null
  docker push "$REGISTRY/$1:$TAG" >/dev/null
}
build_push "$JOB" Dockerfile.worker
WORKER_IMAGE="$REGISTRY/$JOB:$TAG"
JVM_IMAGE=""
if [ "$WORKER_JVM" = "1" ]; then
  build_push "$JVM_JOB" Dockerfile.worker.jvm "BASE=$WORKER_IMAGE"
  JVM_IMAGE="$REGISTRY/$JVM_JOB:$TAG"
fi

# ── Roles.
TRUST='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ecs-tasks.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
ensure_role() { aws iam get-role --role-name "$1" >/dev/null 2>&1 || aws iam create-role --role-name "$1" --assume-role-policy-document "$TRUST" >/dev/null; }

ensure_role "$JOB-exec"
aws iam attach-role-policy --role-name "$JOB-exec" \
  --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy >/dev/null 2>&1 || true
EXEC_ROLE_ARN="$(aws iam get-role --role-name "$JOB-exec" --query Role.Arn --output text)"

# Created with no policy of any kind, and nothing below ever attaches one. It exists so that the
# worker has an identity that is explicitly empty rather than inheriting one by accident.
ensure_role "$JOB-task"
TASK_ROLE_ARN="$(aws iam get-role --role-name "$JOB-task" --query Role.Arn --output text)"

aws logs create-log-group --log-group-name "/ecs/$JOB" >/dev/null 2>&1 || true
aws logs put-retention-policy --log-group-name "/ecs/$JOB" --retention-in-days 30 >/dev/null 2>&1 || true

# ── Task definitions. Two things about the shape of this, both of them bash 3.2, which is what
# ships on macOS and is what most people will run this with:
#
#   the program is in a variable rather than inline in register(), because a heredoc inside a
#   command substitution inside a *function body* is something bash 3.2 will not parse;
#
#   and it carries no apostrophes, because bash 3.2 scans a command substitution for the
#   closing paren while still honouring quotes — including quotes inside a heredoc it should
#   be treating as literal. One "the bot's" in a comment here is an unterminated-quote error
#   reported forty lines further down. The same goes for the policy program below.
TASKDEF_PY="$(cat <<'POL'
import json, sys
family, cpu, mem, exec_role, task_role, image, region, group = sys.argv[1:9]
print(json.dumps({
    "family": family,
    "networkMode": "awsvpc",
    "requiresCompatibilities": ["FARGATE"],
    "cpu": cpu, "memory": mem,
    "executionRoleArn": exec_role,
    "taskRoleArn": task_role,
    "runtimePlatform": {"cpuArchitecture": "X86_64", "operatingSystemFamily": "LINUX"},
    # A clone plus a cold dependency install is more than the 20 GiB a Fargate task gets by
    # default, and running out of it reads as a failing build rather than as a full disk.
    "ephemeralStorage": {"sizeInGiB": 50},
    "containerDefinitions": [{
        # The name the container override from the bot addresses. RunTask refuses an override
        # naming a container the definition does not have, so renaming this without renaming it
        # in internal/app/jobs_ecs.go surfaces as a clear API error rather than a silent no-op.
        "name": "worker",
        "image": image, "essential": True,
        # WORKER_MAX_WALL is the ceiling the worker keeps on itself. ECS has no per-task
        # timeout to set alongside it: the reconciler stops a task that outlives its deadline.
        "environment": [{"name": "WORKER_MODE", "value": "ecs"},
                        {"name": "WORKER_MAX_WALL", "value": "55m"}],
        "logConfiguration": {"logDriver": "awslogs", "options": {
            "awslogs-group": f"/ecs/{group}", "awslogs-region": region,
            "awslogs-stream-prefix": "ecs"}},
    }],
}))
POL
)"
register() { # family cpu memory image
  aws ecs register-task-definition --query 'taskDefinition.taskDefinitionArn' --output text \
    --cli-input-json "$(python3 -c "$TASKDEF_PY" "$1" "$2" "$3" "$EXEC_ROLE_ARN" "$TASK_ROLE_ARN" "$4" "$AWS_REGION" "$JOB")"
}
WORKER_DEF="$(register "$JOB" "$CPU" "$MEMORY" "$WORKER_IMAGE")"
echo "▸ task definition $WORKER_DEF"
if [ -n "$JVM_IMAGE" ]; then
  JVM_DEF="$(register "$JVM_JOB" "$JVM_CPU" "$JVM_MEMORY" "$JVM_IMAGE")"
  echo "▸ task definition $JVM_DEF"
fi

# ── The bot's task role. fargate.sh gives the bot no task role at all, because until now the
# running container needed no AWS permission — its S3 access comes from a static key. Starting a
# worker is the first thing it does as itself, so the role is created here and fargate.sh
# attaches it when it finds it.
#
# RunTask is restricted to these definitions and conditioned on the cluster. DescribeTasks and
# StopTask take a *task* ARN, which does not exist until the task does, so they are Resource:"*"
# narrowed by the same cluster condition — as tight as the ECS API allows. PassRole is limited to
# the two worker roles, so a bug in the bot cannot start a task wearing something else's role.
BOT_ROLE="$SERVICE-task"
ensure_role "$BOT_ROLE"
POLICY="$(python3 - "$AWS_REGION" "$ACCOUNT" "$CLUSTER" "$EXEC_ROLE_ARN" "$TASK_ROLE_ARN" "$JOB" "$JVM_JOB" "$JVM_IMAGE" <<'POL'
import json, sys
region, account, cluster, exec_role, task_role, job, jvm_job, jvm_image = sys.argv[1:9]
families = [job] + ([jvm_job] if jvm_image else [])
cluster_arn = f"arn:aws:ecs:{region}:{account}:cluster/{cluster}"
print(json.dumps({"Version": "2012-10-17", "Statement": [
    {"Effect": "Allow", "Action": "ecs:RunTask",
     "Resource": [f"arn:aws:ecs:{region}:{account}:task-definition/{f}:*" for f in families],
     "Condition": {"ArnEquals": {"ecs:cluster": cluster_arn}}},
    {"Effect": "Allow", "Action": ["ecs:DescribeTasks", "ecs:StopTask"], "Resource": "*",
     "Condition": {"ArnEquals": {"ecs:cluster": cluster_arn}}},
    {"Effect": "Allow", "Action": "iam:PassRole", "Resource": [exec_role, task_role],
     "Condition": {"StringEquals": {"iam:PassedToService": "ecs-tasks.amazonaws.com"}}},
]}))
POL
)"
aws iam put-role-policy --role-name "$BOT_ROLE" --policy-name run-fix-workers --policy-document "$POLICY" >/dev/null
echo "▸ bot task role $BOT_ROLE may run these definitions on cluster $CLUSTER and nothing else"

echo
echo "worker task definition  $JOB → $WORKER_IMAGE"
[ -n "$JVM_IMAGE" ] && echo "heavy worker            $JVM_JOB → $JVM_IMAGE"
echo
echo "Deploy the bot with these set in $ENV_FILE (or the environment):"
echo
echo "  WORKER_MODE=workers"       # or ecs, to pin it rather than let the task work it out
echo "  WORKER_ECS_CLUSTER=$CLUSTER"
echo "  WORKER_ECS_SUBNETS=$SUBNETS"
echo "  WORKER_ECS_SECURITY_GROUPS=$WORKER_SG"
echo "  WORKER_JOB_NAME=$JOB"
[ -n "$JVM_IMAGE" ] && echo "  WORKER_JOB_NAMES=java=$JVM_JOB,java-gradle=$JVM_JOB,java-maven=$JVM_JOB,dotnet=$JVM_JOB"
echo
echo "then: ./deploy/aws/fargate.sh"
echo
echo "Tasks: aws ecs list-tasks --cluster $CLUSTER --family $JOB --desired-status STOPPED"
