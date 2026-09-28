#!/usr/bin/env bash
# Install mediad on this machine.
#   curl -fsSL https://raw.githubusercontent.com/datmt/simple-media-server/master/install.sh | bash
set -euo pipefail

REPO="datmt/simple-media-server"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
BIN_NAME="mediad"

os=$(uname -s)
arch=$(uname -m)

if [ "$os" != "Linux" ]; then
  echo "error: mediad only ships a linux binary, detected $os" >&2
  exit 1
fi

case "$arch" in
  x86_64|amd64) asset="mediad-linux-amd64" ;;
  *) echo "error: unsupported arch $arch (only linux/amd64 is published)" >&2; exit 1 ;;
esac

dest="${INSTALL_DIR}/${BIN_NAME}"
log() { echo "[install] $*" >&2; }
CURL="curl -fsSL --connect-timeout 10 --max-time 300"
log "os=$os arch=$arch dest=$dest"

log "fetching latest release tag"
latest=$($CURL "https://api.github.com/repos/${REPO}/releases/latest" \
  | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')

log "latest=${latest:-<none>}"

if [ -x "$dest" ]; then
  log "existing binary found, checking version"
  # timeout: pre-`version` binaries ignore the arg and start the server
  current=$(timeout 3 "$dest" version 2>/dev/null </dev/null || echo "")
  log "current=${current:-<unknown>}"
  if [ -n "$latest" ] && [ "$current" = "$latest" ]; then
    echo "$dest already at $current, skipping"
    exit 0
  fi
fi

url="https://github.com/${REPO}/releases/latest/download/${asset}"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

echo "downloading $url"
$CURL "$url" -o "$tmp"
chmod +x "$tmp"

if [ -w "$INSTALL_DIR" ]; then
  log "moving to $dest"
  mv "$tmp" "$dest"
else
  log "$INSTALL_DIR not writable, using sudo (may prompt for password)"
  sudo mv "$tmp" "$dest"
fi

echo "installed $dest ($("$dest" version))"
