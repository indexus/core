"""Summarise the goroutine dumps (SIGQUIT) in a directory of gzipped node logs:
per node, the number of goroutines and the most common first frames inside
indexus, which is where a stuck node waits."""
import collections
import gzip
import os
import sys

for name in sorted(os.listdir(sys.argv[1])):
    text = gzip.open(os.path.join(sys.argv[1], name), "rt", errors="replace").read()
    start = text.find("SIGQUIT")
    if start < 0:
        continue
    blocks = text[start:].split("\n\n")
    tops = collections.Counter()
    for block in blocks:
        frames = [l.strip() for l in block.split("\n") if "go-indexus-core" in l and "(" in l]
        if frames:
            tops[frames[0][:120]] += 1
    print(name, len(blocks), "goroutines; most common first frames:")
    for frame, n in tops.most_common(6):
        print("  ", n, frame)
