# timeout=600
# Short elasticity profile (≈5 min) for iteration: split at 5 000 items,
# 200 writes/s, x10 on one prefix for 60 s, 120 s of load, 60 s of calm,
# audit of 20 000 keys bounded to 75 s. Writes enter through every live node. Node events
# are copied every 20 s, so a cut run still explains itself. Machines of 3 slots:
# AGENTS at start (2), up to MACHINES_MAX started on demand with SPARE slots
# kept free and BOOT_DELAY seconds of boot (elastic_local.sh).
set -u
B=/tmp/indexus-bin; D=/tmp/indexus-elastic; O=ops/experiment/results/short-elastic-$(date +%Y%m%d-%H%M%S)
pgrep -fl "indexus-elastic|indexus-bin" && { echo "stale processes: stop"; exit 1; }
rm -rf $B $D; mkdir -p $B $O
for c in node issuer provisioner experiment; do go build -o $B/$c ./cmd/$c || exit 1; done
git rev-parse --short HEAD > $O/commit.txt; git status --short | grep -v '^??' >> $O/commit.txt
BIN=$B AGENTS=${AGENTS:-2} MACHINES_MAX=${MACHINES_MAX:-} SPARE=${SPARE:-0} BOOT_DELAY=${BOOT_DELAY:-0} IDLE_EXIT=${IDLE_EXIT:-0} WRITE_CAP=${WRITE_CAP:-0} SLOTS=3 START=3 ITEMS=${ITEMS:-5000} RETIRED_EXIT=30 DIR=$D ops/experiment/elastic_local.sh up || { DIR=$D ops/experiment/elastic_local.sh down; exit 1; }
events() { cat $D/*.log $D/agent*/*/*.log 2>/dev/null | grep -E "handoff (start|end)|msg=install|first write applied|split|fold|retire|releasing|scale|spawn|join|zone transfer progress|level=(WARN|ERROR)" > $O/events.log; }
trap 'events; DIR=$D ops/experiment/elastic_local.sh down; rm -rf $D $B' EXIT
( while sleep 20; do events; done ) & copier=$!
sleep 10
# Live slots of every machine started so far: "p2p" or "monitor" URLs.
alive() { for a in $(cat $D/agents); do curl -sf $a/slots; echo; done | python3 -c '
import sys,json
for l in sys.stdin:
    if l.strip():
        for r in json.loads(l):
            if r.get("alive"): print(r[sys.argv[1]])' $1; }
# The observer polls every slot of every machine that may start.
MON=$( (echo http://127.0.0.1:19000; for a in $(seq 0 $(( ${MACHINES_MAX:-${AGENTS:-2}} - 1 ))); do for s in 0 1 2; do echo http://127.0.0.1:$((19100 + 100 * a + s)); done; done) | paste -sd, -)
$B/experiment observe -monitors "$MON" -every 2s -duration $((${LOAD_SECONDS:-120} + 20 + ${CALM:-60}))s -out $O >$O/observe.log 2>&1 &
echo "load start $(date +%T)"
# Live counters: ops/experiment/results/live.html redraws every 5 s.
( python3 ops/experiment/live.py agg $O $O/live.jsonl local & a=$!
  while kill -0 $a 2>/dev/null; do sleep 5; python3 ops/experiment/live.py html $O/live.jsonl ops/experiment/results/live.html 2>/dev/null; done ) &
follower=$!
# Watchdog (fail fast), on the live counters: one line every 5 s; with no new
# vnode FAILFAST_AT seconds into the load (default 75: 45 s into the spike)
# it stops the load, so the run captures stacks now instead of finishing.
( start=$(date +%s); base=""
  while sleep 5; do
    t=$(( $(date +%s) - start )); python3 ops/experiment/live.py line $O/live.jsonl
    v=$(tail -1 $O/live.jsonl 2>/dev/null | python3 -c 'import sys,json; print(json.loads(sys.stdin.read() or "{}").get("vnodes", 0))' 2>/dev/null || echo 0)
    (( t >= 30 )) && [[ -z "$base" ]] && base=$v # vnodes when the spike starts
    if (( t >= ${FAILFAST_AT:-75} )) && [[ -n "$base" ]] && (( v <= base )); then
      echo "FAILFAST: no new vnode active ${t}s into the load (base $base)" | tee $O/FAILFAST
      pkill -f "$B/experiment load"; exit 0
    fi
    (( t > ${LOAD_SECONDS:-120} + 10 )) && exit 0
  done ) > $O/watch.log 2>&1 &
watchdog=$!
# LOAD overrides the load arguments (and LOAD_SECONDS its length), e.g. the
# steps scenario: LOAD="-scenario steps -rate 300 -step-ramp 30s -step-plateau 90s"
LOAD_SECONDS=${LOAD_SECONDS:-120}
$B/experiment load -targets http://127.0.0.1:21000 -discover-every 5s -issuer http://127.0.0.1:22000 \
  ${LOAD:--scenario spike -rate 200 -spike-start 30s -spike-length 60s -spike-factor 10} -duration ${LOAD_SECONDS}s -workers 512 -out $O >$O/load.log 2>&1
python3 - $O/load.jsonl <<'PY'
import json,sys
rows=[json.loads(l) for l in open(sys.argv[1]) if l.startswith('{"t"')]
print("offered", sum(r["offered"] for r in rows), "acked", sum(r["acked"] for r in rows), "shed", sum(r["shed"] for r in rows))
for r in rows[::15]: print(f't={r["t"]:4.0f} offered={r["offered"]:5d} acked={r["acked"]:5d} shed={r["shed"]:4d} entries={r.get("entries",0)}')
PY
kill $watchdog 2>/dev/null; cat $O/watch.log
if [[ -f $O/FAILFAST ]]; then CALM=0; fi
sleep ${CALM:-60} # CALM: seconds of calm after the load (folds need a few minutes)
events
python3 - $O/events.log <<'PY'
import re,sys
opens={};ends=[]
for l in open(sys.argv[1]):
    m=re.search(r'msg="?handoff (start|end)"? .*op=(\S+) warm=(\w+) attempt=(\d+)',l)
    if m and m.group(1)=='end':
        s=re.search(r'seconds=([\d.]+)',l).group(1); e=re.search(r'error=(.*)$',l).group(1)[:70]
        ends.append((m.group(2)[:20],m.group(3),m.group(4),float(s),e))
print("handoff attempts:",len(ends),"failed:",sum(1 for x in ends if x[4]!='<nil>'))
for x in ends:
    if x[4]!='<nil>' or x[3]>20: print(*x)
last={}
for l in open(sys.argv[1]):
    if 'zone transfer progress' in l:
        op=re.search(r'operation=(\S+)',l).group(1)[:20]; last[op]=re.search(r'zones=\d+ batches=\d+ elapsed=\S+',l).group(0)
for k,v in last.items(): print("transfer",k,v)
PY
# Quiet: no open handoff and no frozen transfer on any live node for three
# polls in a row (30 s), at most QUIET_MAX s: the audit checks where the
# cluster converged, not a cluster still folding (its reads would wait on 503
# and its nodes leave while it reads).
quiet=0; waited=0
while (( quiet < 3 && waited < ${QUIET_MAX:-300} )) && [[ ! -f $O/FAILFAST ]]; do
  busy=$(for m in http://127.0.0.1:19000 $(alive monitor); do curl -s --max-time 3 $m/metrics; echo; done | python3 -c '
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
echo "quiet after ${waited}s (busy polls reset the count)"
# A transfer open for more than 60 s, or a node that does not answer its
# monitor in 3 s: every node writes its goroutine stacks (SIGQUIT ends it),
# and the run stops here, with the stacks, instead of auditing a stuck cluster.
stuck=""
for m in http://127.0.0.1:19000 $(alive monitor); do
  old=$(curl -s --max-time 3 $m/metrics | python3 -c 'import sys,json; print(int(json.load(sys.stdin)["handoffs"]["open"]["oldest_seconds"]))' 2>/dev/null) || old=silent
  [[ -z "$old" ]] && continue
  # Silent counts only for a node that answered before: a joining node
  # serves its monitor once its first handoff is in.
  if [[ "$old" == silent ]] && ! grep -v '^{"error"' $O/metrics.jsonl | grep -q "\"monitor\":\"$m\""; then continue; fi
  if [[ "$old" == silent ]] || (( old > 60 )); then stuck="$stuck $m:$old"; fi
done
[[ -f $O/FAILFAST ]] && stuck="$stuck failfast"
if [[ -n "$stuck" && -n "$D" && "$D" == /tmp/* ]]; then
  echo "stuck:$stuck (monitor:seconds open, or silent)"
  pkill -QUIT -f "$D/" ; sleep 3
  mkdir -p $O/stacks
  for f in $D/*.log $D/agent*/*/*.log; do [[ -f $f ]] && gzip -c $f > $O/stacks/$(echo ${f#$D/} | tr / _).gz; done
  python3 ops/experiment/stacks.py $O/stacks
  kill $copier 2>/dev/null; rm -f $O/acked.jsonl; echo "OUT=$O"; exit 0
fi
cat $D/machines.log
M=$( (echo http://127.0.0.1:19000; alive monitor) | paste -sd, -); T=$(alive p2p | paste -sd, -)
$B/experiment audit -targets "http://127.0.0.1:21000,$T" -monitors "$M" -issuer http://127.0.0.1:22000 -sample 20000 -out $O >$O/audit.log 2>&1 &
a=$!; ( sleep ${AUDIT:-75}; kill -9 $a 2>/dev/null ) & w=$!   # macOS has no timeout(1)
wait $a; echo "audit exit $? (137: cut at ${AUDIT:-75} s)"; kill $w 2>/dev/null
tail -3 $O/audit.log
kill $copier $follower 2>/dev/null; pkill -f "live.py agg $O" 2>/dev/null
python3 ops/experiment/live.py html $O/live.jsonl ops/experiment/results/live.html 2>/dev/null
rm -f $O/acked.jsonl
echo "OUT=$O"
