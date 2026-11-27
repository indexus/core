#!/bin/bash
# Campaign plan, run by the load machine (or locally against local agents).
# Env: AGENTS (comma list of agent URLs), SLOTS (per agent), ISSUER,
# PROVISIONER (empty: no elastic phase). Optional: SIZES, PCT (shorten every
# phase, e.g. 15), SAMPLE (audit read-back sample), ITEMS, EXPERIMENT (binary).
#
# The cluster is rebuilt through the agents between phases, never by
# recreating machines: reset, genesis, joins one at a time (JOIN-1).
set -u
OUT=$1
X=${EXPERIMENT:-/opt/indexus/experiment}
AUDITOR=${AUDITOR:-$X}
PCT=${PCT:-100}
SIZES=${SIZES:-"8 16 32"}
read -r -a AG <<<"${AGENTS//,/ }"

secs() { echo $(( $1 * PCT / 100 + 1 )); }

# agents: the machines up now. With machines on demand (HOSTS_MAX), hosts
# register under AGENTS_S3.d/ as they boot; AG follows.
agents() {
  [[ -n "${AGENTS_S3:-}" ]] || return 0
  # Host 0 first (it was listed first in $AGENTS_S3), then the others.
  local first; first=$(aws s3 cp "$AGENTS_S3" - 2>/dev/null | cut -d, -f1)
  local listed; listed=$( (echo "$first"; aws s3 ls "$AGENTS_S3.d/" 2>/dev/null | awk '{print "http://" $NF ":22100"}' | grep -vx "$first") | grep . | paste -sd' ' - || true)
  [[ -n "$listed" ]] && read -r -a AG <<<"$listed"
}

