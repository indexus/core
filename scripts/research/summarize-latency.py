#!/usr/bin/env python3
"""Pool raw latency samples from compare-latency.py, retaining failures and n."""
import collections
import json
import math
from pathlib import Path
import sys


def quantiles(values):
    if not values:
        return dict(n=0, p50_ms=None, p95_ms=None, p99_ms=None, max_ms=None)
    values = sorted(values)
    return dict(n=len(values), **{
        name: values[math.ceil(q*len(values))-1]/1000
        for name, q in [('p50_ms', .5), ('p95_ms', .95), ('p99_ms', .99)]},
        max_ms=values[-1]/1000)


def summarize(root):
    manifest = json.loads((root/'manifest.json').read_text())
    groups, intended = collections.defaultdict(list), collections.defaultdict(list)
    errors, milestones, incomplete = collections.Counter(), collections.defaultdict(list), []
    for run in manifest['order']:
        path, label = Path(run['run']), run['label']
        for scenario in ('same-zone', 'distinct-zones', 'join'):
            summary, raw = path/f'{scenario}.json', path/f'{scenario}.jsonl'
            if not summary.exists() or not raw.exists():
                incomplete.append(dict(label=label, run=str(path), scenario=scenario))
                continue
            data = json.loads(summary.read_text())
            errors[label] += sum(data['errors'].values())
            if scenario == 'join':
                if any(data.get(k) is None for k in ('first_point_reply_ms', 'first_native_reply_ms', 'first_receiver_ack_ms')):
                    incomplete.append(dict(label=label, run=str(path), scenario=scenario, reason='missing transfer milestone'))
                milestones[label].append({k: data[k] for k in (
                    'join_ms', 'first_point_reply_ms', 'first_native_reply_ms',
                    'first_receiver_ack_ms', 'peer_calls', 'peer_request_bytes', 'peer_reply_bytes', 'retries')})
            for line in raw.open():
                sample = json.loads(line)
                if sample.get('error'):
                    continue
                phases = [sample['phase']]
                if sample['operation'] == 'write':
                    phases.append('all')
                for phase in phases:
                    key = label, scenario, sample['operation'], phase
                    groups[key].append(sample['latency_us'])
                    if sample['operation'] == 'write':
                        intended[key].append(sample['intended_us'])
    rows = []
    for scenario, operation, phase in sorted({key[1:] for key in groups}):
        row = dict(scenario=scenario, operation=operation, phase=phase)
        for label in ('before', 'after'):
            key = label, scenario, operation, phase
            row[label] = quantiles(groups[key])
            if intended[key]:
                row[label]['intended'] = quantiles(intended[key])
        rows.append(row)
    result = dict(manifest=manifest,
        quantile_method='nearest-rank on pooled successful raw samples; never averages of percentiles',
        notes=['Two runs per revision, A/B/B/A; no confidence interval',
               'Phases use dispatch time; compare write/all for an equal offered workload',
               'During-join phase sample sizes differ when completion times differ',
               'Intended latency includes scheduling/queue wait and retries',
               'Peer RPC is in-process JSON with configured synthetic 1ms RTT; not TCP/QUIC',
               'Post-join readers check total seed+write count; not an exhaustive per-ID audit',
               'Small join: 1024 seeded zones with 1024-byte text payload, two nodes'],
        errors=dict(errors), incomplete=incomplete, milestones=dict(milestones), rows=rows)
    (root/'comparison.json').write_text(json.dumps(result, indent=2)+'\n')
    return result


if __name__ == '__main__':
    result = summarize(Path(sys.argv[1]))
    print(json.dumps(dict(errors=result['errors'], incomplete=result['incomplete']), indent=2))
    if (len(result['manifest']['order']) != 4 or result['incomplete']
            or any(result['errors'].values())
            or any(run['exit_code'] for run in result['manifest']['order'])):
        raise SystemExit(1)
