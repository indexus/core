#!/bin/bash
# Indexus quickstart without Docker (macOS; Linux should work too).
# Needs Go 1.26, Node 22, git, curl. Builds the binaries, fetches the
# sample collections, starts an issuer, one node, the gateway and Explore,
# and stays in the foreground: Ctrl-C stops everything, the data stays.
#
#   bash quickstart-macos.sh
#   open http://127.0.0.1:5174/explore.html
#
# Variables (optional):
#   INDEXUS_HOME   where everything goes (default ~/indexus-quickstart)
#   INDEXUS_SRC    a folder with existing checkouts core/ sdk/ dashboard/ experiences/
#                  (default: $INDEXUS_HOME/src, cloned from github.com/indexus)
#   SAMPLE_URL, SAMPLE_SHA256   the sample archive (build-sample.sh)
#
# The same steps as core scripts/local/explore.sh, without prep-all.sh (no
# Python, no Metal, no 3D scene): the batches come ready from the sample.
# Written for the bash 3.2 of macOS. UNTESTED. See DESIGN.md.
set -euo pipefail

HOME_DIR="${INDEXUS_HOME:-$HOME/indexus-quickstart}"
SRC="${INDEXUS_SRC:-$HOME_DIR/src}"
DATA="$HOME_DIR/data"
BIN="$DATA/bin"
LOGS="$DATA/logs"
SAMPLE_NAME=indexus-sample-explore-v1
SAMPLE_URL="${SAMPLE_URL:-https://github.com/indexus/core/releases/download/sample-explore-v1/$SAMPLE_NAME.tar.xz}"
DEFAULT_SHA256='{{SAMPLE_SHA256}}'   # filled when the asset is published
SAMPLE_SHA256="${SAMPLE_SHA256:-$DEFAULT_SHA256}"
NETWORK=indexus-quickstart
ISSUER_PORT=22030
P2P_PORT=21031
MON_PORT=19031
GATEWAY_PORT=3847
EXPLORE_PORT=5174
ISSUER="http://127.0.0.1:$ISSUER_PORT"
NODE="http://127.0.0.1:$P2P_PORT"

step() { printf '\n== %s\n' "$*"; }
info() { printf '   %s\n' "$*"; }
die() { printf '\nERROR: %s\n' "$*" >&2; exit 1; }
listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
wait_http() { # URL SECONDS WHAT LOG
  local i=0
  while [ $i -lt $(($2 * 4)) ]; do
    curl -fs -o /dev/null --max-time 2 "$1" && return 0
    sleep 0.25
    i=$((i + 1))
  done
  die "$3 does not answer on $1 after $2 s. Log: $4"
}

PIDS=""
stop_all() {
  trap - INT TERM EXIT
  echo
  info "stopping"
  for pid in $PIDS; do kill "$pid" 2>/dev/null || true; done
  for pid in $PIDS; do wait "$pid" 2>/dev/null || true; done
  info "stopped. Data kept in $DATA. Start again: bash $0"
}
trap stop_all INT TERM EXIT

start() { # NAME PORT DIR COMMAND...
  local name=$1 port=$2 dir=$3
  shift 3
  listening "$port" && die "port $port (for $name) is taken. See: lsof -nP -iTCP:$port -sTCP:LISTEN"
  (cd "$dir" && exec "$@" >>"$LOGS/$name.log" 2>&1 </dev/null) &
  PIDS="$! $PIDS"
  info "$name started (pid $!, log $LOGS/$name.log)"
}

