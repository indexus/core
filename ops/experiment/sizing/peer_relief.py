"""Closed-loop relief experiment branched from a single EBS fleet observation."""
import argparse
from dataclasses import dataclass, field
import json
import random
from statistics import median


@dataclass
class Host:
    id: str
    cores: int
    background: float
    rates: list
    reserve: float
    free_at: int
    queue: float = 0
    extra: float = 0
    ram: float = 0
    disk: float = 0

    def work(self, cost, demand):
        return self.background + sum(self.rates)*cost*demand + self.extra


@dataclass
class Move:
    src: Host
    dst: Host
    index: int
    rate: float
    start: int
    copy_at: int
    copy_work: float
    progress: float = 0
    moved: float = 0
    new_index: int = field(default=-1)


def snapshot(path, second):
    with open(path) as stream:
        rows = (json.loads(line) for line in stream)
        frames = [f for f in rows if f['t'] <= second and not f.get('metrics_stale')
                  and not f.get('load_stale') and f.get('offered')]
    if not frames or second-frames[-1]['t'] > 10:
        raise ValueError('no fresh load and fleet snapshot')
    return frames[-1]


def fleet(frame, cost, occupied_for):
    inventory = {}
    for v in frame['fleet']['vnodes']:
        if v.get('applied_s') is not None:
            inventory.setdefault(v['machine'], []).append(max(0,v['applied_s']))
    hosts = []
    for h in frame['fleet']['machines']:
        if any(h.get(k) is None for k in ('cpu','cores','ram','disk','receiving')):
            continue
        rates = inventory.get(h['id'], [])
        # CPU above the inferred write work stays resident during the replay.
        # At 100% the recorded CPU is censored; excess arrivals are unknown.
        bg = max(0,h['cpu']*h['cores']/100-sum(rates)*cost)
        hosts.append(Host(h['id'], h['cores'], bg, rates, h.get('reserve_cores') or .5,
                          occupied_for if h['receiving'] else 0,ram=h['ram'],disk=h['disk']))
    return hosts


