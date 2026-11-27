#!/usr/bin/env bash
# One-command AWS campaign with the AWS CLI alone (no Terraform), run from the
# operator's shell:
#
#   ops/experiment/aws/run.sh                                   # 4 hosts, resource-based admission, sizes 8 16 32
#   MODE=push HOSTS=8 SIZES="8 16 32 64" LOADERS=2 ops/experiment/aws/run.sh
#                                                               # ladder: raise writes/s per size until saturation
#   MODE=passes PASSES=3 RATES_PER_VNODE="1000 2500" SIZES=16 HOSTS=16 SLOTS=1 LOADERS=2 ops/experiment/aws/run.sh
#                                                               # fixed rates, repeated on fresh clusters (noise, REM-94)
#   MODE=dynamics HOSTS=2 SLOTS=4 TTL=60 ops/experiment/aws/run.sh
#                                                               # elasticity only: spike, splits, calm, folds, audit
#   MODE=steps HOSTS=2 SLOTS=4 BUDGET=8 STEP_RATE=2000 ops/experiment/aws/run.sh
#                                                               # machines on demand: the provisioner starts hosts, 64 at most
#   MODE=ramp SIZES="8 16 32 64" HOSTS=64 SLOTS=1 HOST_TYPE=i4i.xlarge RAMP_DOUBLE=60 ops/experiment/aws/run.sh
#                                                               # one ramp per size, x2 every 60 s until the knee (RAMP-1)
#   MODE=ramp RAMP_ELASTIC=1 RAMP_DOUBLES="300 120 60" HOSTS=2 SLOTS=4 BUDGET=10 ops/experiment/aws/run.sh
#                                                               # machines on demand: the shortest doubling time absorbed
#   ops/experiment/aws/run.sh destroy                           # remove anything a run left behind
#
# Everything is created for the run and tagged Project=indexus-experiment,
# Run=<run>: its own VPC, subnet, gateway, security group, IAM role, S3 bucket
# and instances. Never ops/deploy. Every id is recorded in .state/<run>.ids as it
# is created, and the run removes them on success, on error and on Ctrl-C.
# Every instance also terminates itself at a fixed deadline (TTL minutes).
# Results land in ops/experiment/results/<run>/. Works with macOS's bash 3.2.
set -euo pipefail
HERE="${RUN_HERE:-$(cd "$(dirname "$0")" && pwd)}"
# Run from a copy: bash reads a script as it goes, so editing this file during
# a campaign broke one (2026-11-19, 73 min lost).
if [[ -z "${RUN_HERE:-}" ]]; then
  copy=$(mktemp); cp "$0" "$copy"; RUN_HERE="$HERE" exec bash "$copy" "$@"
fi
HARNESS_ROOT="$(cd "$HERE/../../.." && pwd)"
ROOT="${EXPERIMENT_SOURCE_ROOT:-$HARNESS_ROOT}"
LOADER_ROOT="${EXPERIMENT_LOADER_ROOT:-$ROOT}"
[[ -f "$ROOT/go.mod" && -f "$LOADER_ROOT/go.mod" ]] || { echo "Invalid experiment source/loader checkout" >&2; exit 2; }
REGION="${REGION:-eu-west-3}"
HOSTS="${HOSTS:-4}"
# Machines on demand: the provisioner starts one more host (from a launch
# template of this run) when fewer than SPARE slots stay free. The ceiling is
# not a fixed count: it follows the load up to what
# BUDGET (dollars, default 5) pays for TTL minutes at HOST_PRICE per hour,
# and the account's on-demand vCPU quota. HOSTS_MAX, when given, lowers it.
# The step workload admits each new vnode from live machine facts. An explicit
# integer requests a fixed experiment layout. In auto mode the agent
# allocates addresses on demand.
MODE="${MODE:-all}"
SLOTS="${SLOTS:-auto}"
resource_slots=false
if [[ "$SLOTS" == auto ]]; then
  resource_slots=true
  SLOTS=0
  SPARE="${SPARE:-1}" # one immediately admissible receiver per machine
