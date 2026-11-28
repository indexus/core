"""Replay mixed-load telemetry and test a write-capacity forecast on a later plateau."""
import argparse
from collections import Counter
import json
from pathlib import Path
from statistics import median


def records(path):
    with path.open() as stream:
        return [json.loads(line) for line in stream if line.strip()]


def backlog(row):
    return row["queued"] + row["in_flight"]


def exits(rows, initial):
    """Infer terminal jobs from conservation, avoiding per-sample snapshot skew."""
    admitted = sum(row["offered"] - row["shed"] for row in rows)
    return admitted + initial - backlog(rows[-1])


def read_rate(rows, readers, target):
    """Conditional throughput from successful read service times and worker pacing."""
    buckets = Counter()
    for row in rows:
        buckets.update({i: n for i, n in enumerate(row.get("read_latency_us_log2", [])) if n})
    count = sum(buckets.values())
    if not count:
        return None
    period = readers / target

    def estimate(position):
        work = sum(n * max(period, position * 2**i / 1_000_000) for i, n in buckets.items())
        return round(readers * count / work, 1)

    return {"fast_bucket_edge": estimate(.5), "midpoint": estimate(.75), "slow_bucket_edge": estimate(1)}


def replay_writes(rows, initial, capacity, service_rate, ack_share):
    """One-second bounded queue with a fixed rate learned from an earlier period."""
    outstanding, shed, acked = initial, 0, 0.
    for row in rows:
        available = outstanding + row["offered"]
        served = min(service_rate, available)
        outstanding = min(capacity, available - served)
        shed += available - served - outstanding
        acked += served * ack_share
    return {"acked_s": round(acked / len(rows), 1), "shed": shed, "end_backlog": outstanding}


def fleet_window(frames):
    snapshots = []
    phases = Counter()
    for frame in frames:
        if frame.get("metrics_stale"):
            continue
        view = frame["fleet"]
        rates = Counter()
        for node in view["vnodes"]:
            if node["state"] != "stale" and (node.get("applied_s") or 0) > 0:
                rates[node["machine"]] += node["applied_s"]
        phases.update(move["phase"] for move in view["transfers"]
                      if not move.get("stale") and move.get("phase"))
        total = sum(rates.values())
        effective = total**2 / sum(rate**2 for rate in rates.values()) if total else 0
        snapshots.append((len(view["machines"]), len(rates), effective))
    if not snapshots:
        return None
    return {
        "samples": len(snapshots),
        "observed_hosts_median": median(s[0] for s in snapshots),
        "writing_hosts_median": median(s[1] for s in snapshots),
        "effective_writing_hosts_median": round(median(s[2] for s in snapshots), 2),
        "observed_transfer_phases": dict(phases),
    }


def host_timeline(frames):
    hosts = {}
    for frame in frames:
        if frame.get("metrics_stale"):
            continue
        t = frame["t"]
        for machine in frame["fleet"]["machines"]:
            observation = hosts.setdefault(machine["id"], {"first_observed_s": t})
            if machine.get("ready_offer"):
                observation.setdefault("first_offer_s", t)
        for node in frame["fleet"]["vnodes"]:
            if node["state"] != "stale" and (node.get("applied_s") or 0) > 0:
                observation = hosts.setdefault(node["machine"], {"first_observed_s": t})
                observation.setdefault("first_write_s", t)
    return hosts


def report(load, live, ramp, plateau, readers, read_target):
    # Beginning/end snapshots may not bracket counters atomically. Over one
    # plateau their residual measures terminal jobs other than successes.
    windows = []
    sections = []
    for start in range(0, 4 * (ramp + plateau), ramp + plateau):
        stop = start + plateau
        rows = [row for row in load if start + .75 < row["t"] < stop + .75]
        if len(rows) != plateau:
            raise ValueError(f"incomplete plateau from {start}s: {len(rows)} records")
        preceding = [row for row in load if row["t"] <= start + .75]
        initial = backlog(preceding[-1]) if preceding else 0
        terminal = exits(rows, initial)
        acked = sum(row["acked"] for row in rows)
        windows.append({
            "seconds": [start, stop],
            "offered_s": round(sum(row["offered"] for row in rows) / plateau, 1),
            "acked_s": round(acked / plateau, 1),
            "shed": sum(row["shed"] for row in rows),
            "end_backlog": backlog(rows[-1]),
            "non_ack_exits_inferred": terminal - acked,
            "reads_observed_s": round(sum(row.get("reads", 0) for row in rows) / plateau, 1),
            "read_rate_from_observed_service": read_rate(rows, readers, read_target),
            "fleet": fleet_window(frame for frame in live if start < frame["t"] <= stop),
        })
        sections.append((rows, initial, terminal, acked))
    _, _, terminal, acked = sections[2]
    holdout, initial, _, _ = sections[3]
    return {
        "windows": windows,
        "host_timeline": host_timeline(live),
        "first_full_write_backlog_s": next(
            (round(row["t"], 3) for row in load if backlog(row) == row["workers"] * 5),
            None,
        ),
        "write_forecast_holdout": replay_writes(
            holdout, initial, 5 * holdout[0]["workers"], round(terminal / plateau),
            min(1., acked / terminal),
        ),
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("result", type=Path)
    parser.add_argument("--ramp", type=int, default=40)
    parser.add_argument("--plateau", type=int, default=70)
    parser.add_argument("--readers", type=int, default=96)
    parser.add_argument("--read-target", type=float, default=1000)
    args = parser.parse_args()
    if min(args.ramp, args.plateau, args.readers, args.read_target) <= 0:
        parser.error("ramp, plateau and reader settings must be positive")
    print(json.dumps(report(records(args.result / "steps-1/load.jsonl"),
                            records(args.result / "live.jsonl"), args.ramp, args.plateau,
                            args.readers, args.read_target), indent=2))
