"""Build anonymous donor-attempt, hook and first-applied-write episodes from one run."""
import argparse
from collections import defaultdict
from datetime import datetime, timezone
import gzip
import json
from pathlib import Path
import re
from statistics import median

ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
REASONS = (
    ('reconciliation in progress', 'coalesced'), ('capacity calculation busy', 'capacity_lock'),
    ('waiting for a new machine', 'wait_boot'), ('no free slot', 'no_slot'),
    ('api_cooldown', 'api_cd'), ('requester_cooldown', 'donor_cd'),
    ('waiting_for_capacity', 'confirmed_wait'), ('deadline exceeded', 'timeout'),
)


def wall(value):
    return datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp()


def field(line, key):
    match = re.search(r'\b' + key + r'=("[^"]*"|\S+)', line)
    return match[1].strip('"') if match else ''


def target_prefix(identity):
    return identity[:10], ALPHABET.index(identity[10]) >> 2


def observation_window(path, window, load=None):
    with (path / 'live.jsonl').open() as stream:
        dashboard = [json.loads(line) for line in stream if line.endswith('\n')]
    if window <= 0 or not dashboard:
        raise ValueError("a positive injection duration and a dashboard clock are required")
    samples = [r for r in dashboard if r.get('offered') is not None and not r.get('load_stale')]
    initial = [r for r in samples if 10 < r['t'] <= 300]
    origin = median(r['wall'] - r['t'] for r in initial or samples or dashboard)
    if load:
        # Use raw per-second records when the final dashboard scrape is missing.
        with load.open() as stream:
            observed = max((r['t'] for line in stream if line.endswith('\n')
                            and (r := json.loads(line)).get('offered') is not None), default=0)
        evidence = 'load'
    else:
        observed = max((r['t'] for r in samples), default=0)
        evidence = 'dashboard'
    end = min(window, int(observed))
    if end <= 0:
        raise ValueError("no observed load interval")
    return origin, end, evidence


def failure_message(line):
    cause = next((reason for pattern, reason in REASONS if pattern in line), 'other')
    retry = re.search(r'retry_after_ms\\?":(\d+)', line)
    return cause, int(retry[1]) if retry else 0


def extract(path, origin, window):
    attempts = defaultdict(list)
    births = {}
    first_write = {}
    logs = sorted((path / 'logs').glob('*/slots/*/node.log'))
    if not logs:
        raise ValueError(f"no vnode logs under {path}/logs")
    for logfile in logs:
        source = current = None
        for line in logfile.open(errors='replace'):
            if 'msg="starting node"' in line:
                source = field(line, 'name')
                births.setdefault(source, wall(field(line, 'time')) - origin)
                current = None
            elif source and 'msg="first write applied"' in line:
                first_write.setdefault(source, wall(field(line, 'time')) - origin)
            elif source and 'msg="under pressure, asking for peers"' in line:
                t = wall(field(line, 'time')) - origin
                current = None
                if 0 <= t < window:
                    current = [round(t, 3), '', None, 0, int(field(line, 'spawn_count') or 1)]
                    attempts[source, field(line, 'prefer_near')].append(current)
            elif current and 'msg="scale-up failed"' in line:
                end = wall(field(line, 'time')) - origin
                if end < window:
                    cause, retry = failure_message(line)
                    current[1:4] = [cause, round(end, 3), retry]
                current = None
    hooks = defaultdict(list)
    for line in (path / 'logs/host0/provisioner.log').open(errors='replace'):
        if 'placement decided' not in line or not field(line, 'applied'):
            continue
        at = datetime.strptime(line[:19], '%Y/%m/%d %H:%M:%S').replace(tzinfo=timezone.utc).timestamp() - origin
        if 0 <= at < window:
            hooks[field(line, 'requester'), field(line, 'prefer_near')].append(at)
    by_prefix = defaultdict(list)
    for name, birth in births.items():
        if len(name) == 16:
            by_prefix[target_prefix(name)].append((birth, name))
    donors = {}
    episodes = []
    for (source, near), rows in sorted(attempts.items(), key=lambda pair:pair[1][0][0]):
        donor = donors.setdefault(source, len(donors))
        success = [t for t in hooks[source, near] if t >= rows[0][0] - 1]
        hook = min(success) if success else None
        applied = None
        if hook is not None and len(near) == 16:
            names = [name for birth, name in by_prefix[target_prefix(near)] if abs(birth - hook) <= 30]
            if len(names) == 1 and names[0] in first_write:
                seen = first_write[names[0]]
                if rows[0][0] <= seen < window:
                    applied = round(seen, 3)
        episodes.append(dict(donor=donor, attempts=rows, hook=hook, write=applied))
    return episodes


if __name__ == '__main__':
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument('captured', type=Path)
    cli.add_argument('output', type=Path)
    cli.add_argument('--window', type=int, default=660, help='nominal injection length in seconds')
    cli.add_argument('--load', type=Path, help='raw per-second load.jsonl for confirming the injection end')
    args = cli.parse_args()
    origin, end, evidence = observation_window(args.captured, args.window, args.load)
    # Stable representation for checking future extractions of the same archive.
    data = json.dumps(dict(window=end, planned_window=args.window, end_source=evidence,
                           episodes=extract(args.captured, origin, end)), separators=(',', ':')).encode()
    args.output.write_bytes(gzip.compress(data, compresslevel=9, mtime=0))
