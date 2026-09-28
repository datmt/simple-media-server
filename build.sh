#!/usr/bin/env bash
# Local dev build: static binary for the host's own OS/arch.
set -euo pipefail
cd "$(dirname "$0")"

OUT="${1:-./mediad}"

CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$OUT" .

size=$(stat -c%s "$OUT" 2>/dev/null || stat -f%z "$OUT")
echo "built $OUT ($((size / 1024 / 1024)) MB)"
if [ "$size" -gt $((30 * 1024 * 1024)) ]; then
  echo "warning: binary exceeds 30MB budget" >&2
fi
