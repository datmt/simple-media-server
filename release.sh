#!/usr/bin/env bash
# Cross-compile release binaries and tar them up under ./dist/<version>/.
# Usage: ./release.sh [version]   (defaults to git describe, else "dev")
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${1:-$(git describe --tags --always 2>/dev/null || echo dev)}"
DIST="dist/$VERSION"
TARGETS=("linux/amd64" "linux/arm64" "darwin/amd64" "darwin/arm64")

rm -rf "$DIST"
mkdir -p "$DIST"

for target in "${TARGETS[@]}"; do
  os="${target%/*}"
  arch="${target#*/}"
  name="mediad-${VERSION}-${os}-${arch}"
  bin="$name"
  [ "$os" = "windows" ] && bin="${bin}.exe"

  echo "building $target..."
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags="-s -w" -o "$DIST/$bin" .

  ( cd "$DIST" && tar -czf "${name}.tar.gz" "$bin" && rm "$bin" )
done

( cd "$DIST" && sha256sum -- *.tar.gz > SHA256SUMS )

echo "release artifacts in $DIST/"
ls -la "$DIST"
