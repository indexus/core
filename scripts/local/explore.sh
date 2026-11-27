#!/usr/bin/env bash
# Explore de bout en bout : Anglet (3D ; la commune entière une fois
# scripts/local/prep-commune.sh passé), météo et immobilier (DVF 2020)
# sur un nœud Indexus local, la gateway du dashboard et Explore.
#
#   scripts/local/explore.sh         lance tout, ou reprend ce qui manque
#   scripts/local/explore.sh stop    arrête tout ; les données restent
#
# Disposition attendue (voir docs/local-experiences.md) :
#   indexus/{core,sdk-js,dashboard,experiences}           dépôts de travail
#   indexus/_run/{core,sdk-js,dashboard,experiences}      worktrees lancés ici
#   indexus/_run/.data                                 binaires, nœud, batches, journaux
#
# Variables (facultatives) :
#   EXPLORE_ORIGIN   dossier des dépôts de travail (défaut : parent de _run)
#   EXPLORE_DATA     état du montage (défaut : _run/.data)
#   INDEXUS_GO_ENV   fichier à sourcer avant go (une chaîne d'outils sans Go 1.26)
set -euo pipefail

CORE="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUN="$(dirname "$CORE")"
ORIGIN="${EXPLORE_ORIGIN:-$(dirname "$RUN")}"
DATA="${EXPLORE_DATA:-$RUN/.data}"
BIN="$DATA/bin"
LOGS="$DATA/logs"
PIDS="$DATA/pids"

NETWORK=indexus-explore-local
ISSUER_PORT=22030
P2P_PORTS="21031"   # un seul nœud ; monitoring = p2p - 2000
GATEWAY_PORT=3847
EXPLORE_PORT=5174
ISSUER="http://127.0.0.1:$ISSUER_PORT"
ENDPOINTS="http://127.0.0.1:21031"

# Ce qu'Explore lit : une batch par collection dans $DATA/batches, faite par
# scripts/local/prep-all.sh (docs/ingest/pipeline.md) et installée d'un coup
# par localdata -bulk ; la végétation en dernier, Explore déjà ouvert.
BATCHES="$DATA/batches"

step() { printf '\n%s\n' "== $*"; }
info() { printf '   %s\n' "$*"; }
die() {
  printf '\nERREUR : %s\n' "$1" >&2
  shift
  for line in "$@"; do printf '         %s\n' "$line" >&2; done
  exit 1
}

alive() {
  local state
  [ -n "${1:-}" ] && kill -0 "$1" 2>/dev/null || return 1
  # A detached child can stay a zombie until reaped: it is not running.
  state="$(ps -o stat= -p "$1" 2>/dev/null || true)"
  [ -n "$state" ] && [ "${state#Z}" = "$state" ]
}

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

wait_http() { # URL SECONDES QUOI JOURNAL
  local i
  for i in $(seq 1 $(($2 * 4))); do
    curl -sf -o /dev/null --max-time 2 "$1" && return 0
    sleep 0.25
  done
  die "$3 ne répond pas sur $1 après $2 s." "Journal : $4"
}

# Run node on a JSON document: json FILE 'expression on d'.
json() { node -e "const d=JSON.parse(require('fs').readFileSync(process.argv[1],'utf8'));$2" "$1"; }

start() { # NOM PORT DOSSIER COMMANDE...
  local name=$1 port=$2 dir=$3 pid
  shift 3
  pid="$(cat "$PIDS/$name.pid" 2>/dev/null || true)"
  if alive "$pid"; then
    info "$name tourne déjà (pid $pid)"
    return
  fi
  if listening "$port"; then
    die "le port $port, prévu pour $name, est occupé par un autre processus." \
      "Voir : lsof -nP -iTCP:$port -sTCP:LISTEN" "Rien n'a été arrêté."
  fi
  # Job control gives the service its own process group: a Ctrl-C in this
  # terminal stops the script, not the services it started.
  set -m
  (cd "$dir" && exec nohup "$@" >>"$LOGS/$name.log" 2>&1 </dev/null) &
  pid=$!
  set +m
  echo "$pid" >"$PIDS/$name.pid"
  info "$name lancé (pid $pid, journal $LOGS/$name.log)"
}

stop_pid() { # NOM PID
  local i
  alive "$2" || return 0
  kill "$2" 2>/dev/null || true
  for i in $(seq 1 120); do
    alive "$2" || { info "$1 arrêté (pid $2)"; return 0; }
    sleep 0.25
  done
  kill -9 "$2" 2>/dev/null || true
  info "$1 tué après 30 s (pid $2)"
}

