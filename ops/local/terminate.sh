#!/usr/bin/env bash
# Stop a local process. Durable vnode data remains on disk for explicit restart.
# This does not drain a vnode or establish that a physical host is safe to remove.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"

ID="${1:-}"
if [[ -z "$ID" || "$ID" != local-* ]]; then
  echo "usage: terminate.sh local-N" >&2
  exit 2
fi

META="$SPAWNED_DIR/$ID.json"
if [[ ! -f "$META" ]]; then
  echo "unknown instance $ID" >&2
  exit 1
fi

PID=$(python3 -c "import json; print(json.load(open('$META'))['pid'])")

if kill -0 "$PID" 2>/dev/null; then
  kill "$PID" 2>/dev/null || true
  for _ in $(seq 1 20); do
    kill -0 "$PID" 2>/dev/null || break
    sleep 0.2
  done
  kill -9 "$PID" 2>/dev/null || true
fi

rm -f "$META"
echo "terminated $ID"
