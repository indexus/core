from pathlib import Path
import json,gzip,math,collections,urllib.parse
p=Path(__file__).parent
old=Path('/Users/remihenriot/Documents/Codex/2026-11-21/reg/outputs/aws-resource-16k-20261126/captured/steps-1')
def lines(f):
 with f.open() as src:
  for line in src:
   try:yield json.loads(line)
   except ValueError:pass
def q(hist,quant):
 n=sum(hist)
 if not n:return None
 at=math.ceil(n*quant);acc=0
 for i,x in enumerate(hist):
  acc+=x
  if acc>=at:return (1<<i)/1000

def window(path,lo,hi):
 rows=[r for r in lines(path/'load.jsonl') if lo<=r.get('t',0)<hi]
 def h(key):
  a=[]
  for r in rows:
   b=r.get(key,[])
   if len(b)>len(a):a.extend([0]*(len(b)-len(a)))
   for i,v in enumerate(b):a[i]+=v
  return a
 offered=sum(r.get('offered',0) for r in rows);acked=sum(r.get('acked',0) for r in rows)
 return {'from_s':lo,'until_s':hi,'samples':len(rows),'offered':offered,'acked':acked,'acked_share':acked/offered if offered else None,'shed':sum(r.get('shed',0) for r in rows),'service_ms':{str(v):q(h('latency_us_log2'),v) for v in (.5,.95,.99)},'intended_ms':{str(v):q(h('intended_latency_us_log2'),v) for v in (.5,.95,.99)},'max_queue':max((r.get('queued',0) for r in rows),default=0)}
windows={}
for label,lo,hi in [('2k',5,120),('4k',240,360),('8k_common',480,515),('8k',480,600),('last_ramp',600,634)]:
 windows[label]={'before':window(old,lo,hi),'after':window(p/'captured/steps-1',lo,hi)}
ledger={'lines':0,'main':0,'probe':0,'bytes':0}
with gzip.open(p/'captured/operator/acked-final.jsonl.gz','rb') as f:
 for line in f:
  ledger['lines']+=1;ledger['bytes']+=len(line)
  r=json.loads(line)
  if r.get('i','').startswith('p'):ledger['probe']+=1
  else:ledger['main']+=1
last={}; demand_peaks={'in_flight':0,'oldest_wait_ms':0}; max_busy={'wall':0,'host':'','count':0}; buckets=collections.defaultdict(dict)
for r in lines(p/'captured/steps-1/metrics.jsonl'):
 if 'metrics' not in r:continue
 m=r['metrics'];last[r['monitor']]=r
 for k in demand_peaks:demand_peaks[k]=max(demand_peaks[k],m.get('demand',{}).get(k,0))
 host=urllib.parse.urlparse(r['monitor']).hostname
 count=sum(bool(o.get('running')) and o.get('phase')!='copy-slot' for o in (m.get('handoffs',{}).get('open',{}).get('list') or []))
 buckets[(int(r['wall'])//2,host)][r['monitor']]=count
for (t,host),nodes in buckets.items():
 n=sum(nodes.values())
 if n>max_busy['count']:max_busy={'wall':t*2,'host':host,'count':n,'note':'running donor jobs, excluding copy-slot phase; asynchronous 2-second bucket, not an exact CPU-copy concurrency gauge'}
copy={k:sum(r['metrics'].get('handoffs',{}).get(k,0) for r in last.values()) for k in ('copy_slot_waits','copy_slot_timeouts','copy_slot_shared','copy_slot_busy')}
placement=list(lines(p/'captured/operator/placement.jsonl')); placement=[r for r in placement if 'error' not in r.get('placement',{})]
result={'windows':windows,'ledger':ledger,'demand_peaks':demand_peaks,'copy_slot_counters':copy,'max_running_donor_jobs_bucket':max_busy,'last_placement':placement[-1] if placement else None,'load_summary':json.loads((p/'captured/steps-1/load-summary.json').read_text())}
(p/'analysis.json').write_text(json.dumps(result,indent=2)+'\n')
for label,win in windows.items():print(label,json.dumps(win))
print('ledger',ledger,'demand peaks',demand_peaks,'copy slots',copy,'jobs',max_busy)
if placement:print('placement counts',placement[-1]['placement'].get('decided'),placement[-1]['placement'].get('applied'))
