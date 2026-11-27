#!/usr/bin/env bash
# Go suite of the read/write path, in the indexus-perf/1 format of
# sdk/docs/perf.md: /sets encoding (v1 and IXB2), read projection and
# inventory, storage inserts, snapshot seal, knowledge frames.
#
#   scripts/perf/run.sh [result.json]
#
# Without an argument the JSON goes to stdout. The raw `go test` output is
# kept next to it (result.txt) for benchstat. Environment:
#   PERF_COUNT      runs per benchmark (default 6): the samples
#   PERF_BENCHTIME  -benchtime per run (default 300ms)
#   PERF_MACHINE    machine label recorded in env.machine (default: hostname)
#   INDEXUS_GO_ENV  file to source before calling go (see below)
#
# Toolchain: go.mod requires Go 1.26. A plain Go >= 1.26 is used as is;
# INDEXUS_GO_ENV names a file to source first for another toolchain.
# Tests never run here (-run '^$').
set -euo pipefail

cd "$(dirname "$0")/../.."
out="${1:-}"
count="${PERF_COUNT:-6}"
benchtime="${PERF_BENCHTIME:-300ms}"

env_file="${INDEXUS_GO_ENV:-}"
if [ -n "$env_file" ]; then
  # shellcheck disable=SC1090
  source "$env_file"
fi
if ! command -v go >/dev/null; then
  echo "run.sh: go not found in PATH" >&2
  exit 2
fi
version="$(go env GOVERSION)"
minor="$(printf '%s' "$version" | sed -E 's/^go1\.([0-9]+).*/\1/')"
if [ -z "$env_file" ] && [ "${minor:-0}" -lt 26 ] 2>/dev/null; then
  echo "run.sh: $version is older than the Go 1.26 go.mod requires (set INDEXUS_GO_ENV for a backport)" >&2
  exit 2
fi

# Package and -bench pattern. Durable inserts (fsync) are left out: on a
# laptop or shared disk they measure the disk, not the code; the logical
# variants run the same authority, receipts and history without fsync.
suites=(
  "./indexus|^Benchmark(SummaryInventoryPayload|MonitoringZoneCount)$"
  "./internal/wire|^Benchmark(EncodeSetsBlock|EncodeRecords|CloneReadSet)$"
  "./internal/storage|^Benchmark(InventoryPayloadProjection|SnapshotSeal|StoredZoneKeyDecode|RecordMetricsOpaqueProjection)$"
  "./internal/storage|^BenchmarkCausalInsert$/./logical-"
  "./internal/mesh|^BenchmarkKnowledgeFrameCopies$/typed"
)

raw="$(mktemp)"
trap 'rm -f "$raw"' EXIT
for entry in "${suites[@]}"; do
  pkg="${entry%%|*}"
  pattern="${entry#*|}"
  echo "go test $pkg -bench '$pattern' -count $count -benchtime $benchtime" >&2
  go test -run '^$' -bench "$pattern" -benchmem -count "$count" -benchtime "$benchtime" "$pkg" >>"$raw"
done

commit="$(git rev-parse --short HEAD 2>/dev/null || true)"
# Dirty means the measured code differs from the commit, not these scripts.
if [ -n "$commit" ] && [ -n "$(git status --porcelain --untracked-files=no -- . ':(exclude)scripts/perf' ':(exclude)perf' 2>/dev/null)" ]; then
  commit="$commit-dirty"
fi
branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
convert=(go run scripts/perf/benchjson.go -commit "$commit" -branch "$branch" -machine "${PERF_MACHINE:-}" \
  -count "$count" -benchtime "$benchtime")

if [ -n "$out" ]; then
  "${convert[@]}" <"$raw" >"$out"
  cp "$raw" "${out%.json}.txt"
  echo "→ $out" >&2
else
  "${convert[@]}" <"$raw"
fi
