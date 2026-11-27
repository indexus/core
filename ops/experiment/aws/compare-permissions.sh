#!/usr/bin/env bash
# Plan by default. --preflight reads AWS quotas/prices; --run provisions a run.
# Each version gets a separate persistent checkout and independent cluster.
set -euo pipefail
comparison_root="$(cd "$(dirname "$0")/../../.." && pwd)"
action="${1:---plan}"
profile="${2:-smoke}"
label="${3:-after}"
case "$action" in --plan|--preflight|--run) ;; *) echo 'Usage: compare-permissions.sh [--plan|--preflight|--run] [smoke|16k] [before|after]' >&2; exit 2;; esac
case "$profile" in
 smoke) rate=250; ramp=15; plateau=30; tail=45; ttl=20; budget=3; minimum_hosts=4; demand=250; loader=c6i.2xlarge;;
 16k) rate=2000; ramp=60; plateau=120; tail=180; ttl=45; budget=6; minimum_hosts=16; demand=500; loader=c6i.4xlarge;;
 *) echo 'Unknown profile' >&2; exit 2;;
esac
case "$label" in
 before) ref="${BEFORE_REF:-6c1f58e}";;
 after) ref="${AFTER_REF:-HEAD}";;
 *) echo 'Label must be before or after' >&2; exit 2;;
esac
ref=$(git -C "$comparison_root" rev-parse --verify "$ref^{commit}")
loader_ref=$(git -C "$comparison_root" rev-parse --verify "${LOADER_REF:-HEAD}^{commit}")
region="${REGION:-eu-west-3}"
route="${STEP_ROUTE:-direct}"
case "$route" in direct|relay) ;; *) echo 'STEP_ROUTE must be direct or relay' >&2; exit 2;; esac
budget="${BUDGET:-$budget}"
ceiling="${HOSTS_MAX:-64}"
run="${RUN:-perm-${profile}-${route}-${label}-$(date +%m%d-%H%M%S)}"
[[ "$run" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ && ${#run} -le 44 ]] || { echo 'RUN must be lowercase alphanumeric/hyphen, at most 44 characters (S3 bucket suffix)' >&2; exit 2; }
echo "run=$run"
echo "common_loader_commit=$loader_ref"
echo "profile=$profile label=$label commit=$ref route=$route region=$region"
echo "rates=$rate,$((rate*2)),$((rate*4)),$((rate*8)) ramp=${ramp}s plateau=${plateau}s tail=${tail}s"
echo "initial_hosts=2 slots=2 host_type=i4i.xlarge loader=$loader loaders=1 max_hosts=$ceiling"
echo "minimum_effective_hosts=$minimum_hosts budget_per_run=$budget ttl_minutes=$ttl"
echo "scaling_writes_per_vnode=$demand items_threshold=0 write_cap=0"
echo 'Prices, free regional vCPU quota and the resulting machine limit are resolved at preflight.'
echo 'The budget is a planning envelope, not a billing kill switch; prices are not hardcoded.'
if [[ "$action" == --plan ]]; then
 echo 'PLAN ONLY: no AWS calls, worktree, or resource creation.'
 exit 0
fi
plans="${COMPARISON_PLANS:-$comparison_root/../aws-permissions-comparison}"
mkdir -p "$plans"
capacity="$plans/$run.capacity.json"
python3 "$comparison_root/ops/experiment/aws/capacity.py" plan \
 --run "$run" --region "$region" --bucket "indexus-$run-preflight" \
 --host-type i4i.xlarge --loader-type "$loader" --hosts 2 --slots 2 --loaders 1 \
 --ttl "$ttl" --budget "$budget" --max-hosts "$ceiling" > "$capacity"
python3 - "$capacity" "$minimum_hosts" <<'PY'
import json,sys
p=json.load(open(sys.argv[1])); minimum=int(sys.argv[2])
print(json.dumps(p,indent=2))
if p['max_machines'] < minimum:
 raise SystemExit(f"Insufficient effective fleet: {p['max_machines']} hosts < {minimum}; limits={p['limits']}. Adjust quota/budget explicitly before running.")
PY
[[ "$action" != --preflight ]] || exit 0
checkout="$plans/$run/core"
mkdir -p "$(dirname "$checkout")"
git -C "$comparison_root" worktree add --detach "$checkout" "$ref"
loader_checkout="$plans/$run/loader"
git -C "$comparison_root" worktree add --detach "$loader_checkout" "$loader_ref"
python3 "$comparison_root/ops/experiment/loader_tail.py" "$loader_checkout" > "$plans/$run/loader-instrumentation.json"
echo "Persistent experiment checkout/state: $checkout"
echo "Resource recovery state: $comparison_root/ops/experiment/aws/.state/$run.ids"
echo 'Run.sh performs its tagged-resource teardown; this checkout is retained for logs and recovery.'
set +e
env EXPERIMENT_SOURCE_ROOT="$checkout" EXPERIMENT_LOADER_ROOT="$loader_checkout" RUN="$run" REGION="$region" MODE=steps STEP_ROUTE="$route" NET=h3 \
 HOST_TYPE=i4i.xlarge HOSTS=2 SLOTS=2 SIZES=4 HOSTS_MAX="$ceiling" \
 LOADERS=1 LOADER_TYPE="$loader" STEP_RATE="$rate" STEP_RAMP="$ramp" \
 STEP_PLATEAU="$plateau" STEP_TAIL="$tail" ITEMS=0 SCALING_WRITES="$demand" WRITE_CAP=0 \
 BUDGET="$budget" TTL="$ttl" OPEN_LIVE=1 \
 bash "$comparison_root/ops/experiment/aws/run.sh"
status=$?
set -e
results="$checkout/ops/experiment/results/$run"
if [[ -d "$results" ]]; then
 git -C "$comparison_root" rev-parse HEAD > "$results/analysis-source-commit.txt"
 phases=()
 [[ ! -f "$results/load.jsonl" ]] || phases+=("$results")
 for phase in "$results"/*/; do
  [[ ! -f "$phase/load.jsonl" ]] || phases+=("$phase")
 done
 if ((${#phases[@]})); then
  python3 "$comparison_root/ops/experiment/report.py" "${phases[@]}" > "$results/comparison-report.md"
  for phase in "${phases[@]}"; do
   if [[ -f "$phase/metrics.jsonl" ]]; then
    python3 "$comparison_root/ops/experiment/live.py" replay "$phase" "$phase/live-percentiles.jsonl"
    python3 "$comparison_root/ops/experiment/live.py" html "$phase/live-percentiles.jsonl" "$phase/live-percentiles.html"
   fi
  done
 else
  echo "No load results retrieved; comparison is incomplete: $results" >&2
  [[ "$status" != 0 ]] || status=1
 fi
fi
exit "$status"
