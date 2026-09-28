.PHONY: build ui dist hooks test test-deploy evals run deploy worker-build worker-deploy worker-deploy-aws worker-deploy-azure

build: ui
	go build -o attesttag ./cmd/attesttag

ui:
	cd ui && npm ci --no-audit --no-fund && npm run build

# The binaries a GitHub release carries, in dist/: one archive per platform, holding the binary
# with the console embedded beside LICENSE and README.md, and a SHA256SUMS over them. The release
# workflow runs it after `make ui` with VERSION=<the tag without its v>; run it yourself to see
# what a release would hold. CGO_ENABLED=0 and pure-Go SQLite make each one a plain cross-compile
# from any machine. COPYFILE_DISABLE keeps a Mac's tar from adding ._ files for extended attributes.
VERSION ?= dev
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
DIST_PLATFORMS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
DIST_VERSION = $(patsubst v%,%,$(VERSION))

dist:
	@test -d ui/out/_next || { echo "the console is not built: run make ui first, or every binary serves a blank one" >&2; exit 1; }
	rm -rf dist && mkdir dist
	set -e; for p in $(DIST_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; name=attesttag_$(DIST_VERSION)_$${os}_$${arch}; \
		mkdir dist/$$name; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags="-s -w -X attesttag/internal/app.Version=$(DIST_VERSION) -X attesttag/internal/app.Commit=$(COMMIT)" \
			-o dist/$$name/attesttag ./cmd/attesttag; \
		cp LICENSE README.md dist/$$name/; \
		COPYFILE_DISABLE=1 tar -C dist -czf dist/$$name.tar.gz $$name; \
		rm -r dist/$$name; \
	done
	cd dist && if command -v sha256sum >/dev/null; then sha256sum *.tar.gz; else shasum -a 256 *.tar.gz; fi > SHA256SUMS

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
