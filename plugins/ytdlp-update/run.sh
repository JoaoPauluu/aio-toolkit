#!/bin/sh
# Installs/updates yt-dlp into $AIO_DATA/venv, which comes first on PATH.
set -e
VENV="$AIO_DATA/venv"
echo "::status Current: $(yt-dlp --version 2>/dev/null || echo none)"
if [ ! -x "$VENV/bin/pip" ]; then
  echo "::status Creating virtualenv in $VENV"
  python3 -m venv "$VENV"
fi
echo "::progress 20"
CHANNEL=$(python3 -c 'import json,os;print(json.load(open(os.environ["AIO_PARAMS"])).get("channel","stable"))')
PRE=""
[ "$CHANNEL" = "nightly" ] && PRE="--pre"
echo "::status Installing yt-dlp ($CHANNEL)"
"$VENV/bin/pip" install --no-cache-dir --disable-pip-version-check -U $PRE "yt-dlp[default]" 2>&1
echo "::progress 100"
echo "::status Now: $("$VENV/bin/yt-dlp" --version)"
