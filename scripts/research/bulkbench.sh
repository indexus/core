#!/usr/bin/env bash
# Measures a bulk ingest against the record-by-record import, on real
# processes: an issuer, a fresh node (zonelog), localdata. Numbers go to
# stdout as JSON lines; docs/storage/bulk.md reads them.
#
#   scripts/research/bulkbench.sh [work dir] [scale] [write sample]
#
# scale 1 is 1.12 M vegetation records and 20 000 large ones; the write
# sample (default 20 000) is how many vegetation records go through
# localdata -write for the comparison. About 3 GB of disk at scale 1.
set -euo pipefail
cd "$(dirname "$0")/../.."
work=${1:-/tmp/bulkbench}
scale=${2:-1}
sample=${3:-20000}
bin=$work/bin
mkdir -p "$work" "$bin"
log=$work/log
: >"$log"

go build ${GOFLAGS:-} -trimpath -ldflags "-s -w" -o "$bin/node" ./cmd/node
go build ${GOFLAGS:-} -trimpath -ldflags "-s -w" -o "$bin/issuer" ./cmd/issuer
go build ${GOFLAGS:-} -trimpath -ldflags "-s -w" -o "$bin/localdata" ./tools/localdata

if [ ! -s "$work/records/vegetation.ndjson" ]; then
	mkdir -p "$work/records"
	BULKGEN="$work/records" BULKGEN_SCALE="$scale" go test ${GOFLAGS:-} -count=1 -v -run TestGenerateBenchRecords ./tools/localdata |
		grep '^{' >"$work/records/raw.json"
fi
head -n "$sample" "$work/records/vegetation.ndjson" >"$work/records/sample.ndjson"

pids=()
cleanup() { for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT
wait_http() { for _ in $(seq 1 400); do curl -sf -o /dev/null --max-time 2 "$1" && return 0; sleep 0.25; done; echo "no answer: $1" >&2; exit 1; }

"$bin/issuer" -addr 127.0.0.1:23030 -network bulkbench -key "$work/issuer.ed25519" -bootstrap 127.0.0.1 -p2pPort 23031 >>"$log" 2>&1 &
pids+=($!)
wait_http http://127.0.0.1:23030/health

# node NAME: a fresh node, as explore.sh starts it (4 cores, 1 GiB limit).
node_pid=
node() {
	local dir=$work/$1
	rm -rf "$dir"
	mkdir -p "$dir"
	cat >"$dir/node.json" <<JSON
{
  "storage": {"path": "node", "identity_directory": ".", "omit_history": true},
  "identity": {"name": "0000000000000001", "network": "bulkbench", "cluster": "bulkbench", "issuer": "http://127.0.0.1:23030"},
  "network": {"listen": "127.0.0.1:23031", "monitoring": "127.0.0.1:23019", "advertise": ["127.0.0.1"]},
  "limits": {"data_bytes": 34359738368, "memory_refuse_pct": 60}
}
JSON
	"$bin/node" init -config "$dir/node.json" >>"$log" 2>&1
	(cd "$dir" && GOMAXPROCS=4 GOMEMLIMIT=1024MiB exec "$bin/node" serve -config "$dir/node.json") >>"$log" 2>&1 &
	node_pid=$!
	pids+=("$node_pid")
	wait_http http://127.0.0.1:23019/health
	sleep 1
}
stop_node() { kill "$node_pid"; wait "$node_pid" 2>/dev/null || true; }
rss() { awk -v k="$1" '$1==k":" {print $2*1024}' "/proc/$node_pid/status"; }
disk() { du -sb "$work/$1" | cut -f1; }
now() { date +%s.%N; }
count() { # records of a collection the node serves: the sum of the root's children
	curl -sf --max-time 120 "http://127.0.0.1:23031/set?collection=$1&location=@" -H "Authorization: Bearer $token" |
		jq '[.set[]? | .count] | add // 0'
}
token=$(curl -sf -X POST http://127.0.0.1:23030/v1/issue/token -d '{"client_id":"bulkbench","scopes":["read","write"]}' | sed 's/.*"token":"\([^"]*\)".*/\1/')

# 1. Record by record: the import of today, on a sample.
node write
t0=$(now)
"$bin/localdata" -write http://127.0.0.1:23031 -issuer http://127.0.0.1:23030 -records "$work/records/sample.ndjson" -concurrency 32 >>"$log" 2>&1
t1=$(now)
echo "{\"run\":\"write\",\"records\":$sample,\"seconds\":$(echo "$t1 - $t0" | bc),\"rss_peak\":$(rss VmHWM),\"disk\":$(disk write)}"
stop_node

# 2. Staging (the preparation pipeline's work, here localdata -stage).
for set in vegetation large; do
	rm -rf "$work/staged/$set"
	t0=$(now)
	"$bin/localdata" -stage "$work/staged/$set" -records "$work/records/$set.ndjson" >>"$log"
	t1=$(now)
	echo "{\"run\":\"stage\",\"set\":\"$set\",\"seconds\":$(echo "$t1 - $t0" | bc),\"batch_bytes\":$(du -sb "$work/staged/$set" | cut -f1)}"
done

# 3. Bulk: each set on a fresh node. The answer comes once the batch is
# installed and the summaries are rebuilt: queryable.
for set in vegetation large; do
	node "bulk-$set"
	before=$(rss VmRSS)
	t0=$(now)
	"$bin/localdata" -bulk "$work/staged/$set" -write http://127.0.0.1:23031 -issuer http://127.0.0.1:23030 >>"$log"
	t1=$(now)
	collection=$(grep -o '"collection": *"[^"]*"' "$work/staged/$set"/*/manifest.json | head -n 1 | sed 's/.*"\([^"]*\)"$/\1/')
	served=$(count "$collection" || echo 0)
	t2=$(now)
	sleep 5
	echo "{\"run\":\"bulk\",\"set\":\"$set\",\"seconds\":$(echo "$t1 - $t0" | bc),\"queryable_seconds\":$(echo "$t2 - $t0" | bc),\"served\":${served:-0},\"rss_before\":$before,\"rss_peak\":$(rss VmHWM),\"rss_after\":$(rss VmRSS),\"disk\":$(disk "bulk-$set"),\"records_dir\":$(du -sb "$work/bulk-$set/node.causal.db.records" | cut -f1)}"
	stop_node
done
cat "$work/records/raw.json"
