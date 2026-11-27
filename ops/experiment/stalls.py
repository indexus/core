#!/usr/bin/env python3
"""Aggregate goroutine dumps (pprof debug=1) of stalled nodes.

stacks.py DIR: for each file, the goroutine groups by the innermost indexus
frame, split into waiting (on a lock or channel) and working (anything else:
the holders). Then the working frames summed over all files.
"""
import collections, os, re, sys

WAIT = re.compile(r"sync\.\(\*(RW)?Mutex\)\.(R)?Lock|runtime_Semacquire|chanrecv|chansend|selectgo|sync\.\(\*Cond\)\.Wait|netpollblock|gopark")
MOD = "github.com/indexus/go-indexus-core/"

def groups(path):
    text = open(path, errors="ignore").read()
    for block in re.split(r"\n\s*\n", text):
        m = re.match(r"(\d+) @", block.strip())
        if not m:
            continue
        frames = re.findall(r"#\s+0x[0-9a-f]+\s+(\S+)", block)
        own = [f.replace(MOD, "") for f in frames if f.startswith(MOD)]
        if not own:
            continue
        waiting = any(WAIT.search(f) for f in frames[:4])
        yield int(m.group(1)), waiting, own

total_work = collections.Counter()
total_wait = collections.Counter()
for name in sorted(os.listdir(sys.argv[1])):
    work, wait = collections.Counter(), collections.Counter()
    for n, waiting, own in groups(os.path.join(sys.argv[1], name)):
        key = " < ".join(own[:3])
        (wait if waiting else work)[key] += n
        (total_wait if waiting else total_work)[" < ".join(own[:2])] += n
    top = wait.most_common(1)
    print(f"{name}: waiting {sum(wait.values())}, top {top[0][1] if top else 0} at {top[0][0][:110] if top else '-'}")
print("\n== working goroutines (holders), summed over all files")
for k, v in total_work.most_common(15):
    print(f"{v:6d}  {k[:150]}")
print("\n== waiting goroutines, summed over all files")
for k, v in total_wait.most_common(12):
    print(f"{v:6d}  {k[:150]}")
