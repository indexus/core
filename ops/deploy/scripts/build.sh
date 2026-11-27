#!/usr/bin/env bash
# Build the production executables; no data or running service is touched.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
OUT="${1:-$ROOT/bin/linux}"
BUILD_VERSION="${INDEXUS_VERSION:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || printf 'dev')}"
BUILD_COMMIT="${INDEXUS_COMMIT:-$(git -C "$ROOT" rev-parse --short=12 HEAD 2>/dev/null || printf 'unknown')}"
if [[ -z "${INDEXUS_COMMIT:-}" && -n "$(git -C "$ROOT" status --porcelain --untracked-files=normal 2>/dev/null || true)" ]]; then
  BUILD_COMMIT="${BUILD_COMMIT}-dirty"
fi
for value in "$BUILD_VERSION" "$BUILD_COMMIT"; do
  if [[ ! "$value" =~ ^[a-zA-Z0-9._+-]+$ ]]; then
    printf 'Invalid version/commit: use letters, numbers, dot, underscore, + or -\n' >&2
    exit 1
  fi
done
export CGO_ENABLED=0 GOOS="${GOOS:-linux}" GOARCH="${GOARCH:-amd64}"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
cd "$ROOT"
go build -trimpath -ldflags "-s -w -X main.version=$BUILD_VERSION -X main.commit=$BUILD_COMMIT" -o "$OUT/node" ./cmd/node
go build -trimpath -ldflags "-s -w -X main.version=$BUILD_VERSION -X main.commit=$BUILD_COMMIT" -o "$OUT/issuer" ./cmd/issuer
go build -trimpath -ldflags '-s -w' -o "$OUT/provisioner" ./cmd/provisioner
printf 'Production build: %s/%s, version %s (%s)\n' "$GOOS" "$GOARCH" "$BUILD_VERSION" "$BUILD_COMMIT"
wc -c "$OUT/node" "$OUT/issuer" "$OUT/provisioner"
