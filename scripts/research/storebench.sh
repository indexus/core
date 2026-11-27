#!/usr/bin/env bash
# Runs the storage comparison of docs/storage/zonelog.md and prints its table.
#
#   scripts/research/storebench.sh [work dir] [scale] [table]
#
# With "table" it only prints the table from the results already in work dir.
# Each store loads the same generated records in its own process (so RSS is
# its own), is killed, and is reopened. Then each dataset is loaded alone, at
# a fifth of the scale, for its bytes per record: for zonelog these are the
# segments, without the log it keeps for retries (64 MB whatever the data).
# The work dir needs about 6 GB at scale 1; every data directory is removed
# after its run. About 40 minutes on two cores. Needs jq.
set -euo pipefail
cd "$(dirname "$0")/../.."
work=${1:-/tmp/storebench}
scale=${2:-1}
mkdir -p "$work"

run() { # store, extra flags...
	local store=$1 data="$work/data-$1"
	shift
	rm -rf "$data"
	"$work/storebench" -store "$store" -dir "$data" -scale "$scale" "$@" 2>>"$work/log" | grep '^{' || true
}

stores="zonelog storage-nohistory storage"
if [ "${3:-}" = table ]; then
	stores=
else
	go test ${GOFLAGS:-} -c -o "$work/storebench" ./internal/zonelog/compare
fi
for store in $stores; do
	run "$store" >"$work/$store.load.json"
	"$work/storebench" -store "$store" -dir "$work/data-$store" -scale "$scale" -phase open 2>>"$work/log" |
		grep '^{' >"$work/$store.open.json"
	for only in vegetation large dvf; do
		run "$store" -only "$only" -scale "$(jq -n "$scale / 5")" >"$work/$store.$only.json"
	done
	rm -rf "$work/data-$store"
done

row() { # label, file suffix, jq expression
	printf '%-34s' "$1"
	for store in storage storage-nohistory zonelog; do
		printf '%16s' "$(jq -r "$3" "$work/$store.$2.json" 2>/dev/null || echo -)"
	done
	echo
}

printf '%-34s%16s%16s%16s\n' "" storage no-history zonelog
row "inserts/s, vegetation (256 callers)" load .vegetation_inserts_per_s
row "inserts/s, large payloads" load .large_inserts_per_s
row "inserts/s, DVF" load .dvf_inserts_per_s
row "inserts/s, one batch call of 256" load '.batch_call_inserts_per_s // "-"'
row "inserts/s, one by one" load .single_inserts_per_s
row "disk MB, all records" load .disk_mb
row "disk / raw, all records" load '.disk_over_raw * 100 | round / 100'
row "disk B/record, vegetation" vegetation '.data_over_raw * .raw_bytes_per_record | round'
row "disk / raw, vegetation" vegetation '.data_over_raw * 100 | round / 100'
row "disk B/record, large" large '.data_over_raw * .raw_bytes_per_record | round'
row "disk / raw, large" large '.data_over_raw * 100 | round / 100'
row "disk B/record, DVF" dvf '.data_over_raw * .raw_bytes_per_record | round'
row "disk / raw, DVF" dvf '.data_over_raw * 100 | round / 100'
row "RSS after load, MB" load .rss_after_load_mb
row "RSS peak, MB" load .rss_peak_mb
row "open after kill, ms" open '.open_ms | round'
row "open after kill, s/GB" open '.open_s_per_gb * 100 | round / 100'
row "idle CPU over 60 s, ms" load .idle_cpu_ms
for depth in 1 2 3 4 5; do
	row "children at depth $depth, p50 ms" load ".children_depth${depth}_p50_ms"
	row "children at depth $depth, p99 ms" load ".children_depth${depth}_p99_ms"
done
row "one zone, 64 records, p50 ms" load .zone64_p50_ms
row "one zone, 64 records, p99 ms" load .zone64_p99_ms
