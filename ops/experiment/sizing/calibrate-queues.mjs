// Partial calibration only: measured writer wall times and work-counter laws.
// Placement, CPU/IO service splits and burst schedules remain unqualified.
import {readFile,writeFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import {resolve} from 'node:path';
import {replayQueues} from './queues.mjs';

if(process.argv.length!==4)throw Error('usage: node calibrate-queues.mjs /absolute/summary.json /absolute/output.json');
const input=await readFile(resolve(process.argv[2]));
const evidence=JSON.parse(input);
const sinceLog=await readFile(resolve(process.argv[2],'../since-work.log'),'utf8');
const since=sinceLog.match(/8 zones added to 40000: (\d+) subtrees opened/);
if(!since)throw Error('missing subtree work observation (run TestInventorySinceSkipsWhatACommitDidNotTouch -v)');
const collectionLog=await readFile(new URL('../../../docs/benchmarks/claude-collect-summary-20261126/collection-after.log',import.meta.url),'utf8');
const reserved=[...collectionLog.matchAll(/sessions=(\d+) receipts=(\d+) reads=(\d+) scanned=(\d+) commits=(\d+) elapsed=/g)]
 .map(m=>({sessions:+m[1],receipts:+m[2],reads:+m[3],scanned:+m[4],commits:+m[5]}));
if(!reserved.length||reserved.some(x=>x.reads!==8+x.receipts*(x.sessions+1)))throw Error('reservation work law no longer matches evidence');
const installed=[...collectionLog.matchAll(/(\d+) waiting sessions: collect read (\d+) rows, examined (\d+) keys, (\d+) commits/g)]
 .map(m=>({sessions:+m[1],reads:+m[2],scanned:+m[3],commits:+m[4],receipts:64}));
if(installed.length!==3||installed.some(x=>x.reads!==71))throw Error('installed work evidence changed');
const rows=[];
for(const r of evidence.runs.filter(x=>!x.stress)) {
 const measured=r.writer_by_kind.write;
 if(!measured?.attempts)throw Error('no measured write service');
 // Five declared equal steady shares; real joins have unequal time-varying
 // shares. This isolates serial writer occupancy, not the observed p99.
 const costs={writeCpuMs:0,writeIoMs:0,writeHoldMs:measured.mean_hold_ms_per_attempt};
 const config={...costs,durationMs:2000,stepMs:.125,rate:100,
  hosts:[{id:'local',cores:8}],nodes:Array.from({length:5},(_,i)=>({id:`n${i}`,host:'local',weight:1}))};
 rows.push({run_name:r.run_name,gomaxprocs:r.gomaxprocs,copy_slots:r.copy_slots,measuredWriterByKind:r.writer_by_kind,
  assumptions:['equal steady routing shares','mean service only','no maintenance bursts or group commit','CPU and disk work not identified'],
  config,result:replayQueues(config)});
}
await writeFile(resolve(process.argv[3]),JSON.stringify({calibrated:false,
 calibration_scope:'partial: local wall time of writer transactions and measured logical work; not AWS p99',
 source_sha256:createHash('sha256').update(input).digest('hex'),
 collection_evidence_sha256:createHash('sha256').update(collectionLog).digest('hex'),
 laws:{inventory:{catalog_zones:40000,added_zones:8,opened_subtrees:+since[1],scope:'one deterministic persistent-tree fixture; not a universal time coefficient'},reserved:{formula:'reads = 8 + receipts * (sessions + 1)',scope:'measured fixture only',observations:reserved},
       installed:{scope:'64 receipts, measured at 0/8/32 waiting sessions; no extrapolation to other receipt sizes',observations:installed}},
 missing:['CPU and I/O service by operation','per-zone staging cost under larger payloads','fsync tail distribution','group commit and burst schedules',
          'copy timeout/bypass scheduling','installed-share and membership traces across hosts','boot/placement/retirement','cgroup versus host resource scope'],rows},null,2)+'\n');
console.log(`partial calibration: ${rows.length} observed configurations; no AWS forecast`);
