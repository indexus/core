import test from 'node:test';
import assert from 'node:assert/strict';
import {replayQueues,stepsRate} from './queues.mjs';

function conserved(r) {
  assert.equal(r.offered,r.acked+r.shed+r.queued+r.inFlight);
  for(const b of r.background)assert.equal(b.released,b.completed+b.pending);
  for(const h of r.hosts)assert.ok(h.cpuPct<=100+1e-7&&h.ioPct<=100+1e-7);
  for(const n of r.nodes)assert.ok(n.writerPct<=100+1e-7);
}

test('bounded workers report loss, unfinished work and intended latency',()=>{
  const r=replayQueues({durationMs:200,rate:2000,workers:1,queueCapacity:16,writeCpuMs:2,writeIoMs:0});
  conserved(r);assert.equal(r.peakQueued,16);assert.equal(r.peakInFlight,1);
  assert.ok(r.shed>0&&r.unfinishedFraction>0);
  assert.ok(r.intendedMs.p99>r.serviceMs.p99*4);
});

test('an atomic maintenance commit blocks writes even on an otherwise idle multicore host',()=>{
  const base={durationMs:20,rate:1000,background:[{node:'n0',everyMs:1000,cpuMs:100,writer:true}]};
  const r=replayQueues(base),moreCores=replayQueues({...base,hosts:[{id:'h0',cores:32}]});
  conserved(r);conserved(moreCores);
  assert.equal(r.acked,0);assert.equal(moreCores.acked,0);
  assert.equal(r.serviceMs.p99,null,'no successful writes must not become a synthetic zero-latency success');
  assert.ok(r.hosts[0].cpuPct<30);assert.equal(r.nodes[0].writerPct,100);
});

test('ready machines provide no relief until an installed routing share moves',()=>{
  const cfg={durationMs:1000,rate:1800,writeCpuMs:1,writeIoMs:0,workers:64,
    hosts:[{id:'a',cores:1},{id:'b',cores:1}],
    nodes:[{id:'source',host:'a',weight:1},{id:'receiver',host:'b',weight:0}]};
  const ready=replayQueues(cfg);
  const installed=replayQueues({...cfg,installs:[{atMs:100,from:'source',to:'receiver',fraction:.5}]});
  conserved(ready);conserved(installed);
  assert.equal(ready.nodes[1].acked,0);assert.equal(ready.hosts[1].cpuPct,0);
  assert.ok(installed.nodes[1].acked>0&&installed.acked>ready.acked);
  assert.equal(installed.nodes.reduce((n,v)=>n+v.weight,0),1);
});

test('separate vnode writers still share the same physical CPU',()=>{
  const cfg={durationMs:1000,rate:1600,writeCpuMs:1,writeIoMs:0,workers:64,
    hosts:[{id:'a',cores:1},{id:'b',cores:1}],nodes:[{id:'x',host:'a',weight:1},{id:'y',host:'a',weight:1}]};
  const together=replayQueues(cfg),apart=replayQueues({...cfg,nodes:[{id:'x',host:'a',weight:1},{id:'y',host:'b',weight:1}]});
  conserved(together);conserved(apart);
  assert.ok(together.acked<apart.acked*.8);assert.ok(together.hosts[0].cpuPct>90);
});

test('shared disk service can bottleneck independent writers with spare CPU',()=>{
  const cfg={durationMs:1000,rate:1500,writeCpuMs:0,writeIoMs:1,workers:64,
    nodes:[{id:'x',host:'h0',weight:1},{id:'y',host:'h0',weight:1}]};
  const one=replayQueues(cfg),two=replayQueues({...cfg,hosts:[{id:'h0',cores:4,ioSlots:2}]});
  conserved(one);conserved(two);assert.equal(one.hosts[0].cpuPct,0);
  assert.ok(one.acked<two.acked*.8);
});

test('maintenance debt is preserved, not discarded when a periodic job is late',()=>{
  const r=replayQueues({durationMs:200,background:[{node:'n0',everyMs:1,cpuMs:5,writer:true}]});
  conserved(r);assert.equal(r.background[0].released,200);assert.ok(r.background[0].pending>100);
});

test('summary work outside the writer consumes shared CPU',()=>{
  const c={durationMs:500,rate:800,writeCpuMs:1,writeIoMs:0,hosts:[{id:'h0',cores:1}]};
  const base=replayQueues(c);
  const taxed=replayQueues({...c,background:[{node:'n0',everyMs:10,cpuMs:9,writer:false}]});
  conserved(taxed);assert.ok(taxed.acked<base.acked);assert.ok(taxed.background[0].pending>0);
});

test('64 vnodes are deterministic and retain demand/ACK accounting',()=>{
  const c={durationMs:200,rate:16000,
    hosts:Array.from({length:16},(_,i)=>({id:`h${i}`,cores:4})),
    nodes:Array.from({length:64},(_,i)=>({id:`n${i}`,host:`h${i>>2}`,weight:1}))};
  const a=replayQueues(c),b=replayQueues(c);conserved(a);assert.deepEqual(a,b);
  assert.equal(a.nodes.length,64);assert.ok(a.acked>0);
});

test('configuration rejects vacuous or unbounded work instead of hiding it',()=>{
  assert.throws(()=>replayQueues({stepMs:0}));
  assert.throws(()=>replayQueues({background:[{node:'n0',everyMs:.001,cpuMs:1}]}));
  assert.throws(()=>replayQueues({maxOffered:1}));
  assert.throws(()=>replayQueues({writeCpuMs:0,writeIoMs:0}));
  assert.throws(()=>replayQueues({installs:[{atMs:0,from:'n0',to:'missing',fraction:.5}]}));
});

test('AWS workload clock has 120-second ramps, not the fluid example’s 60-second ramps',()=>{
  assert.equal(stepsRate(180000),3000);assert.equal(stepsRate(240000),4000);
  assert.equal(stepsRate(360000),4000);assert.equal(stepsRate(480000),8000);
  assert.equal(stepsRate(600000),8000);assert.equal(stepsRate(720000),16000);
  assert.equal(stepsRate(839000),16000);assert.equal(stepsRate(840000),0);
});

test('measured writer wall time blocks only the writer, without invented CPU or disk work',()=>{
  const c={durationMs:100,stepMs:.25,rate:1000,writeCpuMs:0,writeIoMs:0,writeHoldMs:2};
  const r=replayQueues(c); conserved(r);
  assert.ok(r.acked>0&&r.acked<=50);assert.ok(r.inFlight>0);
  assert.equal(r.hosts[0].cpuPct,0);assert.equal(r.hosts[0].ioPct,0);
  const b=replayQueues({...c,writeHoldMs:1,background:[{node:'n0',everyMs:200,holdMs:80,writer:true}]});
  conserved(b);assert.ok(b.acked<r.acked);
  assert.throws(()=>replayQueues({...c,writeHoldMs:-1}));
});
