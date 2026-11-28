"""CPU capacity replay for homogeneous hosts; observed work is held constant."""
import argparse
from collections import deque
import json
import math
from pathlib import Path
import statistics


def demand_frames(path, duration):
    frames = []
    with path.open() as stream:
        for line in stream:
            p = json.loads(line)
            if p["t"] > duration or p.get("metrics_stale") or p.get("load_stale"):
                continue
            machines = p["fleet"]["machines"]
            if not machines or any(m.get("cpu") is None for m in machines):
                continue
            if len({m["cores"] for m in machines}) != 1:
                raise ValueError("Fleet replay requires homogeneous hosts")
            if p.get("offered") is None:
                continue
            # In host equivalents; comparing to 50% immediately gives a host count.
            frames.append((p["t"], sum(m["cpu"] for m in machines)/100,
                           len(machines), p["offered"]))
    return frames


def capacity(frames, policy, initial, cap, boot, target):
    ready = initial
    pipeline = []
    history = deque()
    machine_seconds = deficit_seconds = 0
    peaks = []
    last = None
    for t, work, _, rate in frames:
        elapsed = 0 if last is None else t - last[0]
        if last is not None:
            # Integrate piecewise constant demand; a boot can finish between samples.
            boundaries = sorted({due for due in pipeline if last[0] < due < t}) + [t]
            begin = last[0]
            for end in boundaries:
                count = ready + sum(due <= begin for due in pipeline)
                deficit_seconds += max(0, last[1] - target * count) * (end - begin)
                machine_seconds += (end - begin) * (ready + len(pipeline))
                begin = end
        ready += sum(due <= t for due in pipeline)
        pipeline = [due for due in pipeline if due > t]
        history.append((t, rate))
        while len(history) > 1 and history[1][0] <= t - boot:
            history.popleft()
        projection = work
        if policy == "lookahead" and elapsed and rate > 0:
            dt = t-history[0][0]
            if dt > 0:
                growth = max(0, rate-history[0][1]) / dt
                projection *= 1 + min(1, growth*boot/rate)
        wanted = math.ceil(projection/target)
        active = ready if policy == "ready_only" else ready+len(pipeline)
        starts = max(0, min(cap-ready-len(pipeline), wanted-active))
        pipeline.extend([t+boot]*starts)
        peaks.append(starts)
        last = t, work
    return dict(final_hosts=ready+len(pipeline), largest_boot_wave=max(peaks,default=0),
                billed_host_seconds=round(machine_seconds),
                demand_above_target_host_seconds=round(deficit_seconds,1))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("results", type=Path)
    parser.add_argument("--boot", type=float, default=45)
    parser.add_argument("--target", type=float, default=.5)
    args = parser.parse_args()
    if args.boot <= 0 or not 0 < args.target <= 1:
        parser.error("positive boot time and target in (0,1] required")
    cap = json.loads((args.results / "capacity.json").read_text())
    f = demand_frames(args.results / "live.jsonl", 400)
    if not f:
        raise ValueError("no complete load and machine frames")
    for lo,rate in enumerate((8000,16000,32000,64000)):
        group = [work for t,work,_,_ in f if lo*110 < t <= lo*110+70]
        print(json.dumps(dict(rate=rate, complete=len(group), cpu_hosts_at_target=
              round(statistics.median(group)/args.target,1) if group else None)))
    for policy in ("ready_only","pipeline","lookahead"):
        print(json.dumps(dict(policy=policy, **capacity(f, policy, cap["hosts"],
                          cap["max_machines"], args.boot, args.target))))


if __name__ == "__main__":
    main()
