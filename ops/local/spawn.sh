#!/usr/bin/env bash
# Start one local spawned node. Prints the instance id (local-<n>) on stdout.
# Invoked by the issuer when -localDir / LaunchTemplate=local is set.
# Node name comes from cmd/node (RandomName / INDEXUS_PREFER_NEAR) — do not pass -name.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"
if in_memory_on; then
  echo "INDEXUS_IN_MEMORY is no longer supported; causal nodes require durable disks" >&2
  exit 2
fi


BOOTSTRAP_HOST="${INDEXUS_BOOTSTRAP_IPS%%,*}"
BOOTSTRAP_HOST="${BOOTSTRAP_HOST:-127.0.0.1}"
BOOTSTRAP="${BOOTSTRAP_HOST}|${BOOT_P2P}"

# Allocate the next free slot.
SLOT=0
while [[ -f "$SPAWNED_DIR/local-$SLOT.json" || -d "$RUN_DIR/nodes/local-$SLOT" ]]; do
  SLOT=$((SLOT + 1))
done
ID="local-$SLOT"
P2P=$((BOOT_P2P + SPAWN_PORT_OFFSET + SLOT))
MON=$((BOOT_MON + SPAWN_PORT_OFFSET + SLOT))
DATA="$RUN_DIR/nodes/$ID"
mkdir -p "$DATA"

if [[ ! -x "$BIN_DIR/node" ]]; then
  echo "missing $BIN_DIR/node — run ops/local/mesh_up.sh first" >&2
  exit 1
fi

# Always drop sticky identity for this slot. cmd/node stickyNodeName prefers
# an existing .cert.json over -name / PreferNear — leftover local-N keys from a
# prior mesh leave the peer with 0 zones and client_ready=false forever.
rm -f "$KEYS_DIR/${ID}.ed25519" "$KEYS_DIR/${ID}.cert.json"

LOG="$RUN_DIR/logs/$ID.log"

ensure_p2p_tls_args
python3 "$SCRIPT_DIR/configure_node.py" "$DATA/node.json" "$DATA/backup" "$DATA" "${INDEXUS_PREFER_NEAR:-}" "$P2P" "$MON" "${P2P_SCHEME}://${BOOTSTRAP_HOST}:${BOOT_P2P}"
nohup "$SCRIPT_DIR/detach.sh" -- "$BIN_DIR/node" serve -config "$DATA/node.json" >"$LOG" 2>&1 &
PID=$!

# Wait until monitoring answers (or die early).
NAME=""
for _ in $(seq 1 40); do
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "spawned process died; see $LOG" >&2
    tail -n 40 "$LOG" >&2 || true
    rm -f "$SPAWNED_DIR/$ID.json"
    exit 1
  fi
  if curl -sf ${CURL_TLS[@]+"${CURL_TLS[@]}"} --max-time 1 "${P2P_SCHEME}://127.0.0.1:${MON}/count" >/dev/null; then
    NAME=$(curl -sf ${CURL_TLS[@]+"${CURL_TLS[@]}"} --max-time 1 "${P2P_SCHEME}://127.0.0.1:${MON}/status" \
      | python3 -c 'import sys,json; print(json.load(sys.stdin).get("name",""))' 2>/dev/null || true)
    break
  fi
  sleep 0.25
done

python3 - <<PY
import json, pathlib
path = pathlib.Path("$SPAWNED_DIR") / "$ID.json"
path.write_text(json.dumps({
  "id": "$ID",
  "pid": $PID,
  "name": "$NAME",
  "p2p": $P2P,
  "mon": $MON,
  "role": "spawned",
  "log": "$LOG",
}, indent=2))
PY

echo "$ID"
