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

latest=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')

if [ -x "$dest" ]; then
  current=$("$dest" version 2>/dev/null || echo "")
  if [ -n "$latest" ] && [ "$current" = "$latest" ]; then
    echo "$dest already at $current, skipping"
    exit 0
  fi
fi

url="https://github.com/${REPO}/releases/latest/download/${asset}"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

echo "downloading $url"
curl -fsSL "$url" -o "$tmp"
chmod +x "$tmp"

if [ -w "$INSTALL_DIR" ]; then
  mv "$tmp" "$dest"
else
  sudo mv "$tmp" "$dest"
fi

echo "installed $dest"
"$dest" help | head -1
