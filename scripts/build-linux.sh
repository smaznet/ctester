#!/usr/bin/env bash
# Cross-compile a static Linux binary of x-tester (no libc / glibc dependency).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

ARCH="${GOARCH:-amd64}"   # amd64 | arm64
OUT="${OUT:-bin/x-tester-linux-${ARCH}}"
mkdir -p "$(dirname "$OUT")"

echo "building static linux/${ARCH} → ${OUT}"

CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH}" go build \
  -trimpath \
  -ldflags="-s -w" \
  -o "${OUT}" \
  ./cmd/x-tester/

# Sanity: should report "statically linked" when file(1) is available
if command -v file >/dev/null 2>&1; then
  file "${OUT}"
fi
ls -lh "${OUT}"
echo "ok: ${OUT}"
