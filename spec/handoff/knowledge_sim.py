#!/usr/bin/env python3
"""64-peer transport model; not an authority or performance proof."""
import hashlib
import json
import random
from collections import OrderedDict
from pathlib import Path
import argparse


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


class Peer:
    def __init__(self):
        self.facts = set()
        self.receipts = OrderedDict()
        self.heads = {}

    def remember(self, version, facts):
        self.receipts[version] = frozenset(facts)
        while len(self.receipts) > 32:
            self.receipts.popitem(last=False)

    def packet(self, base, incremental):
        coverage = self.receipts.get(base) if incremental else None
        body = sorted(self.facts - coverage) if coverage is not None else sorted(self.facts)
        if coverage is not None and not body:
            return {'version': base}
        parent = base if coverage is not None else None
        packet = {'base': parent, 'body': body}
        packet['version'] = digest(packet)
        self.remember(packet['version'], (coverage or set()) | set(body))
        return packet

    def receive(self, packet):
        version = packet['version']
        if version in self.receipts:
            return True
        if 'body' not in packet:
            return False
        parent = packet['base']
        if parent is not None and parent not in self.receipts:
            return False
        assert version == digest({k: v for k, v in packet.items() if k != 'version'})
        coverage = self.receipts.get(parent, frozenset()) | set(packet['body'])
        self.facts.update(packet['body'])
        self.remember(version, coverage)
        return True


def run(seed, incremental):
    rng = random.Random(seed)
    peers = [Peer() for _ in range(64)]
    expected = set()
    wire_bytes = fallbacks = lost = restarts = 0
    for turn in range(100):
        if turn < 24:
            for i, peer in enumerate(peers):
                fact = f'{i:02}:{turn:03}:' + 'x' * 192
                peer.facts.add(fact)
                expected.add(fact)
        if turn in (8, 16, 23, 40):
            peer = peers[rng.randrange(64)]
            peer.receipts.clear()  # durable facts survive process restart
            peer.heads.clear()
            restarts += 1
        edges = [(i, (i + offset) % 64) for i in range(64) for offset in (1, 7)]
        rng.shuffle(edges)
        for i, j in edges:
            a, b = peers[i], peers[j]
            packet = a.packet(a.heads.get(j), incremental)
            wire_bytes += len(json.dumps(packet))
            if rng.random() < .08:
                lost += 1
                continue
            if not b.receive(packet):
                fallbacks += 1
                packet = a.packet(None, incremental)
                wire_bytes += len(json.dumps(packet))
                assert b.receive(packet)
            reply = b.packet(packet['version'], incremental)
            wire_bytes += len(json.dumps(reply))
            if rng.random() < .08:
                lost += 1
                continue
            assert a.receive(reply), 'sender retained its base for this exchange'
            a.heads[j] = reply['version']
    assert all(p.facts == expected for p in peers), 'not converged'
    return dict(seed=seed, incremental=incremental, nodes=64, facts=len(expected),
                wire_bytes=wire_bytes, lost_messages=lost, restarts=restarts,
                base_misses=fallbacks, converged=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    results = [run(seed, mode) for seed in (1, 7, 42) for mode in (False, True)]
    args.out.write_text(json.dumps({'scope': 'abstract transport, no disks or ownership proof',
                                     'runs': results}, indent=2))
    for seed in (1, 7, 42):
        full, delta = [r for r in results if r['seed'] == seed]
        print(f"seed={seed}: converged, full={full['wire_bytes']} delta={delta['wire_bytes']} reduction={1-delta['wire_bytes']/full['wire_bytes']:.1%}")
