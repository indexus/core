"""Replay CPU admission on physical host samples during injection.

Usage: python3 receiver_margin.py RUN/steps-1/machines.jsonl --start EPOCH
One row is one host observation. Other placement checks, competing requests,
and CPU after a rejected placement are outside this replay.
"""
import argparse
import json
import math
from collections import Counter, defaultdict
from statistics import median


def summarize(rows, start, end, budgets):
    counts = Counter()
    eligible = Counter()
    observed = Counter()
    for row in rows:
        if not start <= row['wall'] < end:
            continue
        counts['host_samples'] += 1
        f = row.get('facts', row)
        if not (f.get('host_cpu_ok') and f.get('cpu_ok') and f.get('disk_ok') and
                f.get('mem_ok') and (f.get('cores') or 0) > 0 and
                all(isinstance(f.get(k), (float, int)) and math.isfinite(f[k])
                    for k in ('cpu_pct', 'host_cpu_pct', 'disk_free_pct', 'mem_free_pct'))):
            counts['unknown'] += 1
            continue
        observed['host_cpu_over_60'] += f['host_cpu_pct'] > 60
        observed['effective_cpu_over_60'] += f['cpu_pct'] > 60
        if (f.get('free') or 0) <= 0 or f.get('receiving') != 0 or min(f['mem_free_pct'], f['disk_free_pct']) < 40:
            continue
        counts['room_and_no_receiver'] += 1
        for cores in budgets:
            if f['cpu_pct'] + 100 * cores / f['cores'] <= 60:
                eligible[cores] += 1
    return dict(counts=counts, cpu=observed,
                candidate_samples_by_receiver_cores={str(k): eligible[k] for k in budgets})


def single_arrivals(rows, plateaus, max_gap=25):
    """Four consecutive host observations, with one arrival during a constant plateau.

    The reported elevation includes concurrent writes, outgoing transfers and other
    host work. Polls can miss CPU spikes between observations.
    """
    hosts = defaultdict(list)
    for row in rows:
        hosts[row.get('agent') or row['host']].append(row)
    peaks, changes = [], []
    for history in hosts.values():
        history.sort(key=lambda row: row['wall'])
        for window in zip(history, history[1:], history[2:], history[3:]):
            times = [r['wall'] for r in window]
            if not any(lo <= times[0] and times[-1] < hi for lo, hi in plateaus):
                continue
            if any(not 0 < b - a <= max_gap for a, b in zip(times, times[1:])):
                continue
            before, *after = [r.get('facts', r) for r in window]
            facts = [before, *after]
            if not all(f.get('host_cpu_ok') and isinstance(f.get('host_cpu_pct'), (int, float))
                       and f.get('cores') == before.get('cores') for f in facts):
                continue
            if not before.get('cores') or before.get('running') is None or before.get('receiving') != 0:
                continue
            if any(f.get('running') != before['running'] + 1 for f in after):
                continue
            peak = max(f['host_cpu_pct'] for f in after)
            peaks.append(peak)
            changes.append((peak - before['host_cpu_pct']) * before['cores'] / 100)
    return dict(events=len(peaks), observed_over_60=sum(p > 60 for p in peaks),
                median_peak_increase_cores=median(changes) if changes else None,
                max_peak_increase_cores=max(changes, default=None))


def samples(path):
    with open(path) as file:
        for line in file:
            if line.strip():
                yield json.loads(line)


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('path')
    p.add_argument('--start', required=True, type=float, help='first wall-clock second of the window')
    p.add_argument('--seconds', default=400, type=float)
    p.add_argument('--plateau', type=float, help='plateau duration for single-arrival observations')
    p.add_argument('--ramp', type=float, default=40)
    args = p.parse_args()
    rows = list(samples(args.path))
    summary = summarize(rows, args.start, args.start + args.seconds, [1, 1.25, 1.5, 1.75])
    if args.plateau is not None:
        windows = []
        lo = args.start
        if args.plateau <= 0 or args.ramp < 0:
            p.error('plateau must be positive and ramp non-negative')
        while lo < args.start + args.seconds:
            windows.append((lo, min(lo + args.plateau, args.start + args.seconds)))
            lo += args.ramp + args.plateau
        summary['single_arrivals'] = single_arrivals(rows, windows)
    print(json.dumps(summary, indent=2))
