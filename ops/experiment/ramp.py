#!/usr/bin/env python3
"""Verdict of one ramp step (RAMP-1): ramp.py DIR VNODES [DOUBLE_S HOLD_S].

DIR holds load.jsonl (one line per second, summed over the load machines),
and optionally audit.json, knee.json (or loader-*/knee.json) and
incident/reason.txt. Prints one JSON line:

- best_green: the highest acknowledged rate held for WINDOW seconds with at
  least GREEN of the offered writes acknowledged, no arrival shed and an
  intended p99 (from the scheduled arrival, no coordinated omission) at most
  P99_MS. This is the sustainable throughput, per vnode in best_green_per_vnode.
- knee: the offered rate when the load stopped itself (stop-below), or None.
- doublings: per doubling period, offered and acked per second and the
  seconds from the period's start to the first 5 s window at 95% acked
  (absorb_s): how fast the cluster follows a 2x.
- loader_cores_max: near machine_cores, the load machine was the limit.

Environment: WINDOW (10), GREEN (0.99), P99_MS (1000)."""
import glob, json, os, sys

WINDOW = int(os.environ.get("WINDOW", 10))
GREEN = float(os.environ.get("GREEN", 0.99))
P99_MS = float(os.environ.get("P99_MS", 1000))


def p99_ms(hist):
    n = sum(hist)
    if not n:
        return None
    acc = 0
    for i, c in enumerate(hist):
        acc += c
        if acc >= 0.99 * n:
            return (1 << i) / 1000.0  # upper bound of the log2 bucket, in ms
    return None


def add(a, b):
    a = list(a)
    a += [0] * (len(b) - len(a))
    for i, c in enumerate(b):
        a[i] += c
    return a


def main():
    d, n = sys.argv[1], int(sys.argv[2])
    double = float(sys.argv[3]) if len(sys.argv) > 3 else 0
    hold = float(sys.argv[4]) if len(sys.argv) > 4 else 30
    path = os.path.join(d, "load.jsonl")
    rows = [json.loads(l) for l in open(path) if l.startswith("{")] if os.path.exists(path) else []
    best = None
    for i in range(0, max(len(rows) - WINDOW + 1, 0)):
        w = rows[i:i + WINDOW]
        off = sum(r["offered"] for r in w)
        ack = sum(r["acked"] for r in w)
        shed = sum(r.get("shed", 0) for r in w)
        h = []
        for r in w:
            h = add(h, r.get("intended_latency_us_log2") or r.get("latency_us_log2") or [])
        p = p99_ms(h)
        if off and ack >= GREEN * off and shed == 0 and p is not None and p <= P99_MS:
            rate = ack / WINDOW
            if best is None or rate > best["rate"]:
                best = {"rate": round(rate), "t": rows[i]["t"], "p99_ms": p}
    doublings = []
    if double > 0 and rows:
        end = rows[-1]["t"]
        k = 0
        while hold + k * double < end:
            a, b = hold + k * double, hold + (k + 1) * double
            part = [r for r in rows if a <= r["t"] < b]
            if not part:
                break
            off = sum(r["offered"] for r in part)
            ack = sum(r["acked"] for r in part)
            absorb = None
            for j in range(len(part) - 4):
                o = sum(r["offered"] for r in part[j:j + 5])
                if o and sum(r["acked"] for r in part[j:j + 5]) >= 0.95 * o:
                    absorb = round(part[j]["t"] - a, 1)
                    break
            doublings.append({"k": k, "offered_per_s": round(off / len(part)), "acked_per_s": round(ack / len(part)),
                              "ratio": round(ack / off, 4) if off else None, "absorb_s": absorb})
            k += 1
    knees = []
    for f in [os.path.join(d, "knee.json")] + sorted(glob.glob(os.path.join(d, "loader-*", "knee.json"))):
        if os.path.exists(f):
            try:
                knees.append(json.load(open(f)))
            except ValueError:
                pass
    knee = None
    if knees:
        knee = {"t": min(k["t"] for k in knees), "offered_rate": round(sum(k["offered_rate"] for k in knees))}
    report = {}
    if os.path.exists(os.path.join(d, "audit.json")):
        report = json.load(open(os.path.join(d, "audit.json")))
    durable = bool(report) and report.get("missing", 1) == 0 and report.get("mismatch_keys_lost", 0) == 0 \
        and report.get("double_owned_zones", 1) == 0 and report.get("read_errors", 1) == 0
    reason_path = os.path.join(d, "incident", "reason.txt")
    reason = open(reason_path).read().strip() if os.path.exists(reason_path) else ""
    reads = sum(r.get("reads", 0) for r in rows)
    read_failed = sum(r.get("read_failed", 0) for r in rows)
    read_missing = sum(r.get("read_missing", 0) for r in rows)
    read_hist, total_hist = [], []
    for r in rows:
        read_hist = add(read_hist, r.get("read_latency_us_log2") or [])
        total_hist = add(total_hist, r.get("total_latency_us_log2") or [])
    totals = sum(r.get("totals", 0) for r in rows)
    totals_exact = sum(r.get("totals_exact", 0) for r in rows)
    out = dict(
        dir=os.path.basename(os.path.normpath(d)), vnodes=n, seconds=len(rows),
        best_green=best["rate"] if best else 0, best_green_per_vnode=round(best["rate"] / n) if best else 0,
        best_green_t=best["t"] if best else None, best_green_p99_ms=best["p99_ms"] if best else None,
        knee=knee, doublings=doublings,
        peak_offered=round(max((r.get("offered_rate", 0) for r in rows), default=0)),
        loader_cores_max=round(max((r.get("cpu_cores", 0) for r in rows), default=0), 2),
        loader_machine_cores=max((r.get("machine_cores", 0) for r in rows), default=0),
        durable=durable, audit=report.get("pass"), lost=report.get("mismatch_keys_lost", 0), sentinel_stop=reason,
        green=durable and not reason and best is not None and read_missing == 0)
    if reads or read_failed or read_missing:
        out.update(reads=reads, read_failed=read_failed, read_missing=read_missing, read_p99_ms=p99_ms(read_hist))
    if totals:
        out.update(totals=totals, totals_exact=totals_exact, total_p99_ms=p99_ms(total_hist))
    print(json.dumps(out))


if __name__ == "__main__":
    main()
