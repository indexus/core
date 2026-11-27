#!/usr/bin/env bash
# Start simulated machine number $1 (boot delay $2, default BOOT_DELAY): one agent with SLOTS node slots. It is
# announced in $DIR/agents (and its slots in targets and monitors) after
# BOOT_DELAY seconds, like a cloud machine that takes time to boot. Settings
# come from $DIR/machine.env, written by elastic_local.sh.
set -euo pipefail
a=$1
source "$INDEXUS_ELASTIC_DIR/machine.env"
sleep "${2:-${BOOT_DELAY:-0}}" # $2 overrides: the first machines are already up
"$BIN/experiment" agent -listen "127.0.0.1:$((22100 + a))" -slots "$SLOTS" -node "$BIN/node" -dir "$DIR/agent$a" \
  -issuer "$ISSUER" -bootstrap "$BOOT" -provisioner "$PROVISIONER" -items-per-vnode "$ITEMS" \
  -retired-exit "$RETIRED_EXIT" -idle-exit "${IDLE_EXIT:-0}s" -write-cap "${WRITE_CAP:-0}" -replication "$REPLICATION" -replication-machine endpoint -p2p-base $((21100 + 100 * a)) -mon-base $((19100 + 100 * a)) >"$DIR/agent$a.log" 2>&1 &
echo $! >"$DIR/agent$a.pid"
for _ in $(seq 1 120); do curl -sf --max-time 1 "http://127.0.0.1:$((22100 + a))/count" >/dev/null && break; sleep 0.25; done
for s in $(seq 0 $((SLOTS - 1))); do
  echo "http://127.0.0.1:$((21100 + 100 * a + s))" >>"$DIR/targets"
  echo "http://127.0.0.1:$((19100 + 100 * a + s))" >>"$DIR/monitors"
done
echo "http://127.0.0.1:$((22100 + a))" >>"$DIR/agents"
echo "machine $a up $(date +%T)" >>"$DIR/machines.log"
