#!/usr/bin/env sh
# Build and run attest_tag locally: Slack bot + admin console + API.
# Rebuilds first, then stops any attesttag already on the port, then starts.
#
#   ./start_dev.sh               # bot on :8090 (SELF_TEST=1), console at http://127.0.0.1:8090/admin/
#   PORT=8092 ./start_dev.sh     # listen on a different port
#   SKIP_UI=1 ./start_dev.sh     # never rebuild the console, even if ui/src changed
#   ENV_FILE=.env.prod ./start_dev.sh  # run against the deployed settings, not .env.testing
#   FRESH_DB=1 ./start_dev.sh    # delete the testing database first and start from empty
set -eu
cd "$(dirname "$0")"

PORT="${PORT:-8090}"

# Local testing runs off .env.testing (its own Slack app and DB_PATH), so experiments here —
# and the MASTER_KEY the bot appends when none is set — never touch .env.prod, the file
# deploy/gcp/cloudrun.sh copies into Secret Manager. ENV_FILE=.env.prod opts back into the real one.
ENV_FILE="${ENV_FILE:-.env.testing}"
if [ ! -f "$ENV_FILE" ]; then
  [ -f .env.prod ] || { echo "$ENV_FILE is missing and there is no .env.prod to copy from" >&2; exit 1; }
  echo "==> creating $ENV_FILE from .env.prod (point it at a separate Slack app before starting)"
  cp -p .env.prod "$ENV_FILE"
fi

# Values in $ENV_FILE win over the shell. The binary loads it with godotenv.Load,
# which never overrides an existing variable, so a stale export (for example an
# old OPENROUTER_API_KEY in ~/.zshrc) would silently replace the real key.
for k in $(grep -Eo '^[A-Za-z_][A-Za-z0-9_]*' "$ENV_FILE" || true); do unset "$k"; done

# The testing database is disposable: FRESH_DB=1 throws away the old rows.
DB="$(grep -E '^DB_PATH=' "$ENV_FILE" | tail -1 | cut -d= -f2- | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/")"
DB="${DB:-attesttag.db}"
if [ -n "${FRESH_DB:-}" ]; then
  echo "==> removing $DB (FRESH_DB=1)"
  rm -f "$DB" "$DB-journal" "$DB-wal" "$DB-shm"
fi

# Rebuild the console only when a source file is newer than the last export.
if [ -z "${SKIP_UI:-}" ]; then
  if [ ! -f ui/out/index.html ] || [ -n "$(find ui/src ui/package.json -newer ui/out/index.html -type f -print -quit)" ]; then
    echo "==> building console (ui/)"
    [ -d ui/node_modules ] || (cd ui && npm ci --no-audit --no-fund)
    (cd ui && npm run build)
  fi
fi

echo "==> building attesttag"
go build -o attesttag ./cmd/attesttag

# Stop a previous attesttag on this port (built above, so a failed build never
# takes the old server down). Anything else on the port is left alone.
for pid in $(lsof -t -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true); do
  name="$(ps -o comm= -p "$pid" 2>/dev/null || true)"
  case "$name" in
    *attesttag*) echo "==> stopping attesttag pid $pid on :$PORT"; kill "$pid" 2>/dev/null || true ;;
    *) echo "port $PORT is held by pid $pid ($name), not attesttag; stop it first" >&2; exit 1 ;;
  esac
done
n=0
while lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; do
  n=$((n + 1))
  if [ "$n" -gt 50 ]; then echo "port $PORT is still in use after 10s" >&2; exit 1; fi
  sleep 0.2
done

echo "==> http://127.0.0.1:${PORT}/admin/  (env=${ENV_FILE}, db=${DB}, SELF_TEST=${SELF_TEST:-1})"
ENV_FILE="$ENV_FILE" \
SELF_TEST="${SELF_TEST:-1}" \
HEALTH_ADDR=":${PORT}" \
exec ./attesttag
# ADMIN_BASE_URL is deliberately not defaulted: the bot builds sign-in redirects from the host
# the browser used and learns its public origin from console sign-ins, so an https front such
# as `tailscale serve --bg $PORT` just works. Export it only to pin an origin.