step "1. Tools"
command -v go >/dev/null || die "go not found. Install Go 1.26: https://go.dev/dl/"
minor="$(go env GOVERSION | sed -E 's/^go1\.([0-9]+).*/\1/')"
[ "$minor" -ge 26 ] 2>/dev/null || die "$(go env GOVERSION) found; core needs Go 1.26."
command -v node >/dev/null || die "node not found. Install Node 22: https://nodejs.org/"
[ "$(node -p 'process.versions.node.split(".")[0]')" -ge 22 ] || die "node $(node --version) found; Node 22 or later is needed."
for tool in git curl tar shasum; do command -v $tool >/dev/null || die "$tool not found."; done
info "$(go env GOVERSION), node $(node --version)"
mkdir -p "$SRC" "$BIN" "$LOGS" "$DATA/node1" "$DATA/imported"

step "2. Sources ($SRC)"
for repo in core sdk dashboard experiences; do
  if [ -d "$SRC/$repo" ]; then info "$repo: present"; continue; fi
  [ -z "${INDEXUS_SRC:-}" ] || die "$SRC/$repo is missing"
  git clone --depth 1 "https://github.com/indexus/$repo.git" "$SRC/$repo"
done

step "3. Build: node, issuer, localdata ($BIN)"
for target in cmd/node cmd/issuer tools/localdata; do
  name="$(basename "$target")"
  (cd "$SRC/core" && go build -o "$BIN/$name.new" "./$target")
  mv -f "$BIN/$name.new" "$BIN/$name"
done
info "ok"

step "4. JavaScript packages"
if [ ! -d "$SRC/sdk/node_modules" ]; then (cd "$SRC/sdk" && npm ci --no-audit --no-fund); fi
if [ ! -d "$SRC/experiences/node_modules" ]; then (cd "$SRC/experiences" && npm ci --no-audit --no-fund); fi
if [ ! -d "$SRC/dashboard/node_modules" ]; then
  (cd "$SRC/dashboard" && { COREPACK_ENABLE_DOWNLOAD_PROMPT=0 corepack pnpm install --prod --frozen-lockfile ||
    npm install --omit=dev --no-audit --no-fund; })
fi
info "ok"

step "5. Sample collections"
archive="$HOME_DIR/$SAMPLE_NAME.tar.xz"
if [ ! -d "$DATA/sample" ]; then
  if [ ! -f "$archive" ]; then
    curl -fL --retry 3 -o "$archive.part" "$SAMPLE_URL"
    mv -f "$archive.part" "$archive"
  fi
  case "$SAMPLE_SHA256" in
    *SAMPLE_SHA256*|"") info "warning: no SAMPLE_SHA256, the archive is not checked" ;;
    *) echo "$SAMPLE_SHA256  $archive" | shasum -a 256 -c - >/dev/null || die "checksum mismatch: $archive (delete it and run again)" ;;
  esac
  mkdir -p "$DATA/sample.new"
  tar -xJf "$archive" -C "$DATA/sample.new" --strip-components=1
  mv "$DATA/sample.new" "$DATA/sample"
fi
info "$(du -sh "$DATA/sample" | cut -f1) in $DATA/sample"

step "6. Issuer (:$ISSUER_PORT) and node (p2p :$P2P_PORT, monitoring :$MON_PORT)"
start issuer "$ISSUER_PORT" "$DATA" "$BIN/issuer" -addr "127.0.0.1:$ISSUER_PORT" -network "$NETWORK" \
  -key "$DATA/issuer.ed25519" -bootstrap 127.0.0.1 -p2pPort "$P2P_PORT"
wait_http "$ISSUER/health" 30 issuer "$LOGS/issuer.log"
cat >"$DATA/node1/node.json" <<JSON
{
  "storage": {"path": "node", "identity_directory": ".", "omit_history": true},
  "identity": {"name": "0000000000000001", "network": "$NETWORK", "cluster": "$NETWORK", "issuer": "$ISSUER"},
  "network": {"listen": "127.0.0.1:$P2P_PORT", "monitoring": "127.0.0.1:$MON_PORT", "advertise": ["127.0.0.1"]},
  "limits": {"data_bytes": 8589934592, "memory_refuse_pct": 60}
}
JSON
(cd "$DATA/node1" && "$BIN/node" check -config node.json >>"$LOGS/node1.log" 2>&1) || die "node check refused node.json. Log: $LOGS/node1.log"
if [ ! -f "$DATA/node1/node.causal-identity.json" ]; then
  (cd "$DATA/node1" && "$BIN/node" init -config node.json >>"$LOGS/node1.log" 2>&1) || die "node init failed. Log: $LOGS/node1.log"
