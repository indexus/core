"""Reassign observed remote grants. CPU, host boots and grant times stay exogenous."""
import argparse
from bisect import bisect_right
from collections import defaultdict
from datetime import datetime
import json
from pathlib import Path


def rows(path):
    with path.open() as stream:
        for line in stream:
            if line.strip():
                yield json.loads(line)


def decisions(path, start, duration):
    seen = {}
    for sample in rows(path):
        for row in (sample.get("placement") or {}).get("last", []):
            donor, prefix = row.get("requester_id"), row.get("prefer_near")
            if not (donor and prefix and row.get("at")):
                continue
            at = datetime.fromisoformat(row["at"].replace("Z", "+00:00")).timestamp()
            if start <= at < start + duration:
                seen[at, donor, prefix] = row
    return sorted(seen.items())


class Inventory:
    def __init__(self, path):
        self.hosts = defaultdict(list)
        for row in rows(path):
            if row.get("facts"):
                self.hosts[row["agent"]].append((row["wall"], row["facts"]))
        for frames in self.hosts.values():
            frames.sort(key=lambda frame: frame[0])

    def at(self, at, max_age):
        machines = {}
        for host, frames in self.hosts.items():
            index = bisect_right(frames, at, key=lambda frame: frame[0]) - 1
            if index >= 0 and at - frames[index][0] <= max_age:
                machines[host] = frames[index][1]
        return machines


def load(facts):
    items = facts.get("items") or {}
    names = facts.get("vnodes") or []
    if len(names) == facts["running"] and all(items.get(n, -1) >= 0 for n in names):
        return sum(items[n] for n in names)
    return None


def relieves(donor, target, machines):
    """Same known-item comparison as reliefRule. Unknown alternatives are skipped."""
    destination = machines.get(target)
    if not destination:
        return False
    sources = [h for h, f in machines.items() if donor in (f.get("vnodes") or [])]
    if len(sources) != 1 or sources[0] == target:
        return False
    source = machines[sources[0]]
    d, t = load(source), load(destination)
    if d is None or t is None or min(source.get("cores", 0), destination.get("cores", 0)) <= 0:
        return False
    half = (source["items"][donor] + 1) // 2
    return destination["running"] == 0 or (half > 0 and
        (t + half) * source["cores"] < d * destination["cores"])


def replay(events, inventory, start, duration, policy, lease, fact_age, focus=None):
    pending, first, completed = {}, {}, {}
    grants = 0
    for (at, donor, prefix), row in events:
        key = donor, prefix
        if key not in completed:
            first.setdefault(key, at)
            since, last, _ = pending.get(key, (at, at, 0))
            pending[key] = (since if at - last <= lease else at, at, row.get("write_rate") or 0)
        target = row.get("machine")
        if not target or row.get("applied") not in ("existing", "new"):
            continue
        grants += 1
        selected = key
        if policy != "observed":
            machines = inventory.at(at, fact_age)
            candidates = [k for k, (_, last, _) in pending.items() if at - last <= lease
                          and (k == key or relieves(k[0], target, machines))]
            if not candidates:
                continue

            def priority(k):
                since, _, rate = pending[k]
                age = at - since
                if policy == "host_pressure":
                    # Keep the observed grant unless another donor has waited
                    # a full lease with a fresh over-budget source machine.
                    overloaded = any(k[0] in (f.get("vnodes") or [])
                                     and f.get("host_cpu_ok") and f.get("host_cpu_pct", 0) > 60
                                     for f in machines.values())
                    urgent = overloaded and at - first[k] >= lease
                    return urgent, (rate if urgent else k == key), age, k
                if policy == "oldest":
                    return age, rate, k
                if policy == "rate":
                    return rate, age, k
                # Integral of excess demand under a constant last reported rate.
                return max(0, rate - 50) * age, age, k

            selected = max(candidates, key=priority)
        if selected in pending:
            completed[selected] = at
            del pending[selected]
    end = start + duration
    # Include censored waits through the end. A served-only p99 rewards starvation.
    waits = [completed.get(k, end) - t for k, t in first.items()]
    result = dict(demands=len(first), grants=grants, served=len(completed),
                  outstanding=len(pending), donor_prefix_wait_seconds=round(sum(waits)),
                  longest_observed_or_censored_s=round(max(waits, default=0), 1))
    if focus:
        result["focus"] = [(k[1], round(t-start, 1),
                            round(completed[k]-start, 1) if k in completed else None)
                           for k, t in first.items() if k[0] == focus]
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("results", type=Path)
    p.add_argument("--duration", type=float, default=400)
    p.add_argument("--lease", type=float, default=30)
    p.add_argument("--fact-age", type=float, default=20)
    p.add_argument("--focus")
    a = p.parse_args()
    start = next(rows(a.results / "live.jsonl"))["wall"]
    trace = decisions(a.results / "operator/placement.jsonl", start, a.duration)
    inventory = Inventory(a.results / "steps-1/machines.jsonl")
    for policy in ("observed", "oldest", "rate", "pressure_age", "host_pressure"):
        print(json.dumps(dict(policy=policy, **replay(trace, inventory, start, a.duration,
                         policy, a.lease, a.fact_age, a.focus))))


if __name__ == "__main__":
    main()
