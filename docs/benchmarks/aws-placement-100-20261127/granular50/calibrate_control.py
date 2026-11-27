"""Extract a training prefix from an archived run, without retaining node identifiers."""
import argparse
from collections import Counter, defaultdict
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import re
from statistics import median


def timestamp(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def extract(run, cutoff):
    points = []
    with (run / "remote-live.jsonl").open() as stream:
        for line in stream:
            point = json.loads(line)
            if point["t"] > cutoff:
                break
            if not point.get("load_stale") and point.get("fleet"):
                points.append(point)
    origin = median(p["wall"] - p["t"] for p in points)
    end = origin + cutoff

    # One source can ask many times for the same target. Eliminate fast HTTP
    # rejections, which never run the placement hook, before joining on time.
    asks = defaultdict(list)
    births = defaultdict(list)
    for path in sorted((run / "control-logs").glob("*/slots/*/node.log")):
        source = current = None
        for line in path.open(errors="replace"):
            found = re.match(r"time=(\S+)", line)
            if not found:
                continue
            t = timestamp(found[1])
            if t > end:
                break
            if 'msg="starting node"' in line:
                source = re.search(r" name=(\S+)", line)[1]
                current = None
                births[source[:10]].append((t, source))
            elif source and 'msg="under pressure, asking for peers"' in line:
                target = re.search(r" prefer_near=(\S+)", line)
                if target and t >= origin:
                    current = [t, None]
                    asks[source, target[1]].append(current)
            elif current and 'msg="scale-up failed"' in line:
                current[1] = "timeout" if "deadline exceeded" in line else "failed"
                if "status 429" in line:
                    current[1] = "429"
                current = None

    completions = []
    with (run / "control-logs/host0/provisioner.log").open() as stream:
        for line in stream:
            if "placement decided" not in line:
                continue
            t = datetime.strptime(line[:19], "%Y/%m/%d %H:%M:%S").replace(tzinfo=timezone.utc).timestamp()
            # The server timestamp has one-second resolution. Only use full seconds.
            if t + 1 > end:
                break
            if t < origin:
                continue
            match = re.search(r'kind=(\S+).* applied=(\S+) requester=(\S+) prefer_near=(\S+)', line)
            if match:
                completions.append((t, match[3], match[4], match[2] != '""', match[1]))

    by_attempt = defaultdict(list)
    samples = Counter()
    existing = Counter()
    assignments = []
    for t, source, target, applied, kind in completions:
        # A response may be logged just before or after its caller starts the
        # next attempt within the same wall-clock second. Drop ambiguous pairs.
        attempts = asks[source, target]
        candidates = []
        for i, (start, status) in enumerate(attempts):
            following = attempts[i + 1][0] if i + 1 < len(attempts) else math.inf
            if status != "429" and start <= t + 1 and following >= t and t - start < 179:
                candidates.append((start, status))
        if len(candidates) == 1:
            start, status = candidates[0]
            delay = max(1, round(t + 0.5 - start))
            if status == "timeout":
                delay = max(16, delay)
            elif status == "failed":
                delay = min(15, delay)
            by_attempt[source, target, start].append((delay, applied, kind))
        if applied:
            # Spawned names retain the first 64 target bits. Ten base64
            # characters are 60 bits; insist on exactly one candidate, then
            # verify the remaining four bits of the eleventh character.
            alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
            matches = [(birth, name) for birth, name in births[target[:10]]
                       if abs(birth - t) <= 30 and
                       alphabet.index(name[10]) >> 2 == alphabet.index(target[10]) >> 2]
            if len(matches) == 1:
                assignments.append((t, source, matches[0][1]))

    for results in by_attempt.values():
        if len(results) == 1:
            delay, applied, kind = results[0]
            samples[delay, applied] += 1
            if kind == "existing":
                existing[delay, applied] += 1
    fractions = []
    for t, source, child in assignments:
        # Use the first fresh nonzero pair 15–30 s after placement. Stop if a
        # second placement would confound this pair. Fractions include any
        # workload skew and intermediate routing in this observation window.
        for p in points:
            if not t + 15 <= p["wall"] <= t + 30 or p.get("metrics_stale"):
                continue
            if any(t < other <= p["wall"] and donor in (source, child)
                   for other, donor, _ in assignments):
                break
            rates = {v["label"]: v["applied_s"] for v in p["fleet"]["vnodes"]
                     if v.get("applied_s") is not None and v["state"] == "ready"
                     and p["wall"] - v.get("wall", 0) <= 7}
            if rates.get(source, 0) > 0 and rates.get(child, 0) > 0:
                fractions.append(round(rates[child] / (rates[source] + rates[child]), 3))
                break
    return {"training_end_s": cutoff, "completed": len(completions),
            "placed": sum(applied for _, _, _, applied, _ in completions),
            "existing_completed": sum(kind == "existing" for *_, kind in completions),
            "existing_placed": sum(applied and kind == "existing" for *_, applied, kind in completions),
            "existing_samples": [[d, p, n] for (d, p), n in sorted(existing.items())],
            "matched": sum(samples.values()), "placements_linked_to_child": len(assignments),
            "response_samples": [[delay, applied, count] for (delay, applied), count in sorted(samples.items())],
            "child_share_samples": sorted(fractions)}


if __name__ == "__main__":
    cmd = argparse.ArgumentParser(description=__doc__)
    cmd.add_argument("run", type=Path)
    cmd.add_argument("--until", type=int, default=300)
    args = cmd.parse_args()
    print(json.dumps(extract(args.run, args.until), indent=2))
