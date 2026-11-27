#!/usr/bin/env bash
# Une scène « commune » d'Explore : tout le contour (Anglet, INSEE 64024, 27 km²),
# en lots dans $DATA/batches/<collection>/, comme prep-all.sh (docs/ingest/pipeline.md).
#
#   scripts/local/prep-commune.sh [plan]     ce qui sera fait : surface, téléchargement, disque
#   scripts/local/prep-commune.sh            récupère, prépare, écrit les lots (reprise où il s'est arrêté)
#
# Variables (facultatives) :
#   COMMUNE_SCENE     l'entrée du catalogue (défaut : anglet)
#   PREP_PYTHON       le python du LiDAR (tools/prep/requirements.txt : laspy[lazrs], numpy, Pillow ; matplotlib ;
#                     défaut : experiences/.local/lidar-venv/bin/python)
#   PREP_NO_METAL=1   végétation sur le CPU (numpy) au lieu du GPU
#   COMMUNE_YES=1     ne pas demander avant de télécharger
#
# Lit : le catalogue d'experiences lancé ($RUN/experiences/scripts/world/catalogue.json) et le
# contour à côté (boundaries/). Écrit : $DATA/prep/commune/<scène> (le travail : effaçable une
# fois les lots faits, sauf pour une reprise), $DATA/batches, $DATA/bin/{prep,prep-metal}.
# Ensuite : scripts/local/explore.sh importe les lots (la végétation en dernier).
set -euo pipefail

CORE="${PREP_CORE:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
RUN="$(dirname "$CORE")"
DATA="${EXPLORE_DATA:-$RUN/.data}"
ORIGIN="${EXPLORE_ORIGIN:-$(dirname "$RUN")}"
EXP="${PREP_EXPERIENCES:-$RUN/experiences}"
SCENE="${COMMUNE_SCENE:-anglet}"
BIN="$DATA/bin"
BATCHES="$DATA/batches"
CATALOGUE="$EXP/scripts/world/catalogue.json"
mkdir -p "$BIN" "$BATCHES"
say() { printf '   %s\n' "$*"; }

PY="${PREP_PYTHON:-}"
if [ -z "$PY" ]; then
  for candidate in "$EXP/.local/lidar-venv/bin/python" "$ORIGIN/experiences/.local/lidar-venv/bin/python" python3; do
    if command -v "$candidate" >/dev/null 2>&1 && "$candidate" -c 'import laspy, lazrs, numpy, PIL' 2>/dev/null; then
      PY="$candidate"
      break
    fi
  done
fi
[ -n "$PY" ] || {
  echo "ERREUR : aucun python avec laspy[lazrs], numpy, Pillow." >&2
  echo "         python3 -m venv $ORIGIN/experiences/.local/lidar-venv && $ORIGIN/experiences/.local/lidar-venv/bin/pip install -r $CORE/tools/prep/requirements.txt matplotlib" >&2
  exit 1
}
"$PY" -c 'import matplotlib' 2>/dev/null || say "attention : matplotlib absent de $PY (bâtiments) : pip install matplotlib"

if [ "${1:-}" = plan ]; then
  "$PY" "$CORE/tools/prep/commune/commune.py" plan "$CATALOGUE" "$SCENE" --data "$DATA" --exp "$EXP"
  exit 0
fi
[ $# -eq 0 ] || { echo "usage : $0 [plan]" >&2; exit 2; }

if [ -n "${INDEXUS_GO_ENV:-}" ]; then
  # shellcheck disable=SC1090
  source "$INDEXUS_GO_ENV"
fi
(cd "$CORE" && go build -o "$BIN/prep.new" ./tools/prep) && mv -f "$BIN/prep.new" "$BIN/prep"
metal=()
if [ -z "${PREP_NO_METAL:-}" ] && command -v xcrun >/dev/null 2>&1; then
  if [ ! -x "$BIN/prep-metal" ] || [ "$CORE/tools/prep/metal/main.swift" -nt "$BIN/prep-metal" ] || [ "$CORE/tools/prep/metal/prep.metal" -nt "$BIN/prep.metal" ]; then
    xcrun swiftc -O "$CORE/tools/prep/metal/main.swift" -o "$BIN/prep-metal.new" 2>&1 | grep -v warning || true
    mv -f "$BIN/prep-metal.new" "$BIN/prep-metal"
    cp -f "$CORE/tools/prep/metal/prep.metal" "$BIN/prep.metal"
  fi
  metal=(--metal "$BIN/prep-metal")
fi
yes=()
[ -n "${COMMUNE_YES:-}" ] && yes=(--yes)

say "commune ($SCENE), du catalogue $CATALOGUE"
"$PY" "$CORE/tools/prep/commune/commune.py" run "$CATALOGUE" "$SCENE" --data "$DATA" --exp "$EXP" \
  --prep "$BIN/prep" ${metal[@]+"${metal[@]}"} ${yes[@]+"${yes[@]}"}

# La liste des scènes pointe vers les collections du catalogue (le contour dans l'enregistrement).
say "scènes (SceneExplo2026v1), de $CATALOGUE"
"$PY" "$CORE/tools/prep/scenes/scenes.py" "$CATALOGUE" "$DATA/prep/work/scenes/scenes.ndjson" --batch "$BATCHES" --prep "$BIN/prep"
du -sh "$BATCHES"/*/ | sed 's/^/   /'
say "travail : $(du -sh "$DATA/prep/commune/$SCENE" | cut -f1) dans $DATA/prep/commune/$SCENE (effaçable une fois les lots importés)"
