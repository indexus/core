"""Compare the cost of throttling vnode placement across independent machines.

Both policies reserve one copy lane on each physical endpoint before the hook
starts. The fleet policy also pauses admissions for two seconds *after* a hook
succeeds; other hooks can already be in flight. Both limit EC2 boot starts to
one per two seconds. Resource estimates and copy durations are model inputs.

Run python3 host_pair_admission.py --scenario mixed_roles --hook-ms 250.
"""
import argparse
from collections import defaultdict
from dataclasses import dataclass
from enum import IntEnum
import heapq
import json
import math
import random
from statistics import median


HOSTS_PER_ROLE = 8
REQUESTS = 64
CPU_CEILING = 60
COPY_CPU = 10  # percentage points on both physical endpoints
INTERVAL = 2.0  # seconds between boots or after a successful fleet-scoped hook
SCENARIOS = ("stock", "hot_donor", "few_receivers", "cold_boots", "mixed_roles")


class Event(IntEnum):
    HOOK_COMPLETED = 0
    ASK = 1


@dataclass(frozen=True)
class Copy:
    request: int
    source: int
    target: int
    requested_at: float
    admitted_at: float
    finished_at: float

    @property
    def wait(self):
        return self.admitted_at - self.requested_at


@dataclass
class Run:
    copies: list[Copy]
    calls: int
    boots: int
    peak_cpu: float

    def summary(self):
        waits = sorted(copy.wait for copy in self.copies)
        by_source = defaultdict(list)
        for copy in self.copies:
            by_source[copy.source].append(copy.wait)
        return {
            "placed": len(self.copies),
            "calls": self.calls,
            "p50_admit_s": median(waits),
            "p95_admit_s": waits[math.ceil(.95 * len(waits)) - 1],
            "longest_wait_s": max(waits),
            "last_copy_s": max(copy.finished_at for copy in self.copies),
            "worst_donor_median_s": max(map(median, by_source.values())),
            "boots": self.boots,
            "peak_projected_cpu_pct": self.peak_cpu,
        }


def simulate(seed, scenario, fleet_cooldown, hook_seconds):
    if scenario not in SCENARIOS or hook_seconds < 0:
        raise ValueError("unknown scenario or negative hook duration")
    rng = random.Random(seed)
    hosts = HOSTS_PER_ROLE
    cpu = [38 + i % 3 * 4 for i in range(hosts)] + [20 + i % 4 * 4 for i in range(hosts)]
    ready_at = [0.0] * len(cpu)
    if scenario == "few_receivers":
        cpu[-4:] = [54] * 4
    if scenario == "cold_boots":
        ready_at[hosts + 2:] = [math.inf] * (hosts - 2)
    receivers = range(hosts) if scenario == "mixed_roles" else range(hosts, 2 * hosts)

    # A host has one identity irrespective of its role in successive copies.
    free_at = [0.0] * len(cpu)
    durations = [rng.uniform(4, 7) for _ in range(REQUESTS)]
    arrivals = [rng.uniform(0, 3) for _ in range(REQUESTS)]
    events = []
    for request, arrival in enumerate(arrivals):
        source = 0 if scenario == "hot_donor" and request < REQUESTS // 2 else request % hosts
        heapq.heappush(events, (arrival, Event.ASK, request, source))

    fleet_opens = next_boot = -math.inf
    run = Run([], 0, 0, 0)
    while events:
        now, event, request, source = heapq.heappop(events)
        if now > 900:
            raise RuntimeError(f"request {request} on host {source} still pending after 900 s")
        if event == Event.HOOK_COMPLETED:
            fleet_opens = now + INTERVAL
            continue
        run.calls += 1
        if fleet_cooldown and now < fleet_opens:
            retry_at = fleet_opens
        else:
            eligible = [host for host in receivers if host != source and ready_at[host] <= now
                        and free_at[host] <= now and cpu[host] + COPY_CPU <= CPU_CEILING]
            if eligible and free_at[source] <= now and cpu[source] + COPY_CPU <= CPU_CEILING:
                target = min(eligible, key=lambda host: (cpu[host], host))
                hook_ends = now + hook_seconds
                end = hook_ends + durations[request]
                free_at[source] = free_at[target] = end
                run.peak_cpu = max(run.peak_cpu, max(cpu[source], cpu[target]) + COPY_CPU)
                run.copies.append(Copy(request, source, target, arrivals[request], now, end))
                heapq.heappush(events, (hook_ends, Event.HOOK_COMPLETED, request, source))
                continue
            if not eligible and math.inf in ready_at and now >= next_boot:
                ready_at[ready_at.index(math.inf)] = now + 20
                next_boot = now + INTERVAL
                run.boots += 1
            retry_at = now + rng.uniform(5, 10)
        # A donor acts on hints at its next one-second controller tick.
        phase = arrivals[request] % 1
        next_tick = phase + math.ceil(retry_at - phase)
        heapq.heappush(events, (next_tick, Event.ASK, request, source))
    return run


def main():
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("--scenario", choices=SCENARIOS, action="append")
    cli.add_argument("--hook-ms", type=float, nargs="+", default=[250, 1000])
    cli.add_argument("--seeds", type=int, default=100)
    args = cli.parse_args()
    if args.seeds < 1 or any(delay < 0 for delay in args.hook_ms):
        cli.error("seeds must be positive and hook durations nonnegative")
    rows = []
    for delay in args.hook_ms:
        for scenario in args.scenario or SCENARIOS:
            for fleet in (True, False):
                trials = [simulate(seed, scenario, fleet, delay / 1000).summary() for seed in range(args.seeds)]
                rows.append({
                    "scenario": scenario, "policy": "fleet_and_pairs" if fleet else "pairs",
                    "hook_ms": delay, "seeds": args.seeds,
                    "median": {key: round(median(trial[key] for trial in trials), 2) for key in trials[0]},
                })
    print(json.dumps(rows, indent=2))


if __name__ == "__main__":
    main()
