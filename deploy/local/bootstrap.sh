#!/bin/sh
# Write the .env that docker-compose.yml reads, generating the secrets that have to be random.
#
#   ./deploy/local/bootstrap.sh
#
# Refuses to overwrite an existing .env. That is not politeness: MASTER_KEY seals every stored
# credential, and a second run that generated a new one would leave every connected Slack
# workspace and every saved connection undecryptable, with nothing to say so until something
# tried to use one.
set -eu

cd "$(dirname "$0")/../.."
TARGET=.env
TEMPLATE=deploy/env/selfhost.env.example

if [ -e "$TARGET" ]; then
	echo "$TARGET already exists — not touching it." >&2
	echo "" >&2
	echo "If you meant to start over, move it aside first. Keep the MASTER_KEY line: it unlocks" >&2
	echo "every credential already stored, and a new one cannot be made to fit them." >&2
	exit 1
fi

if ! command -v openssl >/dev/null 2>&1; then
	echo "openssl is needed to generate MASTER_KEY. Install it, or generate 32 random bytes" >&2
	echo "another way and write MASTER_KEY into $TARGET by hand." >&2
	exit 1
fi

cp "$TEMPLATE" "$TARGET"

# Same shape the app uses when it generates one for itself: base64 of 32 random bytes.
KEY=$(openssl rand -base64 32)
# The key contains / and + and =, so use a delimiter that cannot appear in base64.
sed -i.bak "s|^MASTER_KEY=.*|MASTER_KEY=$KEY|" "$TARGET" && rm -f "$TARGET.bak"

# The passwords for a Postgres and a MinIO run inside this compose project. They are read only
# under those profiles, and generating them costs nothing — an unused password is better than a
# default one somebody forgets to change. Hex, because these two end up inside URLs.
for VAR in POSTGRES_PASSWORD MINIO_ROOT_PASSWORD; do
	PW=$(openssl rand -hex 16)
	sed -i.bak "s|^$VAR=.*|$VAR=$PW|" "$TARGET" && rm -f "$TARGET.bak"
done

chmod 600 "$TARGET"

cat <<'NEXT'
Wrote .env (mode 600) and generated a MASTER_KEY.

  1. BACK UP THE MASTER_KEY LINE, somewhere other than this machine. It seals every stored
     credential. There is no recovery if it is lost — only reconnecting everything by hand.

  2. Create a Slack app. Once you know your public URL:

       BASE_URL=https://your-host ./deploy/slack/manifest.sh

     paste the output at api.slack.com/apps -> Create New App -> From a manifest, then copy
     the Signing Secret, Client ID and Client Secret into .env.

     Do not have a public URL yet? Start with the quick tunnel below, read the hostname out of
     its logs, and come back to this step.

  3. Get an OpenRouter key (openrouter.ai/keys) into OPENROUTER_API_KEY, or point
     LLM_BASE_URL at any OpenAI-compatible endpoint.

  4. Start it:

       docker compose --profile quicktunnel up -d
       docker compose logs quicktunnel      # the https URL to use as BASE_URL

     Set ADMIN_BASE_URL in .env to that URL, or links in emails will point at whichever host
     reaches the console first, and apply it by running the same command again:

       docker compose --profile quicktunnel up -d

     It recreates the bot with the new .env and leaves the tunnel, and its name, alone. Not
     `docker compose restart`, which keeps the old .env and gives the tunnel a new name.

  5. Open the console, sign up — the first sign-up founds the deployment and everybody after
     it arrives by invitation — and connect your workspace.

deploy/docs/https.md explains the other two ways of getting an address, and why the quick
tunnel is for trying this out rather than keeping it.
NEXT
