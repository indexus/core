#!/usr/bin/env python3
"""Run the identical opt-in Go harness in A/B/B/A order, never in parallel.
The baseline checkout may differ only in its protocol and existing fixture;
this script supplies the current measurement file without editing production.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--before', type=Path, required=True)
p.add_argument('--after', type=Path, required=True)
p.add_argument('--out', type=Path, required=True)
a = p.parse_args()
if a.out.exists() and any(a.out.iterdir()):
    p.error('--out must be a new empty directory')
a.out.mkdir(parents=True, exist_ok=True)
harness = a.after / 'indexus/latency_profile_test.go'
source = harness.read_bytes()
(a.out/'harness-measured.go.txt').write_bytes(source)
manifest = {'harness_sha256': hashlib.sha256(source).hexdigest(), 'order': [],
            'gomaxprocs': 4, 'note': 'real fsync; in-process JSON peer RPC with synthetic RTT; not AWS throughput'}
for slot, label in enumerate(['before', 'after', 'after', 'before']):
    repo = getattr(a, label).resolve()
    target = repo / 'indexus/latency_profile_test.go'
    if repo != a.after.resolve():
        if target.exists() and target.read_bytes() != source:
            raise SystemExit(f'refusing to overwrite a different harness: {target}')
        target.write_bytes(source)
    run = a.out.resolve() / f'{slot+1}-{label}'
    run.mkdir()
    entry = {'slot': slot+1, 'label': label, 'repo': str(repo),
             'commit': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip(),
             'go_version': subprocess.check_output(['go', 'version'], cwd=repo, text=True).strip(),
             'run': str(run)}
    cmd = ['go', 'test', './indexus', '-run', '^TestLocalLatencyProfile$', '-count=1', '-v', '-timeout=4m']
    entry['command'] = cmd
    started = time.monotonic()
    with (run/'go-test.log').open('w') as log:
        result = subprocess.run(cmd, cwd=repo, stdout=log, stderr=subprocess.STDOUT,
                                env=dict(os.environ, GOMAXPROCS='4', INDEXUS_LATENCY_PROFILE='1', INDEXUS_LATENCY_OUT=str(run)))
    entry.update(exit_code=result.returncode, elapsed_s=round(time.monotonic()-started, 3))
    manifest['order'].append(entry)
    (a.out/'manifest.json').write_text(json.dumps(manifest, indent=2)+'\n')
    print(label, entry['commit'][:8], entry['exit_code'], entry['elapsed_s'], flush=True)
    # Keep subsequent runs if a protocol assertion fails: the comparison must
    # retain failures, not silently replace the baseline with successful samples.
subprocess.run(['python3', str(Path(__file__).with_name('summarize-latency.py')),
                str(a.out.resolve())], check=True)
