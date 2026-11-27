#!/usr/bin/env bash
# Bring up a local mesh: issuer (local scale) + bootstrap with autoscale.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"
if in_memory_on; then
  echo "INDEXUS_IN_MEMORY is no longer supported; causal nodes require durable disks" >&2
  exit 2
fi

chmod +x "$SCRIPT_DIR"/*.sh

echo "==> build binaries"
(cd "$ROOT" && go build -o "$BIN_DIR/issuer" ./cmd/issuer)
(cd "$ROOT" && go build -o "$BIN_DIR/node" ./cmd/node)
(cd "$ROOT" && go build -o "$BIN_DIR/provisioner" ./cmd/provisioner)

"$SCRIPT_DIR/mesh_down.sh" >/dev/null 2>&1 || true
rm -rf "$SPAWNED_DIR" "$RUN_DIR/nodes" "$RUN_DIR/logs"
# Drop prior spawned identities so PreferNear names are not overridden by
# sticky certs left from an earlier mesh (empty nodes that never own zones).
rm -f "$KEYS_DIR"/local-*.ed25519 "$KEYS_DIR"/local-*.cert.json
mkdir -p "$SPAWNED_DIR" "$RUN_DIR/nodes" "$RUN_DIR/logs" "$KEYS_DIR"


ensure_p2p_tls_args
if p2p_tls_on; then
  echo "==> P2P TLS/HTTP/2  sslStorage=$TLS_DIR"
else
  echo "==> P2P plain HTTP (set INDEXUS_P2P_TLS=1 for HTTPS/HTTP/2)"
fi

# Cap on concurrent spawned processes (not including bootstrap). Lab default
# 15 — room past items_limit without starving hot nodes at SPAWN_MAX.
SPAWN_MAX="${SPAWN_MAX:-15}"
echo "==> issuer :22000, provisioner :22010"
nohup "$SCRIPT_DIR/detach.sh" -- "$BIN_DIR/issuer" \
 -addr 127.0.0.1:22000 -network "$NETWORK_ID" -key "$KEYS_DIR/issuer.ed25519" \
 -bootstrap 127.0.0.1 >"$RUN_DIR/logs/issuer.log" 2>&1 &
echo $! >"$RUN_DIR/issuer.pid"

for _ in $(seq 1 40); do
  curl -sf "$ISSUER_URL/health" >/dev/null && break
  sleep 0.25
done
curl -sf "$ISSUER_URL/health" >/dev/null

BOOT_DATA="$RUN_DIR/nodes/bootstrap"
mkdir -p "$BOOT_DATA"

nohup "$SCRIPT_DIR/detach.sh" -- "$BIN_DIR/provisioner" \
 -issuer "$ISSUER_URL" -network "$NETWORK_ID" -bootstrap 127.0.0.1 \
 -localDir "$SCRIPT_DIR" -spawnMax "$SPAWN_MAX" >"$RUN_DIR/logs/provisioner.log" 2>&1 &
echo $! >"$RUN_DIR/provisioner.pid"
python3 "$SCRIPT_DIR/configure_node.py" "$BOOT_DATA/node.json" "$BOOT_DATA/backup" "$BOOT_DATA" "" "$BOOT_P2P" "$BOOT_MON" ""
"$BIN_DIR/node" init -config "$BOOT_DATA/node.json"
nohup "$SCRIPT_DIR/detach.sh" -- "$BIN_DIR/node" serve -config "$BOOT_DATA/node.json" \
 >"$RUN_DIR/logs/bootstrap.log" 2>&1 &
echo $! >"$RUN_DIR/bootstrap.pid"

for _ in $(seq 1 40); do
  curl -sf ${CURL_TLS[@]+"${CURL_TLS[@]}"} "${P2P_SCHEME}://127.0.0.1:${BOOT_MON}/count" >/dev/null && break
  sleep 0.25
done

TOKEN=$(curl -sf -X POST "$ISSUER_URL/v1/issue/token" \
  -H 'Content-Type: application/json' \
  -d '{"client_id":"local-mesh","scopes":["read","write"]}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
[[ -n "$TOKEN" ]]

# Opt-in connectivity check. Default off so Ops/Data start empty (no phantom local-smoke).
if [[ "${SMOKE:-0}" == "1" ]]; then
  CODE=$(curl -s ${CURL_TLS[@]+"${CURL_TLS[@]}"} -o /dev/null -w '%{http_code}' -X POST "${P2P_SCHEME}://127.0.0.1:${BOOT_P2P}/item" \
    -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{"item":{"collection":"local-smoke","location":"a","id":"1","metrics":[1,2,3,4,5]},"root":"@","current":"a"}')
  echo "smoke write => $CODE (expect 201)"
  [[ "$CODE" == "201" ]]
else
  echo "smoke write: skipped (set SMOKE=1 to insert local-smoke/id=1)"
fi

echo "issuer:    $ISSUER_URL"
echo "bootstrap: ${P2P_SCHEME}://127.0.0.1:${BOOT_P2P}  mon :${BOOT_MON}"
echo "storage:   $BOOT_DATA/backup.causal.db"
echo "autoscale: $(curl -sf ${CURL_TLS[@]+"${CURL_TLS[@]}"} "${P2P_SCHEME}://127.0.0.1:${BOOT_MON}/autoscale")"
