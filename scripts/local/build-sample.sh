#!/usr/bin/env bash
# The quickstart's sample: the Explore weather run and the 2020 real estate
# sales, as IXP1 batches, in one archive to publish as a GitHub release asset.
#
#   scripts/local/build-sample.sh [OUT_DIR]      (from core; default OUT_DIR: $EXPLORE_DATA/release)
#
# Reads the batches scripts/local/prep-all.sh wrote in $EXPLORE_DATA/batches
# (default _run/.data/batches). Builds tools/prep, verifies every batch
# (prep verify: hashes, CRCs, key order, zone sums), writes sample.json, then
#   OUT_DIR/indexus-sample-explore-v1.tar.xz          (about 89 MB [TO CHECK])
#   OUT_DIR/indexus-sample-explore-v1.tar.xz.sha256
# Nothing in the batches is changed. macOS (bsdtar) or Linux (GNU tar).
#
# UNTESTED as a whole: the sizes come from xz -6 of the batches on disk.
set -euo pipefail

CORE="${PREP_CORE:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
RUN="$(dirname "$CORE")"
DATA="${EXPLORE_DATA:-$RUN/.data}"
BATCHES="$DATA/batches"
OUT="${1:-$DATA/release}"
NAME="${SAMPLE_NAME:-indexus-sample-explore-v1}"
BIN="$DATA/bin"
# What Explore reads for its weather view (data/collections.js WEATHER_KINDS)
# and its real estate view, plus the scene list (one record). The 3D scene's
# own collections (ortho, terrain, buildings, voxels: about 7 GB) are left out.
COLLECTIONS="${SAMPLE_COLLECTIONS:-TempeAROME0128v2 RainfAROME0128v2 CloudAROME0128v2 SolarAROME0128v2 WindUAROME0128v2 WindVAROME0128v2 TownsAROME0128v2 DvFMV2020idx0001 SceneExplo2026v1}"

say() { printf '   %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

if [ -n "${INDEXUS_GO_ENV:-}" ]; then
  # shellcheck disable=SC1090
  source "$INDEXUS_GO_ENV"
fi
mkdir -p "$BIN" "$OUT"
(cd "$CORE" && go build -o "$BIN/prep.new" ./tools/prep) && mv -f "$BIN/prep.new" "$BIN/prep"

STAGE="$OUT/$NAME"
[ ! -e "$STAGE" ] || die "$STAGE exists: remove it first"
mkdir "$STAGE"
for c in $COLLECTIONS; do
  [ -f "$BATCHES/$c/manifest.json" ] || die "no batch $BATCHES/$c (run scripts/local/prep-all.sh)"
  say "verify $c"
  "$BIN/prep" verify "$BATCHES/$c" >/dev/null || die "prep verify refused $c"
  # a link: tar -h archives the batch itself, nothing is copied
  ln -s "$BATCHES/$c" "$STAGE/$c"
done

# sample.json: what is inside, where it comes from.
commit="$(git -C "$CORE" rev-parse --short HEAD 2>/dev/null || echo unknown)"
node - "$STAGE" "$commit" $COLLECTIONS <<'JS' >"$STAGE/sample.json"
const fs = require("fs"), path = require("path");
const [stage, commit, ...names] = process.argv.slice(2);
const collections = names.map((name) => {
  const m = JSON.parse(fs.readFileSync(path.join(stage, name, "manifest.json"), "utf8"));
  const bytes = m.parts.reduce((s, p) => s + p.bytes, 0);
  return { collection: m.collection, id: m.id, format: m.format, records: m.records, metrics: m.metrics, bytes };
});
console.log(JSON.stringify({
  name: path.basename(stage),
  built: new Date().toISOString().slice(0, 10),
  core_commit: commit,
  sources: {
    weather: "AROME HD run of 2025-01-28, 72 hours, France: Meteo-France, through Open-Meteo [TO CHECK licence and attribution wording]",
    real_estate: "DVF 2020 (Demandes de valeurs foncieres), France: DGFiP, data.gouv.fr [TO CHECK licence and attribution wording]",
  },
  collections,
}, null, 1));
JS

say "archive"
TARBALL="$OUT/$NAME.tar.xz"
if tar --version 2>/dev/null | grep -q bsdtar; then
  flags=(--uid 0 --gid 0 --uname "" --gname "" --no-xattrs --no-mac-metadata)
else
  flags=(--sort=name --owner=0 --group=0 --numeric-owner --mtime=2026-01-01 --mode=u+rwX,go+rX)
fi
if command -v xz >/dev/null; then
  COPYFILE_DISABLE=1 tar "${flags[@]}" -chf - -C "$OUT" "$NAME" | xz -6 -T0 >"$TARBALL.new"
else
  COPYFILE_DISABLE=1 tar "${flags[@]}" -chJf "$TARBALL.new" -C "$OUT" "$NAME"
fi
mv -f "$TARBALL.new" "$TARBALL"
(cd "$OUT" && shasum -a 256 "$NAME.tar.xz" >"$NAME.tar.xz.sha256")
say "$(du -h "$TARBALL" | cut -f1)  $TARBALL"
say "sha256 $(cut -d' ' -f1 "$TARBALL.sha256")"
say "then: gh release create sample-explore-v1 $TARBALL $TARBALL.sha256 --repo indexus/core"
