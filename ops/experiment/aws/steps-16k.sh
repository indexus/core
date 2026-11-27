#!/usr/bin/env bash
# Direct 2k -> 4k -> 8k -> 16k comparison. Print the effective settings
# without touching AWS unless --run is explicit. Each setting is overridable.
set -euo pipefail
case "${1:---plan}" in
  --plan|--run) ;;
  *) echo "usage: $0 [--plan|--run]" >&2; exit 2 ;;
esac
# Defaults scoped to this experiment, including the split trigger. WRITE_CAP
# is independent: crossing SCALING_WRITES requests relief and keeps accepting writes.
settings=(
  MODE=steps STEP_ROUTE=direct NET=h3
  HOST_TYPE=i4i.xlarge HOSTS=4 SLOTS=auto HOSTS_MAX=32 SIZES=16 STEP_VNODES=16
  LOADERS=1 LOADER_TYPE=c6i.4xlarge
  STEP_RATE=2000 STEP_RAMP=60 STEP_PLATEAU=120 STEP_TAIL=180 STEP_PASSES=1 STEP_WORKERS=4096
  ITEMS=500000 SCALING_WRITES=50 WRITE_CAP=0 RESOURCE_SIGNALS=true
  CPU_TARGET_PCT=50 READY_RESERVE_PCT=15 CPU_PROFILE=1 SAMPLE=0
  TTL=30 BUDGET=8 OPEN_LIVE=1
)
for setting in "${settings[@]}"; do
  key=${setting%%=*}
  [[ -n "${!key:-}" ]] || export "$setting"
  printf '%s=%s\n' "$key" "${!key}"
done
if [[ ${1:---plan} == --run ]]; then
  exec bash "$(dirname "$0")/run.sh"
fi
