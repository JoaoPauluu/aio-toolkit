#!/bin/sh
set -e
mkdir -p "$AIO_DATA_DIR" "$HOME" "$XDG_CACHE_HOME" "$AIO_DOWNLOADS_DIR" 2>/dev/null || true
exec "$@"
