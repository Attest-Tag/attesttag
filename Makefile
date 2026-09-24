.PHONY: build ui hooks test test-deploy evals run deploy worker-build worker-deploy worker-deploy-aws worker-deploy-azure

build: ui
	go build -o attesttag ./cmd/attesttag

ui:
	cd ui && npm ci --no-audit --no-fund && npm run build

# The pre-push check: this repository is public, so a push that would publish a secret, a
# dotenv file or one of your deployment's identifiers is refused before it leaves (.githooks/).
hooks:
	git config core.hooksPath .githooks

test:
	go vet ./... && go test -skip TestEvals ./...

# The deploy scripts, run against fake aws/az/gcloud/docker CLIs: no cloud account, no network. Not
# part of `make test`, which is the Go gate — this one needs python3 and shellcheck.
test-deploy:
	./deploy/test/run.sh

# Live evals against the running bot (start it with SELF_TEST=1 first).
evals:
	go test ./internal/app/ -run TestEvals -eval -v -timeout 30m

run:
	SELF_TEST=1 HEALTH_ADDR=:8090 ADMIN_BASE_URL=http://127.0.0.1:8090 ./attesttag

# The deploy targets take PROJECT from the environment, and each script falls back to
# `gcloud config get-value project` — so these work against whichever project your gcloud is
# pointed at. Override either: make deploy PROJECT=my-project REGION=europe-west1
REGION ?= us-central1

deploy:
	REGION=$(REGION) ./deploy/gcp/cloudrun.sh

# The fix-job worker image (Dockerfile.worker) and the job the bot runs it as. One target per
# platform, because the job is the platform-specific half; deploy/docs/platforms.md has the rest.
# Kubernetes needs no target — the chart creates the Job per request from worker.enabled.
worker-build:
	docker build -f Dockerfile.worker -t attesttag-worker .

worker-deploy:
	REGION=$(REGION) ./deploy/gcp/worker.sh

worker-deploy-aws:
	./deploy/aws/worker.sh

worker-deploy-azure:
	./deploy/azure/worker.sh
