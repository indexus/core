"""Fleet telemetry, independent of the pinned workload generator.

Schema 1 is shared with sizing/model.mjs. None means unobserved, never zero.
Per-node rates are computed only between samples of the same incarnation.
"""
import concurrent.futures
import json
import time
import urllib.parse
import urllib.request


def host(url):
    return urllib.parse.urlsplit(url).hostname or url


def collect(agents):
    def one(agent):
        row = dict(agent=agent, wall=time.time())
        for key in ('facts', 'slots'):
            try:
                with urllib.request.urlopen(agent.rstrip('/')+'/'+key, timeout=2) as r:
                    row[key] = json.load(r)
            except (OSError, ValueError) as e:
                row[key+'_error'] = type(e).__name__
        return row
    # A bounded observer: no all-to-all topology queries, no blocking node path.
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        return list(pool.map(one, sorted(set(agents))))


class Fleet:
    def __init__(self):
        self.previous, self.nodes, self.machines, self.transfers = {}, {}, {}, {}

    def frame(self, samples, machines, wall):
        for row in machines:
            self.machines[row['agent']] = row
        endpoint = {}
        hosts = {}
        for agent, row in self.machines.items():
            f = row.get('facts') or {}
            fresh = wall-row.get('wall', 0) <= 45 and 'facts' in row
            h = dict(id=host(agent), label=host(agent), agent=agent, state='unknown',
                     cpu=None, ram=None, disk=None, cpu_scope='unknown', slots=None)
            if fresh:
                h.update(state='warm' if f.get('running') == 0 else 'active',
                         cpu=f.get('cpu_pct') if f.get('cpu_ok') else None,
                         cpu_scope='host+vnodes' if f.get('host_cpu_ok') else 'vnodes',
                         ram=100-f['mem_free_pct'] if f.get('mem_ok') else None,
                         disk=100-f['disk_free_pct'] if f.get('disk_ok') else None,
                         cores=f.get('cores'), receiving=f.get('receiving'),
                         slots=None if f.get('resource_managed') else f.get('running', 0)+f.get('free', 0))
                h['ready_offer'] = f.get('receiver_ready')
                if h['state'] == 'warm' and not h['ready_offer']:
                    h['state'] = 'unavailable'
            hosts[h['id']] = h
            for slot in row.get('slots') or []:
                if slot.get('p2p'):
                    endpoint[slot['p2p']] = slot.get('monitor')
        transfers = []
        current = set()
        for monitor, m in samples.items():
            ident = str(m.get('name', monitor))+'@'+monitor
            current.add(ident)
            old = self.previous.get(monitor)
            uptime = m.get('uptime_seconds')
            writes = m.get('writes') or {}
            applied = writes.get('applied')
            rate = None
            if (old and old['id'] == ident and wall > old['wall'] and
                    uptime is not None and old['uptime'] is not None and uptime > old['uptime'] and
                    applied is not None and old['applied'] is not None and applied >= old['applied']):
                # Observation and aggregation clocks differ (e.g. 4/6s vs 5s).
                # Use the same process's monotonic sample interval as the counter.
                rate = (applied-old['applied'])/(uptime-old['uptime'])
            self.previous[monitor] = dict(id=ident, wall=wall, uptime=uptime, applied=applied)
            machine = host(monitor)
            hosts.setdefault(machine, dict(id=machine, label=machine, state='unknown', cpu=None, ram=None, disk=None))
            n = dict(id=ident, label=m.get('name', monitor), machine=machine,
                     state='receiving' if m.get('joining') else (('ready' if m['client_ready'] else 'joining') if 'client_ready' in m else 'unknown'),
                     applied_s=rate, items=(m.get('storage') or {}).get('items'), monitor=monitor, wall=wall)
            storage = m.get('storage') or {}
            n['items_counted'] = storage.get('items_counted')
            self.nodes[ident] = n
            self.transfers[ident] = []
            for tr in ((m.get('handoffs') or {}).get('open') or {}).get('list') or []:
                target = tr.get('target', tr.get('targetP2P', ''))
                target_monitor = endpoint.get(target)
                self.transfers[ident].append(dict(id=ident+':'+str(tr.get('op')), source=ident,
                                      source_machine=machine, target_machine=host(target) if target else None,
                                      target=target_monitor, phase=tr.get('phase'), age_s=tr.get('seconds'),
                                      phase_age_s=tr.get('phase_seconds'), progress=None,
                                      frozen=tr.get('op') in (m.get('frozen_transfers') or [])))
        nodes = []
        for ident, node in self.nodes.items():
            n = dict(node)
            if ident not in current:
                n.update(state='stale', applied_s=None)
            nodes.append(n)
            hosts.setdefault(n['machine'], dict(id=n['machine'], label=n['machine'], state='unknown', cpu=None, ram=None, disk=None))
        for ident, known in self.transfers.items():
            transfers.extend(known if ident in current else [dict(tr, stale=True, frozen=None) for tr in known])
        return dict(schema=1, source='aws', machines=list(hosts.values()), vnodes=nodes, transfers=transfers)
