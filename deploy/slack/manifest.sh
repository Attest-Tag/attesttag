#!/bin/sh
# Print the Slack app manifest with this deployment's origin filled in, ready to paste into
# api.slack.com/apps -> Create New App -> From a manifest.
#
#   BASE_URL=https://bot.example.com ./deploy/slack/manifest.sh
#
# The origin must be https and reachable from the internet. Slack will not deliver events to
# localhost and will not accept an http redirect URL, so a laptop needs a tunnel in front of it
# — deploy/docs/https.md covers the shortest way to get one.
set -eu

if [ "${BASE_URL:-}" = "" ]; then
	echo "set BASE_URL to the public https origin of this deployment, with no trailing slash" >&2
	echo "  BASE_URL=https://bot.example.com $0" >&2
	exit 64
fi

case "$BASE_URL" in
https://*) ;;
*)
	echo "BASE_URL must start with https:// — Slack rejects anything else, localhost included" >&2
	exit 64
	;;
esac

BASE_URL="${BASE_URL%/}"
sed "s|\${BASE_URL}|$BASE_URL|g" "$(dirname "$0")/manifest.json"
