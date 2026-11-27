#!/usr/bin/env bash
# Local qualification only: never provisions machines or contacts AWS.
set -euo pipefail

handoff_root="$(cd "$(dirname "$0")/../.." && pwd)"
handoff_mode="${1:---quick}"
if [[ "$handoff_mode" != --quick && "$handoff_mode" != --full ]]; then
  printf 'Usage: %s [--quick|--full]\n' "$0" >&2
  exit 2
fi
if [[ "$handoff_mode" == --full && ( -z "${TLA2TOOLS_JAR:-}" || ! -r "${TLA2TOOLS_JAR:-}" ) ]]; then
  printf 'Full qualification requires TLA2TOOLS_JAR (pinned jar in spec/handoff/README.md).\n' >&2
  exit 2
fi
handoff_out="${INDEXUS_HANDOFF_RESULTS:-$(mktemp -d "${TMPDIR:-/tmp}/indexus-handoff.XXXXXX")}"
mkdir -p "$handoff_out"
handoff_out="$(cd "$handoff_out" && pwd)"
cd "$handoff_root"
printf 'running\n' > "$handoff_out/status.txt"
git rev-parse HEAD > "$handoff_out/commit.txt"
git status --porcelain > "$handoff_out/worktree.txt"
go version > "$handoff_out/go-version.txt"
printf 'Results: %s\n' "$handoff_out"

handoff_run() {
  local handoff_check="$1"
  shift
  printf 'Checking %s\n' "$handoff_check"
  if "$@" > "$handoff_out/$handoff_check.log" 2>&1; then
    printf 'Passed %s\n' "$handoff_check"
  else
    printf 'failed: %s\n' "$handoff_check" > "$handoff_out/status.txt"
    tail -n 60 "$handoff_out/$handoff_check.log" >&2
    return 1
  fi
}

handoff_run unit go test ./...
handoff_run vet go vet ./...
if [[ "$handoff_mode" == --full ]]; then
  handoff_run race go test -race ./...
  handoff_run fuzz go test ./internal/storage -run '^$' -fuzz '^FuzzHandoffSchedule$' -fuzztime=30s -parallel=2
  handoff_run directory-fuzz go test ./internal/storage -run '^$' -fuzz '^FuzzDirectoryVersionRankPreservesPossibleUpdates$' -fuzztime=30s -parallel=2
  handoff_run schedule64 env INDEXUS_HANDOFF_64=1 "INDEXUS_HANDOFF_TRACE=$handoff_out/trace64.json" go test ./internal/storage -run '^TestHandoffSchedule64$' -count=2 -v
  handoff_run replay64 python3 -c '
import pathlib, re, sys
values = re.findall(r"transcript=([0-9a-f]{64})", pathlib.Path(sys.argv[1]).read_text())
if len(values) != 2 or values[0] != values[1]:
    raise SystemExit("64-disk runs did not produce two identical transcripts")
print(values[0])
' "$handoff_out/schedule64.log"
  handoff_run tlc python3 spec/handoff/check.py --jar "$TLA2TOOLS_JAR" --out "$handoff_out/tlc"
else
  handoff_run race go test -race ./internal/causal ./internal/storage ./internal/mesh ./internal/summary ./indexus -run 'LocalTickets|Membership|QueryCoverage|QueryFollows|InventoryAlwaysCompact|HandoffScheduleReplaysSeed|ContinuousReadsKeepEarlierACKs|JoinAfterBulk|ReadsAndSingleWriter|SummaryDelayedOldPhysical|RemoteCession'
fi
printf 'passed %s\n' "$handoff_mode" > "$handoff_out/status.txt"
printf 'Passed %s qualification. Results: %s\n' "$handoff_mode" "$handoff_out"
