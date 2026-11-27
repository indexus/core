#!/usr/bin/env python3
"""Aggregate repeated passes of a run (REM-94).

    python3 ops/experiment/passes.py RESULTS_DIR [RESULTS_DIR ...]

Reads every push.jsonl under the directories (rows written by MODE=passes,
with a "pass" field) and prints, per (vnodes, offered rate): the number of
passes, the mean and the coefficient of variation of acknowledged writes/s,
the median of each tail across passes, and whether every pass was green.
"""
import json, statistics, sys
from pathlib import Path

FIELDS = ["acked_per_s", "per_vnode", "write_p99_ms", "commit_p99_ms", "fresh_p50_ms", "fresh_p99_ms", "busiest_node_cores", "loader_cores"]


def rows(dirs):
    for d in dirs:
        for f in sorted(Path(d).rglob("push.jsonl")):
            for line in f.read_text().splitlines():
                try:
                    r = json.loads(line)
                except json.JSONDecodeError:
                    continue
                r["_run"] = f.parent.name
                yield r


def main():
    groups = {}
    for r in rows(sys.argv[1:]):
        groups.setdefault((r["_run"], r["n"], int(r["rate"])), []).append(r)
    print("| run | vnodes | offered/s | passes | acked/s mean | CV | per vnode | write p99 ms | commit p99 ms | fresh p50 ms | fresh p99 ms | busiest node cores | all green |")
    print("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |")
    out = []
    for (run, n, rate), rs in sorted(groups.items()):
        acked = [r["acked_per_s"] for r in rs]
        mean = statistics.fmean(acked)
        cv = statistics.stdev(acked) / mean if len(acked) > 1 and mean else 0.0
        med = {}
        for k in FIELDS:
            v = [r[k] for r in rs if r.get(k) is not None]
            med[k] = statistics.median(v) if v else None
        green = all(r.get("green") for r in rs)
        fmt = lambda v, d=0: "—" if v is None else f"{v:,.{d}f}".replace(",", " ")
        print(f"| {run} | {n} | {fmt(rate)} | {len(rs)} | {fmt(mean)} | {cv:.1%} | {fmt(mean / n)} | {fmt(med['write_p99_ms'], 1)} | {fmt(med['commit_p99_ms'], 1)} | {fmt(med['fresh_p50_ms'])} | {fmt(med['fresh_p99_ms'])} | {fmt(med['busiest_node_cores'], 2)} | {'yes' if green else 'no'} |")
        out.append(dict(run=run, n=n, rate=rate, passes=len(rs), acked_mean=mean, acked_cv=cv, green=green, **{k + "_median": med[k] for k in FIELDS}))
    for d in sys.argv[1:]:
        (Path(d) / "passes.json").write_text(json.dumps(out, indent=1))


if __name__ == "__main__":
    main()
