# Builds zonecheck against a scratch copy of this core tree (the tree itself
# is not touched). Usage from the core root: bash scripts/research/indexus-cl/zonecheck/build.sh
set -eu
ROOT=$(pwd)
W=/tmp/idx-cl-build; rm -rf "$W"; mkdir -p "$W"
rsync -a --exclude .local --exclude 'ops/agent' --exclude bin --exclude perf "$ROOT/" "$W/core/"
mkdir -p "$W/core/cmd/zonecheck" && cp "$ROOT/scripts/research/indexus-cl/zonecheck/main.go" "$W/core/cmd/zonecheck/"
( cd "$W/core" && go vet ./cmd/zonecheck && go build -o /tmp/idx-cl-zonecheck ./cmd/zonecheck )
echo "built /tmp/idx-cl-zonecheck from tree $(cd "$ROOT" && git rev-parse --short HEAD) dirty=$(cd "$ROOT" && git status --porcelain | wc -l | tr -d ' ')"
