#!/usr/bin/env python3
"""Handoff phase durations of a run, from the nodes' "handoff end" lines.

  handoffs.py results/<run>

The warm phase copies while the source still writes; the final phase moves
the zones to the receiver batch by batch, holding the writes of a batch.
"""
import glob, re, sys

rows = {"true": [], "false": []}
failed = 0
for path in glob.glob(sys.argv[1] + "/**/node.log", recursive=True):
    for line in open(path, errors="replace"):
        if 'msg="handoff end"' not in line:
            continue
        m = re.search(r"warm=(\w+) .*seconds=([0-9.e+-]+) error=(.*)", line)
        if not m:
            continue
        if m.group(3).strip() != "<nil>":
            failed += 1
            continue
        rows[m.group(1)].append(float(m.group(2)))
for warm, name in (("true", "warm copy"), ("false", "final (zone batches)")):
    v = sorted(rows[warm])
    if v:
        print(f"{name:20s} n={len(v):3d} median={v[len(v)//2]:6.2f} s p90={v[int(len(v)*.9)]:6.2f} s max={v[-1]:6.2f} s")
print(f"failed attempts: {failed}")
