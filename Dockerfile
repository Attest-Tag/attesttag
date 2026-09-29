# attest_tag: console (Next.js static export) + static Go binary + Litestream.
#
# Multi-arch without emulating the build. CGO_ENABLED=0 and pure-Go SQLite (modernc.org/sqlite)
# mean Go cross-compiles, so the two build stages are pinned to the BUILDER's platform and only
# the `go build` is told what to target. Building linux/arm64 on an amd64 runner therefore costs
# the same as building amd64: npm and Go always run natively, and the only foreign-architecture
# work is the final stage's two short RUN lines, which the builder emulates.
#
# The last two stages are deliberately NOT pinned: they contribute real binaries (litestream,
# alpine's poppler) and must resolve to the TARGET architecture.

# BuildKit sets BUILDPLATFORM to the platform of the machine doing the build, and it is declared
# here without a default on purpose: a global ARG's default replaces BuildKit's value rather than
# backing it up, so a default of linux/amd64 had an arm64 Mac build these stages as amd64 under
# emulation. The fallback is in the FROM lines instead, where it only fills a blank: the classic
# `docker build` that Cloud Build runs for `gcloud run deploy --source .` (deploy/gcp/cloudrun.sh)
# sets no platform ARGs, `gcloud` has no way to pass one, and it would die on `failed to parse
# platform ""`. A bare `linux` is completed with the daemon's own architecture, so that builder
# stays native too. TARGETOS and TARGETARCH get no default for the same reason: declared bare in
# the build stage they stay BuildKit's, where a default would give an arm64 image an amd64 binary.
ARG BUILDPLATFORM

FROM --platform=${BUILDPLATFORM:-linux} node:24-alpine AS uibuild
WORKDIR /ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY ui/ ./
RUN npm run build

FROM --platform=${BUILDPLATFORM:-linux} golang:1.27.1-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=uibuild /ui/out ./ui/out
# The release workflow passes the tag (v0.1.0) for `attesttag version` and the startup line, which
# print it without the v. Declared here, after the module download, so a new version reuses that
# layer; a build that passes none, a Cloud Run source deploy among them, says "dev".
ARG VERSION="dev"
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X attesttag/internal/app.Version=${VERSION#v}" -o /attesttag ./cmd/attesttag

FROM litestream/litestream:0.5.17 AS litestream

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata poppler-utils
WORKDIR /app
COPY --from=build /attesttag /app/attesttag
COPY --from=litestream /usr/local/bin/litestream /usr/local/bin/litestream
# No litestream.yml is baked in: the bot renders one at startup for whichever bucket it was
# given (internal/app/replica_target.go), because a file written at build time cannot know
# whether this deployment replicates to GCS or to an S3-compatible store.
# Nothing here needs root: the bot feeds attacker-supplied PDFs to pdftotext, so it runs as
# an ordinary user that owns only the database and docs directories.
RUN adduser -D -u 10001 attest \
 && mkdir -p /data /app/docs \
 && chown -R attest:attest /data /app/docs
USER attest
# -trimpath strips the VCS stamp out of build info, so the commit has to arrive as a build arg —
# the first thing worth knowing about a bug report. It becomes the revision label at the bottom of
# this file, and, where SQLite is replicated to a bucket, the commit the write lease records for
# the container holding the database (internal/app/lease.go). `attesttag version` and the startup
# line print it beside the version; neither !whoami nor /health says it.
ARG GIT_COMMIT=""
ENV GIT_COMMIT=$GIT_COMMIT
ENV DB_PATH=/data/attesttag.db DOCS_DIR=/app/docs PORT=8080
# Fail closed on a missing MASTER_KEY rather than minting a throwaway one that silently loses
# every stored credential on the next restart. In a container there is no Cloud Run metadata to
# infer "managed" from, so this is what makes NewSealer refuse instead of generate. It also turns
# on the matching hardening (no LLM_DUMP_DIR, cookies always Secure).
ENV REQUIRE_MASTER_KEY=1
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:8080/health || exit 1
# The binary is PID 1 and runs Litestream as its child, not the other way round: the restore has
# to wait for the write lease, which only the bot can take. See internal/app/replication.go.
ENTRYPOINT ["/app/attesttag"]

ARG SOURCE_URL="https://github.com/Attest-Tag/attesttag"
ARG VERSION="dev"
LABEL org.opencontainers.image.title="attest_tag" \
      org.opencontainers.image.description="An AI teammate for Slack that acts — and never holds a key." \
      org.opencontainers.image.source=$SOURCE_URL \
      org.opencontainers.image.revision=$GIT_COMMIT \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.licenses="MIT"