# The live slots of every agent: a slot left dead by a reset is not a target.
endpoints() {
  agents
  TARGETS=""; MONITORS=""
  for a in "${AG[@]}"; do
    for row in $(curl -sf "$a/slots" | python3 -c 'import sys,json;[print(r["p2p"]+"|"+r["monitor"]) for r in json.load(sys.stdin) if r.get("alive")]'); do
      TARGETS="$TARGETS,${row%%|*}"; MONITORS="$MONITORS,${row##*|}"
    done
  done
  TARGETS=${TARGETS#,}; MONITORS=${MONITORS#,}
}

# Rediscovery includes newly started vnodes. Resource mode monitors live
# processes only; fixed layouts retain their predeclared monitor addresses.
every_monitor() {
  agents
  for a in "${AG[@]}"; do curl -sf "$a/slots" | python3 -c 'import sys,json;[print(r["monitor"]) for r in json.load(sys.stdin) if sys.argv[1] != "true" or r.get("alive")]' "${RESOURCE_SLOTS:-false}"; done | paste -sd, -
}

# balanced: every vnode's XOR cell within a factor 2 of every other (JOIN-1),
# from the names the nodes report. A layout with a double cell makes one vnode
# carry twice the load and turns a scaling step red for a placement reason
# (REM-120): such a cluster is rebuilt before any load.
balanced() {
  python3 - "$MONITORS" "$1" <<'PY'
import json, sys, urllib.request
A = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
bits = []
for m in sys.argv[1].split(","):
    with urllib.request.urlopen(m + "/metrics", timeout=5) as r:
        name = json.load(r)["name"]
    bits.append("".join(format(A.index(c), "06b") for c in name))
def depth(i):
    d = 0
    for j, o in enumerate(bits):
        if j != i:
            k = 0
            while k < len(o) and o[k] == bits[i][k]:
                k += 1
            d = max(d, k + 1)
    return d
ds = [depth(i) for i in range(len(bits))]
print(f"cells: {len(bits)} vnodes, depths {min(ds)}..{max(ds)}")
# A joiner that never joined leaves fewer vnodes than asked (2026-11-20).
sys.exit(0 if len(bits) == int(sys.argv[2]) and max(ds) - min(ds) <= 1 else 1)
PY
}

# cluster N SPAWN_QUERY: genesis on the first agent, N-1 joins spread over the
# agents, each one client ready before the next; rebuilt (up to 3 times) when
# the layout is not balanced.
cluster() {
  local try
  for try in 1 2 3; do
    build "$@" 2>>"$OUT/cluster.log"
    balanced "$1" >>"$OUT/cluster.log" 2>&1 && return 0
    echo "cluster $1: unbalanced layout, rebuilding (try $try)" >>"$OUT/cluster.log"
  done
  [[ "${RESOURCE_SLOTS:-false}" != true ]] # auto layout must qualify before load
}
# A rejected new receiver has created no process. Retry only that exact
# response, since a timed-out POST can have already started a process.
seed_spawn() {
  local timeout=$1 url=$2 reply code body until=$((SECONDS + 90))
  if [[ "${RESOURCE_SLOTS:-false}" != true ]]; then
    curl -sf --max-time "$timeout" -X POST "$url"
    return
  fi
  while true; do
    reply=$(curl -sS --max-time "$timeout" -w $'\n%{http_code}' -X POST "$url") || return 1
    code=${reply##*$'\n'}
    body=${reply%$'\n'*}
    body=${body%$'\n'}
    if [[ "$code" == 200 ]]; then printf '%s\n' "$body"; return 0; fi
    if [[ "$code" != 503 || "$body" != "no receiver capacity on this machine" || $SECONDS -ge $until ]]; then
      echo "seed spawn refused ($code): $body" >&2
      return 1
    fi
    sleep 2
  done
}
build() {
  local n=$1 extra=${2:-} a i # i: the caller's loop variable otherwise (bash scope)
  # Twice: the provisioner can still start a node for the old cluster on an
  # agent reset a moment before (2026-11-20: the joiners then bootstrapped
  # on that stray node and the pass ran on one vnode).
  for a in "${AG[@]}" "${AG[@]}"; do curl -sf --max-time 60 -X POST "$a/reset" >/dev/null; done
  local genesis; genesis=$(seed_spawn 60 "${AG[0]}/spawn?genesis=1&$extra")
  BOOT=$(curl -sf "${AG[0]}/slots" | python3 -c 'import sys,json;i=int(sys.argv[1].rsplit("-",1)[1]);print(next(r["p2p"] for r in json.load(sys.stdin) if r["slot"] == i))' "$genesis")
  for i in $(seq 1 $((n - 1))); do
    seed_spawn 240 "${AG[$((i % ${#AG[@]}))]}/spawn?wait=ready&bootstrap=$BOOT&$extra" >/dev/null || echo "spawn $i failed" >&2
  done
  endpoints
  sleep 20
}

# phase NAME SECONDS TAIL_SECONDS load-args...: observe, load, settle, audit.
phase() {
  local name=$1 seconds tail; seconds=$(secs "$2"); tail=$(secs "$3"); shift 3
  local dir="$OUT/$name"
  mkdir -p "$dir"
  # The list is rewritten every 15 s: machines started during the phase are observed.
  every_monitor >"$dir/monitors"
  printf '%s\n' "${AG[@]}" >"$dir/agents"
  ( while sleep 15; do every_monitor >"$dir/monitors.new" && mv "$dir/monitors.new" "$dir/monitors"; printf '%s\n' "${AG[@]}" >"$dir/agents.new" && mv "$dir/agents.new" "$dir/agents"; done ) &
  local lister=$!
  $X observe -monitors-file "$dir/monitors" -every 2s -duration "${OBSERVE_SECONDS:-3600}s" -out "$dir" >/dev/null 2>&1 &
  local observer=$!
  # Live counters (ops/experiment/live.py): one compact line every 5 s,
  # pushed to the results every 10 s so the operator's page follows the run.
  local live=/opt/indexus/live.py evidence=/opt/indexus/evidence.py follower=""
  [[ -f $live ]] || aws s3 cp "${RESULTS_URL%/results}/plan/live.py" $live --only-show-errors 2>/dev/null || true
  [[ -f /opt/indexus/fleet.py ]] || aws s3 cp "${RESULTS_URL%/results}/plan/fleet.py" /opt/indexus/fleet.py --only-show-errors 2>/dev/null || true
  [[ -z "${RESULTS_URL:-}" ]] || aws s3 cp "${RESULTS_URL%/results}/plan/evidence.py" "$evidence" --only-show-errors 2>/dev/null || true
  if [[ -f $live ]]; then
    ( python3 $live machines "$dir/agents" "$dir" & machine_observer=$!
      trap 'kill "$machine_observer" "${a:-}" 2>/dev/null || true' EXIT TERM INT
      python3 $live agg "$dir" "$OUT/live.jsonl" "$name" & a=$!
      round=0
      while kill -0 $a 2>/dev/null; do
        sleep 10
        [[ -n "${RESULTS_URL:-}" ]] && aws s3 cp "$OUT/live.jsonl" "$RESULTS_URL/live.jsonl" --only-show-errors || true
        round=$((round + 1))
        if [[ -n "${RESULTS_URL:-}" && -f "$evidence" ]] && (( round % 3 == 0 )); then
          python3 "$evidence" "$dir" "$RESULTS_URL/$name" || true
        fi
      done ) &
    follower=$!
  fi
  $X load -targets "$TARGETS" -issuer "$ISSUER" -duration "${seconds}s" -out "$dir" "$@" >"$dir/load.log" 2>&1
  sleep $((tail + 30))
  if [[ "${PHASE_AUDIT:-1}" == 0 ]]; then
    kill $observer 2>/dev/null; wait $observer 2>/dev/null; kill $lister 2>/dev/null
    if [[ -n "$follower" ]]; then sleep 12; kill $follower 2>/dev/null; pkill -f "live.py agg" 2>/dev/null; aws s3 cp "$OUT/live.jsonl" "$RESULTS_URL/live.jsonl" --only-show-errors || true; fi
    [[ -n "${RESULTS_URL:-}" ]] && aws s3 cp "$dir" "$RESULTS_URL/$name/" --recursive --exclude acked.jsonl --only-show-errors || true
    return 0
  fi
  # The audit reads a quiet cluster: no open handoff and no frozen transfer
  # for three polls in a row, at most QUIET_MAX s (2026-11-19: late joins and
  # folds during the audit made each read take seconds).
  endpoints # the vnodes started during the phase are audited too
  local quiet=0 waited=0 busy
  while (( quiet < 3 && waited < ${QUIET_MAX:-300} )); do
    busy=$(for m in ${MONITORS//,/ }; do curl -sk --max-time 3 "$m/metrics"; echo; done | python3 -c '
import sys,json
n=0
for l in sys.stdin:
    try: m=json.loads(l)
    except Exception: continue
    n+=m["handoffs"]["open"]["count"]+len(m.get("frozen_transfers") or [])
print(n)')
    if [[ "$busy" == 0 ]]; then quiet=$((quiet + 1)); else quiet=0; fi
    sleep 10; waited=$((waited + 10))
  done
  echo "quiet after ${waited}s" >"$dir/quiet.log"
  # Bounded: complete reads are refused while a transfer is open, and an
  # unbounded audit kept a whole campaign from uploading anything (2026-11-18).
  timeout "${AUDIT_SECONDS:-900}" "$AUDITOR" audit -targets "$TARGETS" -monitors "$MONITORS" -monitors-file "$dir/monitors" -issuer "$ISSUER" -sample "${SAMPLE:-20000}" -out "$dir" >"$dir/audit.log" 2>&1 || echo "audit exit $? (124: stopped after ${AUDIT_SECONDS:-600} s)" >>"$dir/audit.log"
  kill $observer 2>/dev/null; wait $observer 2>/dev/null; kill $lister 2>/dev/null
  if [[ -n "$follower" ]]; then sleep 12; kill $follower 2>/dev/null; pkill -f "live.py agg" 2>/dev/null; aws s3 cp "$OUT/live.jsonl" "$RESULTS_URL/live.jsonl" --only-show-errors || true; fi
  # Upload each phase as soon as it ends: a later phase cannot lose it.
  [[ -n "${RESULTS_URL:-}" ]] && aws s3 cp "$dir" "$RESULTS_URL/$name/" --recursive --exclude acked.jsonl --only-show-errors || true
}

# 0. PUSH (ladder): per size, raise the offered rate step by step until less
#    than 95% is acknowledged or the sentinel stops the step; the best green
#    rate is the size's ceiling. A size is climbed only after the previous one
#    had a green step. MODE=push runs only this.
# dist_load DIR RATE SECONDS [LOAD ARGS...]: run one step on every load agent in LOADERS
# (LOAD-SYNC): each takes RATE/k with its own seed and id prefix, all start at
# the same instant, then their per-second lines are summed into DIR/load.jsonl
# and their acknowledged keys concatenated into DIR/acked.jsonl for the audit.
dist_load() {
  local dir=$1 rate=$2 secs=$3 i a step at tag share x extra=()
  read -r -a LA <<<"${LOADERS//,/ }"
  shift 3
  # Extra load arguments; rate ceilings (-ramp-max, -read-rate) are split like the rate.
  while (($#)); do
    if [[ $1 == -ramp-max || $1 == -read-rate ]]; then set -- "$1" "$(( $2 / ${#LA[@]} ))" "${@:3}"; fi
    extra+=(--data-urlencode "arg=$1"); shift
  done
  step=$(basename "$dir"); at=$(( $(date +%s%3N) + 5000 )); share=$(( rate / ${#LA[@]} ))
  tag=$(LC_ALL=C tr -dc 'a-z' </dev/urandom | head -c 5)
  i=0
  for a in "${LA[@]}"; do
    curl -sf -X POST -G "$a/run" --data-urlencode "step=$step" \
      --data-urlencode "arg=-targets" --data-urlencode "arg=$TARGETS" --data-urlencode "arg=-issuer" --data-urlencode "arg=$ISSUER" \
      --data-urlencode "arg=-rate" --data-urlencode "arg=$share" --data-urlencode "arg=-duration" --data-urlencode "arg=${secs}s" \
      --data-urlencode "arg=-workers" --data-urlencode "arg=4096" --data-urlencode "arg=-start-at" --data-urlencode "arg=$at" \
      --data-urlencode "arg=-seed" --data-urlencode "arg=$((i + 1))" --data-urlencode "arg=-id-prefix" --data-urlencode "arg=l$i-" \
      --data-urlencode "arg=-tag" --data-urlencode "arg=$tag" --data-urlencode "arg=-probe-every" --data-urlencode "arg=200ms" --data-urlencode "arg=-stacks-at" --data-urlencode "arg=$((secs * 3 / 4))s" \
      ${extra[@]+"${extra[@]}"} \
      >/dev/null || echo "load agent $a refused step $step" >&2
    i=$((i + 1))
  done
  # A load that stops at its knee (-stop-below) ends before SECONDS: poll.
  sleep 15
  for _ in $(seq 1 $(( (secs + 180) / 2 ))); do
    busy=0; for a in "${LA[@]}"; do [[ $(curl -sf "$a/status") == idle ]] || busy=1; done
    ((busy)) || break; sleep 2
  done
  i=0
  for a in "${LA[@]}"; do
    mkdir -p "$dir/loader-$i"
    for f in load.jsonl acked.jsonl load.log stacks.txt knee.json; do curl -sf "$a/file?step=$step&name=$f" -o "$dir/loader-$i/$f" || true; done
    i=$((i + 1))
  done
  cat "$dir"/loader-*/acked.jsonl >"$dir/acked.jsonl" 2>/dev/null || true
  python3 - "$dir" <<'EOF'
import glob, json, os, sys
d = sys.argv[1]
per = {}
for f in sorted(glob.glob(os.path.join(d, "loader-*", "load.jsonl"))):
    for l in open(f):
        r = json.loads(l); k = int(r["t"])
        m = per.setdefault(k, {"t": float(k + 1), "offered_rate": 0, "offered": 0, "shed": 0, "acked": 0, "hot_acked": 0,
                               "latency_us_log2": [], "in_flight": 0, "cpu_cores": 0.0, "cpu_total": 0.0, "machine_cores": 0, "loaders": 0})
        for f2 in ("offered_rate", "offered", "shed", "acked", "hot_acked", "in_flight",
                   "reads", "read_failed", "read_missing", "read_oversized",
                   "totals", "totals_exact", "totals_deficit"):
            m[f2] = m.get(f2, 0) + r.get(f2, 0)
        m["cpu_cores"] = max(m["cpu_cores"], r.get("cpu_cores", 0))  # the busiest load machine
        m["cpu_total"] += r.get("cpu_cores", 0)
        m["machine_cores"] = max(m["machine_cores"], r.get("machine_cores", 0))
        m["loaders"] += 1
        for hkey in ("latency_us_log2", "intended_latency_us_log2", "fresh_lag_us_log2", "read_latency_us_log2", "total_latency_us_log2"):
            h = r.get(hkey) or []
            dst = m.setdefault(hkey, [])
            dst += [0] * (len(h) - len(dst))
            for i, c in enumerate(h): dst[i] += c
        m["fresh_timeouts"] = m.get("fresh_timeouts", 0) + r.get("fresh_timeouts", 0)
        for node, c in (r.get("slow_by_node") or {}).items():
            m.setdefault("slow_by_node", {})[node] = m.get("slow_by_node", {}).get(node, 0) + c
with open(os.path.join(d, "load.jsonl"), "w") as out:
    for k in sorted(per): out.write(json.dumps(per[k]) + "\n")
EOF
}

push_step() { # N RATE DIR -> 0 green, 1 red
  local n=$1 rate=$2 dir=$3
  mkdir -p "$dir"
  $X sentinel -agents "$AGENTS" -out "$dir" -grace 15s -duration 30m -stacks-at "$(( $(secs ${STEP_SECONDS:-40}) * 3 / 4 ))s" >"$dir/sentinel.log" 2>&1 &
  local sp=$!
  if [[ -n "${LOADERS:-}" ]]; then
    dist_load "$dir" "$rate" "$(secs ${STEP_SECONDS:-40})" >"$dir/load.log" 2>&1 &
  else
    $X load -targets "$TARGETS" -issuer "$ISSUER" -duration "$(secs ${STEP_SECONDS:-40})s" -rate "$rate" -workers 4096 -probe-every 200ms -out "$dir" >"$dir/load.log" 2>&1 &
  fi
  local lp=$! status=0
  while kill -0 $lp 2>/dev/null && kill -0 $sp 2>/dev/null; do sleep 1; done
  if ! kill -0 $sp 2>/dev/null; then wait $sp; status=$?; kill $lp 2>/dev/null; fi
  wait $lp 2>/dev/null; kill $sp 2>/dev/null; wait $sp 2>/dev/null
  [[ $status == 0 ]] && "$AUDITOR" audit -targets "$TARGETS" -monitors "$MONITORS" -issuer "$ISSUER" -sample 5000 -out "$dir" >"$dir/audit.log" 2>&1
  python3 - "$dir" "$n" "$rate" "$status" >>"$OUT/push.jsonl" <<'EOF'
import json, os, sys
d, n, rate, sentinel = sys.argv[1], int(sys.argv[2]), float(sys.argv[3]), int(sys.argv[4])
rows = [json.loads(l) for l in open(os.path.join(d, "load.jsonl"))] if os.path.exists(os.path.join(d, "load.jsonl")) else []
steady = rows[len(rows)//4:] or rows or [{"offered": 0, "acked": 0, "shed": 0}]
off = sum(r["offered"] for r in steady); ack = sum(r["acked"] for r in steady); shed = sum(r.get("shed", 0) for r in steady)
report = json.load(open(os.path.join(d, "audit.json"))) if os.path.exists(os.path.join(d, "audit.json")) else {}
audit = report.get("pass")
# Durability, the part of the audit a write-throughput step depends on: no
# acknowledged key missing (sampled read-back, and every key of a mismatched
# collection, AUDIT-2), no zone with two writers, every node answering. A total
# that disagrees while every key reads back is a read-path defect, tracked as
# totals_off, not a reason to stop measuring writes.
durable = bool(report) and report.get("missing", 1) == 0 and report.get("mismatch_keys_lost", 0) == 0 \
    and report.get("double_owned_zones", 1) == 0 and report.get("read_errors", 1) == 0
totals_off = len(report.get("count_mismatches") or {})
reason = open(os.path.join(d, "incident", "reason.txt")).read().strip() if os.path.exists(os.path.join(d, "incident", "reason.txt")) else ""
ratio = ack / off if off else 0
# Who is the limit: the loader's cores, and the busiest node's cores, over the steady part.
loader_cpu = sum(r.get("cpu_cores", 0) for r in steady) / len(steady)
cores = max((r.get("machine_cores", 0) for r in steady), default=0)
node_cpu = 0.0
sp = os.path.join(d, "sentinel.jsonl")
if os.path.exists(sp):
    by = {}
    for l in open(sp):
        x = json.loads(l)
        if x.get("up"): by.setdefault(x["url"], []).append(x)
    for xs in by.values():
        xs = xs[len(xs)//4:]
        if len(xs) > 1:
            dt = 2.0 * (len(xs) - 1)  # the sentinel polls every 2 s
            node_cpu = max(node_cpu, (xs[-1].get("cpu_seconds", 0) - xs[0].get("cpu_seconds", 0)) / dt)
# Where a write's time goes: service time per mesh request kind across nodes,
# and the share of mesh requests landing on the busiest node.
kinds, share = {}, 0.0
mf, ml = os.path.join(d, "metrics-first.json"), os.path.join(d, "metrics-last.json")
if os.path.exists(mf) and os.path.exists(ml):
    first, last = json.load(open(mf)), json.load(open(ml))
    per_node = {}
    for url, m in last.items():
        f = first.get(url, {})
        for kind, w in (m.get("mesh_handled") or {}).items():
            w0 = (f.get("mesh_handled") or {}).get(kind, {})
            c, ns = w.get("calls", 0) - w0.get("calls", 0), w.get("nanos", 0) - w0.get("nanos", 0)
            k = kinds.setdefault(kind, [0, 0]); k[0] += c; k[1] += ns
            per_node[url] = per_node.get(url, 0) + c
    total = sum(per_node.values())
    share = max(per_node.values()) / total if total else 0
    kinds = {k: dict(calls=v[0], ms=round(v[1] / v[0] / 1e6, 2)) for k, v in sorted(kinds.items(), key=lambda kv: -kv[1][1])[:6] if v[0]}
# Where the tail is: p99 in ms of a write at its entry node, of one group
# commit, and of a write's wait for its commit, from the log2 µs histograms.
def hist(m, path):
    for p in path:
        m = (m or {}).get(p)
    return m or []
def p99(path):
    if not (os.path.exists(mf) and os.path.exists(ml)):
        return None
    tot = []
    for url, m in last.items():
        a, b = hist(first.get(url), path), hist(m, path)
        for i, v in enumerate(b):
            tot += [0] * (i + 1 - len(tot))
            tot[i] += v - (a[i] if i < len(a) else 0)
    n, c = sum(tot), 0
    for i, v in enumerate(tot):
        c += v
        if n and c >= .99 * n:
            return 2 ** i / 1000
    return None
tails = dict(write_p99_ms=p99(["writes", "latency_us_log2"]), commit_p99_ms=p99(["disk", "commit_us_log2"]), commit_wait_p99_ms=p99(["disk", "wait_us_log2"]))
# Freshness: from a probe write's ACK until the root total (summary tree)
# counts it, over the whole step; and UDP datagrams the hosts dropped.
fresh = []
for r in rows:
    for i, v in enumerate(r.get("fresh_lag_us_log2") or []):
        fresh += [0] * (i + 1 - len(fresh)); fresh[i] += v
def q(h, x):
    n, c = sum(h), 0
    for i, v in enumerate(h):
        c += v
        if n and c >= x * n:
            return 2 ** i / 1000
    return None
tails.update(fresh_p50_ms=q(fresh, .5), fresh_p99_ms=q(fresh, .99), fresh_max_ms=q(fresh, 1.0), fresh_probes=sum(fresh),
             fresh_timeouts=sum(r.get("fresh_timeouts", 0) for r in rows))
if os.path.exists(mf) and os.path.exists(ml):
    tails["udp_rcvbuf_errors"] = sum(((m.get("udp") or {}).get("RcvbufErrors", 0) - ((first.get(u) or {}).get("udp") or {}).get("RcvbufErrors", 0)) for u, m in last.items())
print(json.dumps(dict(n=n, rate=rate, offered_per_s=off/len(steady), acked_per_s=ack/len(steady), per_vnode=ack/len(steady)/n,
    mesh_by_kind=kinds, busiest_node_mesh_share=round(share, 3), **tails,
    shed_per_s=shed/len(steady), ack_ratio=round(ratio, 4), loaders=max((r.get("loaders", 1) for r in steady), default=1),
    loader_cores=round(loader_cpu, 2), loader_machine_cores=cores,
    busiest_node_cores=round(node_cpu, 2), audit=audit, sentinel=sentinel, stop=reason,
    durable=durable, totals_off=totals_off, lost=report.get("mismatch_keys_lost", 0),
    green=sentinel == 0 and durable and ratio >= .95, **({"pass": int(os.environ["PASS"])} if os.environ.get("PASS") else {}))))
EOF
  tail -n1 "$OUT/push.jsonl" | grep -q '"green": true'
}
push() {
  local prev_green=1
  for N in $SIZES; do
    (( prev_green )) || { echo "size $N skipped: the previous size had no green step" >>"$OUT/push.log"; break; }
    cluster "$N" "compaction=false&provisioner="
    prev_green=0
    rate=$(( ${PUSH_START:-300} * N ))
    while :; do
      if push_step "$N" "$rate" "$OUT/push-n$N-r$rate"; then prev_green=1; else break; fi
      rate=$(( rate * 3 / 2 ))
    done
  done
}
# PASSES (REM-94): the same fixed rates, PASSES times per size, each pass on a
# cluster rebuilt from genesis, so run-to-run noise is measured, not guessed.
# RATES_PER_VNODE: offered writes/s per vnode, one step each. Rows land in
# push.jsonl with their pass; ops/experiment/passes.py aggregates them.
passes() {
  local N p r
  for N in $SIZES; do
    for p in $(seq 1 "${PASSES:-3}"); do
      cluster "$N" "compaction=false&provisioner="
      for r in ${RATES_PER_VNODE:-1000 2000 3000}; do
        PASS=$p push_step "$N" $((r * N)) "$OUT/pass$p-n$N-r$((r * N))" || true
      done
    done
  done
}
# 4. Dynamics: start small, spike one prefix, then calm; scale up through the
#    provisioner, fold back when cool (COLD-2), retired slots freed.
#    MODE=dynamics runs only this (elasticity.md).
dynamics() {
  [[ -n "${PROVISIONER:-}" ]] || return 0
  cluster 4 "compaction=true&provisioner=$PROVISIONER&items=${ITEMS:-500000}"
  phase "dynamics" 300 420 -scenario spike -rate 400 -spike-start "$(secs 60)s" -spike-length "$(secs 120)s" -spike-factor 8 -workers 2048 -readers 32 -read-rate 200
}
if [[ "${MODE:-all}" == push ]]; then push; exit 0; fi
if [[ "${MODE:-all}" == passes ]]; then passes; exit 0; fi
# 5. Steps: elasticity as others test it (DynamoDB absorbs 2x the previous
#    peak at once; faster growth is spread over minutes). Uniform keys,
#    STEP_RATE/s for STEP_PLATEAU s, then x2 over STEP_RAMP s and held, three
#    times. MODE=steps runs only this; ops/experiment/steps.py gives the
#    verdict per level.
steps() {
  [[ -n "${PROVISIONER:-}" ]] || return 0
  local r=${STEP_RAMP:-60} p=${STEP_PLATEAU:-120} i id_prefix=""
  local seed=${STEP_SEED:-$(python3 -c 'import secrets; print(secrets.randbits(32))')}
  # STEP_PASSES passes, each on a fresh cluster: a step is validated by three
  # green passes in a row (indexus-scale-ladder).
  for i in $(seq 1 "${STEP_PASSES:-1}"); do
    if (( i > 1 )); then id_prefix="pass$i-"; fi
    cluster "${STEP_VNODES:-${SIZES%% *}}" "compaction=true&provisioner=$PROVISIONER&items=${ITEMS:-500000}"
    phase "steps-$i" $((p + 3 * (r + p))) "${STEP_TAIL:-300}" -scenario steps -rate "${STEP_RATE:-1000}" -step-ramp "${r}s" -step-plateau "${p}s" -workers "${STEP_WORKERS:-4096}" -discover-every 5s -probe-every 200ms -route "${STEP_ROUTE:-relay}" -seed "$((10#$seed + i - 1))" -tag "${STEP_TAG:-}" -id-prefix "$id_prefix"
  done
}
if [[ "${MODE:-all}" == dynamics ]]; then dynamics; exit 0; fi
# 6. RAMP (RAMP-1): the most dynamic load, as breakpoint tests run it (k6
#    ramping-arrival-rate, YCSB -target raised until the achieved rate falls
#    behind): open-loop arrivals whose rate doubles every RAMP_DOUBLE s,
#    RAMP_SHARE of each doubling linear and the rest one jump (1: continuous,
#    0: instant 2x as DynamoDB on demand absorbs), until fewer than RAMP_STOP
#    of the writes offered over 20 s are acknowledged: the knee. One step per
#    size, so a size costs minutes, not one fixed-rate step per level.
#    - Fixed sizes (default): SIZES, no provisioner. Q1 holds if the best green
#      rate per vnode at the largest size is >= 80% of the smallest's.
#    - RAMP_ELASTIC=1: 4 vnodes with the provisioner; RAMP_DOUBLES lists the
#      doubling periods to try, slowest first: the shortest one still green is
#      how fast the network follows its load.
#    Verdict per step: ops/experiment/ramp.py, one line in $OUT/ramp.jsonl.
ramp_step() { # N DOUBLE DIR START_RATE -> 0 green
  local n=$1 dbl=$2 dir=$3 start=$4 status=0 hold=${RAMP_HOLD:-30}
  local secs=${RAMP_SECONDS:-1200}
  local args=(-scenario ramp -double-every "${dbl}s" -ramp-share "${RAMP_SHARE:-1}" -ramp-hold "${hold}s" -stop-below "${RAMP_STOP:-0.9}" -stop-for 20s -discover-every 5s -route "${RAMP_ROUTE:-relay}")
  [[ -n "${RAMP_MAX:-}" ]] && args+=(-ramp-max "$RAMP_MAX")
  if [[ "${RAMP_READ_RATE:-0}" -gt 0 ]]; then
    args+=(-readers "${RAMP_READERS:-32}" -read-rate "$RAMP_READ_RATE" -total-share "${RAMP_TOTAL_SHARE:-0}")
  fi
  mkdir -p "$dir"
  $X sentinel -agents "$AGENTS" -out "$dir" -grace 15s -duration "$((secs + 300))s" >"$dir/sentinel.log" 2>&1 &
  local sp=$!
  every_monitor >"$dir/monitors"
  ( while sleep 15; do every_monitor >"$dir/monitors.new" && mv "$dir/monitors.new" "$dir/monitors"; done ) &
  local lister=$!
  $X observe -monitors-file "$dir/monitors" -every 2s -duration "$((secs + 60))s" -out "$dir" >/dev/null 2>&1 &
  local op=$!
  if [[ -n "${LOADERS:-}" ]]; then
    dist_load "$dir" "$start" "$secs" "${args[@]}" -workers "${RAMP_WORKERS:-8192}" >"$dir/load.log" 2>&1 &
  else
    $X load -targets "$TARGETS" -issuer "$ISSUER" -duration "${secs}s" -rate "$start" -workers "${RAMP_WORKERS:-8192}" -probe-every 200ms -out "$dir" "${args[@]}" >"$dir/load.log" 2>&1 &
  fi
  local lp=$!
  while kill -0 $lp 2>/dev/null && kill -0 $sp 2>/dev/null; do sleep 1; done
  if ! kill -0 $sp 2>/dev/null; then wait $sp; status=$?; kill $lp 2>/dev/null; fi
  wait $lp 2>/dev/null; kill $sp $op 2>/dev/null; wait $sp $op 2>/dev/null
  endpoints # the vnodes started during the step are audited too
  [[ $status == 0 ]] && timeout "${AUDIT_SECONDS:-900}" "$AUDITOR" audit -targets "$TARGETS" -monitors "$MONITORS" -monitors-file "$dir/monitors" -issuer "$ISSUER" -sample "${SAMPLE:-20000}" -out "$dir" >"$dir/audit.log" 2>&1
  kill "$lister" 2>/dev/null; wait "$lister" 2>/dev/null
  local rp=/opt/indexus/ramp.py
  [[ -f $rp ]] || { [[ -n "${RESULTS_URL:-}" ]] && aws s3 cp "${RESULTS_URL%/results}/plan/ramp.py" $rp --only-show-errors 2>/dev/null; } || rp="$(dirname "$0")/../ramp.py"
  python3 "$rp" "$dir" "$n" "$dbl" "$hold" | tee -a "$OUT/ramp.jsonl"
  [[ -n "${RESULTS_URL:-}" ]] && aws s3 cp "$dir" "$RESULTS_URL/$(basename "$dir")/" --recursive --exclude acked.jsonl --only-show-errors || true
  tail -n1 "$OUT/ramp.jsonl" | grep -q '"green": true'
}
ramp() {
  local i N d
  for i in $(seq 1 "${RAMP_PASSES:-1}"); do
    if [[ "${RAMP_ELASTIC:-0}" == 1 ]]; then
      [[ -n "${PROVISIONER:-}" ]] || { echo "RAMP_ELASTIC=1 needs a provisioner" >>"$OUT/ramp.log"; return 1; }
      for d in ${RAMP_DOUBLES:-300 120 60 30}; do
        cluster 4 "compaction=true&provisioner=$PROVISIONER&items=${ITEMS:-500000}"
        ramp_step 4 "$d" "$OUT/ramp-elastic-d$d-p$i" $(( 4 * ${RAMP_START:-500} )) || break
      done
    else
      for N in $SIZES; do
        cluster "$N" "compaction=false&provisioner="
        ramp_step "$N" "${RAMP_DOUBLE:-60}" "$OUT/ramp-n$N-p$i" $(( N * ${RAMP_START:-500} )) || true
      done
    fi
  done
}
if [[ "${MODE:-all}" == ramp ]]; then ramp; exit 0; fi
# Stall reproduction (2026-11-20): with a fixed set of vnodes (no
# provisioner: no split, no fold, no transfer), does a node still stop
# answering for seconds? Fill to ~150 000 items per vnode, then steady
# writes, the same with reads of totals, no writes, then half the writes on
# one prefix. Stalls in the
# steady or quiet phase come from the node's own background work, not from
# topology changes. The observer saves the stacks of a node that takes more
# than 2 s to answer.
stall() {
  local n=${STALL_VNODES:-8} r=${STALL_RATE:-250}
  cluster "$n" "compaction=false&provisioner=&items=0" # no fold, no split
  PHASE_AUDIT=0 phase stall-fill "${STALL_FILL:-200}" 20 -scenario normal -rate $((n * 750)) -workers 4096 -discover-every 5s -seed 1
  PHASE_AUDIT=0 phase stall-steady 180 20 -scenario normal -rate $((n * r)) -workers 4096 -discover-every 5s -seed 2
  # The same writes plus reads of collection totals, which go through the
  # summary tree whose lock every write takes (summary.Tree.Mu).
  PHASE_AUDIT=0 phase stall-totals 180 20 -scenario normal -rate $((n * r)) -workers 4096 -discover-every 5s -readers 64 -read-rate $((n * 50)) -total-share 1 -seed 5
  PHASE_AUDIT=0 phase stall-quiet 120 0 -scenario normal -rate 1 -workers 16 -seed 3
  phase stall-skew 180 60 -scenario spike -spike-start 0s -spike-length 180s -spike-factor 2 -rate $((n * r)) -workers 4096 -discover-every 5s -seed 4
}
# The same data, but the cluster grows under steady writes: 4 vnodes fill,
# then the item threshold makes them split up to the slots there are, then
# steady writes and no writes go on after the last split. Stalls here and not
# in stall() come from what a split leaves behind (2026-11-20: summary edges
# re-pushed to new parents).
stall_grow() {
  local n=${STALL_VNODES:-8} r=${STALL_RATE:-250}
  cluster 4 "compaction=false&provisioner=$PROVISIONER&items=${STALL_ITEMS:-150000}"
  # STALL_FILL_RATE under the write threshold (70 % of the cap on 4 vnodes)
  # makes the splits come from items, with data in them, as on the steps
  # runs; above it they come from write pressure, early and nearly empty.
  PHASE_AUDIT=0 phase grow-fill "${STALL_FILL:-200}" 20 -scenario normal -rate "${STALL_FILL_RATE:-$((n * 750))}" -workers 4096 -discover-every 5s -seed 1
  PHASE_AUDIT=0 phase grow-steady 300 20 -scenario normal -rate $((n * r)) -workers 4096 -discover-every 5s -seed 2
  phase grow-quiet 120 30 -scenario normal -rate 1 -workers 16 -seed 3
}
if [[ "${MODE:-all}" == stall ]]; then stall; exit 0; fi
if [[ "${MODE:-all}" == stall-grow ]]; then stall_grow; exit 0; fi
if [[ "${MODE:-all}" == steps ]]; then steps; exit 0; fi

# 1. Scaling at fixed size: same load per node at every size (Q1-Q5, Q9).
for N in $SIZES; do
  cluster "$N" "compaction=false&provisioner="
  phase "n$N-weak" 150 0 -scenario normal -rate $((150 * N)) -workers 1024 -readers 64 -read-rate $((30 * N))
  phase "n$N-strong" 180 0 -scenario normal -rate $((100 * N)) -ramp $((100 * N)) -ramp-every "$(secs 30)s" -workers 2048
done

# 2. One atomic zone on the largest size (Q8).
phase "hot" 120 0 -scenario hot -rate 500 -ramp 500 -ramp-every "$(secs 20)s" -workers 1024

# 3. Churn: crash and restart 10% of the vnodes one minute in (Q7).
N=${SIZES##* }
cluster "$N" "compaction=false&provisioner="
(
  sleep "$(secs 60)"
  for k in $(seq 1 $(( (N + 9) / 10 ))); do
    a=${AG[$((RANDOM % ${#AG[@]}))]}
    if [[ "${RESOURCE_SLOTS:-false}" == true ]]; then
      s=$(curl -sf "$a/slots" | python3 -c 'import json, random, sys; live=[r["slot"] for r in json.load(sys.stdin) if r.get("alive") and (sys.argv[1] != sys.argv[2] or r["slot"] != 0)]; print(random.choice(live) if live else "")' "$a" "${AG[0]}")
      [[ -n "$s" ]] || continue
    else
      s=$(( RANDOM % SLOTS ))
      [[ "$a" == "${AG[0]}" && $s == 0 ]] && s=1 # keep the genesis up
    fi
    echo "restart $a slot $s" >>"$OUT/churn-restarts.txt"
    curl -sf -X POST "$a/restart?slot=$s" >/dev/null
  done
) &
phase "churn" 240 0 -scenario normal -rate $((100 * N)) -workers 1024 -readers 32 -read-rate $((20 * N))
wait

# 4. Dynamics (above).
dynamics
