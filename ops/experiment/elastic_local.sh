#!/usr/bin/env bash
# Elastic cluster on loopback: genesis + agents (simulated machines) with SLOTS
# node slots each. Nodes scale up through the provisioner when a vnode owns
# ITEMS rows, and fold back when cool (COLD-2); a retired vnode's process exits
# and frees its slot. With MACHINES_MAX above AGENTS, machines start on demand:
# one boots (after BOOT_DELAY seconds) when fewer than SPARE slots stay free.
#
#   BIN=dir AGENTS=2 SLOTS=8 START=4 ITEMS=20000 DIR=/tmp/el ops/experiment/elastic_local.sh up
#   AGENTS=1 MACHINES_MAX=6 SPARE=2 BOOT_DELAY=20 ... elastic_local.sh up
#   DIR=/tmp/el ops/experiment/elastic_local.sh down
set -euo pipefail
cmd="${1:-up}"
HERE="$(cd "$(dirname "$0")" && pwd)"
DIR="${DIR:-/tmp/indexus-elastic}"
BIN="${BIN:-$(pwd)/bin}"
AGENTS="${AGENTS:-2}"
SLOTS="${SLOTS:-8}"
START="${START:-4}"
ITEMS="${ITEMS:-20000}"
RETIRED_EXIT="${RETIRED_EXIT:-60}"
REPLICATION="${REPLICATION:-}" # "" off, sync or ack; each local process counts as a machine
MACHINES_MAX="${MACHINES_MAX:-$AGENTS}"
SPARE="${SPARE:-0}"
BOOT_DELAY="${BOOT_DELAY:-0}"
IDLE_EXIT="${IDLE_EXIT:-0}" # seconds a machine without live vnodes waits before it is released
WRITE_CAP="${WRITE_CAP:-0}" # writes/s each vnode applies (CAP-2); scale-up at 70% of it
ISSUER="http://127.0.0.1:22000"
PROVISIONER="http://127.0.0.1:22010"
BOOT="http://127.0.0.1:21000"

down() {
  for pid in "$DIR"/*.pid; do [[ -f "$pid" ]] && kill "$(cat "$pid")" 2>/dev/null || true; done
  sleep 2
  pkill -f "elastic/machine.sh" 2>/dev/null || true # machines still booting
  pkill -f "$DIR/" 2>/dev/null || true # nodes started by the agents
  rm -f "$DIR"/*.pid
}

wait_url() { for _ in $(seq 1 120); do curl -sf --max-time 1 "$1" >/dev/null && return 0; sleep 0.25; done; echo "timeout $1" >&2; return 1; }

up() {
  down
  rm -rf "$DIR" && mkdir -p "$DIR/n0"
  if curl -s --max-time 1 -o /dev/null http://127.0.0.1:22000/; then echo "port 22000 busy: stop the other cluster" >&2; exit 1; fi
  "$BIN/issuer" -addr 127.0.0.1:22000 -network experiment -key "$DIR/issuer.ed25519" -bootstrap 127.0.0.1 >"$DIR/issuer.log" 2>&1 &
  echo $! >"$DIR/issuer.pid"
  wait_url "$ISSUER/health"
  cat >"$DIR/machine.env" <<ENV
BIN="$BIN" DIR="$DIR" SLOTS=$SLOTS ITEMS=$ITEMS RETIRED_EXIT=$RETIRED_EXIT REPLICATION="$REPLICATION"
ISSUER="$ISSUER" BOOT="$BOOT" PROVISIONER="$PROVISIONER"
MACHINES_MAX=$MACHINES_MAX SPARE=$SPARE BOOT_DELAY=$BOOT_DELAY IDLE_EXIT=$IDLE_EXIT WRITE_CAP=$WRITE_CAP
ENV
  touch "$DIR/agents"
  INDEXUS_ELASTIC_DIR="$DIR" "$BIN/provisioner" -addr 127.0.0.1:22010 -issuer "$ISSUER" -network experiment -bootstrap 127.0.0.1 \
    -localDir "$HERE/elastic" -spawnMax $((MACHINES_MAX * SLOTS)) -requesterCooldown 5s >"$DIR/provisioner.log" 2>&1 &
  echo $! >"$DIR/provisioner.pid"
  python3 - "$DIR/n0" "$ITEMS" "$REPLICATION" "$WRITE_CAP" <<'PY'
import json, sys
d, items, replication, cap = sys.argv[1], int(sys.argv[2]), sys.argv[3], float(sys.argv[4])
c = dict(storage=dict(path=d + "/backup", identity_directory=d),
         identity=dict(network="experiment", cluster="experiment", issuer="http://127.0.0.1:22000"),
         network=dict(listen="127.0.0.1:21000", monitoring="127.0.0.1:19000", advertise=["127.0.0.1"]),
         limits=dict(data_bytes=1 << 30, writes_per_second=cap), compaction=True, provisioner="http://127.0.0.1:22010",
         scaling=dict(items_per_vnode=items, resource_signals=False))
if replication:
    c["replication"] = replication
    c["replication_machine"] = "endpoint"
open(d + "/node.json", "w").write(json.dumps(c, indent=2))
PY
  "$BIN/node" init -config "$DIR/n0/node.json" >"$DIR/n0.log" 2>&1
  "$BIN/node" serve -config "$DIR/n0/node.json" >>"$DIR/n0.log" 2>&1 &
  echo $! >"$DIR/n0.pid"
  wait_url "http://127.0.0.1:19000/health"
  echo "$BOOT" >"$DIR/targets"; echo "http://127.0.0.1:19000" >"$DIR/monitors"
  for a in $(seq 0 $((AGENTS - 1))); do
    INDEXUS_ELASTIC_DIR="$DIR" "$HERE/elastic/machine.sh" "$a" 0
  done
  for i in $(seq 1 $((START - 1))); do
    curl -sf -X POST "http://127.0.0.1:$((22100 + i % AGENTS))/spawn" >/dev/null
  done
  # The spare machine boots now, not at the first scale-up.
  if (( AGENTS * SLOTS - START < SPARE && AGENTS < MACHINES_MAX )) && mkdir "$DIR/booting" 2>/dev/null; then
    date +%s >"$DIR/booting/at"
    ( INDEXUS_ELASTIC_DIR="$DIR" "$HERE/elastic/machine.sh" "$AGENTS"; rm -rf "$DIR/booting" ) >/dev/null 2>&1 &
  fi
  echo "elastic cluster: $START nodes, $((AGENTS * SLOTS)) free slots across $AGENTS agents (up to $MACHINES_MAX, spare $SPARE, boot ${BOOT_DELAY}s), split at $ITEMS items"
}

case "$cmd" in
  up) up ;;
  down) down ;;
  *) echo "usage: $0 up|down" >&2; exit 2 ;;
esac
