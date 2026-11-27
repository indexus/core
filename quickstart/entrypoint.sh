#!/usr/bin/env bash
# Indexus quickstart, inside the container: the steps of core
# scripts/local/explore.sh (steps 3 to 5) without building anything.
#
#   1. issuer (22030)
#   2. node: check, init the first time, serve (p2p 21031, monitoring 19031)
#   3. bulk install of every sample batch in /opt/indexus/sample (localdata -bulk)
#   4. gateway (dashboard/server.js, 3847) serving the built Explore
#   5. forwarder 0.0.0.0:5174 -> 127.0.0.1:3847 (the gateway listens on loopback only)
#
# State in /data (a volume): issuer key, node store, installed batch ids, logs.
# A restart keeps the data and skips the batches already installed.
#
# The Go executable is indexus-node: an amd64 executable named node receives
# extra V8 flags under Docker Desktop emulation and fails at flag parsing.
# Startup/restart validation is recorded in README.md.
set -euo pipefail

ROOT="${INDEXUS_ROOT:-/opt/indexus}"
BIN="$ROOT/bin"
SAMPLE="${INDEXUS_SAMPLE:-$ROOT/sample}"
DATA="${INDEXUS_DATA:-/data}"
NETWORK="${INDEXUS_NETWORK:-indexus-quickstart}"
ISSUER_PORT=22030
P2P_PORT=21031
MON_PORT=19031
GATEWAY_PORT=3847
PUBLIC_PORT="${INDEXUS_PUBLIC_PORT:-5174}"
ISSUER="http://127.0.0.1:$ISSUER_PORT"
NODE="http://127.0.0.1:$P2P_PORT"

say() { printf '[quickstart] %s\n' "$*"; }
die() { printf '[quickstart] ERROR: %s\n' "$*" >&2; exit 1; }

pids=()
stop_all() {
  trap - TERM INT
  say "stopping"
  # reverse order: forwarder, gateway, node, issuer
  for ((i = ${#pids[@]} - 1; i >= 0; i--)); do kill -TERM "${pids[i]}" 2>/dev/null || true; done
  wait || true
}
trap 'stop_all; exit 0' TERM INT

# run NAME COMMAND... : start in the background, each line of output prefixed.
run() {
  local name=$1
  shift
  "$@" > >(sed -u "s/^/[$name] /") 2>&1 &
  pids+=("$!")
}

wait_http() { # URL SECONDS WHAT
  local i
  for ((i = 0; i < $2 * 4; i++)); do
    curl -fs -o /dev/null --max-time 2 "$1" && return 0
    sleep 0.25
  done
  die "$3 does not answer on $1 after $2 s"
}

mkdir -p "$DATA/node1" "$DATA/imported"
rm -f "$DATA/ready"

say "1. issuer ($NETWORK, :$ISSUER_PORT)"
run issuer "$BIN/issuer" -addr "127.0.0.1:$ISSUER_PORT" -network "$NETWORK" \
  -key "$DATA/issuer.ed25519" -bootstrap 127.0.0.1 -p2pPort "$P2P_PORT"
wait_http "$ISSUER/health" 30 issuer

say "2. restoring local vnodes (fresh volumes start with one)"
run vnodes node "$ROOT/vnodes.mjs"
socket="${INDEXUS_SLOT_SOCKET:-/tmp/indexus-vnodes.sock}"
for ((i=0; i<1200; i++)); do
  if NODE="$(curl -fs --unix-socket "$socket" http://localhost/endpoints)" && [ -n "$NODE" ]; then break; fi
  kill -0 "${pids[-1]}" 2>/dev/null || die "vnode supervisor exited"
  sleep .25
done
[ -n "$NODE" ] || die "vnodes did not start"
cd "$ROOT"

say "3. sample collections ($SAMPLE)"
collections=()
for dir in "$SAMPLE"/*/; do
  dir="${dir%/}"
  [ -f "$dir/manifest.json" ] || continue
  name="$(basename "$dir")"
  collections+=("$name")
  id="$(sed -n 's/^ *"id" *: *"\([0-9a-f]*\)".*/\1/p' "$dir/manifest.json" | head -1)"
  mode=()
  if [ -f "$DATA/imported/$name" ]; then
    if [ "$(cat "$DATA/imported/$name")" = "$id" ]; then say "   $name: already installed"; continue; fi
    mode=(-replace)
  fi
  say "   $name: $(du -sh "$dir" | cut -f1) ${mode[*]:-}"
  stage="$(mktemp -d "$DATA/import.XXXXXX")"
  cp -R "$dir" "$stage/batch"
  if ! "$BIN/localdata" -bulk "$stage/batch" -write "$NODE" -issuer "$ISSUER" ${mode[@]+"${mode[@]}"} 2>&1 |
    sed -u 's/^/[import] /'; then die "install of $name refused"; fi
  rm -rf "$stage"
  printf '%s\n' "$id" >"$DATA/imported/$name"
done
[ ${#collections[@]} -gt 0 ] || die "no batch in $SAMPLE"

# Bulk partitions its bootstrap files over a stable owner list. Let live writes
# ask for new vnodes only after those initial files have been installed.
if (( ${INDEXUS_MAX_VNODES:-1} > 1 )); then
  run provisioner "$BIN/provisioner" -addr 127.0.0.1:22010 -issuer "$ISSUER" -network "$NETWORK" \
    -localDir "$ROOT/hooks" -spawnMax "$((INDEXUS_MAX_VNODES - 1))"
fi

say "4. gateway (:$GATEWAY_PORT, loopback) and Explore"
list="$(printf '"%s",' "${collections[@]}")"
cat >"$DATA/networks.json" <<JSON
{
  "active": "$NETWORK",
  "networks": [{
    "id": "$NETWORK", "label": "Quickstart", "kind": "local", "project": "$NETWORK",
    "boot_ip": "127.0.0.1", "issuer_url": "$ISSUER", "mon_port": $MON_PORT, "p2p_port": $P2P_PORT,
    "state": "awake", "run_dir": "$DATA",
    "collections": [${list%,}]
  }]
}
JSON
cd "$ROOT/dashboard"
NODE_ENV=production DEMO_MODE="${DEMO_MODE:-1}" PORT=$GATEWAY_PORT NETWORK_ID="$NETWORK" \
  BOOT_IP=127.0.0.1 ISSUER_URL="$ISSUER" MON_PORT=$MON_PORT P2P_PORT=$P2P_PORT POLL_MS=5000 \
  INDEXUS_NETWORKS_FILE="$DATA/networks.json" INDEXUS_CORE_ROOT="$DATA/core" \
  run gateway node server.js
cd "$ROOT"
wait_http "http://127.0.0.1:$GATEWAY_PORT/api/health" 30 gateway

say "5. forwarder 0.0.0.0:$PUBLIC_PORT -> 127.0.0.1:$GATEWAY_PORT"
run forward node "$ROOT/forward.mjs" "0.0.0.0:$PUBLIC_PORT" "127.0.0.1:$GATEWAY_PORT"
wait_http "http://127.0.0.1:$PUBLIC_PORT/api/health" 10 forwarder
touch "$DATA/ready"

cat <<TEXT
[quickstart] Ready.
[quickstart]   Open: http://localhost:${INDEXUS_PORT:-$PUBLIC_PORT}/
[quickstart]   (Explore draws with WebGPU: a recent Chrome or Edge.)
TEXT

# One service down stops the container: Docker reports it, a restart resumes.
set +e
wait -n "${pids[@]}"
code=$?
say "a service stopped (exit $code)"
stop_all
exit "$code"
