#!/usr/bin/env python3
"""Replay captured local metrics with the same read-only dashboard as AWS.
Two agents are logical pools on one Docker VM; CPU values are not independent.
"""
import argparse, json, pathlib, sys
sys.path.insert(0,str(pathlib.Path(__file__).resolve().parents[1]))
from live import build, page
from fleet import Fleet, host


def render(run, output):
    samples=[json.loads(line) for line in (run/'samples.jsonl').read_text().splitlines()]
    loads=[json.loads(line) for line in (run/'controller/load/load.jsonl').read_text().splitlines()]
    audit_path=run/'controller/load/audit.json'
    audit=json.loads(audit_path.read_text()) if audit_path.exists() else None
    groups={}
    for row in loads:groups.setdefault(int(row['t'])//5,[]).append(row)
    latest={}
    for row in samples:latest[int(row['t'])//5]=row
    fleet=Fleet();prev=None;points=[];known={}
    for bucket,row in sorted(latest.items()):
        ms={f"http://{n['host']}:{19100+n['slot']}":n['metrics'] for n in row['nodes']}
        for monitor,m in ms.items():known[m['name']]=monitor
        point={'phase':'Local · 2 pools / 1 VM · demande réelle','t':row['t'],'wall':row['t'],
               'metrics_stale':False,'load_stale':bucket not in groups}
        prev=build(point,groups.get(bucket),ms,prev)
        machines=[{'agent':f'http://{label}:22100','facts':row[label],'wall':row['t']} for label in ['donor','reserve'] if 'error' not in row.get(label,{})]
        frame=fleet.frame(ms,machines,row['t']);frame['source']='local-multiprocess'
        for m in frame['machines']:m['cpu_scope']='shared Docker VM'
        for tr in frame['transfers']:
            target=tr.get('target_machine')
            if target in known:tr['target']=known[target];tr['target_machine']=host(known[target])
            elif target not in {'donor','reserve'}:tr['target_machine']=None
        point['fleet']=frame;points.append(point)
    if audit and points:points[-1].update(audit='pass' if audit['pass'] else 'fail',read_errors=audit.get('read_errors'),missing=audit.get('missing'),double=audit.get('double_owned_zones'))
    output.mkdir(parents=True,exist_ok=True)
    (output/'live.jsonl').write_text(''.join(json.dumps(p)+'\n' for p in points))
    status=('audit réussi' if audit['pass'] else 'audit échoué') if audit else 'audit absent'
    title='Indexus local — demande réelle ; '+status
    (output/'live.html').write_text(page(points,title=title,live=False))
    return points


if __name__=='__main__':
    ap=argparse.ArgumentParser(description=__doc__);ap.add_argument('run',type=pathlib.Path);ap.add_argument('out',type=pathlib.Path)
    args=ap.parse_args();render(args.run,args.out)
