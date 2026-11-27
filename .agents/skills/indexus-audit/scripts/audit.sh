#!/usr/bin/env bash
# Audit des couches d'Indexus. Lecture seule.
# Usage : audit.sh [racine indexus]   (le dossier qui contient core, sdk, dashboard, experiences)
set -u
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Default root: the first parent folder that holds core/ and sdk/ (the copy
# lives in core/.agents/skills, often reached through a link at the root).
find_root() {
  local d="$here"
  while [ "$d" != / ]; do
    [ -d "$d/core" ] && [ -d "$d/sdk" ] && { echo "$d"; return; }
    d="$(dirname "$d")"
  done
  echo "$here/../../../../.."
}
root="${1:-$(find_root)}"
budgets="$here/../../indexus-layers/references/budgets.tsv"
cd "$root" || { echo "racine introuvable : $root"; exit 2; }
violations=0

src_files() { # fichiers source hors tests, dépendances et builds
  rg --files "$1" -g '*.{go,js,jsx,mjs,ts,tsx,wgsl}' \
    -g '!*_test.go' -g '!*.test.*' -g '!node_modules' -g '!dist' -g '!wasm/pkg' 2>/dev/null
}
loc() { local n; n=$(src_files "$1" | tr '\n' '\0' | xargs -0 -r cat 2>/dev/null | wc -l); echo "${n:-0}"; }

echo "== 1. Budgets (lignes hors tests) =="
printf '%-34s %8s %8s %8s\n' couche actuel cible écart
while IFS=$'\t' read -r path _ target; do
  case "$path" in ''|\#*) continue;; esac
  now=0; [ -d "$path" ] && now=$(loc "$path")
  [ "$now" = 0 ] && [ "$target" = 0 ] && continue
  printf '%-34s %8d %8d %+8d\n' "$path" "$now" "$target" "$((now - target))"
done < "$budgets"

echo; echo "== 2. Imports interdits =="
rule() { # rule <libellé> <motif rg> <chemin...>
  local label="$1" pattern="$2"; shift 2
  local out; out=$(rg -n "$pattern" "$@" -g '!*_test.go' -g '!*.test.*' -g '!node_modules' -g '!dist' 2>/dev/null)
  if [ -n "$out" ]; then
    echo "-- $label"; echo "$out" | sed 's/^/   /' | cut -c1-200
    violations=$((violations + $(echo "$out" | wc -l)))
  fi
}
mod='github.com/indexus/go-indexus-core/internal'
# --- règles Go : les bords passent par la façade
# app/node (racine de composition) et adapter/meshhttp (transport du mesh) en sont exemptés.
rule "l'API client ou cmd/ importent mesh, causal ou storage" "\"$mod/(mesh|causal|storage)\"" \
  core/internal/adapter/http core/cmd
rule "la production importe benchmark, simulation ou modelcheck" "\"$mod/(benchmark|simulation|modelcheck)\"" \
  core/indexus core/internal/adapter core/internal/app core/internal/mesh core/internal/storage core/internal/causal core/internal/protocol
# --- règles JS : une couche n'importe que les couches au-dessus
js_from() { # js_from <dossier> <dossiers interdits séparés par |>
  [ -d "sdk/src/$1" ] || return 0
  rule "sdk/src/$1 importe $2" "from ['\"](\\.\\./)+($2)/" "sdk/src/$1"
}
js_from utilities 'gpu|network|api|grid|cube|local|map|react|runtime'
js_from model     'network|api|grid|cube|local|gpu|map|react|runtime'
js_from library   'network|api|grid|cube|local|gpu|map|react|runtime'
js_from entities  'network|api|grid|cube|local|gpu|map|react|runtime'
js_from tree      'wire|store|walk|gpu|map|react|network|api|grid|cube|local|runtime'
js_from api       'network|grid|cube|local|gpu|map|react|runtime'
js_from network   'grid|cube|local|gpu|map|react|runtime'
js_from wire      'store|walk|gpu|map|react'
js_from store     'walk|gpu|map|react'
js_from cube      'grid|local|map|react'
js_from gpu       'network|api|grid|cube|local|map|react|runtime|wire|store|walk'
rule "le SDK s'importe lui-même par son nom de paquet" "from ['\"]js-indexus-sdk" sdk/src
rule "React ou MapLibre hors de map/ et react/" "from ['\"](react|react-dom|maplibre-gl)['\"/]" \
  $(ls -d sdk/src/*/ 2>/dev/null | rg -v '/(map|react)/$')
# Compte les fichiers, pas les lignes : le but est d'avoir un seul module DVF, puis zéro.
dvf=$(rg -l '(?i)\bdvf|valeur_fonciere|surface_bati' sdk/src -g '!testdata' -g '!*.test.*' 2>/dev/null)
if [ -n "$dvf" ]; then echo "-- métier DVF dans le SDK ($(echo "$dvf" | wc -l) fichiers)"; echo "$dvf" | sed 's/^/   /'; violations=$((violations + $(echo "$dvf" | wc -l))); fi
rule "une app importe le source du SDK par chemin relatif" "from ['\"](\\.\\./)+sdk/" \
  dashboard/src dashboard/lib experiences/src
rule "nouveau programme WebGL hors SDK" 'createProgram\(' experiences/src dashboard/src
[ "$violations" = 0 ] && echo "aucune"

echo; echo "== 3. Poids =="
echo "-- fichiers de plus de 600 lignes (hors tests)"
for d in core sdk/src dashboard/src dashboard/lib dashboard/server.js experiences/src; do
  [ -e "$d" ] && src_files "$d" | tr '\n' '\0' | xargs -0 -r wc -l 2>/dev/null | awk '$2!="total" && $1>600 {printf "   %6d %s\n",$1,$2}'
done | sort -rn | head -40
echo "-- lignes de plus de 160 caractères"
for d in core sdk/src dashboard/src experiences/src; do
  [ -e "$d" ] && printf '   %-18s %s\n' "$d" "$(src_files "$d" | tr '\n' '\0' | xargs -0 -r awk 'length>160' 2>/dev/null | wc -l)"
done

echo; echo "== 4. Git =="
for d in core sdk dashboard experiences landing; do
  [ -d "$d/.git" ] && printf '   %-12s %-34s %s fichiers non commités\n' "$d" \
    "$(git -C "$d" rev-parse --abbrev-ref HEAD 2>/dev/null)" "$(git -C "$d" status --porcelain 2>/dev/null | wc -l)"
done

echo; echo "violations de couches : $violations"
