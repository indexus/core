#!/usr/bin/env bash
# Shared paths for the local mesh (issuer + bootstrap + spawned processes).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LOCAL_DIR="${INDEXUS_LOCAL_DIR:-$ROOT/ops/local}"
RUN_DIR="${INDEXUS_RUN_DIR:-$ROOT/.local/data}"
SPAWNED_DIR="$RUN_DIR/spawned"
KEYS_DIR="$RUN_DIR/keys"
BIN_DIR="$ROOT/.local/bin"

ISSUER_URL="${ISSUER_URL:-http://127.0.0.1:22000}"
BOOT_P2P="${BOOT_P2P:-21000}"
BOOT_MON="${BOOT_MON:-19000}"
# Spawned node local-N binds BOOT_P2P+SPAWN_PORT_OFFSET+N (and the same on the
# monitoring base). spawn.sh allocates from it and mesh_down.sh sweeps up to it,
# so the two cannot drift apart as SPAWN_MAX changes.
SPAWN_PORT_OFFSET="${SPAWN_PORT_OFFSET:-10}"
NETWORK_ID="${NETWORK_ID:-indexus-aws}"
# Only operational lab settings are sourced here; the runtime reads node.json.
MESH_CONFIG_FILE="${MESH_CONFIG:-$RUN_DIR/mesh-config.env}"
if [[ -f "$MESH_CONFIG_FILE" ]]; then
 set -a
 source "$MESH_CONFIG_FILE"
 set +a
fi
export NETWORK_ID ISSUER_URL
# Recognize the removed RAM-only mode so launch scripts can reject it early.
in_memory_on() {
  case "${INDEXUS_IN_MEMORY:-0}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}
mkdir -p "$SPAWNED_DIR" "$KEYS_DIR" "$BIN_DIR" "$RUN_DIR/logs"

# Lab TLS for P2P+monitoring (Go ServeTLS → HTTP/2). Off by default so an
# already-running http mesh keeps answering; set INDEXUS_P2P_TLS=1 to enable.
p2p_tls_on() {
  case "${INDEXUS_P2P_TLS:-0}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

# Populate TLS_ARGS=(-sslStorage DIR) when TLS is on; empty otherwise.
# Echoes nothing; sets TLS_DIR / TLS_ARGS / P2P_SCHEME in the caller.
ensure_p2p_tls_args() {
  # Always define arrays so `set -u` callers can expand "${TLS_ARGS[@]}" safely.
  TLS_ARGS=()
  CURL_TLS=()
  TLS_DIR="${INDEXUS_TLS_DIR:-$RUN_DIR/tls}"
  P2P_SCHEME=http
  if p2p_tls_on; then
    TLS_DIR="$("$LOCAL_DIR/ensure_tls.sh")"
    TLS_ARGS=(-sslStorage "$TLS_DIR")
    P2P_SCHEME=https
    CURL_TLS=(-k)
    export INDEXUS_TLS_DIR="$TLS_DIR"
  fi
  export P2P_SCHEME
  # bash 3.2 + set -u: empty array expand still errors unless set +u around use,
  # so export a single optional flag string for scripts that prefer it.
  export INDEXUS_SSL_STORAGE_ARGS="${TLS_ARGS[*]-}"
}
