#!/bin/zsh
# Installs attest_tag as a per-user launchd service that starts at login and restarts on crash.
set -euo pipefail
DIR="$(cd "$(dirname "$0")/../.." && pwd)"
LABEL=com.attesttag.bot
PLIST=~/Library/LaunchAgents/$LABEL.plist
cd "$DIR"
go build -o attesttag ./cmd/attesttag
mkdir -p ~/Library/LaunchAgents
sed "s|__DIR__|$DIR|g" deploy/local/$LABEL.plist > "$PLIST"
launchctl bootout gui/$(id -u) "$PLIST" 2>/dev/null || true
launchctl bootstrap gui/$(id -u) "$PLIST"
launchctl kickstart -k gui/$(id -u)/$LABEL
echo "installed: $PLIST"
echo "logs: tail -f $DIR/bot.log   |  stop: launchctl bootout gui/$(id -u) $PLIST"
