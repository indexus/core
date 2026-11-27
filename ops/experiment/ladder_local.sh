#!/usr/bin/env bash
# One ladder step on this host: fresh TLS cluster of N vnodes per pass, the
# sentinel watching, uniform writes at RATE per vnode against a CAP per vnode,
# then audit and teardown (also on failure). See the indexus-scale-ladder skill.
#
#   BIN=$PWD/bin N=4 PASSES=3 ops/experiment/ladder_local.sh /tmp/ladder/step1
#
# A pass is green when the sentinel did not stop, the audit passes and at least
# 95% of offered writes were acknowledged. The script stops at the first red
# pass (stop, reproduce, fix, replay) and prints one summary line per pass.
set -uo pipefail
OUT="${1:?usage: ladder_local.sh OUT_DIR}"
BIN="${BIN:-$(pwd)/bin}"
N="${N:-4}"
PASSES="${PASSES:-3}"
RATE="${RATE:-80}"         # offered writes/s per vnode
CAP="${CAP:-100}"          # limits.writes_per_second per vnode (CAP-2)
DURATION="${DURATION:-60s}"
READERS="${READERS:-0}"
EXTRA_LOAD="${EXTRA_LOAD:-}"  # more `experiment load` flags, e.g. "-scenario spike"
D="${WORK:-/tmp/indexus-ladder}"
X="$BIN/experiment"

teardown() {
  kill $(cat "$D"/*.pid 2>/dev/null) 2>/dev/null
  pkill -CONT -f "$BIN/node serve" 2>/dev/null
  pkill -f "$BIN/node serve" 2>/dev/null
  sleep 1
}
trap teardown EXIT INT TERM

summary() { # pass_dir sentinel_status
  python3 - "$1" "$2" "$N" "$RATE" <<'EOF'
import json, sys, os
d, sentinel, n, rate = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), float(sys.argv[4])
rows = [json.loads(l) for l in open(os.path.join(d, "load.jsonl"))]
steady = rows[len(rows)//4:] or rows
offered = sum(r["offered"] for r in steady); acked = sum(r["acked"] for r in steady)
hist = [0]*40
for r in steady:
    for i, c in enumerate(r.get("latency_us_log2") or []): hist[i] += c
def pct(p):
    total, run = sum(hist), 0
    for i, c in enumerate(hist):
        run += c
        if total and run >= p*total: return 2**i/1000
    return 0
audit = json.load(open(os.path.join(d, "audit.json"))) if os.path.exists(os.path.join(d, "audit.json")) else {}
ratio = acked/offered if offered else 0
green = sentinel == 0 and audit.get("pass") is True and ratio >= .95
s = dict(n=n, offered_per_s=offered/len(steady), acked_per_s=acked/len(steady), per_vnode=acked/len(steady)/n,
         ack_ratio=round(ratio, 4), p50_ms=pct(.5), p99_ms=pct(.99), audit=audit.get("pass"), sentinel=sentinel, green=green)
json.dump(s, open(os.path.join(d, "summary.json"), "w"), indent=1)
print(json.dumps(s))
sys.exit(0 if green else 1)
EOF
}

mkdir -p "$OUT"
for pass in $(seq 1 "$PASSES"); do
  teardown
  rm -rf "$D" && mkdir -p "$D"
  P="$OUT/pass$pass" && rm -rf "$P" && mkdir -p "$P"
  export SSL_CERT_FILE="$D/ca/ca.crt"
  "$BIN/issuer" -addr 127.0.0.1:22000 -network experiment -key "$D/issuer.ed25519" -bootstrap 127.0.0.1 >"$D/issuer.log" 2>&1 & echo $! >"$D/issuer.pid"
  "$X" ca -dir "$D/ca"
  "$X" agent -listen 127.0.0.1:22100 -slots "$N" -node "$BIN/node" -dir "$D/slots" -issuer http://127.0.0.1:22000 \
    -ca "$D/ca" -bootstrap https://127.0.0.1:21100 -compaction=false -write-cap "$CAP" >"$D/agent.log" 2>&1 & echo $! >"$D/agent.pid"
  sleep 1
  timeout 120 curl -sf "http://127.0.0.1:22100/spawn?genesis=1&wait=ready" >/dev/null || { echo "genesis failed" >&2; exit 1; }
  for _ in $(seq 2 "$N"); do timeout 180 curl -sf "http://127.0.0.1:22100/spawn?wait=ready" >/dev/null || { echo "join failed" >&2; exit 1; }; done
  targets=$(for i in $(seq 0 $((N-1))); do printf "https://127.0.0.1:%d," $((21100+i)); done); targets=${targets%,}
  monitors=$(for i in $(seq 0 $((N-1))); do printf "https://127.0.0.1:%d," $((19100+i)); done); monitors=${monitors%,}
  "$X" sentinel -agents http://127.0.0.1:22100 -out "$P" -grace 15s -duration 30m >"$P/sentinel.log" 2>&1 & sp=$!
  "$X" load -targets "$targets" -issuer http://127.0.0.1:22000 -rate "$(( RATE * N ))" -duration "$DURATION" \
    -readers "$READERS" -read-rate "$(( READERS * 10 ))" -out "$P" $EXTRA_LOAD >"$P/load.log" 2>&1 & lp=$!
  while kill -0 $lp 2>/dev/null && kill -0 $sp 2>/dev/null; do sleep 1; done
  status=0
  if ! kill -0 $sp 2>/dev/null; then wait $sp; status=$?; kill $lp 2>/dev/null; fi
  wait $lp 2>/dev/null
  if [[ $status == 0 ]]; then
    kill $sp 2>/dev/null; wait $sp 2>/dev/null
    "$X" audit -targets "$targets" -monitors "$monitors" -issuer http://127.0.0.1:22000 -sample 5000 -out "$P" >"$P/audit.log" 2>&1
  fi
  for i in $(seq 0 $((N-1))); do cp "$D/slots/$i/node.log" "$P/node-$i.log" 2>/dev/null; done
  printf "pass %d: " "$pass"
  summary "$P" "$status" || { echo "RED pass $pass: see $P" >&2; exit 1; }
done
echo "GREEN: $PASSES passes at n=$N"