fi
GOMAXPROCS=4 GOMEMLIMIT=1024MiB start node1 "$P2P_PORT" "$DATA/node1" "$BIN/node" serve -config node.json
wait_http "http://127.0.0.1:$MON_PORT/health" 120 node1 "$LOGS/node1.log"

step "7. Bulk install of the sample"
collections=""
for dir in "$DATA/sample"/*/; do
  dir="${dir%/}"
  [ -f "$dir/manifest.json" ] || continue
  name="$(basename "$dir")"
  collections="$collections\"$name\","
  id="$(sed -n 's/^ *"id" *: *"\([0-9a-f]*\)".*/\1/p' "$dir/manifest.json" | head -1)"
  replace=""
  if [ -f "$DATA/imported/$name" ]; then
    if [ "$(cat "$DATA/imported/$name")" = "$id" ]; then info "$name: already installed"; continue; fi
    replace=-replace
  fi
  info "$name: $(du -sh "$dir" | cut -f1) $replace"
  "$BIN/localdata" -bulk "$dir" -write "$NODE" -issuer "$ISSUER" $replace >>"$LOGS/import.log" 2>&1 ||
    die "install of $name refused. Log: $LOGS/import.log"
  printf '%s\n' "$id" >"$DATA/imported/$name"
done

step "8. Gateway (:$GATEWAY_PORT) and Explore (:$EXPLORE_PORT)"
cat >"$DATA/networks.json" <<JSON
{
  "active": "$NETWORK",
  "networks": [{
    "id": "$NETWORK", "label": "Quickstart", "kind": "local", "project": "$NETWORK",
    "boot_ip": "127.0.0.1", "issuer_url": "$ISSUER", "mon_port": $MON_PORT, "p2p_port": $P2P_PORT,
    "state": "awake", "run_dir": "$DATA",
    "collections": [${collections%,}]
  }]
}
JSON
DEMO_MODE=1 PORT=$GATEWAY_PORT NETWORK_ID=$NETWORK BOOT_IP=127.0.0.1 ISSUER_URL=$ISSUER MON_PORT=$MON_PORT \
  P2P_PORT=$P2P_PORT POLL_MS=5000 INDEXUS_NETWORKS_FILE="$DATA/networks.json" \
  start gateway "$GATEWAY_PORT" "$SRC/dashboard" node server.js
wait_http "http://127.0.0.1:$GATEWAY_PORT/api/health" 30 gateway "$LOGS/gateway.log"
INDEXUS_API_GATEWAY="http://127.0.0.1:$GATEWAY_PORT" \
  start explore "$EXPLORE_PORT" "$SRC/experiences" node node_modules/vite/bin/vite.js \
  --host 127.0.0.1 --port "$EXPLORE_PORT" --strictPort
wait_http "http://127.0.0.1:$EXPLORE_PORT/explore.html" 60 explore "$LOGS/explore.log"

cat <<TEXT

Ready.
  Weather:      http://127.0.0.1:$EXPLORE_PORT/explore.html
  Real estate:  http://127.0.0.1:$EXPLORE_PORT/explore.html?data=realestate
  (WebGPU: a recent Chrome or Edge.)  Logs: $LOGS
  Ctrl-C stops everything; the data stays in $DATA.
TEXT

# Stay in the foreground; stop when a service dies.
while :; do
  for pid in $PIDS; do
    kill -0 "$pid" 2>/dev/null || die "a service stopped (pid $pid). Logs: $LOGS"
  done
  sleep 2
done
