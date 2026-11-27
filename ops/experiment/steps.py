#!/usr/bin/env python3
"""Per-level verdict of a steps run: steps.py load.jsonl RAMP PLATEAU [COUNT].

For each level: offered and acked per second over the second half of its
plateau, their ratio, and the time from the start of the ramp to the first
5 s window at 95% or more acked (the time to absorb the step), and the longest
hole: consecutive seconds under half of the offered writes acked. A level
passes with 95% on its second half and no hole above HOLE_MAX seconds."""
import json, sys

rows = [json.loads(l) for l in open(sys.argv[1]) if l.startswith('{"t"')]
ramp, plateau = float(sys.argv[2]), float(sys.argv[3])
count = int(sys.argv[4]) if len(sys.argv) > 4 else 9
by = {int(r["t"]): r for r in rows}
end = max(by) if by else 0
HOLE_MAX = 10
ok = True
for k in range(count + 1):
    start = 0 if k == 0 else plateau + (k - 1) * (ramp + plateau)
    level_at = start + (0 if k == 0 else ramp)
    stop = level_at + plateau
    if stop > end + 1:
        break
    half = [by[t] for t in range(int(level_at + plateau / 2), int(stop)) if t in by]
    off = sum(r["offered"] for r in half) / max(len(half), 1)
    ack = sum(r["acked"] for r in half) / max(len(half), 1)
    absorbed = None
    for t in range(int(start), int(stop) - 4):
        w = [by[x] for x in range(t, t + 5) if x in by]
        o = sum(r["offered"] for r in w)
        if o and sum(r["acked"] for r in w) >= 0.95 * o and t >= level_at:
            absorbed = t - start
            break
    hole = run = 0
    for t in range(int(start), int(stop)):
        r = by.get(t)
        run = run + 1 if r and r["offered"] and r["acked"] < 0.5 * r["offered"] else 0
        hole = max(hole, run)
    ratio = ack / off if off else 0
    ok = ok and ratio >= 0.95 and hole <= HOLE_MAX
    print(f"level {k}: offered {off:6.0f}/s acked {ack:6.0f}/s ({ratio:5.1%}) absorbed after {absorbed if absorbed is not None else 'never'} s, longest hole {hole} s")
print("steps verdict:", "pass" if ok else "fail", f"(>= 95% acked on the second half of every plateau, no hole above {HOLE_MAX} s)")
