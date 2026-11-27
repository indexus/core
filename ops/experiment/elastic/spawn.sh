#!/usr/bin/env bash
# Provisioner hook (cmd/provisioner -localDir ops/experiment/elastic): start one
# node in the agent the provisioner chose from the machines' resource facts
# (INDEXUS_PLACE_*, see facts.sh and docs/design/placement-and-resources.md).
# Without a decision (old agents, no facts): the agent with the most free
# slots (spread-first, empty hosts first).
#
# Ready reserve: with resource facts the provisioner counts it
# (autoscale.ReadyReserve: the percentage floor plus one machine per distinct
# request waiting, less the machines ready or warming) and hands the result
# over as INDEXUS_READY_RESERVE_DEFICIT. This hook only subtracts the boots it
# already accepted and applies the machine cap. Without facts: the slot rule,
# max(1, ceil(busy_machines * SPARE_RATIO / 100)) empty machines ready or
# booting (at least SPARE free slots). Deficits launch in parallel.
#   local: elastic/machine.sh (settings in $INDEXUS_ELASTIC_DIR/machine.env)
#   AWS:   instances from LAUNCH_TEMPLATE; each registers itself (agents.sh)
set -euo pipefail
HERE="$(dirname "$0")"
E=${INDEXUS_ELASTIC_DIR:-}
[[ -n "$E" && -f "$E/machine.env" ]] && source "$E/machine.env"
if [[ "${INDEXUS_PLACE_KIND:-}" == refuse ]]; then
  echo "placement refused: ${INDEXUS_PLACE_REASON:-resource admission}" >&2
  exit 1
