#!/usr/bin/env python3
"""Publish append-only experiment evidence once, in bounded complete-line parts.

Run periodically while load/observe are alive. Offsets advance only after S3
accepts a part, so retrying publication is idempotent and never recopies the
whole growing metrics file. ACK key files are deliberately excluded.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess

PART_BYTES = 8 << 20
FILES = ('metrics.jsonl', 'machines.jsonl', 'load.jsonl', 'load.log')


def publish(directory, destination, send=None):
    directory = Path(directory)
    checkpoint = directory / '.evidence.json'
    state = json.loads(checkpoint.read_text()) if checkpoint.exists() else {}
    def upload(key, data):
        subprocess.run(['aws', 's3', 'cp', '-', destination.rstrip('/') + '/evidence/' + key,
                        '--only-show-errors'], input=data, check=True)
    send = send or upload
    def save():
        temporary = checkpoint.with_suffix('.tmp')
        temporary.write_text(json.dumps(state))
        os.replace(temporary, checkpoint)
    for name in FILES:
        path = directory / name
        if not path.exists():
            continue
        offset = state.get(name, 0)
        if path.stat().st_size < offset:
            raise ValueError(f'append-only evidence truncated: {path}')
        with path.open('rb') as source:
            source.seek(offset)
            while True:
                data = source.read(PART_BYTES)
                end = data.rfind(b'\n') + 1
                if not end:
                    if len(data) == PART_BYTES:
                        raise ValueError(f'evidence line exceeds part size: {path}')
                    break
                data = data[:end]
                send(f'{name}/{offset:016d}.part', data)
                offset += end
                source.seek(offset)
                state[name] = offset
                save()
    for path in sorted((directory / 'stacks').glob('*.txt')):
        name = 'stacks/' + path.name
        if name not in state:
            send(name, path.read_bytes())
            state[name] = path.stat().st_size
            save()
    return state


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('directory')
    parser.add_argument('destination')
    args = parser.parse_args()
    publish(args.directory, args.destination)
