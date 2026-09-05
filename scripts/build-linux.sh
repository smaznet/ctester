#!/usr/bin/env bash
# Cross-compile a static Linux binary of x-tester (no libc / glibc dependency).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

ARCH="${GOARCH:-amd64}"   # amd64 | arm64
OUT="${OUT:-bin/x-tester-linux-${ARCH}}"
mkdir -p "$(dirname "$OUT")"

echo "building static linux/${ARCH} → ${OUT}"

VER="${VERSION:-dev}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || true)}"
LDFLAGS="-s -w -X github.com/aria/x-tester/internal/version.Version=${VER}"
if [ -n "${COMMIT}" ]; then
  LDFLAGS="${LDFLAGS} -X github.com/aria/x-tester/internal/version.Commit=${COMMIT}"
fi

CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH}" go build \
  -trimpath \
  -ldflags="${LDFLAGS}" \
  -o "${OUT}" \
  ./cmd/x-tester/

# Sanity: should report "statically linked" when file(1) is available
if command -v file >/dev/null 2>&1; then
  file "${OUT}"
fi
ls -lh "${OUT}"
echo "ok: ${OUT}"
