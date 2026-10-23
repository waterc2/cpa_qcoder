#!/usr/bin/env bash
# Build the Qoder CLIProxyAPI plugin as a c-shared dynamic library.
#
# Usage:
#   scripts/build-cpa-plugin.sh [version]
#
# Output: dist/qoder-v<version>.so (+ .h)
# Install: copy the .so to <CPA>/plugins/<goos>/<goarch>/ and add
#          `qoder: {enabled: true}` under plugins.configs in config.yaml.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VERSION="${1:-0.1.1}"
GOOS="${GOOS:-$(go env GOOS)}"
GOARCH="${GOARCH:-$(go env GOARCH)}"

mkdir -p dist
OUT="dist/qoder-v${VERSION}.so"

echo ">> building $OUT ($GOOS/$GOARCH, buildmode=c-shared)"
CGO_ENABLED=1 GOOS="$GOOS" GOARCH="$GOARCH" \
  go build -buildmode=c-shared \
  -ldflags "-X main.pluginVersion=${VERSION}" \
  -o "$OUT" ./cmd/cpa-plugin/

echo ">> done: $OUT"
ls -la "$OUT" "${OUT%.so}.h"
