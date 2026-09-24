#!/bin/zsh
D="${D:-${TMPDIR:-/tmp}/attesttag-mkt}"   # where gen.py wrote the HTML; override with D=...
O="$(cd "$(dirname "$0")/.." && pwd)"     # assets/marketplace, relative to this script
mkdir -p "$O"
for f in $D/0*.html; do
  b=$(basename "$f" .html)
  rm -f "$D/$b.png"
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --disable-gpu --hide-scrollbars \
    --force-device-scale-factor=2 --window-size=1600,1000 --user-data-dir=/tmp/chrome-mkt-$b \
    --screenshot="$D/$b.png" --virtual-time-budget=1200 "file://$f" >/dev/null 2>&1 &
  pid=$!
  for i in {1..60}; do [[ -s "$D/$b.png" ]] && break; sleep 0.25; done
  sleep 0.4; kill $pid 2>/dev/null; wait $pid 2>/dev/null
  cp "$D/$b.png" "$O/$b.png"
  sips -z 1000 1600 "$O/$b.png" >/dev/null 2>&1
  echo "$b $(sips -g pixelWidth -g pixelHeight "$O/$b.png" | tail -2 | tr -d '\n' | tr -s ' ')"
done