fi
# A facts response identifies an agent even with incomplete resource measurements.
# EC2 and the stamped client tokens account for machines absent from the survey.
surveyed_aws=0
if [[ -n "${LAUNCH_TEMPLATE:-}" && -n "${INDEXUS_PLACE_KIND:-}" &&
      -n "${INDEXUS_RESERVE_AGENTS:-}" &&
      "${INDEXUS_PLACE_MACHINES:-}" =~ ^[0-9]+$ ]]; then
  surveyed_aws=1
  # With no immediate or at-cap target, the only possible effect is a boot.
  # One current survey suffices. Distinct at-cap vnode launches stay independent.
  if [[ $# == 0 && "${INDEXUS_PLACE_KIND:-}" == new && -z "${INDEXUS_PLACE_AGENT:-}" && -z "${INDEXUS_PLACE_CAP_AGENT:-}" ]]; then
    exec python3 "$HERE/boot_supply.py" --wait "${E:-/var/lib/indexus}/reserve.lock" "$0"
  fi
fi
machines_max="${MACHINES_MAX:-0}"
limit_reason=operator
if [[ -n "${CAPACITY_S3:-}" ]]; then
  policy=$(aws s3 cp "$CAPACITY_S3" - --only-show-errors) || { echo "capacity policy unavailable" >&2; exit 1; }
  values=$(printf '%s' "$policy" | python3 -c '
import json,sys
p=json.load(sys.stdin)
assert type(p["max_machines"]) is int and p["max_machines"] > 0
assert type(p["slots"]) is int and p["slots"] == int(sys.argv[1]), "slot policy requires machine restart"
assert set(p["reason"].split("+")) <= {"budget", "quota", "operator"}
print(p["max_machines"], p["reason"])
' "${SLOTS:-1}") || { echo "invalid capacity policy" >&2; exit 1; }
  read -r machines_max limit_reason <<<"$values"
fi
log="${E:-/var/lib/indexus}/machines.log"

# Reserved leading line and EX_TEMPFAIL jointly identify a pre-dispatch wait.
# Preserve curl's own diagnostics and exit code once a spawn has been sent.
wait_for_capacity() {
  echo "INDEXUS_NO_RECEIVER" >&2
  echo "$1" >&2
  exit 75
}

spawn_on() {
  echo "placed kind=$1 agent=$2" >&2
  exec curl -sf --max-time 60 -X POST "$2/spawn?prefer_near=${INDEXUS_PREFER_NEAR:-}"
}

# The reserve is the total demand beyond ready and warming hosts. The
# allowance is the part of the current boot window not already warming.
wanted=${INDEXUS_READY_RESERVE_DEFICIT:-}
# Distinguish slot-only accounting from a resource survey whose reserve is unknown.
basis=slots
[[ -n "${INDEXUS_PLACE_KIND:-}" ]] && basis=unknown
if [[ -n "$wanted" ]]; then
  [[ "$wanted" =~ ^[0-9]+$ ]] || { echo "invalid ready reserve deficit" >&2; exit 1; }
  if [[ -n "${INDEXUS_BOOT_ALLOWANCE:-}" ]]; then
    [[ "$INDEXUS_BOOT_ALLOWANCE" =~ ^[0-9]+$ ]] || { echo "invalid boot allowance" >&2; exit 1; }
    (( wanted > INDEXUS_BOOT_ALLOWANCE )) && wanted=$INDEXUS_BOOT_ALLOWANCE
  fi
fi

# A concrete target admits the vnode immediately. Replenish any remaining
# reserve separately: EC2 accounting can fail without invalidating that slot.
if [[ ${1:-} != --reserve-only && -n "${INDEXUS_PLACE_AGENT:-}" ]]; then
  case "${INDEXUS_PLACE_KIND:-}" in
    existing|new|local)
      wanted=${wanted:-0} # incomplete facts never authorize speculative boots
      echo "$(date +%T) spawn: capacity=$machines_max reason=$limit_reason surveyed=${INDEXUS_PLACE_MACHINES:-} boot_credit=$wanted decision_deficit=${INDEXUS_READY_RESERVE_DEFICIT:-0} reserve=${INDEXUS_RESERVE_DETAIL:-$basis} place=$INDEXUS_PLACE_KIND chosen=$INDEXUS_PLACE_AGENT why=${INDEXUS_PLACE_REASON:-}" >>"$log" 2>/dev/null || true
      (spawn_on "$INDEXUS_PLACE_KIND" "$INDEXUS_PLACE_AGENT") || exit $?
      if (( wanted > 0 )); then
        python3 "$HERE/boot_supply.py" --reconcile "${E:-/var/lib/indexus}/reserve.lock" "$0" </dev/null >>"$log" 2>&1 &
      fi
      exit 0
      ;;
  esac
fi

live_agents=""; best=""; most=0; free=0; running=0; live=0; busy=0; empty=0
if (( surveyed_aws )); then
  live=$INDEXUS_PLACE_MACHINES
  inventory="surveyed=$live"
else
  source "$HERE/agents.sh"
  for agent in ${AGENTS//,/ }; do
    read -r r f < <(curl -sf --max-time 2 "$agent/count") || continue
    live_agents+=" $agent"
    running=$((running + r)); free=$((free + f)); live=$((live + 1))
    if (( r > 0 )); then busy=$((busy + 1)); else empty=$((empty + 1)); fi
    if (( f > most )); then best=$agent; most=$f; fi
  done
  inventory="live=$live surveyed=${INDEXUS_PLACE_MACHINES:-$live} busy=$busy empty=$empty free=$free best=$best($most)"
fi
# Track in-flight boots individually so multiple machines can boot in parallel.
# Guard the deficit calculation with a short atomic lock so concurrent spawns
# cannot double-launch past MACHINES_MAX or leave stale stamps.
boot_dir="${E:-/var/lib/indexus}/booting.d"
mkdir -p "$boot_dir"
calc_lock="$boot_dir/.calc.lock"
lock_acquired=0
for _ in $(seq 1 50); do
  if mkdir "$calc_lock" 2>/dev/null; then lock_acquired=1; break; fi
  sleep 0.1
done
(( lock_acquired == 1 )) || wait_for_capacity "capacity calculation busy"
trap 'rmdir "$calc_lock" 2>/dev/null || true' EXIT
# Include machines accepted by EC2 that have not registered their agent yet.
accounted=0
unissued=0
unseen=0
now=$(date +%s)
boot_patience=180
if [[ -n "${LAUNCH_TEMPLATE:-}" ]]; then
  : "${EXPERIMENT_RUN:?AWS fleet accounting requires EXPERIMENT_RUN}"
  counts=$(aws ec2 describe-instances --filters "Name=tag:Run,Values=$EXPERIMENT_RUN" "Name=tag:Name,Values=$EXPERIMENT_RUN-h*" \
    'Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down' \
    --query 'Reservations[].Instances[].[PrivateIpAddress,LaunchTime,State.Name,ClientToken]' --output json |
    python3 "$HERE/boot_supply.py" "$boot_dir" "$live_agents" "$now" "$boot_patience") || exit 1
  read -r accounted unissued unseen <<<"$counts"
else
  for stamp in "$boot_dir"/*.boot; do
    [[ -f "$stamp" ]] || continue
    at=$(cat "$stamp" 2>/dev/null || echo 0)
    if (( now - at > boot_patience )); then rm -f "$stamp"; else unissued=$((unissued+1)); fi
  done
fi
booting=$((unissued + unseen))
reserve_known=0
deficit=0
if [[ -n "$wanted" ]]; then
  reserve_known=1
  deficit=$(( wanted - booting ))
elif [[ -z "${INDEXUS_PLACE_KIND:-}" ]]; then
  # Without a resource decision, keep enough free slots after this placement.
  slots_per_host="${SLOTS:-1}"
  [[ "${RESOURCE_SLOTS:-false}" != true ]] || slots_per_host=1
  (( slots_per_host < 1 )) && slots_per_host=1
  post_free=$((free > 0 ? free - 1 : 0))
  want_free=$(( ((running + 1) * ${SPARE_RATIO:-50} + 99) / 100 ))
  (( want_free < slots_per_host )) && want_free=$slots_per_host
  (( ${SPARE:-0} > want_free )) && want_free=$SPARE
  deficit=$(( (want_free - post_free + slots_per_host - 1) / slots_per_host - booting ))
fi
(( deficit < 0 )) && deficit=0
(( live > accounted )) && accounted=$live
# Reservations are deliberately conservative until accepted boots register.
room=$(( machines_max - (accounted + unissued) ))
(( room < 0 )) && room=0
# Keep the resource-selected target even if other machines appeared since
# the decision. An unavailable target fails the request; it never falls back
# to an unchecked free slot.
chosen=$best; applied=slots; wait_new=0
if [[ -n "${INDEXUS_PLACE_KIND:-}" ]]; then
  chosen=${INDEXUS_PLACE_AGENT:-}; applied=$INDEXUS_PLACE_KIND
  if [[ -z "$chosen" ]]; then
    # No machine has headroom for a receiver: the donor asks again once one
    # is up. The reserve counted this request; without it one must boot,
    # whatever the spare ratio says.
    wait_new=1
    (( reserve_known == 0 && deficit == 0 && booting == 0 )) && deficit=1
  fi
fi
(( deficit > room )) && deficit=$room
if (( wait_new == 1 && deficit == 0 && booting == 0 && room == 0 )); then
  # Machine cap reached: nothing will boot, take the fallback of the decision.
  wait_new=0; chosen=${INDEXUS_PLACE_CAP_AGENT:-}; applied=cap
  INDEXUS_PLACE_REASON=${INDEXUS_PLACE_CAP_REASON:-}
fi
stamps=()
if (( deficit > 0 )); then
  token=""
  if [[ -n "${LAUNCH_TEMPLATE:-}" ]]; then token=$(python3 -c 'import uuid; print(uuid.uuid4().hex)'); fi
  for i in $(seq 1 "$deficit"); do
    s="$boot_dir/${now}-$$-${i}.boot"
    echo "${now}${token:+ $token}" >"$s"
    stamps+=("$s")
  done
fi
rmdir "$calc_lock"
trap - EXIT
if (( deficit > 0 )); then
  if [[ -n "${LAUNCH_TEMPLATE:-}" ]]; then
    # Reservations keep their EC2 client token until expiry. Registering in
    # S3 alone never satisfies an older resource snapshot. An ambiguous AWS
    # response also keeps the reservation until EC2 can report the outcome.
    aws ec2 run-instances --client-token "$token" --launch-template "LaunchTemplateName=$LAUNCH_TEMPLATE" --count "$deficit" --query 'Instances[].InstanceId' --output text >>"$log" 2>&1 &
  elif [[ -n "$E" ]]; then
    for s in "${stamps[@]}"; do
      next=$(ls "$E"/agent*.pid 2>/dev/null | wc -l | tr -d ' ')
      ( "$HERE/machine.sh" "$next"; rm -f "$s" ) >/dev/null 2>&1 &
    done
  else
    rm -f "${stamps[@]}"
  fi
fi
event=spawn
[[ ${1:-} == --reserve-only ]] && event=reserve
echo "$(date +%T) $event: capacity=$machines_max reason=$limit_reason accounted=$accounted $inventory unseen=$unseen unissued=$unissued booting=$booting boot_credit=${wanted:-$basis} deficit=$deficit reserve=${INDEXUS_RESERVE_DETAIL:-$basis} place=$applied chosen=$chosen why=${INDEXUS_PLACE_REASON:-}" >>"$log" 2>/dev/null || true
[[ ${1:-} == --reserve-only ]] && exit 0
if (( wait_new == 1 )); then
  wait_for_capacity "waiting for a new machine: ${INDEXUS_PLACE_REASON:-} capacity=$machines_max booting=$booting launched=$deficit"
fi
[[ -n "$chosen" ]] || wait_for_capacity "no free slot: capacity=$machines_max reason=$limit_reason accounted=$accounted booting=$booting launched=$deficit"
spawn_on "$applied" "$chosen"
