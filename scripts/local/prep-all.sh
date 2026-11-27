#!/usr/bin/env bash
# Every batch Explore imports, rebuilt into $DATA/batches/<collection>/ (docs/ingest/pipeline.md).
# A batch whose inputs did not change is skipped (its manifest id is a hash of them).
#
#   scripts/local/prep-all.sh
#
# Reads only: $DATA/collections, $DATA/launch, the raw LiDAR and orthophoto of
# the scene in $PREP_EXPERIENCES. Writes $DATA/prep (prepared returns, one file
# per 32 m tile), $DATA/batches, $DATA/bin/{prep,prep-metal}.
set -euo pipefail

CORE="${PREP_CORE:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
RUN="$(dirname "$CORE")"
DATA="${EXPLORE_DATA:-$RUN/.data}"
ORIGIN="${EXPLORE_ORIGIN:-$(dirname "$RUN")}"
EXP="${PREP_EXPERIENCES:-$ORIGIN/experiences}"
BIN="$DATA/bin"
BATCHES="$DATA/batches"
PREP="$DATA/prep"
mkdir -p "$BIN" "$BATCHES" "$PREP"
say() { printf '   %s\n' "$*"; }
T=()
[ -n "${PREP_TIME:-}" ] && T=(/usr/bin/time -l)  # PREP_TIME=1: time and peak memory of each step

if [ -n "${INDEXUS_GO_ENV:-}" ]; then
  # shellcheck disable=SC1090
  source "$INDEXUS_GO_ENV"
fi
(cd "$CORE" && go build -o "$BIN/prep.new" ./tools/prep) && mv -f "$BIN/prep.new" "$BIN/prep"
if [ ! -x "$BIN/prep-metal" ] || [ "$CORE/tools/prep/metal/main.swift" -nt "$BIN/prep-metal" ] || [ "$CORE/tools/prep/metal/prep.metal" -nt "$BIN/prep.metal" ]; then
  xcrun swiftc -O "$CORE/tools/prep/metal/main.swift" -o "$BIN/prep-metal.new" 2>&1 | grep -v warning || true
  mv -f "$BIN/prep-metal.new" "$BIN/prep-metal"
  cp -f "$CORE/tools/prep/metal/prep.metal" "$BIN/prep.metal"
fi

say "vegetation (AngVegetation001), from the prepared returns, on the GPU"
${T[@]+"${T[@]}"} python3 "$CORE/tools/prep/lidar/vegetation.py" "$EXP" "$PREP/work/vegetation" --prep "$PREP" \
  --batch "$BATCHES/AngVegetation001" --metal "$BIN/prep-metal"

# Crowns (AngVegCrowns0001, tools/prep/lidar/crowns.py) are not built: Explore does not read them.
say "ground (TerraLidHD2025v1), from the prepared returns"
${T[@]+"${T[@]}"} python3 "$CORE/tools/prep/lidar/ground.py" "$EXP" "$PREP/work/ground" --prep "$PREP"

# The scene list is code, not data: read it from the launched copy Explore runs.
CATALOGUE="$RUN/experiences/scripts/world/catalogue.json"
say "scenes (SceneExplo2026v1), from $CATALOGUE"
python3 "$CORE/tools/prep/scenes/scenes.py" "$CATALOGUE" "$PREP/work/scenes/scenes.ndjson" \
  --batch "$BATCHES" --prep "$BIN/prep"

say "weather (TempeAROME<day>v2 at the native depth 16, and the other variables), from the export of the former content"
WEATHER="$PREP/work/weather/weather.json"
if [ -f "$WEATHER" ] && [ "$WEATHER" -nt "$DATA/launch/weather-ortho.ndjson" ] && [ "$WEATHER" -nt "$CORE/tools/prep/weather/weather.py" ]; then
  say "weather: unchanged, skipped"
elif [ -f "$DATA/launch/weather-ortho.ndjson" ]; then
  # native depth 16: about 645 000 temperature records, a 400 MB batch of 72 hours (prep peaks near 1.5 GB)
  GOMEMLIMIT="${PREP_GOMEMLIMIT:-1500MiB}" ${T[@]+"${T[@]}"} python3 "$CORE/tools/prep/weather/weather.py" --content "$DATA/launch/weather-ortho.ndjson" \
    --out "$PREP/work/weather" --batch "$BATCHES" --prep "$BIN/prep"
  python3 "$CORE/tools/prep/weather/check.py" --batches "$BATCHES" --descriptor "$WEATHER" \
    --prep "$BIN/prep" --content "$DATA/launch/weather-ortho.ndjson" | sed 's/^/   /'
else
  say "absent : $DATA/launch/weather-ortho.ndjson"
fi

# The NDJSON collections: one batch per collection. A collection found in several
# files is one batch: two batches of one collection would overlap in the node's
# key space.
files=()
for f in collections/dvf-2020 collections/ortho prep/work/ground/terrain \
         collections/buildings-v2 collections/buildings-v2-fine; do
  if [ -f "$DATA/$f.ndjson" ]; then files+=("$DATA/$f.ndjson"); else say "absent : $DATA/$f.ndjson"; fi
done
say "today's collections: ${#files[@]} files"
GOMEMLIMIT="${PREP_GOMEMLIMIT:-1500MiB}" ${T[@]+"${T[@]}"} "$BIN/prep" ndjson -out "$BATCHES" "${files[@]}"
du -sh "$BATCHES"/*/ | sed 's/^/   /'