stop_all() {
  local name
  for name in experiences gateway node1 issuer; do
    stop_pid "$name" "$(cat "$PIDS/$name.pid" 2>/dev/null || true)"
    rm -f "$PIDS/$name.pid"
  done
}

if [ "${1:-}" = stop ]; then
  step "Arrêt du montage Explore ($DATA)"
  stop_all
  info "Données conservées. Relancer : $0"
  exit 0
fi
[ $# -eq 0 ] || die "usage : $0 [stop]"

step "1. Outils"
if [ -n "${INDEXUS_GO_ENV:-}" ]; then
  # shellcheck disable=SC1090
  source "$INDEXUS_GO_ENV"
fi
command -v go >/dev/null || die "go introuvable. Installez Go 1.26 (https://go.dev/dl/)."
go_version="$(go env GOVERSION)"
go_minor="$(printf '%s' "$go_version" | sed -E 's/^go1\.([0-9]+).*/\1/')"
if [ -z "${INDEXUS_GO_ENV:-}" ] && [ "$go_minor" -lt 26 ] 2>/dev/null; then
  die "$go_version trouvé ; core exige Go 1.26 (go.mod)." "Installez-le depuis https://go.dev/dl/."
fi
command -v node >/dev/null || die "node introuvable (Node 22 ou plus)."
command -v curl >/dev/null || die "curl introuvable."
info "$go_version, node $(node --version)"
sdk=sdk-js
[ -d "$RUN/$sdk" ] || sdk=sdk
for repo in core "$sdk" dashboard experiences; do
  [ -d "$RUN/$repo/.git" ] || [ -f "$RUN/$repo/.git" ] || die "worktree absent : $RUN/$repo"
  [ "$repo" = core ] && continue
  # The worktrees reuse the installed packages of the working copies.
  if [ ! -e "$RUN/$repo/node_modules" ]; then
    [ -d "$ORIGIN/$repo/node_modules" ] || die "$ORIGIN/$repo/node_modules absent." "Lancez npm install (ou pnpm install) dans $ORIGIN/$repo."
    if [ -z "${EXPLORE_ORIGIN:-}" ]; then
      ln -s "../../$repo/node_modules" "$RUN/$repo/node_modules"
    else
      ln -s "$ORIGIN/$repo/node_modules" "$RUN/$repo/node_modules"
    fi
    info "$repo/node_modules -> $(readlink "$RUN/$repo/node_modules")"
  fi
  # Local settings stay in the working copy (the basemap key in .env.local).
  if [ -f "$ORIGIN/$repo/.env.local" ] && [ ! -e "$RUN/$repo/.env.local" ]; then
    ln -s "$ORIGIN/$repo/.env.local" "$RUN/$repo/.env.local"
    info "$repo/.env.local -> $ORIGIN/$repo/.env.local"
  fi
done
mkdir -p "$BIN" "$LOGS" "$PIDS"

step "2. Construction de node, issuer et localdata ($BIN)"
for command in node issuer; do
  # A binary staged beside (node.prebuilt) wins: it carries fixes not yet here.
  if [ -x "$BIN/$command.prebuilt" ]; then cp -f "$BIN/$command.prebuilt" "$BIN/$command"; continue; fi
  # Build beside, then rename: a running binary is never rewritten in place.
  (cd "$CORE" && go build -o "$BIN/$command.new" "./cmd/$command")
  mv -f "$BIN/$command.new" "$BIN/$command"
done
(cd "$CORE" && go build -o "$BIN/localdata.new" ./tools/localdata)
mv -f "$BIN/localdata.new" "$BIN/localdata"
info "ok"

step "3. Nœud ($NETWORK)"
start issuer "$ISSUER_PORT" "$DATA" "$BIN/issuer" -addr "127.0.0.1:$ISSUER_PORT" -network "$NETWORK" \
  -key "$DATA/issuer.ed25519" -bootstrap 127.0.0.1 -p2pPort 21031
wait_http "$ISSUER/health" 20 issuer "$LOGS/issuer.log"
dir="$DATA/node1"
mkdir -p "$dir"
[ -f "$DATA/names" ] || printf '%s\n' 0000000000000001 >"$DATA/names"
cat >"$dir/node.json" <<JSON
{
  "storage": {"path": "node", "identity_directory": ".", "omit_history": true},
  "identity": {"name": "$(head -n 1 "$DATA/names")", "network": "$NETWORK", "cluster": "$NETWORK", "issuer": "$ISSUER"},
  "network": {"listen": "127.0.0.1:21031", "monitoring": "127.0.0.1:19031", "advertise": ["127.0.0.1"]},
  "limits": {"data_bytes": 34359738368, "memory_refuse_pct": 60}
}
JSON
# node init pins the store identity once the cluster genesis is written.
if [ ! -f "$dir/node.causal-identity.json" ]; then
  "$BIN/node" init -config "$dir/node.json" >>"$LOGS/node1.log" 2>&1 || die "init du nœud impossible." "Journal : $LOGS/node1.log"
fi
# One node, bounded: four cores and 1 GiB at most.
GOMAXPROCS=4 GOMEMLIMIT=1024MiB start node1 21031 "$dir" "$BIN/node" serve -config "$dir/node.json"
wait_http "http://127.0.0.1:19031/health" 120 node1 "$LOGS/node1.log"
info "nœud prêt"

# import_batch installs one batch; $DATA/imported/<collection> keeps its id, so
# an unchanged batch is skipped and a changed one replaces the collection.
import_batch() {
  local dir="$1" name id mode=()
  name="$(basename "$dir")"
  id="$(sed -n 's/^ *"id" *: *"\([0-9a-f]*\)".*/\1/p' "$dir/manifest.json" | head -1)"
  if [ -f "$DATA/imported/$name" ]; then
    if [ "$(cat "$DATA/imported/$name")" = "$id" ]; then info "$name : déjà importé"; return; fi
    mode=(-replace)
  fi
  info "$name : $(du -sh "$dir" | cut -f1) ${mode[*]:-}"
  "$BIN/localdata" -bulk "$dir" -write "$ENDPOINTS" -issuer "$ISSUER" ${mode[@]+"${mode[@]}"} >>"$LOGS/import.log" 2>&1 ||
    die "import de $name refusé." "Journal : $LOGS/import.log" "Relancez $0 : une batch déjà installée est reconnue."
  printf '%s\n' "$id" >"$DATA/imported/$name"
}

step "4. Batches ($BATCHES), puis import d'un coup par collection"
"$CORE/scripts/local/prep-all.sh" >>"$LOGS/prep.log" 2>&1 || die "préparation des batches impossible." "Journal : $LOGS/prep.log"
mkdir -p "$DATA/imported"
# vegetation: AngVegetation001 (the 1 km disc), Voxel* (a commune, prep-commune.sh): last
vegetation() { case "$1" in AngVegetation001 | Voxel*) return 0 ;; *) return 1 ;; esac; }
for dir in "$BATCHES"/*/; do
  vegetation "$(basename "$dir")" || import_batch "${dir%/}"
done

step "5. Gateway (:$GATEWAY_PORT) et Explore (:$EXPLORE_PORT)"
cat >"$DATA/networks.json" <<JSON
{
  "active": "$NETWORK",
  "networks": [{
    "id": "$NETWORK", "label": "Local · Explore", "kind": "local", "project": "$NETWORK",
    "boot_ip": "127.0.0.1", "issuer_url": "$ISSUER", "mon_port": 19031, "p2p_port": 21031,
    "state": "awake", "run_dir": "$DATA",
    "collections": ["DvFMV2020idx0001", "TempeAROME0128v2", "SceneExplo2026v1"]
  }]
}
JSON
PORT=$GATEWAY_PORT NETWORK_ID=$NETWORK BOOT_IP=127.0.0.1 ISSUER_URL=$ISSUER MON_PORT=19031 P2P_PORT=21031 \
  POLL_MS=5000 INDEXUS_NETWORKS_FILE="$DATA/networks.json" \
  start gateway "$GATEWAY_PORT" "$RUN/dashboard" node "$RUN/dashboard/server.js"
wait_http "http://127.0.0.1:$GATEWAY_PORT/api/health" 30 gateway "$LOGS/gateway.log"
start experiences "$EXPLORE_PORT" "$RUN/experiences" node "$RUN/experiences/node_modules/vite/bin/vite.js" \
  --host 127.0.0.1 --port "$EXPLORE_PORT" --strictPort
wait_http "http://127.0.0.1:$EXPLORE_PORT/explore.html" 60 experiences "$LOGS/experiences.log"
info "Explore : http://127.0.0.1:$EXPLORE_PORT/explore.html?scene=anglet"

step "6. Végétation (Explore est déjà ouvert)"
for dir in "$BATCHES"/*/; do
  ! vegetation "$(basename "$dir")" || import_batch "${dir%/}"
done

cat <<TEXT

Prêt.
  Explore :  http://127.0.0.1:$EXPLORE_PORT/explore.html?scene=anglet   (immobilier : &data=realestate)
  Gateway :  http://127.0.0.1:$GATEWAY_PORT/api/mesh
  Nœud : p2p 21031, monitoring 19031, issuer $ISSUER_PORT · disque : $(du -sh "$DATA/node1" | cut -f1)
  Journaux : $LOGS
  Arrêt : $0 stop
TEXT