fi
[[ "$resource_slots" == true || "$SLOTS" =~ ^[1-9][0-9]*$ ]] || { echo "SLOTS must be auto or a positive integer" >&2; exit 2; }
SIZES="${SIZES:-8 16 32}"
ITEMS="${ITEMS:-500000}"
TTL="${TTL:-120}"
NET="${NET:-h3}"
HOST_TYPE="${HOST_TYPE:-c6i.2xlarge}"
HOST_PRICE="${HOST_PRICE:-}" # resolved from AWS Pricing unless explicitly supplied
# The load machine grows with the peak it must offer (steps: 8 x STEP_RATE):
# about 2 000 writes/s per vCPU measured on c6i, kept under half of that.
if [[ -z "${LOADER_TYPE:-}" && "$MODE" == steps ]]; then
  peak=$(( ${STEP_RATE:-1000} * 8 ))
  if ((peak <= 8000)); then LOADER_TYPE=c6i.2xlarge; elif ((peak <= 16000)); then LOADER_TYPE=c6i.4xlarge
  elif ((peak <= 32000)); then LOADER_TYPE=c6i.8xlarge; else LOADER_TYPE=c6i.16xlarge; fi
fi
# A recorded pair replays the exact offered keys on fresh comparison clusters.
# Keep the seed in 32 bits so adding the pass index is safe in bash arithmetic.
if [[ "$MODE" == steps ]]; then
  : "${STEP_SEED:=$(python3 -c 'import secrets; print(secrets.randbits(32))')}"
  : "${STEP_TAG:=$(python3 -c 'import secrets; print(secrets.token_urlsafe(5)[:6])')}"
