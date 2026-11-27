"""Count AWS fleet capacity and outstanding receiver offers under spawn.sh's lock."""
from collections import Counter
from datetime import datetime
import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys
from urllib.parse import urlsplit


def count(instances, stamps, agents, now, patience):
    # An instance can be visible in EC2 before the launching process returns.
    # Its client token links it to our reservation even without an address.
    visible = Counter(token for _, _, _, token in instances if token)
    unseen_reservations = sum(max(0, count - visible[token]) for token, count in stamps.items())
    surveyed = {urlsplit(agent).hostname for agent in agents}
    unseen_instances = sum(
        state in ('pending', 'running') and ip not in surveyed and
        now - datetime.fromisoformat(launched.replace('Z', '+00:00')).timestamp() <= patience
        for ip, launched, state, _ in instances
    )
    # The agent survey and the EC2 census are asynchronous. Count the union
    # by identity: equally sized lists may describe different hosts.
    represented = {ip for ip, _, _, _ in instances}
    accounted = len(instances) + len(surveyed - represented)
    return accounted, unseen_reservations, unseen_instances


if __name__ == '__main__':
    if sys.argv[1] in ('--reconcile', '--wait'):
        # Never share a vnode launch. Only these two paths reconcile fleet
        # capacity without an executable vnode destination.
        waiter = sys.argv[1] == '--wait'
        with open(sys.argv[2], 'a') as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                if waiter:
                    print("INDEXUS_NO_RECEIVER", file=sys.stderr)
                    print('waiting for a new machine: fleet capacity reconciliation in progress', file=sys.stderr)
                    sys.exit(os.EX_TEMPFAIL)
                sys.exit(0)
            mode = '--unplaced' if waiter else '--reserve-only'
            sys.exit(subprocess.call(['bash', sys.argv[3], mode]))

    directory, live, now, patience = sys.argv[1:]
    now, patience = int(now), int(patience)
    stamps = Counter()
    for stamp in Path(directory).glob('*.boot'):
        fields = stamp.read_text().split()
        if now - int(fields[0]) > patience:
            stamp.unlink()
        else:
            # A pre-upgrade stamp has only a timestamp.
            stamps[fields[1] if len(fields) > 1 else ''] += 1
    agents = json.loads(os.environ['INDEXUS_RESERVE_AGENTS']) if 'INDEXUS_RESERVE_AGENTS' in os.environ else live.split()
    print(*count(json.load(sys.stdin), stamps, agents, now, patience))
