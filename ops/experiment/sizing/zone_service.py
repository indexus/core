"""Estimate sustained installation speed on one run and score it on another."""
import argparse
from collections import defaultdict
import json
import math
from pathlib import Path
from statistics import median


def intervals(path, after=0, before=float("inf")):
    previous = {}
    samples = defaultdict(list)
    with path.open() as stream:
        for line in stream:
            frame = json.loads(line)
            if not after <= frame["t"] < before:
                previous = {}
                continue
            current = {}
            for tx in frame["fleet"]["transfers"]:
                key = tx["id"]
                age, zones = tx.get("age_s"), tx.get("installed_zones")
                if age is None or zones is None or frame.get("metrics_stale"):
                    continue
                current[key] = age, zones
                if key in previous:
                    old, previous_zones = previous[key]
                    dt = age-old
                    if age >= 10 and 0 < dt <= 15 and zones >= previous_zones:
                        samples[key].append((dt,zones-previous_zones))
            previous = current
    return samples


def op_rates(samples):
    return [sum(n for _,n in rows)/sum(t for t,_ in rows) for rows in samples.values()]


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("train", type=Path)
    p.add_argument("test", type=Path)
    p.add_argument("--split-time", type=float, help="train before and evaluate after this second")
    a = p.parse_args()
    boundary = a.split_time
    if a.train.resolve() == a.test.resolve() and boundary is None:
        p.error("a temporal holdout is required on a single trace")
    train_ops = intervals(a.train, before=float("inf") if boundary is None else boundary)
    test_ops = intervals(a.test, after=boundary or 0)
    if a.train.resolve() == a.test.resolve():
        for key in train_ops:
            test_ops.pop(key,None)
    train, test = op_rates(train_ops), op_rates(test_ops)
    if not train or not test:
        p.error("two traces with sustained handoffs required")
    speed = median(train)
    # Rolling forecast on held-out operations. Score each interval before
    # incorporating its installations, with equal weight per operation.
    fixed_errors, rolling_errors = [], []
    for observations in test_ops.values():
        estimate = speed
        errors = [0., 0.]
        elapsed = 0.
        for dt, installed in observations:
            errors[0] += abs(installed - speed * dt)
            errors[1] += abs(installed - estimate * dt)
            estimate += -math.expm1(-dt/10) * (installed/dt-estimate)
            elapsed += dt
        fixed_errors.append(errors[0]/elapsed)
        rolling_errors.append(errors[1]/elapsed)
    print(json.dumps(dict(fixed_interval_error=median(fixed_errors),
                     rolling_interval_error=median(rolling_errors), train_operations=len(train),test_operations=len(test),
                     train_zones_s=speed,test_zones_s=median(test),
                     test_median_absolute_error_zones_s=median(abs(v-speed) for v in test))))


if __name__ == "__main__":
    main()
