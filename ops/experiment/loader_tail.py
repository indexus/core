#!/usr/bin/env python3
"""Instrument the archived experiment loader identically for A/B AWS runs.
Only the terminal load-summary gains histogram remainders after worker drain;
node/issuer/provisioner protocol sources are never modified.
"""
import hashlib
import json
from pathlib import Path
import sys

FIELDS = {
    'latency_us_log2_remaining': 'latency',
    'intended_latency_us_log2_remaining': 'intended',
    'read_latency_us_log2_remaining': 'readLatency',
    'total_latency_us_log2_remaining': 'totalLatency',
}
ANCHOR = '"queued_unfinished": len(jobs), "drain_completed": drained'


def instrument(source):
    if all('"'+key+'"' in source for key in FIELDS):
        return source
    if any('"'+key+'"' in source for key in FIELDS):
        raise ValueError('loader has a partial terminal-summary patch')
    extra = ''.join(', "'+key+'": '+value+'.drain()' for key, value in FIELDS.items())
    if source.count(ANCHOR) == 1:
        return source.replace(ANCHOR, ANCHOR+extra)
    # Reviewed 6c1f58e layout waits for all workers but has no final JSON.
    legacy = '\tclose(jobs)\n\twg.Wait()\n\tfmt.Fprintf(os.Stderr, "offered='
    if source.count(legacy) != 1:
        raise ValueError('loader has neither reviewed terminal-summary layout')
    summary = ('"offered": offered.Load(), "acked": ackedN.Load(), "shed": shed.Load(), '
               '"exhausted": offered.Load()-shed.Load()-ackedN.Load(), '
               '"queued_unfinished": len(jobs), "drain_completed": true')
    terminal = ('\tclose(jobs)\n\twg.Wait()\n'
                '\tsummary, _ := json.Marshal(map[string]any{'+summary+extra+'})\n'
                '\tif err := os.WriteFile(filepath.Join(*out, "load-summary.json"), append(summary, \'\\n\'), 0o644); err != nil { return err }\n'
                '\tfmt.Fprintf(os.Stderr, "offered=')
    return source.replace(legacy, terminal)


def apply(root):
    file = root/'cmd/experiment/main.go'
    before = file.read_text()
    after = instrument(before)
    file.write_text(after)
    return dict(path=str(file), before_sha256=hashlib.sha256(before.encode()).hexdigest(),
                after_sha256=hashlib.sha256(after.encode()).hexdigest(),
                patch_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                node_sources_changed=False)


if __name__ == '__main__':
    print(json.dumps(apply(Path(sys.argv[1])), indent=2))
