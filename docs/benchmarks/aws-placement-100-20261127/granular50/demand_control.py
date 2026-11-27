"""Compare useful service, held-out early-regime forecasts and conditional retry traffic."""
import argparse
from collections import Counter
import gzip
import heapq
import json
from pathlib import Path
from random import Random
from statistics import median

NO_DISPATCH = {'coalesced', 'capacity_lock', 'wait_boot', 'no_slot', 'confirmed_wait'}


def cohort(episodes, start, end, horizon):
    # Exclude every demand with incomplete follow-up, even visible early successes.
    opened = [e for e in episodes if start <= e['attempts'][0][0] < end]
    eligible = [e for e in opened if e['attempts'][0][0] + horizon <= end]
    observations = []
    for e in eligible:
        first = e['attempts'][0][0]
        deadline = first + horizon
        served = e['write'] is not None and first <= e['write'] <= deadline
        attempts = sum(a[0] <= deadline for a in e['attempts'])
        observations.append((served, attempts))
    return observations, len(opened) - len(eligible)


def scores(rows):
    n = len(rows)
    useful = sum(s for s, _ in rows)
    attempts = sum(a for _, a in rows)
    return dict(eligible=n, useful=useful, attempts=attempts,
                served_pct=round(100 * useful / n, 1) if n else None,
                calls_per_useful=round(attempts / useful, 2) if useful else None)


def early_forecast(episodes, horizon, end=660):
    split = min(300, end / 2)
    train, _ = cohort(episodes, 0, split, horizon)
    test, censored = cohort(episodes, split, end, horizon)
    # Only test cohort size is known to this forecast. Preserve attempts and
    # service as an observed pair. Siblings from one donor are dependent, so
    # the point forecast carries no independence-based confidence interval.
    return dict(calibration=scores(train), observed=scores(test), holdout_censored=censored,
                predicted_useful=round(sum(s for s, _ in train) * len(test) / len(train), 1) if train else None,
                predicted_calls=round(sum(a for _, a in train) * len(test) / len(train), 1) if train else None)


def replay(episodes, policy, seed=1, tolerance=.05):
    """Thin actual attempts only. Acceptances and missed opportunities cannot be inferred."""
    events = []
    for e in episodes:
        for a in e['attempts']:
            # A reply to a different RPC at an identical timestamp precedes the next send.
            events.append((a[0], 1, e['donor'], len(events), a))
    heapq.heapify(events)
    rng = Random(seed)
    next_allowed = {}
    waiting_since = {}
    gate_delays = {}
    gate_reopens = None
    calls = Counter()
    followers = 0
    while events:
        t, is_send, donor, index, a = heapq.heappop(events)
        _, cause, reply, retry_ms, count = a
        if is_send:
            if t < next_allowed.get(donor, 0):
                calls['suppressed'] += 1
                if not cause:
                    calls['suppressed_unknown'] += 1
                continue
            calls['sent'] += 1
            if cause == 'wait_boot':
                # The hook explicitly chose a new machine. Other hooks may
                # update waiting as well; this donor-side trace cannot see all.
                waiting_since[donor] = t
            if not cause:
                waiting_since.pop(donor, None)
            if policy != 'observed' and reply is not None and reply >= t:
                heapq.heappush(events, (reply, 0, donor, index, a))
        elif count == 1 and cause in NO_DISPATCH:
            next_allowed[donor] = max(next_allowed.get(donor, 0), t + rng.uniform(5, 10))
        elif policy == 'spread' and cause == 'api_cd' and retry_ms > 0:
            # No gate ID on the wire. The global opening is inferred from the
            # advertised time + local reply time, with clock/transport tolerance.
            opens = t + retry_ms / 1000
            if gate_reopens is None or abs(opens - gate_reopens) > tolerance:
                gate_reopens = opens
                gate_delays = {donor: 0}
            if donor not in gate_delays:
                gate_delays[donor] = rng.random()
            if gate_delays[donor]:
                spread = 4
                asked = waiting_since.get(donor)
                if asked is not None and t - asked < 15:
                    spread = min(spread, max(0, asked + 10 - opens))
                next_allowed[donor] = max(next_allowed.get(donor, 0), opens + spread * gate_delays[donor])
                followers += 1
    return dict(calls, followers=followers)


def simulate_burst(donors, stagger, seed, interval=2):
    """Demand arrives together at t=0. One ready receiver offered per API interval."""
    rng = Random(seed)
    queue = [(0, donor) for donor in range(donors)]
    gate_open = interval
    first_waiter = None
    calls = 0
    served = []
    while queue:
        at, donor = heapq.heappop(queue)
        calls += 1
        if at >= gate_open:
            served.append(at)
            gate_open = at + interval
            first_waiter = None
            continue
        delay = 0
        if first_waiter is None:
            first_waiter = donor
        elif stagger:
            delay = rng.uniform(0, min(2 * interval, 5))
        heapq.heappush(queue, (gate_open + delay, donor))
    return dict(attempts=calls, within_30=sum(t <= 30 for t in served),
                within_120=sum(t <= 120 for t in served), last=served[-1])


def summarize(episodes, window=660):
    reasons = Counter(a[1] or 'unclassified' for e in episodes for a in e['attempts'])
    result = {'window': window, 'attempts': sum(reasons.values()), 'reasons': reasons, 'episodes': len(episodes),
              'placements': sum(e['hook'] is not None for e in episodes),
              'applied': sum(e['write'] is not None for e in episodes)}
    for horizon in (30, 120):
        rows, censored = cohort(episodes, 0, window, horizon)
        result[horizon] = dict(all=scores(rows), censored=censored, **early_forecast(episodes, horizon, window))
    # Empirical retention conditional on the original arrival times and responses.
    # Report sensitivity to the retry delays across thirty deterministic seeds.
    for policy in ('shipped', 'spread'):
        runs = [replay(episodes, policy, seed) for seed in range(30)]
        result[policy] = {k: median(r.get(k, 0) for r in runs)
                          for k in ('sent', 'suppressed', 'suppressed_unknown', 'followers')}
    return result


if __name__ == '__main__':
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument('episodes', type=Path, nargs='+')
    for stagger in (False, True):
        runs = [simulate_burst(32, stagger, seed) for seed in range(100)]
        print('burst stagger', stagger, {k: median(r[k] for r in runs) for k in runs[0]})
    for path in cli.parse_args().episodes:
        with gzip.open(path) as stream:
            archive = json.load(stream)
        # The first two published anonymous archives cover the historic 660 s run.
        if isinstance(archive, list):
            result = summarize(archive)
        else:
            result = summarize(archive['episodes'], archive['window'])
        print(path.name, json.dumps(result, indent=2))
