// Counterexamples only. These synthetic costs are not a fit to AWS p99.
import {mkdir,writeFile,readFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import {resolve} from 'node:path';
import {replayQueues} from './queues.mjs';

if(!process.argv[2])throw Error('usage: node run-queues.mjs /absolute/output');
const out=resolve(process.argv[2]);await mkdir(out,{recursive:true});
const hosts=Array.from({length:32},(_,i)=>({id:`h${i}`,cores:4,ioSlots:1}));
const nodes=Array.from({length:64},(_,i)=>({id:`n${i}`,host:`h${i>>1}`,weight:i<8?1:0}));
const collection=(cpuMs,everyMs)=>Array.from({length:8},(_,i)=>({node:`n${i}`,cpuMs,everyMs,writer:true}));
const summary=Array.from({length:8},(_,i)=>({node:`n${i}`,cpuMs:8,everyMs:10,writer:false}));
const installs=Array.from({length:8},(_,i)=>({atMs:1000,from:`n${i}`,to:`n${i+8}`,fraction:.5}));
const scenarios=[
  {name:'no_background',background:[]},
  {name:'short_collector_50pct',background:collection(1,2)},
  {name:'long_collector_50pct',background:collection(100,200)},
  {name:'collector_90pct_ready_hosts_only',background:collection(9,10)},
  {name:'collector_90pct_installed_piece',background:collection(9,10),installs},
  {name:'collector_and_summary',background:[...collection(9,10),...summary]},
];
console.log('SYNTHETIC COSTS: no measured AWS throughput or p99 forecast');
const rows=[];
for(const scenario of scenarios)for(const stepMs of [.25,.125]) {
  const {name,...variant}=scenario;
  const config={durationMs:4000,stepMs,rate:8000,hosts,nodes,...variant};
  const result=replayQueues(config);
  rows.push({name,config,result});
  console.log(name,stepMs,'ACK',result.acked,'offered',result.offered,'intended p99',result.intendedMs.p99,
    'maintenance debt',result.background.reduce((n,b)=>n+b.pending,0));
}
const sha=createHash('sha256').update(await readFile(new URL('./queues.mjs',import.meta.url))).digest('hex');
await writeFile(resolve(out,'queues.json'),JSON.stringify({source_sha256:sha,calibrated:false,
  note:'64 model identities, 32 declared ready hosts, 8 installed traffic shares. No AWS/p2p execution; no boot/placement/authority proof. Costs synthetic; compare mechanisms and timestep sensitivity, not absolute AWS latencies.',rows},null,2)+'\n');
const fields=['source','name','stepMs','offered','acked','shed','queued','inFlight','p99_service_ms','p99_intended_ms','maintenance_pending','mean_cpu_pct','max_host_mean_cpu_pct'];
const csv=rows.map(({name,config,result:r})=>['synthetic',name,config.stepMs,r.offered,r.acked,r.shed,r.queued,r.inFlight,r.serviceMs.p99,r.intendedMs.p99,
 r.background.reduce((n,b)=>n+b.pending,0),r.hosts.reduce((n,h)=>n+h.cpuPct,0)/r.hosts.length,Math.max(...r.hosts.map(h=>h.cpuPct))]);
await writeFile(resolve(out,'queues.csv'),[fields,...csv].map(row=>row.join(',')).join('\n')+'\n');