def simulate(frame, policy, seed=1, cost=.001, seconds=120, occupied_for=30,
             breadth=4, hot_fraction=.5, copy_seconds=10, warmup=15, ramp=1, heat=1.,
             additional_hosts=0, boot_seconds=45):
    hosts = fleet(frame,cost,occupied_for)
    if additional_hosts < 0 or boot_seconds < 0:
        raise ValueError("additional host count and boot delay must be nonnegative")
    initial_hosts = len(hosts)
    initial_rate = sum(sum(h.rates) for h in hosts)
    rng = random.Random(seed)
    offsets = {h.id:i%5 for i,h in enumerate(hosts)}
    moves = []
    started = finished = hot_seconds = queue_seconds = probes = 0
    peak_queue = max_drift = last_hot = 0
    borrowed = 0
    first_relief = {}
    initial_cpu = {h.id:h.work(cost,1)/h.cores for h in hosts}
    initially_hot = {key for key,val in initial_cpu.items() if val > .60}
    good = {key:0 for key in initially_hot}
    for t in range(seconds):
        if additional_hosts and t == boot_seconds:
            reference = hosts[0]
            for i in range(additional_hosts):
                host = Host(f"new-{i}", reference.cores, 0., [], reference.reserve, t)
                offsets[host.id] = i % 5
                hosts.append(host)
        demand = 1 + (ramp-1)*min(t/40,1)
        for h in hosts:
            h.extra = 0
        for m in moves[:]:
            if t < m.copy_at:
                continue
            if policy=='share_lane' and t < m.dst.free_at:
                # Hypothetical resumable warm work. Service the old receiver
                # four ticks out of five. Borrowing pushes its completion out.
                if t%5 != offsets[m.dst.id]:
                    continue
                m.dst.free_at+=1
                borrowed+=1
            speed = 1.
            if policy=='periodic_paced':
                usage=m.src.work(cost,demand)/m.src.cores
                speed=max(.1,1-usage) if usage>.5 else 1.
            if policy in ('paced','elastic_grain','leased','share_lane','potential'):
                # Keep a small copy lane even at saturation to avoid deadlock.
                spare = m.src.cores - m.src.work(cost,demand)
                speed = min(1.,max(.1,spare/m.src.reserve))
            m.src.extra += m.src.reserve*speed
            m.dst.extra += m.dst.reserve*speed
            m.progress = min(1.,m.progress+speed/m.copy_work)
            fraction = 1-(1-m.progress)**heat
            increment = max(0,m.rate*fraction-m.moved)
            m.moved += increment
            m.src.rates[m.index] -= increment
            m.dst.rates[m.new_index] += increment
            if m.progress >= 1:
                finished += 1
                moves.remove(m)
        # Each donor samples from the known peers, including unqualified peers.
        # Endpoints recheck actual budgets and reserve both lanes on acceptance.
        active = {h.id for m in moves for h in (m.src,m.dst)}
        sources = [h for h in hosts if h.rates and h.work(cost,demand) > .6*h.cores]
        rng.shuffle(sources)
        for src in sources:
            if src.id in active or src.free_at > t:
                continue
            if policy in ('leased','share_lane','potential') and t%5 != offsets[src.id]:
                continue
            if policy in ('periodic','periodic_paced') and t % 15:
                continue
            i = max(range(len(src.rates)),key=src.rates.__getitem__)
            rate = src.rates[i]*hot_fraction
            if rate*demand < 1:
                continue
            others = [h for h in hosts if h != src]
            contacts = rng.sample(others,min(len(others),breadth))
            probes += len(contacts)
            targets = []
            for h in contacts:
                if h.id in active or (h.free_at>t and policy!='share_lane') or max(h.ram,h.disk)>60:
                    continue
                if policy not in ('periodic','periodic_paced','spawn') and not h.rates:
                    continue
                spare = .6*h.cores-h.work(cost,demand)-h.reserve
                offer = rate
                if policy in ('elastic_grain','leased','share_lane','potential'):
                    # A full vnode is a candidate if the receiver can carry it;
                    # otherwise move just the prefix traffic needed for relief.
                    need = src.work(cost,demand)-.55*src.cores
                    limit = min(need,spare)/(cost*demand)
                    offer = src.rates[i]
                    # Binary subdivisions with a minimum useful arrival rate.
                    while offer > limit and offer*demand >= 1:
                        offer /= 2
                projected = h.work(cost,demand)+offer*cost*demand+h.reserve
                if offer*demand >= 1 and projected <= .6*h.cores+1e-9:
                    rank = offer
                    if policy=='potential':
                        # Sustained excess CPU squared, integrated over the
                        # remaining horizon, minus transient copy pressure.
                        gain=offer*cost*demand
                        cpu=src.work(cost,demand)/src.cores
                        excess=max(0,cpu-.6)
                        after=max(0,cpu-gain/src.cores-.6)
                        speed=max(.1,min(1.,(1-cpu)*src.cores/src.reserve))
                        duration=copy_seconds*offer/(src.rates[i]*hot_fraction*speed)
                        during=max(0,cpu+speed*src.reserve/src.cores-.6)
                        rank=(60-duration)*(excess**2-after**2)-duration*(during**2-excess**2)
                        if rank<=0:
                            continue
                    targets.append((-rank,projected/h.cores,h.id,h,offer))
            if not targets:
                continue
            winner=min(targets)
            rate,dst=winner[4],winner[3]
            delay = warmup if policy in ('periodic','periodic_paced','spawn') else 1
            copy_at=t+delay
            dst.rates.append(0.)
            copy_work = max(1.,copy_seconds*rate/(src.rates[i]*hot_fraction))
            moves.append(Move(src,dst,i,rate,t,copy_at,copy_work,
                              new_index=len(dst.rates)-1))
            active.update((src.id,dst.id))
            started+=1
        last_hot=0
        for h in hosts:
            work=h.work(cost,demand)
            cpu=work/h.cores
            last_hot += cpu>.60
            hot_seconds += cpu>.85
            if h.id in good:
                good[h.id] = good[h.id]+1 if cpu<=.60 else 0
                if good[h.id]>=5:
                    first_relief.setdefault(h.id,t-4)
            # One scalar queue of CPU work per host, in writes. No EBS latency.
            h.queue=max(0,h.queue+(work-h.cores)/cost)
            peak_queue=max(peak_queue,h.queue)
            queue_seconds+=h.queue
        max_drift=max(max_drift,abs(sum(sum(h.rates) for h in hosts)-initial_rate))
    waits=[first_relief.get(k,seconds) for k in initially_hot]
    return dict(hosts=len(hosts),initial_hosts=initial_hosts,initial_hot=len(waits),served=len(first_relief),
                median_delay_censored=median(waits) if waits else None,
                hot_host_seconds=hot_seconds,last_above_60=last_hot,
                peak_queue=round(peak_queue),queued_write_seconds=round(queue_seconds),
                borrowed_lane_seconds=borrowed,probes=probes,started=started,finished=finished,open=len(moves),rate_drift=max_drift)


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('live')
    p.add_argument('--at',type=int,default=345)
    p.add_argument('--seeds',type=int,default=10)
    p.add_argument('--cost-ms',type=float,default=1.)
    p.add_argument('--occupied-for',type=int,default=30)
    p.add_argument('--fraction',type=float,default=.5)
    p.add_argument('--copy-seconds',type=int,default=10)
    p.add_argument('--ramp',type=float,default=1.)
    p.add_argument('--heat',type=float,default=1.)
    p.add_argument('--additional-hosts', type=int, default=0)
    p.add_argument('--boot-seconds', type=int, default=45)
    a=p.parse_args()
    if min(a.seeds,a.copy_seconds,a.cost_ms,a.ramp,a.heat)<=0 or not 0<a.fraction<=1 or a.occupied_for<0:
        p.error('invalid scenario parameter')
    if min(a.additional_hosts, a.boot_seconds) < 0:
        p.error('additional hosts and boot delay must be nonnegative')
    frame=snapshot(a.live,a.at)
    for policy,k in [('periodic_paced',1),('periodic',1),('periodic',4),('spawn',4),('reuse',2),('reuse',4),('reuse',8),('paced',4),('elastic_grain',4),('leased',4),('share_lane',4),('potential',4)]:
        runs=[simulate(frame,policy,seed, cost=a.cost_ms/1000,occupied_for=a.occupied_for,
                       hot_fraction=a.fraction,copy_seconds=a.copy_seconds,breadth=k,ramp=a.ramp,heat=a.heat,
                       additional_hosts=a.additional_hosts,boot_seconds=a.boot_seconds)
              for seed in range(a.seeds)]
        summary={key:median([r[key] for r in runs]) for key in runs[0]
                 if all(r[key] is not None for r in runs)}
        print(json.dumps(dict(policy=policy,contacts=k,t=frame['t'],**summary)))
