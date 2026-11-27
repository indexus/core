#!/usr/bin/env bash
# Start a fixed-size cluster of real node processes on loopback.
#
#   BIN=dir N=8 DIR=/tmp/exp ops/experiment/cluster_local.sh up
#   DIR=/tmp/exp ops/experiment/cluster_local.sh down
#
# Node i listens on 21000+i (p2p) and 19000+i (monitoring); the issuer on 22000.
# COMPACTION=false keeps the cluster at N vnodes (scaling runs); the default
# lets cool vnodes fold (COLD-2).
# DIR/targets and DIR/monitors list the URLs for cmd/experiment.
set -euo pipefail
cmd="${1:-up}"
N="${N:-8}"
DIR="${DIR:-/tmp/indexus-experiment}"
BIN="${BIN:-$(pwd)/bin}"
DATA_BYTES="${DATA_BYTES:-$((1 << 30))}"
NETWORK_ID="${NETWORK_ID:-experiment}"
ISSUER="http://127.0.0.1:22000"

down() {
  for pid in "$DIR"/*.pid; do
    [[ -f "$pid" ]] || continue
    kill "$(cat "$pid")" 2>/dev/null || true
  done
  sleep 1
  rm -f "$DIR"/*.pid
}

config() { # index bootstrap-url
  local i=$1 boot=$2
  python3 - "$DIR/n$i/node.json" "$DIR/n$i" "$((21000 + i))" "$((19000 + i))" "$boot" <<EOF
import json, sys
out, data, p2p, mon, boot = sys.argv[1:]
c = dict(storage=dict(path=data + "/backup", identity_directory=data),
         identity=dict(network="$NETWORK_ID", cluster="experiment", issuer="$ISSUER"),
         network=dict(listen=f"127.0.0.1:{p2p}", monitoring=f"127.0.0.1:{mon}", advertise=["127.0.0.1"]),
         limits=dict(data_bytes=$DATA_BYTES),
         compaction="${COMPACTION:-true}" == "true")
if boot: c["network"]["bootstrap"] = [boot]
open(out, "w").write(json.dumps(c, indent=2))
EOF
}

wait_url() {
  for _ in $(seq 1 120); do curl -sf --max-time 1 "$1" >/dev/null && return 0; sleep 0.25; done
  echo "timeout waiting for $1" >&2; return 1
}

free() { # refuse a port another process already answers on
  if curl -s --max-time 1 -o /dev/null "http://127.0.0.1:$1/"; then
    echo "port $1 is already in use: stop the other cluster first" >&2; exit 1
  fi
}

started() { # pid url: the process we launched must be the one answering
  wait_url "$2"
  kill -0 "$1" 2>/dev/null || { echo "process $1 died; see logs in $DIR" >&2; exit 1; }
}

up() {
  down
  free 22000
  for i in $(seq 0 $((N - 1))); do free $((21000 + i)); free $((19000 + i)); done
  rm -rf "$DIR" && mkdir -p "$DIR"
  "$BIN/issuer" -addr 127.0.0.1:22000 -network "$NETWORK_ID" -key "$DIR/issuer.ed25519" -bootstrap 127.0.0.1 >"$DIR/issuer.log" 2>&1 &
  echo $! >"$DIR/issuer.pid"
  wait_url "$ISSUER/health"
  : >"$DIR/targets"; : >"$DIR/monitors"
  for i in $(seq 0 $((N - 1))); do
    mkdir -p "$DIR/n$i"
    if [[ $i == 0 ]]; then
      config 0 ""
      "$BIN/node" init -config "$DIR/n0/node.json" >"$DIR/n0.log" 2>&1
    else
      config "$i" "http://127.0.0.1:21000"
    fi
    "$BIN/node" serve -config "$DIR/n$i/node.json" >>"$DIR/n$i.log" 2>&1 &
    echo $! >"$DIR/n$i.pid"
    started "$(cat "$DIR/n$i.pid")" "http://127.0.0.1:$((19000 + i))/health"
    # The next joiner asks for a JOIN-1 hint: let this one become active first.
    for _ in $(seq 1 240); do
      curl -sf --max-time 1 "http://127.0.0.1:$((19000 + i))/status" | grep -q '"client_ready":true' && break
      sleep 0.25
    done
    echo "http://127.0.0.1:$((21000 + i))" >>"$DIR/targets"
    echo "http://127.0.0.1:$((19000 + i))" >>"$DIR/monitors"
  done
  echo "cluster of $N nodes up in $DIR"
}

case "$cmd" in
  up) up ;;
  down) down ;;
  *) echo "usage: $0 up|down" >&2; exit 2 ;;
esac
