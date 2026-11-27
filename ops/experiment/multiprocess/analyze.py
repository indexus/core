#!/usr/bin/env python3
"""Summarize owned multi-process runs without turning wall time into CPU work."""
import argparse, hashlib, json, math, pathlib


def quantile(bins, q):
    count=sum(bins)
    if not count: return None
    seen=0
    for i,n in enumerate(bins):
        seen+=n
        if seen>=math.ceil(count*q): return 2**i/1000  # upper bucket bound, ms


def summed_hist(rows,key):
    bins=[]
    for row in rows:
        h=row.get(key,[])
        bins += [0]*max(0,len(h)-len(bins))
        for i,n in enumerate(h): bins[i]+=n
    return {"samples":sum(bins),"p50_ms_upper":quantile(bins,.5),"p95_ms_upper":quantile(bins,.95),"p99_ms_upper":quantile(bins,.99)}


def summarize(path):
    samples=[json.loads(s) for s in (path/'samples.jsonl').read_text().splitlines()]
    load=[json.loads(s) for s in (path/'data/load/load.jsonl').read_text().splitlines()]
    config=json.loads((path/'config.json').read_text())
    audit=json.loads((path/'data/load/audit.json').read_text())
    final=json.loads((path/'data/load/load-summary.json').read_text())
    latest={}; process_peak=0; previous={}; first_installed={}; first_applied={}; stress_at=None; pressure_at=None
    host_values=[]; phase_peak=0; requests=[]
    for row in samples:
        if row.get('facts',{}).get('host_cpu_ok'): host_values.append(row['facts']['host_cpu_pct'])
        requests=row.get('linux',{}).get('requests',requests) or []
        if row.get('linux',{}).get('stress_active') and stress_at is None: stress_at=row['t']
        if any(x.get('reason')=='host_pressure' for x in requests) and pressure_at is None: pressure_at=row['t']
        active=0
        for n in row['nodes']:
            m=n.get('metrics'); slot=str(n['slot'])
            if not m: continue
            latest[slot]=m
            if m.get('handoffs',{}).get('taken',0)>0 and slot not in first_installed: first_installed[slot]=row['t']
            applied=m.get('demand',{}).get('applied',0)
            if applied and slot not in first_applied: first_applied[slot]=row['t']
            sec=m.get('process',{}).get('cpu_seconds',0)
            if slot in previous:
                t,cpu=previous[slot]
                if row['t']>t: process_peak=max(process_peak,100*(sec-cpu)/(row['t']-t)/row['facts'].get('cores',8))
            previous[slot]=(row['t'],sec)
            active+=sum(bool(x.get('running')) for x in m.get('handoffs',{}).get('open',{}).get('list',[]) or [])
        phase_peak=max(phase_peak,active)
    counters={k:sum(m.get('handoffs',{}).get(k,0) for m in latest.values()) for k in ['copy_slot_waits','copy_slot_timeouts','copy_slot_shared','copy_slot_busy']}
    kinds={}
    for m in latest.values():
        for kind,v in m.get('disk',{}).get('by_kind',{}).items():
            a=kinds.setdefault(kind,{'attempts':0,'commits':0,'errors':0,'hold_ns':0,'wait_ns':0,'max_hold_ns':0,'hold_hist':[]})
            for src,dst in [('Attempts','attempts'),('Commits','commits'),('Errors','errors')]: a[dst]+=v[src]
            a['hold_ns']+=v['Hold']['nanos']; a['wait_ns']+=v['Wait']['nanos']; a['max_hold_ns']=max(a['max_hold_ns'],v['Hold']['max_nanos'])
            h=v['Hold']['us_log2']; a['hold_hist'] += [0]*max(0,len(h)-len(a['hold_hist']))
            for i,n in enumerate(h): a['hold_hist'][i]+=n
    for v in kinds.values():
        v['mean_hold_ms_per_attempt']=v['hold_ns']/max(1,v['attempts'])/1e6
        v['p99_hold_ms_upper']=quantile(v.pop('hold_hist'),.99)
    return {**config,'run_name':path.name,'audit':audit,'load':final,'latency':summed_hist(load,'latency_us_log2'),
            'intended_latency':summed_hist(load,'intended_latency_us_log2'),
            'host_cpu_peak_pct':max(host_values,default=0),'process_cpu_peak_pct_of_host':process_peak,
            'copy_budget':counters,'sampled_active_handoffs_peak':phase_peak,
            'first_taken_observed_s':first_installed,'first_applied_observed_s':first_applied,
            'scale_requests':requests,'writer_by_kind':kinds,
            'stress_first_observed_s':stress_at,'host_pressure_first_observed_s':pressure_at,
            'pressure_delay_observed_s':pressure_at-stress_at if pressure_at is not None and stress_at is not None else None,
            'max_periodic_sum_of_summary_deficits':max((r.get('total_max_deficit',r.get('totals_deficit',0)) for r in load),default=0),
            'read_missing_events':sum(r.get('read_miss',0) for r in load),
            'read_failed_events':sum(r.get('read_failed',0) for r in load),
            'final_observed_storage_items':sum(m.get('storage',{}).get('items',0) for m in latest.values()),
            'notes':['Quantiles are upper bounds of base-2 buckets; periodic loader histograms exclude the final unreported fraction.',
                     'Taken is operation completion, not first installed right. Observation timestamps have about 1s resolution.',
                     'Active handoffs are not the maximum simultaneous copies; short phases can fall between samples.',
                     'Writer hold is wall time including fsync and scheduling; it must not be charged again as measured CPU or disk work.']}


def main():
    ap=argparse.ArgumentParser(); ap.add_argument('out',type=pathlib.Path); args=ap.parse_args()
    runs=[]
    for p in sorted(args.out.iterdir()):
        if (p/'config.json').exists(): runs.append(summarize(p))
    result={'calibrated':False,'calibration_scope':'measured local writer wall time only; no CPU/IO split or EC2 validation','runs':runs}
    (args.out/'summary.json').write_text(json.dumps(result,indent=2)+'\n')
    print(json.dumps([{k:r[k] for k in ['gomaxprocs','copy_slots','stress','load','latency','copy_budget','host_cpu_peak_pct']} for r in runs],indent=2))
    manifest={str(p.relative_to(args.out)):hashlib.sha256(p.read_bytes()).hexdigest() for p in args.out.glob('*/config.json')}
    (args.out/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')

if __name__=='__main__': main()
