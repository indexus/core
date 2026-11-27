"""Fit a conditional CPU cost model to per-process counter deltas on four-vCPU hosts.

Rebuild the anonymous 30-second observations with --metrics path/to/metrics.jsonl.
Only observed process time contributes. A new process starts a new baseline.
"""

import argparse
import csv
import json
from collections import defaultdict
from pathlib import Path
from urllib.parse import urlsplit

import numpy as np
from scipy.optimize import nnls


WINDOW = 30
TRAIN_END = 300
CORES = 4
HERE = Path(__file__).resolve().parent
FEATURES = ["vnode_seconds", "writes", "summary_calls", "summary_mb", "handoff_mb"]
TRANSFER = {"preload", "install-zone", "handoff", "admit", "install", "stage"}


def counter_vector(metrics):
    calls = summary_bytes = handoff_bytes = 0
    for side in ("mesh_sent", "mesh_handled"):
        for op, record in metrics.get(side, {}).items():
            size = record["request_bytes"] + record["response_bytes"]
            if op in ("summary-get", "summary-contribute"):
                calls += record["calls"]
                summary_bytes += size
            elif op in TRANSFER:
                handoff_bytes += size
    return np.array([metrics["process"]["cpu_seconds"], metrics["writes"].get("applied", 0),
                     calls, summary_bytes / 1e6, handoff_bytes / 1e6])


def extract(path, output):
    previous = {}
    bins = defaultdict(lambda: np.zeros(6))
    hosts = {}
    with path.open() as stream:
        for line in stream:
            row = json.loads(line)
            metrics = row.get("metrics") or {}
            if not metrics.get("name") or "cpu_seconds" not in metrics.get("process", {}):
                continue
            monitor, now = row["monitor"], row["t"]
            host = urlsplit(monitor).hostname
            host_id = hosts.setdefault(host, len(hosts) + 1)
            uptime = metrics["uptime_seconds"]
            totals = counter_vector(metrics)
            old = previous.get(monitor)
            if old is not None and now <= old[0]:
                continue
            previous[monitor] = (now, uptime, metrics["name"], totals)
            if old is None:
                continue
            start, previous_uptime, previous_name, previous_totals = old
            elapsed = now - start
            if (metrics["name"] != previous_name or uptime <= previous_uptime
                    or not 0 < elapsed <= WINDOW or start < TRAIN_END < now):
                continue
            delta = totals - previous_totals
            if min(delta) < 0:
                continue
            sample = np.concatenate(([elapsed], delta))
            for index in range(int(start // WINDOW), int(now // WINDOW) + 1):
                lower = index * WINDOW
                overlap = min(now, lower + WINDOW) - max(start, lower)
                if overlap > 0 and 0 <= lower < 900:
                    bins[host_id, lower] += sample * (overlap / elapsed)
    with output.open("w", newline="") as stream:
        writer = csv.writer(stream, lineterminator="\n")
        writer.writerow(["host", "start", "vnode_seconds", "cpu_seconds", *FEATURES[1:]])
        for (host, start), values in sorted(bins.items()):
            writer.writerow([host, start, *(round(value, 6) for value in values)])


def evaluate(path):
    with path.open() as stream:
        rows = list(csv.DictReader(stream))
    start = np.array([int(row["start"]) for row in rows])
    y = np.array([float(row["cpu_seconds"]) for row in rows])
    train = start < TRAIN_END
    result = {}
    for model, features in [("writes", FEATURES[:2]), ("writes_and_mesh", FEATURES)]:
        x = np.array([[float(row[key]) for key in features] for row in rows])
        scale = np.maximum(x[train].max(axis=0), 1)
        weights, _ = nnls(x[train] / scale, y[train])
        weights /= scale
        predicted = np.clip(x @ weights, 0, WINDOW * CORES)
        result[model] = {"coefficients": dict(zip(features, weights))}
        for phase, selected in [("load", (start >= TRAIN_END) & (start < 660)),
                                ("16k", (start >= 540) & (start < 660)),
                                ("after_load", (start >= 660) & (start < 900))]:
            errors_pct = np.abs(predicted[selected] - y[selected]) * 100 / (CORES * WINDOW)
            result[model][phase] = {"host_windows": len(errors_pct),
                                    "mae_cpu_points": float(errors_pct.mean()),
                                    "p90_absolute_error_points": float(np.percentile(errors_pct, 90))}
            if phase == "16k":
                by_host = defaultdict(lambda: np.zeros(2))
                for i in np.flatnonzero(selected):
                    by_host[rows[i]["host"]] += (y[i], predicted[i])
                hottest = max(by_host.values(), key=lambda pair: pair[0])
                actual, estimate = hottest * (100 / (120 * CORES))
                result[model][phase].update(hottest_actual_pct=actual, hottest_estimate_pct=estimate)
    return result


if __name__ == "__main__":
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("--metrics", type=Path)
    cli.add_argument("--observations", type=Path, default=HERE / "cpu-30s.csv")
    args = cli.parse_args()
    if args.metrics:
        extract(args.metrics, args.observations)
    print(json.dumps(evaluate(args.observations), indent=2))