fi
if [[ -n "${STEP_SEED:-}" ]]; then
  [[ $STEP_SEED =~ ^[0-9]{1,10}$ ]] && (( 10#$STEP_SEED <= 4294967295 )) || { echo "invalid STEP_SEED (uint32)" >&2; exit 2; }
fi
if [[ -n "${STEP_TAG:-}" && ! $STEP_TAG =~ ^[0-9A-Za-z_-]{1,6}$ ]]; then echo "invalid STEP_TAG (1-6 identifier characters)" >&2; exit 2; fi
export STEP_SEED STEP_TAG
if [[ "$MODE" == steps ]]; then
  export STEP_VNODES="${STEP_VNODES:-${SIZES%% *}}"
  if ! [[ "$STEP_VNODES" =~ ^[1-9][0-9]*$ ]]; then
    echo "STEP_VNODES must be a positive integer" >&2
    exit 1
  fi
  peak=$(( ${STEP_RATE:-1000} * 8 ))
  export STEP_WORKERS="${STEP_WORKERS:-$(( peak > 4096 ? peak : 4096 ))}"
fi
# ramp: the knee is unknown, so the load machines are sized for a ceiling,
# RAMP_MAX: 5 000 writes/s per vnode at the largest size (the green ceiling
# measured on i4i.xlarge is about 3 000), 100 000 for RAMP_ELASTIC=1. One
# c6i.4xlarge offered 32 000 writes/s at 3.3 of its 16 cores (2026-11-17):
# 40 000 per machine leaves it under half.
if [[ "$MODE" == ramp ]]; then
  if [[ "${RAMP_ELASTIC:-0}" == 1 ]]; then RAMP_MAX="${RAMP_MAX:-100000}"
  else largest=$(echo $SIZES | tr ' ' '\n' | sort -n | tail -1); RAMP_MAX="${RAMP_MAX:-$(( largest * 5000 ))}"; fi
  export RAMP_MAX
  LOADER_TYPE="${LOADER_TYPE:-c6i.4xlarge}"
  LOADERS="${LOADERS:-$(( (RAMP_MAX + 39999) / 40000 ))}"
fi
LOADER_TYPE="${LOADER_TYPE:-c6i.2xlarge}"
LOADERS="${LOADERS:-1}" # load machines, all synchronized on each step (LOAD-SYNC)
DATA_BYTES="${DATA_BYTES:-4294967296}"
WRITE_CAP="${WRITE_CAP:-0}" # writes/s per vnode (CAP-2): with it, a vnode scales up on its write demand
RUN="${RUN:-exp-$(date +%Y%m%d-%H%M%S)}"
RESULTS="$ROOT/ops/experiment/results/$RUN"
BIN="$HERE/.bin/$RUN" # one per run: campaigns may run side by side
STATE="$HERE/.state"
IDS="$STATE/$RUN.ids"
mkdir -p "$STATE"

cli() { aws --region "$REGION" --output text "$@"; }
note() { echo "$1=$2" >>"$IDS"; }
ids_of() { grep "^$1=" "$2" 2>/dev/null | cut -d= -f2- || true; }
tagspec() { echo "ResourceType=$1,Tags=[{Key=Project,Value=indexus-experiment},{Key=Run,Value=$RUN},{Key=Name,Value=$RUN-$2}]"; }
retry() { local n=0; until "$@"; do n=$((n + 1)); ((n < 12)) || return 1; sleep 5; done; }

# teardown IDS_FILE: delete what the file records, newest dependency first.
teardown() {
  local f=$1 x
  [[ -f $f ]] || return 0
  set +e # remove all that can be removed, report the rest
  echo "==> removing the resources of $(basename "$f" .ids)"
  local run; run=$(basename "$f" .ids)
  # No new machine from here on: the template goes first.
  for x in $(ids_of TEMPLATE "$f"); do cli ec2 delete-launch-template --launch-template-name "$x" >/dev/null || echo "!! launch template $x left" >&2; done
  # Recorded instances and those the provisioner started (tagged with the run).
  local instances; instances=$( (ids_of INSTANCE "$f"; cli ec2 describe-instances --filters "Name=tag:Run,Values=$run" Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[].Instances[].InstanceId' | tr '\t' '\n') | sort -u | tr '\n' ' ')
  if [[ -n ${instances// /} ]]; then
    cli ec2 terminate-instances --instance-ids $instances >/dev/null || true
    cli ec2 wait instance-terminated --instance-ids $instances || true
  fi
  # A machine that was starting while the first list was read.
  local late; late=$(cli ec2 describe-instances --filters "Name=tag:Run,Values=$run" Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[].Instances[].InstanceId')
  if [[ -n ${late//[[:space:]]/} ]]; then
    cli ec2 terminate-instances --instance-ids $late >/dev/null || true
    cli ec2 wait instance-terminated --instance-ids $late || true
  fi
  for x in $(ids_of SG "$f"); do retry cli ec2 delete-security-group --group-id "$x" >/dev/null || echo "!! security group $x left" >&2; done
  for x in $(ids_of ASSOC "$f"); do cli ec2 disassociate-route-table --association-id "$x" >/dev/null || true; done
  for x in $(ids_of RTB "$f"); do cli ec2 delete-route-table --route-table-id "$x" >/dev/null || echo "!! route table $x left" >&2; done
  local vpc; vpc=$(ids_of VPC "$f" | head -1)
  for x in $(ids_of IGW "$f"); do
    [[ -n $vpc ]] && cli ec2 detach-internet-gateway --internet-gateway-id "$x" --vpc-id "$vpc" >/dev/null || true
    cli ec2 delete-internet-gateway --internet-gateway-id "$x" >/dev/null || echo "!! gateway $x left" >&2
  done
  for x in $(ids_of SUBNET "$f"); do retry cli ec2 delete-subnet --subnet-id "$x" >/dev/null || echo "!! subnet $x left" >&2; done
  [[ -n $vpc ]] && { cli ec2 delete-vpc --vpc-id "$vpc" >/dev/null || echo "!! vpc $vpc left" >&2; }
  for x in $(ids_of PROFILE "$f"); do
    cli iam remove-role-from-instance-profile --instance-profile-name "$x" --role-name "$x" >/dev/null 2>&1 || true
    cli iam delete-instance-profile --instance-profile-name "$x" >/dev/null || echo "!! instance profile $x left" >&2
  done
  for x in $(ids_of ROLE "$f"); do
    cli iam delete-role-policy --role-name "$x" --policy-name s3 >/dev/null 2>&1 || true
    cli iam delete-role-policy --role-name "$x" --policy-name machines >/dev/null 2>&1 || true
    cli iam delete-role --role-name "$x" >/dev/null || echo "!! role $x left" >&2
  done
  for x in $(ids_of BUCKET "$f"); do aws --region "$REGION" s3 rb "s3://$x" --force >/dev/null || echo "!! bucket $x left" >&2; done
  mv "$f" "$f.removed"
}

if [[ "${1:-}" == capacity ]]; then
  target="${2:?usage: run.sh capacity RUN [--budget USD] [--max-hosts N|auto] [--dry-run]}"
  [[ "$target" =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
  shift 2
  exec python3 "$HERE/capacity.py" update "$STATE/$target.capacity.json" "$@"
fi

if [[ "${1:-}" == "destroy" ]]; then
  for f in "$STATE"/*.ids; do [[ -f $f ]] && teardown "$f"; done
  # Safety net: any experiment instance still alive, recorded or not.
  left=$(cli ec2 describe-instances --filters Name=tag:Project,Values=indexus-experiment Name=instance-state-name,Values=pending,running,stopping,stopped --query 'Reservations[].Instances[].InstanceId' | tr '\t\n' '  ')
  if [[ -n ${left// /} ]]; then echo "==> terminating stray instances: $left"; cli ec2 terminate-instances --instance-ids $left >/dev/null; fi
  echo "==> nothing left from experiment runs"
  exit 0
fi

largest=${SIZES##* }
if [[ "$MODE" == steps ]]; then largest=$STEP_VNODES; fi
if [[ "$resource_slots" != true ]] && ((largest > HOSTS * SLOTS)); then
  echo "SIZES up to $largest need at least $largest slots; HOSTS x SLOTS = $((HOSTS * SLOTS))" >&2
  exit 2
fi

# render TEMPLATE OUT key=value...: the .tftpl syntax, ${key} and $${ for a literal ${.
render() {
  local tpl=$1 out=$2 kv k v script='s/\$\${/@@LITERAL@@{/g;'
  shift 2
  for kv in "$@"; do
    k=${kv%%=*}; v=${kv#*=}; v=${v//\\/\\\\}; v=${v//|/\\|}; v=${v//&/\\&}
    script="$script s|\${$k}|$v|g;"
  done
  sed -e "$script" -e 's/@@LITERAL@@{/${/g' "$tpl" >"$out"
}

# Resolve all bounds before creating any paid resource. No implicit machine cap.
bucket="indexus-$RUN-$(date +%s)"
capacity="$STATE/$RUN.capacity.json"
policy_args=(plan --run "$RUN" --region "$REGION" --bucket "$bucket"
  --host-type "$HOST_TYPE" --loader-type "$LOADER_TYPE" --hosts "$HOSTS"
  --slots "$SLOTS" --loaders "$LOADERS" --ttl "$TTL" --budget "${BUDGET:-5}")
[[ "$resource_slots" != true ]] || policy_args+=(--resource-slots)
[[ -z "$HOST_PRICE" ]] || policy_args+=(--host-price "$HOST_PRICE")
[[ -z "${LOADER_PRICE:-}" ]] || policy_args+=(--loader-price "$LOADER_PRICE")
operator_max="${HOSTS_MAX:-${HOSTS_CEILING:-}}"
if [[ -n "${HOSTS_CEILING:-}" && -n "$operator_max" ]] && ((HOSTS_CEILING < operator_max)); then operator_max=$HOSTS_CEILING; fi
[[ -z "$operator_max" ]] || policy_args+=(--max-hosts "$operator_max")
python3 "$HERE/capacity.py" "${policy_args[@]}" >"$capacity"
HOSTS_MAX=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["max_machines"])' "$capacity")
echo "==> scaling: items=$ITEMS writes/s=${SCALING_WRITES:-0} write-cap=$WRITE_CAP (0 disables each threshold)"
echo "==> capacity policy (budget, regional quota, optional operator limit):"
cat "$capacity"

cleanup() { trap - EXIT INT TERM; teardown "$IDS"; }
trap cleanup EXIT
trap 'exit 130' INT TERM

# The tree under test, recorded with the results (REM-94).
mkdir -p "$RESULTS"
cp "$capacity" "$RESULTS/capacity.json"
{
  echo "commit=$(git -C "$ROOT" rev-parse HEAD)"
  echo "loader_commit=$(git -C "$LOADER_ROOT" rev-parse HEAD)"
  echo "loader_diff_sha256=$(git -C "$LOADER_ROOT" diff HEAD -- cmd/experiment | shasum -a 256 | cut -d' ' -f1)"
  echo "infrastructure_commit=$(git -C "$HARNESS_ROOT" rev-parse HEAD)"
  echo "agent_commit=$(git -C "$ROOT" rev-parse HEAD) cpu_profile=${CPU_PROFILE:-0}"
  echo "diff_sha256=$(git -C "$ROOT" diff HEAD -- . ':!ops/agent' | shasum -a 256 | cut -d' ' -f1)"
  echo "scaling_items=$ITEMS scaling_writes=${SCALING_WRITES:-0} cpu_target_pct=${CPU_TARGET_PCT:-0} write_cap=$WRITE_CAP resource_signals=${RESOURCE_SIGNALS:-true} ready_reserve_pct=${READY_RESERVE_PCT:-15}"
  echo "hosts=$HOSTS host_type=$HOST_TYPE slots=$SLOTS resource_slots=$resource_slots loaders=$LOADERS loader_type=$LOADER_TYPE sizes=$SIZES mode=$MODE net=$NET region=$REGION"
  env | grep -E '^(SCALING_WRITES|OBSERVE_SECONDS|PUSH_START|STEP_SECONDS|PCT|PASSES|RATES_PER_VNODE|SAMPLE|STEP_VNODES|STEP_RATE|STEP_RAMP|STEP_PLATEAU|STEP_PASSES|STEP_WORKERS|STEP_TAIL|STEP_ROUTE|STEP_SEED|STEP_TAG|PHASE_AUDIT|QUIET_MAX|STALL_VNODES|STALL_RATE|STALL_FILL|STALL_ITEMS|STALL_FILL_RATE|RAMP_[A-Z0-9_]+)=' || true
} >"$RESULTS/tree.txt"
echo "==> building linux/amd64 binaries"
rm -rf "$BIN" && mkdir -p "$BIN/elastic"
trap 'rm -rf "$BIN"; cleanup' EXIT
# CPU_PROFILE=1 builds the node with loopback profiling (-tags diagnostic) and
# makes each agent relay it: GET <agent>/profile?slot=I&seconds=N.
node_tags=(); debug_base=0
if [[ "${CPU_PROFILE:-0}" == 1 ]]; then node_tags=(-tags diagnostic); debug_base=19200; fi
for cmd in node issuer provisioner experiment; do
  build_root="$ROOT"; tags=()
  [[ "$cmd" != experiment ]] || build_root="$LOADER_ROOT"
  [[ "$cmd" != node ]] || tags=(${node_tags[@]+"${node_tags[@]}"})
  (cd "$build_root" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath ${tags[@]+"${tags[@]}"} -ldflags "-s -w" -o "$BIN/$cmd" "./cmd/$cmd")
done
# The machine agent is cmd/experiment too, but it belongs to the version under
# test (its /facts, its spawn): it is built from ROOT, whatever LOADER_ROOT
# pins for the load generator.
(cd "$ROOT" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$BIN/agent" ./cmd/experiment)
# NET=h3 (default): the node from the quic/ module, HTTP/3 between nodes
# (NET-1). NET=tcp: the production node, HTTPS over TCP. The loader is the
# core build either way and reaches nodes over TCP.
if [[ $NET == h3 ]]; then
  (cd "$ROOT/quic" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath ${node_tags[@]+"${node_tags[@]}"} -ldflags "-s -w" -o "$BIN/node" ./cmd/node)
fi
cp "$ROOT"/ops/experiment/elastic/*.sh "$BIN/elastic/"
cp "$ROOT/ops/experiment/elastic/boot_supply.py" "$BIN/elastic/"
# Nodes run TLS under a throwaway CA for this run only (HTTP/3 with NET=h3).
(cd "$ROOT" && go run ./cmd/experiment ca -dir "$BIN/ca")

echo "==> $RUN: $HOSTS x $HOST_TYPE (slots=$SLOTS, resource admission=$resource_slots) + $LOADERS x $LOADER_TYPE, sizes $SIZES, mode $MODE, net $NET, region $REGION"
if [[ $REGION == us-east-1 ]]; then
  cli s3api create-bucket --bucket "$bucket" >/dev/null
else
  cli s3api create-bucket --bucket "$bucket" --create-bucket-configuration "LocationConstraint=$REGION" >/dev/null
fi
note BUCKET "$bucket"
aws --region "$REGION" s3 cp "$capacity" "s3://$bucket/plan/capacity.json" --only-show-errors
aws --region "$REGION" s3 cp "$BIN" "s3://$bucket/bin/" --recursive --only-show-errors
aws --region "$REGION" s3 cp "$HERE/scenarios.sh" "s3://$bucket/plan/scenarios.sh" --only-show-errors
aws --region "$REGION" s3 cp "$HERE/../evidence.py" "s3://$bucket/plan/evidence.py" --only-show-errors
aws --region "$REGION" s3 cp "$HERE/../live.py" "s3://$bucket/plan/live.py" --only-show-errors
aws --region "$REGION" s3 cp "$HERE/../fleet.py" "s3://$bucket/plan/fleet.py" --only-show-errors
aws --region "$REGION" s3 cp "$HERE/../ramp.py" "s3://$bucket/plan/ramp.py" --only-show-errors
# Plan parameters the load machine exports before scenarios.sh (REM-94).
# Quoted: RATES_PER_VNODE holds spaces, and the loader sources this file.
env | grep -E '^(SCALING_WRITES|OBSERVE_SECONDS|PUSH_START|STEP_SECONDS|PCT|PASSES|RATES_PER_VNODE|SAMPLE|STEP_VNODES|STEP_RATE|STEP_RAMP|STEP_PLATEAU|STEP_PASSES|STEP_WORKERS|STEP_TAIL|STEP_ROUTE|STEP_SEED|STEP_TAG|PHASE_AUDIT|QUIET_MAX|STALL_VNODES|STALL_RATE|STALL_FILL|STALL_ITEMS|STALL_FILL_RATE|RAMP_[A-Z0-9_]+)=' | sed "s/=\(.*\)/='\1'/" >"$BIN/plan.env" || true
aws --region "$REGION" s3 cp "$BIN/plan.env" "s3://$bucket/plan/env" --only-show-errors

# Network: one public subnet; members reach each other on any port, nothing
# reaches them from outside.
vpc=$(cli ec2 create-vpc --cidr-block 10.77.0.0/16 --tag-specifications "$(tagspec vpc vpc)" --query Vpc.VpcId); note VPC "$vpc"
cli ec2 modify-vpc-attribute --vpc-id "$vpc" --enable-dns-hostnames '{"Value":true}'
igw=$(cli ec2 create-internet-gateway --tag-specifications "$(tagspec internet-gateway igw)" --query InternetGateway.InternetGatewayId); note IGW "$igw"
cli ec2 attach-internet-gateway --internet-gateway-id "$igw" --vpc-id "$vpc"
az=$(cli ec2 describe-availability-zones --filters Name=state,Values=available --query 'AvailabilityZones[0].ZoneName')
subnet=$(cli ec2 create-subnet --vpc-id "$vpc" --cidr-block 10.77.0.0/20 --availability-zone "$az" --tag-specifications "$(tagspec subnet subnet)" --query Subnet.SubnetId); note SUBNET "$subnet"
cli ec2 modify-subnet-attribute --subnet-id "$subnet" --map-public-ip-on-launch
rtb=$(cli ec2 create-route-table --vpc-id "$vpc" --tag-specifications "$(tagspec route-table rtb)" --query RouteTable.RouteTableId); note RTB "$rtb"
cli ec2 create-route --route-table-id "$rtb" --destination-cidr-block 0.0.0.0/0 --gateway-id "$igw" >/dev/null
assoc=$(cli ec2 associate-route-table --route-table-id "$rtb" --subnet-id "$subnet" --query AssociationId); note ASSOC "$assoc"
sg=$(cli ec2 create-security-group --group-name "$RUN" --description "indexus experiment $RUN: members only" --vpc-id "$vpc" --tag-specifications "$(tagspec security-group sg)" --query GroupId); note SG "$sg"
cli ec2 authorize-security-group-ingress --group-id "$sg" --ip-permissions "IpProtocol=-1,UserIdGroupPairs=[{GroupId=$sg}]" >/dev/null

# Instances may read the binaries and write results in this run's bucket only.
cli iam create-role --role-name "$RUN" --tags Key=Project,Value=indexus-experiment Key=Run,Value="$RUN" \
  --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}' >/dev/null
note ROLE "$RUN"
cli iam put-role-policy --role-name "$RUN" --policy-name s3 --policy-document "$(python3 "$HERE/template.py" s3 "$bucket")"
cli iam create-instance-profile --instance-profile-name "$RUN" >/dev/null
note PROFILE "$RUN"
cli iam add-role-to-instance-profile --instance-profile-name "$RUN" --role-name "$RUN"

ami=$(cli ssm get-parameter --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 --query Parameter.Value)
work=$(mktemp -d)
template="$RUN-host" # always available, including after a live policy increase
common=(bucket="$bucket" region="$REGION" ttl="$TTL" slots="$SLOTS" resource_slots="$resource_slots" capacity_s3="s3://$bucket/plan/capacity.json" run="$RUN" data_bytes="$DATA_BYTES" write_cap="${WRITE_CAP:-0}" write_demand="${SCALING_WRITES:-0}" items="$ITEMS"
  cpu_target="${CPU_TARGET_PCT:-0}" resource_signals="${RESOURCE_SIGNALS:-true}" ready_reserve_pct="${READY_RESERVE_PCT:-15}" replication="${REPLICATION:-}" replication_machine="${REPLICATION_MACHINE:-host}"
  launch_template="$template" machines_max="$HOSTS_MAX" spare="${SPARE:-$SLOTS}" debug_base="$debug_base")

# launch NAME TYPE USERDATA -> prints the private IP; the profile can take a
# few seconds to become usable after creation, hence the retry.
launch() {
  local name=$1 type=$2 userdata=$3 out n=0
  until out=$(cli ec2 run-instances --image-id "$ami" --instance-type "$type" --subnet-id "$subnet" \
    --security-group-ids "$sg" --iam-instance-profile "Name=$RUN" --instance-initiated-shutdown-behavior terminate \
    --user-data "file://$userdata" --count 1 --tag-specifications "$(tagspec instance "$name")" \
    --block-device-mappings "DeviceName=/dev/xvda,Ebs={VolumeSize=40,VolumeType=gp3,Iops=${EBS_IOPS:-3000},Throughput=${EBS_THROUGHPUT:-125},DeleteOnTermination=true}" \
    --query 'Instances[0].[InstanceId,PrivateIpAddress]' 2>"$work/err"); do
    n=$((n + 1))
    if ((n >= 12)) || ! grep -q "Invalid IAM Instance Profile\|iamInstanceProfile" "$work/err"; then cat "$work/err" >&2; return 1; fi
    sleep 5
  done
  note INSTANCE "${out%%[[:space:]]*}"
  echo "${out##*[[:space:]]}"
}

render "$HERE/host.sh.tftpl" "$work/h0.sh" "${common[@]}" index=0 host0=""
ip0=$(launch h0 "$HOST_TYPE" "$work/h0.sh")
if [[ -n $template ]]; then
  # The hosts started on demand run the same script as any host but the
  # first. Host 0 may start instances from a launch template only, and pass
  # them this run's role only; teardown ends every instance tagged with the run.
  render "$HERE/host.sh.tftpl" "$work/hx.sh" "${common[@]}" index=x host0="$ip0"
  account=$(cli sts get-caller-identity --query Account)
  python3 "$HERE/template.py" "$work/hx.sh" "$ami" "$HOST_TYPE" "$subnet" "$sg" "$RUN" "${EBS_IOPS:-3000}" "${EBS_THROUGHPUT:-125}" >"$work/lt.json"
  cli ec2 create-launch-template --launch-template-name "$template" --launch-template-data "file://$work/lt.json" \
    --tag-specifications "$(tagspec launch-template host)" >/dev/null
  note TEMPLATE "$template"
  python3 "$HERE/template.py" policy "$REGION" "$account" "$RUN" >"$work/machines.json"
  cli iam put-role-policy --role-name "$RUN" --policy-name machines --policy-document "file://$work/machines.json"
fi
ips="$ip0"
i=1
while ((i < HOSTS)); do
  render "$HERE/host.sh.tftpl" "$work/h$i.sh" "${common[@]}" index="$i" host0="$ip0"
  ips="$ips $(launch "h$i" "$HOST_TYPE" "$work/h$i.sh")"
  i=$((i + 1))
done
load_ips=""
i=1
while ((i < LOADERS)); do
  render "$HERE/loadworker.sh.tftpl" "$work/lw$i.sh" "${common[@]}"
  load_ips="$load_ips $(launch "load$i" "$LOADER_TYPE" "$work/lw$i.sh")"
  i=$((i + 1))
done
render "$HERE/loader.sh.tftpl" "$work/loader.sh" "${common[@]}" sizes="$SIZES" items="$ITEMS" mode="$MODE" ips="$ips" load_ips="$load_ips"
launch loader "$LOADER_TYPE" "$work/loader.sh" >/dev/null
echo "==> $((HOSTS + LOADERS)) instances launched; campaign running on the load machine, progress every 60 s"

deadline=$(($(date +%s) + TTL * 60))
until aws --region "$REGION" s3 ls "s3://$bucket/results/DONE" >/dev/null 2>&1; do
  if (($(date +%s) > deadline)); then echo "!! no result before the TTL" >&2; break; fi
  aws --region "$REGION" s3 ls "s3://$bucket/results/" 2>/dev/null | awk '/PRE/ && $2 != "logs/"{print "   done: " $2}' | tail -n 3 || true
  # Follow the run: results/<run>/live.html redraws from the counters every 15 s (one page per campaign: several may run side by side).
  mkdir -p "$RESULTS"
  for _ in 1 2 3 4; do
    if aws --region "$REGION" s3 cp "s3://$bucket/results/live.jsonl" "$RESULTS/live.jsonl" --only-show-errors 2>/dev/null; then
      python3 "$HERE/../live.py" html "$RESULTS/live.jsonl" "$RESULTS/live.html" 2>/dev/null || true
      # The operator's browser opens on the page once (macOS); it reloads itself.
      if [[ -z "${opened:-}" && "${OPEN_LIVE:-1}" == 1 ]] && command -v open >/dev/null; then open "$RESULTS/live.html" || true; opened=1; fi
    fi
    sleep 15
  done
  [[ -f "$RESULTS/live.jsonl" ]] && python3 "$HERE/../live.py" line "$RESULTS/live.jsonl" || true
done
mkdir -p "$RESULTS"
aws --region "$REGION" s3 sync "s3://$bucket/results/" "$RESULTS/" --only-show-errors || true
aws --region "$REGION" s3 cp "s3://$bucket/plan/capacity.json" "$RESULTS/capacity.json" --only-show-errors || true
cleanup

if command -v python3 >/dev/null; then
  echo "==> report"
  python3 "$ROOT/ops/experiment/report.py" "$RESULTS"/*/ | tee "$RESULTS/report.md" || true
fi
echo "results: $RESULTS"
