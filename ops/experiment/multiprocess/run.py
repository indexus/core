#!/usr/bin/env python3
"""Linux multi-process qualification using real agent/node/loader binaries.
No AWS, no automatic retries or resource-threshold changes. Data stay in one
owned disposable container; unrelated Docker services are never stopped.
"""
import argparse, collections, concurrent.futures, json, os, pathlib, subprocess, threading, time, urllib.request, urllib.parse


def request(port, path, post=False, timeout=4, body=None, token=None):
    headers={}
    if token: headers['Authorization']='Bearer '+token
    data=None
    if body is not None:
        data=json.dumps(body).encode();headers['Content-Type']='application/json'
    req=urllib.request.Request(f'http://127.0.0.1:{port}{path}',data=data,
                               headers=headers,method='POST' if post or body is not None else 'GET')
    with urllib.request.urlopen(req,timeout=timeout) as r:
        raw=r.read().decode()
        try: return json.loads(raw)
        except ValueError: return raw


def collection_count(body):
    # Shallow /set can return a contact with no set: that is not zero.
    if body.get('set') is None:
        return None
    return sum(v.get('count',0) for v in body['set'].values())


def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--bin',required=True,type=pathlib.Path)
    ap.add_argument('--out',required=True,type=pathlib.Path)
    ap.add_argument('--image',required=True)
    ap.add_argument('--gomaxprocs',choices=['1','2','default'],default='default')
    ap.add_argument('--copy-slots',type=int,choices=[1,2],default=1)
    ap.add_argument('--quiet-seconds',type=int,default=0,help='observe cached totals after writes stop, before exact audit')
    ap.add_argument('--stress',action='store_true',help='40s controlled CPU pressure; requires an 8-core VM')
    args=ap.parse_args(); out=args.out.resolve(); out.mkdir(parents=True,exist_ok=False)
    name=f'indexus-mp-{os.getpid()}'
    def docker(*a,**kw): return subprocess.run(['docker',*map(str,a)],check=True,**kw)
    def launch(*a): return subprocess.Popen(['docker','exec',name,*map(str,a)],stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
    def joblog(p,filename,timeout):
        raw,_=p.communicate(timeout=timeout); (out/filename).write_bytes(raw)
        if p.returncode: raise RuntimeError(f'{filename}: exit {p.returncode}')
    samples=[]; failures=[]; stop=threading.Event(); collector=None; live_jobs=[]
    try:
        command=['run','-d','--init','--name',name,'--mount',f'type=bind,src={args.bin.resolve()},dst=/bench,readonly',
                 '-p','127.0.0.1:33800:22100','-p','127.0.0.1:33801:22200','-p','127.0.0.1:33802:22000']
        for i in range(5): command+=['-p',f'127.0.0.1:{33900+i}:{19100+i}','-p',f'127.0.0.1:{33810+i}:{21100+i}']
        command+=['-e','GOMEMLIMIT=512MiB']
        if args.gomaxprocs!='default': command+=['-e',f'GOMAXPROCS={args.gomaxprocs}']
        command+=['--entrypoint','/bin/sh',args.image,'-c','mkdir -p /data; exec sleep 1800']
        docker(*command,stdout=subprocess.DEVNULL)
        docker('inspect',name,stdout=(out/'container.json').open('w'))
        docker('exec','-d',name,'/bin/sh','-c','exec /bench/probe > /data/probe.log 2>&1')
        docker('exec','-d',name,'/bin/sh','-c','exec /bench/issuer -addr :22000 -network experiment -key /data/issuer.key -bootstrap 127.0.0.1 > /data/issuer.log 2>&1')
        time.sleep(1)
        snap=request(33801,'/snapshot')
        if args.stress and snap['cpus']!=8: raise RuntimeError('stress fixture requires exactly 8 visible cores')
        agent=['/bench/experiment','agent','-listen',':22100','-slots','5','-node','/bench/node','-dir','/data/slots',
               '-issuer','http://127.0.0.1:22000','-bootstrap','http://127.0.0.1:21100','-copy-slots',str(args.copy_slots),
               '-compaction=false','-resource-signals','-provisioner','http://127.0.0.1:22200']
        docker('exec','-d',name,'/bin/sh','-c','exec '+' '.join(agent)+' > /data/agent.log 2>&1')
        for _ in range(40):
            try: request(33800,'/slots'); break
            except OSError: time.sleep(.25)
        else: raise RuntimeError('agent failed to start')
        request(33800,'/spawn?genesis=1',True,timeout=190)
        begin=time.monotonic()
        def collect():
            with (out/'samples.jsonl').open('w') as f:
                while not stop.is_set():
                    row={'t':time.monotonic()-begin,'nodes':[]}
                    for label,port,path in [('facts',33800,'/facts'),('slots',33800,'/slots'),('linux',33801,'/snapshot')]:
                        try: row[label]=request(port,path,timeout=1)
                        except OSError as e: row[label]={'error':str(e)}
                    for i in range(5):
                        try: row['nodes'].append({'slot':i,'metrics':request(33900+i,'/metrics',timeout=1)})
                        except OSError as e: row['nodes'].append({'slot':i,'error':str(e)})
                    for slot in row.get('slots',[]) if isinstance(row.get('slots'),list) else []:
                        if slot.get('started') and not slot.get('alive'):
                            failures.append(f"slot {slot['slot']} exited during qualification")
                    f.write(json.dumps(row)+'\n'); f.flush(); samples.append(row); stop.wait(1)
        collector=threading.Thread(target=collect); collector.start()
        targets=','.join(f'http://127.0.0.1:{21100+i}' for i in range(5))
        mons=','.join(f'http://127.0.0.1:{19100+i}' for i in range(5))
        # One loader process, deterministic seed and arrival clock; four joins
        # start while writes continue. The initial target is the genesis.
        duration=70 if args.stress else 35
        load=launch('/bench/experiment','load','-targets','http://127.0.0.1:21100','-issuer','http://127.0.0.1:22000',
                    '-route','direct','-rate','100','-duration',f'{duration}s','-collections','8','-seed','19','-tag','Multi',
                    '-workers','64','-readers','1','-read-rate','10','-total-share','0.2','-out','/data/load','-drain-timeout','30s')
        live_jobs.append(load); time.sleep(6)
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            futures=[pool.submit(request,33800,'/spawn',True,190) for _ in range(4)]
            for f in futures: f.result(timeout=195)
        if args.stress:
            time.sleep(8); request(33801,'/stress',True)
        joblog(load,'load.log',150)
        if failures: raise RuntimeError(failures[0])
        final=json.loads(subprocess.check_output(['docker','exec',name,'cat','/data/load/load-summary.json']))
        if final['acked']!=final['offered'] or final.get('shed') or final.get('exhausted') or final.get('queued_unfinished'):
            raise RuntimeError(f'load did not complete: {final}')
        if args.quiet_seconds:
            raw=subprocess.check_output(['docker','exec',name,'cat','/data/load/acked.jsonl'],text=True)
            expected=collections.Counter(json.loads(line)['c'] for line in raw.splitlines())
            issue=urllib.request.Request('http://127.0.0.1:33802/v1/issue/token',data=b'{"client_id":"experiment","scopes":["read"]}',headers={'Content-Type':'application/json'})
            with urllib.request.urlopen(issue,timeout=5) as res: token=json.load(res)['token']
            observations=[]; quiet_at=time.monotonic()
            while True:
                elapsed=time.monotonic()-quiet_at; row={'quiet_s':elapsed,'collections':{}}
                for collection,want in expected.items():
                    got=[]
                    for i in range(5):
                        query=urllib.parse.urlencode({'location':'@','collection':collection,'deep':'false','refresh':'false'})
                        req=urllib.request.Request(f'http://127.0.0.1:{33810+i}/set?{query}',headers={'Authorization':'Bearer '+token})
                        with urllib.request.urlopen(req,timeout=5) as res: body=json.load(res)
                        got.append(collection_count(body))
                    row['collections'][collection]={'expected':want,'by_node':got}
                observations.append(row)
                if elapsed>=args.quiet_seconds: break
                time.sleep(min(5,args.quiet_seconds-elapsed))
            (out/'quiet-totals.json').write_text(json.dumps(observations,indent=2)+'\n')
        audit=launch('/bench/experiment','audit','-targets',targets,'-monitors',mons,'-issuer','http://127.0.0.1:22000',
                     '-out','/data/load','-sample','0','-settle','60s')
        live_jobs.append(audit); joblog(audit,'audit.log',150)
        result={'gomaxprocs':args.gomaxprocs,'copy_slots':args.copy_slots,'stress':args.stress,'quiet_seconds':args.quiet_seconds,
                'source_commit':subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip(),
                'scope':'five processes, one Linux Docker VM; no EC2 throughput claim; no provisioning'}
        (out/'config.json').write_text(json.dumps(result,indent=2)+'\n')
    finally:
        stop.set()
        if collector: collector.join(timeout=15)
        for p in live_jobs:
            if p.poll() is None: p.terminate()
        # Preserve evidence even on a failed qualification, then stop only
        # the container this invocation created. No pkill or global prune.
        subprocess.run(['docker','cp',f'{name}:/data',str(out/'data')],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        subprocess.run(['docker','rm','-f',name],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    audit=json.loads((out/'data/load/audit.json').read_text())
    print(json.dumps({'out':str(out),'audit':audit}))
    if not audit.get('pass'): raise SystemExit('qualification failed; no automatic next run')

if __name__=='__main__': main()
