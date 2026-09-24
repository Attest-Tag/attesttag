# attest_tag: console (Next.js static export) + static Go binary + Litestream.
#
# Multi-arch without QEMU. CGO_ENABLED=0 and pure-Go SQLite (modernc.org/sqlite) mean Go
# cross-compiles, so the two build stages are pinned to the BUILDER's platform and only the
# `go build` is told what to target. Building linux/arm64 on an amd64 runner therefore costs
# the same as building amd64, with no emulation anywhere.
#
# The last two stages are deliberately NOT pinned: they contribute real binaries (litestream,
# alpine's poppler) and must resolve to the TARGET architecture.

# BUILDPLATFORM carries a default so that a builder which does not set it — Cloud Build's source
# deploys run classic `docker build`, not buildx — still parses these FROM lines rather than dying
# on `failed to parse platform ""`. BuildKit overrides it, so buildx is unaffected. TARGETOS and
# TARGETARCH must NOT be declared here: a global default wins over BuildKit's own value, and an
# arm64 image would quietly get an amd64 binary. Declared per stage, they stay the builder's.
ARG BUILDPLATFORM=linux/amd64

FROM --platform=$BUILDPLATFORM node:22-alpine AS uibuild
WORKDIR /ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY ui/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=uibuild /ui/out ./ui/out
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /attesttag ./cmd/attesttag

FROM litestream/litestream:0.5.17 AS litestream

FROM alpine:3.22
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
# the container holding the database (internal/app/lease.go). Neither !whoami nor /health says it.
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
