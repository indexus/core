#!/usr/bin/env python3
"""Summarize raw writes and resources, preserving failures and sample counts."""

import argparse
import collections
import json
import math
from pathlib import Path


def rows(path):
    if not path.exists():
        return []
    return [json.loads(line) for line in path.open()]


def quantiles(values, divisor=1000):
    values = sorted(values)
    if not values:
        return {"n": 0}
    return dict(
        n=len(values),
        **{
            k: values[math.ceil(q * len(values)) - 1] / divisor
            for k, q in [("p50", 0.5), ("p95", 0.95), ("p99", 0.99), ("max", 1)]
        },
    )


def write_summary(writes):
    success = [w for w in writes if w["Success"]]
    result = dict(
        count=len(writes),
        failed=len(writes) - len(success),
        retried=sum(w["Attempts"] > 1 for w in success),
        direct=sum(w["Direct"] for w in success),
        degraded=sum(w["Degraded"] for w in success),
    )
    for key in ("LatencyUS", "IntendedUS", "RPCUS", "BackoffUS"):
        result[key] = quantiles([w[key] for w in success])
    result["queue_ms"] = quantiles([w["StartUS"] - w["DueUS"] for w in success])
    tail = sorted(success, key=lambda w: w["LatencyUS"])[-max(1, math.ceil(len(success) * 0.01)) :]
    result["slowest_1pct"] = dict(
        n=len(tail),
        retried=sum(w["Attempts"] > 1 for w in tail),
        mean_rpc_ms=sum(w["RPCUS"] for w in tail) / max(1, len(tail)) / 1000,
        mean_backoff_ms=sum(w["BackoffUS"] for w in tail) / max(1, len(tail)) / 1000,
    )
    result["errors"] = dict(collections.Counter(e for w in writes for e in w.get("Errors", [])))
    return result


def resource_summary(resources, start, finish, cpus):
    rs = [r for r in resources if start <= r["unix_ns"] <= finish]
    hosts = [r for r in rs if "cpu_idle_pct" in r]
    result = dict(samples=len(rs), host_samples=len(hosts))
    for field in ("rss_bytes", "process_cpu_pct"):
        result[field] = quantiles([r[field] for r in rs if field in r], 1)
    result["probe_fullfsync_ms"] = quantiles([r["probe_fullfsync_us"] for r in rs])
    result["min_host_idle_pct"] = min((r["cpu_idle_pct"] for r in hosts), default=None)
    result["min_memory_free_pct"] = min((r["memory_free_pct"] for r in hosts), default=None)
    result["process_cpu_capacity_pct"] = result["process_cpu_pct"].get("max", 0) / cpus
    if len(hosts) > 1:
        result["swapout_pages_delta"] = hosts[-1]["vm_pages"].get("Swapouts", 0) - hosts[0][
            "vm_pages"
        ].get("Swapouts", 0)
        result["swapin_pages_delta"] = hosts[-1]["vm_pages"].get("Swapins", 0) - hosts[0][
            "vm_pages"
        ].get("Swapins", 0)
        rates = []
        for a, b in zip(hosts, hosts[1:]):
            if a.get("loopback") and b.get("loopback"):
                rates.append(
                    8
                    * (b["loopback"]["Ibytes"] - a["loopback"]["Ibytes"])
                    / (b["unix_ns"] - a["unix_ns"])
                    * 1000
                )
        result["max_loopback_mbps_one_direction"] = max(rates, default=None)
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("roots", type=Path, nargs="+")
    p.add_argument("--out", type=Path, required=True)
    args = p.parse_args()
    result = []
    for root in args.roots:
        manifest = (
            json.loads((root / "manifest.json").read_text())
            if (root / "manifest.json").exists()
            else {}
        )
        for run in sorted(root.glob("*-r*")):
            if not run.is_dir():
                continue
            events = rows(run / "events.jsonl")
            writes = rows(run / "writes.jsonl")
            if not writes:
                continue
            config = next(e for e in events if e.get("event") == "configuration")
            start = next(e["unix_ns"] for e in events if e.get("event") == "load-began")
            finish = start + max(w["FinishUS"] for w in writes) * 1000
            audit = next((e for e in events if e.get("event") == "audit"), None)
            samples = [e for e in events if e.get("event") == "sample"]
            r = dict(
                run=str(run),
                configuration=config,
                audit=audit,
                writes=write_summary(writes),
                initial_phases=write_summary(
                    [w for w in writes if w["DueUS"] <= config["phase_seconds"] * 4 * 1_000_000]
                ),
                resources=resource_summary(
                    rows(run / "resources.jsonl"), start, finish, manifest.get("cpu_count", 8)
                ),
                initial_phase_resources=resource_summary(
                    rows(run / "resources.jsonl"),
                    start,
                    start + config["phase_seconds"] * 4 * 1_000_000_000,
                    manifest.get("cpu_count", 8),
                ),
            )
            buckets = collections.defaultdict(list)
            for w in writes:
                n = w["Ready"]
                key = (
                    "unknown"
                    if not n
                    else "4-7"
                    if n < 8
                    else "8-15"
                    if n < 16
                    else "16-31"
                    if n < 32
                    else "32-63"
                    if n < 64
                    else "64"
                )
                buckets[key].append(w)
            r["by_ready_at_dispatch"] = {k: write_summary(v) for k, v in sorted(buckets.items())}
            groups = collections.defaultdict(list)
            for w in writes:
                groups["frozen" if w["Frozen"] else "no_frozen_observed"].append(w)
            r["by_frozen_at_dispatch"] = {k: write_summary(v) for k, v in groups.items()}
            r["sampler"] = dict(
                max_tick_lag_us=max((e.get("tick_lag_us", 0) for e in samples), default=0),
                max_work_us=max((e.get("sample_us", 0) for e in samples), default=0),
                max_queued=max((e["queued"] for e in samples), default=0),
                max_open=max((e.get("open", 0) for e in samples), default=0),
                max_frozen=max((e.get("frozen_count", 0) for e in samples), default=0),
            )
            measured = [s for s in samples if s["t"] * 1e9 <= finish - start]
            if len(measured) > 1:
                first, last = measured[0], measured[-1]
                seconds = last["t"] - first["t"]
                pause = (last["gc_pause_ns"] - first["gc_pause_ns"]) / 1e9
                r["gc_between_samples"] = dict(
                    elapsed_s=seconds,
                    cycles=last["gc_cycles"] - first["gc_cycles"],
                    stop_world_s=pause,
                    stop_world_wall_pct=100 * pause / seconds,
                    note="shared Go runtime across all simulated vnodes; excludes first second",
                )
            result.append(r)
    args.out.mkdir(parents=True, exist_ok=True)
    (args.out / "comparison.json").write_text(json.dumps(result, indent=2) + "\n")
    text = [
        "| Run | n | p50 ACK ms | p95 | p99 | p99 intended | retries | CPU host idle min % | memory free min % |",
        "|---|---:|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for r in result:
        w, res = r["writes"], r["resources"]
        q = w["LatencyUS"]
        text.append(
            f"| {Path(r['run']).name} | {w['count']} | {q.get('p50', 0):.2f} | {q.get('p95', 0):.2f} | {q.get('p99', 0):.2f} | {w['IntendedUS'].get('p99', 0):.2f} | {w['retried']} | {res['min_host_idle_pct']} | {res['min_memory_free_pct']} |"
        )
    (args.out / "comparison.md").write_text("\n".join(text) + "\n")
    print("\n".join(text))


if __name__ == "__main__":
    main()
