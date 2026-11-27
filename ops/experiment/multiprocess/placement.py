#!/usr/bin/env python3
"""Real provisioner/hooks + two agents, on one shared Linux VM.
Demand mode uses real node write-pressure requests; controlled mode diagnoses coalescing without creating donor demand.
"""
import argparse, concurrent.futures, json, os, pathlib, shlex, subprocess, threading, time
from run import request


def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--bin',type=pathlib.Path,required=True)
    ap.add_argument('--out',type=pathlib.Path,required=True)
    ap.add_argument('--image',required=True)
    ap.add_argument('--data-volume-prefix', help='existing empty Docker volumes PREFIX-donor and PREFIX-reserve for the two stores')
    ap.add_argument('--trigger',choices=['demand','controlled'],default='demand')
    ap.add_argument('--resource-receiver', action='store_true',
                    help='use resource-based admission on the receiving host; expose twelve monitor ports')
    ap.add_argument('--linear', action='store_true', help='start at 200 writes/s and add 10 writes/s each second during the demand test')
    ap.add_argument('--writes-per-vnode', type=int, default=150)
    ap.add_argument('--cpu-target', type=float, default=0, help='0 uses the node default')
    args=ap.parse_args()
    if args.writes_per_vnode < 1 or not 0 <= args.cpu_target <= 100:
        ap.error('invalid writes-per-vnode or cpu-target')
    if args.linear and args.trigger != 'demand':
        ap.error('--linear requires --trigger demand')
    out=args.out.resolve();out.mkdir(parents=True,exist_ok=False)
    tag=f'indexus-place-{os.getpid()}';network=tag;containers=[];jobs=[]
    stop=threading.Event();collector=None;failures=[];network_created=False
    def docker(*a,**kw): return subprocess.run(['docker',*map(str,a)],check=True,**kw)
    def service(container,argv,log):
        docker('exec','-d',container,'/bin/sh','-c','exec '+shlex.join(argv)+' > /data/'+log+' 2>&1')
    def finish(job,log,timeout):
        deadline=time.monotonic()+timeout
        while True:
            if failures:raise RuntimeError(failures[0])
            remaining=deadline-time.monotonic()
            if remaining<=0:raise TimeoutError(f'{log}: deadline exceeded')
            try:raw,_=job.communicate(timeout=min(1,remaining));break
            except subprocess.TimeoutExpired:pass
        (out/log).write_bytes(raw)
        if job.returncode: raise RuntimeError(f'{log}: exit {job.returncode}')
    def wait(call,seconds=60):
        end=time.monotonic()+seconds
        while time.monotonic()<end:
            try:
                value=call()
                if value: return value
            except OSError: pass
            time.sleep(.25)
        raise RuntimeError('readiness timeout')
    windows={'donor':5, 'reserve':12 if args.resource_receiver else 5}
    hooks=pathlib.Path(__file__).resolve().parents[1]/'elastic'
    names={label:tag+'-'+label for label in ['controller','donor','reserve']}
    (out/'setup.json').write_text(json.dumps({'trigger':args.trigger,'linear':args.linear,'writes_per_vnode':args.writes_per_vnode,'cpu_target':args.cpu_target,'resource_receiver':args.resource_receiver,'source':subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip(),
        'scope':'shared Linux VM; demand thresholds fixed before startup; no independent host CPU qualification'},indent=2)+'\n')
    try:
        docker('network','create',network,stdout=subprocess.DEVNULL);network_created=True
        for label in names:
            cmd=['run','-d','--init','--name',names[label],'--hostname',label,'--network',network,'--network-alias',label,
                 '--mount',f'type=bind,src={args.bin.resolve()},dst=/bench,readonly','-e','GOMEMLIMIT=512MiB','-e','GOMAXPROCS=2']
            if label=='controller':
                cmd+=['--mount',f'type=bind,src={hooks},dst=/hooks,readonly',
                      '-p','127.0.0.1:34802:22000','-p','127.0.0.1:34803:22010']
            else:
                if args.data_volume_prefix:
                    volume = args.data_volume_prefix + '-' + label
                    docker('volume', 'inspect', volume, stdout=subprocess.DEVNULL)
                    cmd += ['--mount', f'type=volume,src={volume},dst=/data']
                agent=34800 if label=='donor' else 34801;mon=34900 if label=='donor' else 34910
                cmd+=['-p',f'127.0.0.1:{agent}:22100']
                for i in range(windows[label]):cmd+=['-p',f'127.0.0.1:{mon+i}:{19100+i}']
            cmd+=['--entrypoint','/bin/sh',args.image,'-c','mkdir -p /data; exec sleep 1800']
            docker(*cmd,stdout=subprocess.DEVNULL);containers.append(names[label])
        control=names['controller']
        service(control,['/bench/issuer','-addr',':22000','-network','experiment','-key','/data/issuer.key','-bootstrap','donor'],'issuer.log')
        wait(lambda:request(34802,'/health'))
        token=request(34802,'/v1/issue/token',body={'client_id':'experiment','scopes':['read','write']})['token']
        docker('exec',control,'/bin/sh','-c',"printf '%s\\n' http://donor:22100 http://reserve:22100 > /data/agents; printf '%s\\n' 'MACHINES_MAX=2' 'SLOTS=5' 'SPARE_RATIO=0' > /data/machine.env")
        docker('exec','-d','-e','INDEXUS_ELASTIC_DIR=/data',control,'/bin/sh','-c',
               'exec /bench/provisioner -addr :22010 -issuer http://controller:22000 -network experiment -localDir /hooks -spawnMax 0 > /data/provisioner.log 2>&1')
        wait(lambda:request(34803,'/v1/placement',token=token))
        for label in ['donor','reserve']:
            name=names[label]
            info=json.loads(subprocess.check_output(['docker','inspect',name]))[0]
            ip=info['NetworkSettings']['Networks'][network]['IPAddress']
            argv=['/bench/experiment','agent','-listen',':22100','-slots',str(windows[label]),'-node','/bench/node','-dir','/data/slots',
                  '-issuer','http://controller:22000','-bootstrap','http://donor:21100','-advertise',ip,
                  '-copy-slots','2','-compaction=false','-resource-signals','-provisioner','http://controller:22010']
            if args.resource_receiver and label=='reserve':argv+=['-resource-slots']
            if args.trigger=='demand':argv+=['-writes-per-vnode',str(args.writes_per_vnode)]
            if args.cpu_target:argv+=['-cpu-target',str(args.cpu_target)]
            docker('exec','-d','-e','INDEXUS_BEARER='+token,name,'/bin/sh','-c','exec '+shlex.join(argv)+' > /data/agent.log 2>&1')
        for port in (34800,34801):
            # A healthy resource-admitted agent has an empty inventory until its first spawn.
            wait(lambda: isinstance(request(port,'/slots'),list))
        request(34800,'/spawn?genesis=1',True,190)
        for i in range(1,5):
            request(34800,'/spawn?wait=ready',True,190)
            wait(lambda i=i:request(34900+i,'/status').get('client_ready'))
        wait(lambda:len(request(34800,'/facts').get('vnodes') or [])==5)
        initial={'donor':request(34800,'/facts'),'reserve':request(34801,'/facts')}
        (out/'initial.json').write_text(json.dumps(initial,indent=2)+'\n')
        if initial['donor']['free']!=0 or not initial['reserve'].get('receiver_ready'):raise RuntimeError('no full donor and ready reserve')
        began=time.monotonic()
        def collect():
            with (out/'samples.jsonl').open('w') as f:
                while not stop.is_set():
                    row={'t':time.monotonic()-began,'nodes':[]}
                    for label,port in [('donor',34800),('reserve',34801)]:
                        slots=[]
                        try:
                            row[label]=request(port,'/facts');slots=request(port,'/slots')
                            for slot in slots:
                                if slot.get('started') and not slot.get('alive'):failures.append(f'{label} exited: {slot}')
                        except OSError as e:row[label]={'error':str(e)}
                        base=34900 if label=='donor' else 34910
                        for i in [s['slot'] for s in slots if s.get('alive')]:
                            try:row['nodes'].append({'host':label,'slot':i,'metrics':request(base+i,'/metrics',timeout=1)})
                            except OSError:pass
                    try:row['placement']=request(34803,'/v1/placement',token=token)
                    except OSError as e:row['placement']={'error':str(e)}
                    f.write(json.dumps(row)+'\n');f.flush();stop.wait(.5)
        collector=threading.Thread(target=collect);collector.start()
        load=subprocess.Popen(['docker','exec',control,'/bench/experiment','load','-targets','http://donor:21100',
            '-issuer','http://controller:22000','-route','direct','-rate',str(200 if args.linear or args.trigger=='controlled' else 400),'-duration','45s','-collections','16','-seed','19',
            '-scenario','spike' if args.trigger=='demand' else 'normal','-spike-factor','1','-spike-start','0s','-spike-length','45s',
            *(['-ramp','10','-ramp-every','1s'] if args.linear else []),
            '-tag','Place','-workers','64','-readers','1','-read-rate','10','-out','/data/load','-drain-timeout','30s'],stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
        jobs.append(load)
        spawned=coalesced=0
        if args.trigger=='controlled':
            time.sleep(15)
            vnodes=request(34800,'/facts')['vnodes']
            def scale(vnode):
                body={'requester_id':vnode,'prefer_near':vnode,'reason':'host_pressure','signal':'cpu','spawn_count':1,'transfers_in_flight':0}
                at=time.monotonic()-began
                return {'requester':vnode,'at':at,'answer':request(34803,'/v1/scale',body=body,token=token,timeout=90),'end':time.monotonic()-began}
            with concurrent.futures.ThreadPoolExecutor(max_workers=5) as pool:answers=list(pool.map(scale,vnodes))
            (out/'answers.json').write_text(json.dumps(answers,indent=2)+'\n')
            spawned=sum(x['answer'].get('action')=='spawned' for x in answers)
            coalesced=sum(x['answer'].get('coalesced')=='host_pressure' for x in answers)
            if (spawned,coalesced)!=(1,4):raise RuntimeError(f'wave started {spawned}, coalesced {coalesced}')
        finish(load,'load.log',150)
        if failures:raise RuntimeError(failures[0])
        final=request(34801,'/facts')
        if args.trigger=='demand':
            decision=request(34803,'/v1/placement',token=token)
            spawned=sum(decision.get('applied',{}).values());coalesced=decision.get('decided',{}).get('host_coalesced',0)
            if spawned<1 or final['running']<1:raise RuntimeError('no useful capacity was started')
            applied=sum(request(34910+i,'/metrics')['demand']['applied'] for i in [s['slot'] for s in request(34801,'/slots') if s.get('alive')])
            if applied<1:raise RuntimeError('receiver never applied a client write')
        elif final['running']!=1:raise RuntimeError(f'reserve slots: {final}')
        summary=json.loads(subprocess.check_output(['docker','exec',control,'cat','/data/load/load-summary.json']))
        if summary['acked']!=summary['offered'] or summary.get('exhausted') or summary.get('shed'):raise RuntimeError(f'incomplete load: {summary}')
        targets=','.join(f'http://{host}:{21100+i}' for host,count in windows.items() for i in range(count))
        monitors=','.join(f'http://{host}:{19100+i}' for host,count in windows.items() for i in range(count))
        audit=subprocess.Popen(['docker','exec',control,'/bench/experiment','audit','-targets',targets,'-monitors',monitors,
            '-issuer','http://controller:22000','-out','/data/load','-sample','0','-settle','60s'],stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
        jobs.append(audit);finish(audit,'audit.log',150)
        # A pending spawn can complete while audit settles. Report final
        # facts and counters from this stage, not stale load-end counters.
        final=request(34801,'/facts')
        placement=request(34803,'/v1/placement',token=token)
        spawned=sum(placement.get('applied',{}).values())
        coalesced=placement.get('decided',{}).get('host_coalesced',0)
        result={'base':subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip(),
                'trigger':args.trigger,'scope':'two agents/three containers, shared VM CPU; real hooks and resource facts',
                'spawned':spawned,'coalesced':coalesced,'final_reserve':final,'placement':placement}
        (out/'result.json').write_text(json.dumps(result,indent=2)+'\n')
    except Exception as error:
        (out/'failure.json').write_text(json.dumps({'error':str(error)},indent=2)+'\n')
        raise
    finally:
        stop.set()
        if collector:collector.join(timeout=20)
        for job in jobs:
            if job.poll() is None:job.terminate()
        for name in containers:
            subprocess.run(['docker','cp',name+':/data',str(out/name.removeprefix(tag+'-'))],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
            subprocess.run(['docker','rm','-f',name],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        if network_created:subprocess.run(['docker','network','rm',network],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    audit=json.loads((out/'controller/load/audit.json').read_text())
    if not audit['pass']:raise RuntimeError('audit failed')
    print(json.dumps({'out':str(out),'audit':audit,'spawned':spawned,'coalesced':coalesced}))

if __name__=='__main__':main()
