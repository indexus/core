import test from 'node:test';
import assert from 'node:assert/strict';
import {SizingSimulation,simulate,configWith,demandAt} from './model.mjs';

test('demand has real ramps and unannounced doublings, independent of packing',()=>{
  assert.equal(demandAt(150),3000);assert.equal(demandAt(150,'double'),4000);
  assert.equal(demandAt(600),16000);assert.throws(()=>configWith({trigger:60}));
  assert.throws(()=>configWith({dataGiB:0}));
});

test('rate is conserved through local/remote split and progressive installation',()=>{
  const s=new SizingSimulation({initialVnodes:1,warmHosts:1,slots:5,insertFraction:0});
  const originalWeight=s.vnodes[0].weight;
  s.start(s.vnodes[0],{host:1,projection:{cpu:40,ram:20,disk:20,io:40}},true,2000,'fixture');
  let previous=0,sawProgress=false;
  for(let i=0;i<20;i++) {
    s.advanceCopies();const j=s.jobs[0];sawProgress ||= j.share>0&&j.share<1;
    assert.ok(j.share>=previous);previous=j.share;
    assert.ok(Math.abs(s.resources().reduce((n,r)=>n+r.rate,0)-2000)<1e-7);
    assert.ok(Math.abs(s.vnodes.reduce((n,v)=>n+v.weight,0)-originalWeight)<1e-8);
    s.t+=s.c.dt;
  }
  assert.ok(sawProgress,'fixture must exercise an intermediate installed fraction');
});

test('dirty work can prevent progress without returning installed ownership',()=>{
  const s=new SizingSimulation({initialVnodes:1,warmHosts:1,bytesPerItem:1048576,
    bandwidthMiB:1,dirtyFraction:1,insertFraction:0});
  s.start(s.vnodes[0],{host:1,projection:{cpu:40,ram:20,disk:20,io:40}},false,2000,'fixture');
  s.advanceCopies();assert.equal(s.jobs[0].fraction,0);assert.ok(s.stats.stalledCopySeconds>0);
  assert.equal(s.vnodes[0].host,0);
});

test('incoming reservation respects all projected resource ceilings and slot limits',()=>{
  const s=new SizingSimulation({scenario:'hotspot',trigger:45,seed:11});
  const progress=new Map();
  while(s.t<s.c.seconds) {
    s.step();
    for(const j of s.jobs){assert.ok(j.share>=(progress.get(j.id)||0));progress.set(j.id,j.share);}
    for(const r of s.resources()) {
      assert.ok(r.slots<=s.c.slots,`overbooked host ${r.id}`);
      assert.ok(r.incoming<=1);assert.ok(r.outgoing<=1);
    }
  }
  assert.ok(s.admissions.length>0);
  for(const a of s.admissions)for(const value of Object.values(a.projection))assert.ok(value<=a.ceiling+1e-7);
});

test('fixed receiver budget excludes a host that a bounded stream can use',()=>{
  const s=new SizingSimulation({initialVnodes:1,warmHosts:1,slots:5});
  const rs=s.resources();Object.assign(rs[1],{cpu:40,ram:10,disk:10,io:0,slots:3});
  assert.equal(s.target(s.vnodes[0],100,0,rs).host,1);
  s.c.policy='fixed';assert.equal(s.target(s.vnodes[0],100,0,rs),null);
});

test('co-resident split does not relieve the physical foreground CPU',()=>{
  const s=new SizingSimulation({initialVnodes:1,warmHosts:0,slots:5,insertFraction:0});
  const before=s.resources()[0].cpu;
  s.start(s.vnodes[0],{host:0,projection:{cpu:40,ram:20,disk:20,io:40}},true,2000,'fixture');
  const after=s.resources(0,false)[0].cpu;
  assert.ok(after>before,'additional process adds overhead; no new physical CPU was created');
});

test('unpredicted bursts are recorded, never presented as a hard 60% guarantee',()=>{
  const r=simulate({scenario:'hotspot',warmHosts:0});
  assert.ok(r.summary.overCpuSeconds>0);assert.ok(r.summary.peakCpuDemand>60);
});

test('large stored volumes enforce disk limits even with spare CPU and vnode slots',()=>{
  const small=new SizingSimulation({dataGiB:.25,slots:5});
  const large=new SizingSimulation({dataGiB:128,itemsPerZone:128,slots:5});
  assert.ok(large.hosts.length>small.hosts.length);
  assert.ok(large.resources().every(r=>r.disk<=60));
});

test('the experiment is deterministic and records its artificial scope bounds',()=>{
  const cfg={initialVnodes:8,seconds:120,policy:'items',itemLimit:20000,maxVnodes:16};
  assert.deepEqual(simulate(cfg),simulate(cfg));assert.ok(simulate(cfg).summary.modelLimitHit);
});

test('zone metadata is not inferred solely from stored byte volume',()=>{
 const dense=new SizingSimulation({dataGiB:1,initialVnodes:8,itemsPerZone:128});
 const sparse=new SizingSimulation({dataGiB:1,initialVnodes:8,itemsPerZone:1});
 assert.ok(sparse.resources().reduce((n,r)=>n+r.ram,0)>dense.resources().reduce((n,r)=>n+r.ram,0));
});

test('forecast never peeks at a future hotspot or instantaneous doubling',()=>{
 const s=new SizingSimulation({scenario:'hotspot'});s.t=590;
 assert.deepEqual(s.rates(635),s.rates(590));
 s.c.scenario='double';s.t=110;
 assert.ok(Math.abs(s.rates(155).reduce((n,r)=>n+r,0)-2000)<1e-6);
 assert.equal(demandAt(155,'double'),4000);
});

test('granular GC releases copied data before the whole vnode finishes',()=>{
 const make=gcMode=>{const s=new SizingSimulation({initialVnodes:1,warmHosts:1,gcMode,gcSeconds:0});
 s.start(s.vnodes[0],{host:1,projection:{cpu:40,ram:20,disk:20,io:40}},false,2000,'fixture');s.advanceCopies();return s;};
 const a=make('granular'),b=make('end');
 assert.ok(a.jobs[0].fraction>0&&a.jobs[0].fraction<1);
 assert.ok(a.resources()[0].disk<b.resources()[0].disk);
 const stored=a.resources().reduce((sum,r)=>sum+r.disk*a.c.diskGiB*1024/100/a.c.diskAmplification,0);
 assert.ok(stored>=a.vnodes[0].dataMiB-1e-7,'GC must not delete uncopied data in the model');
});

test('warm reserve grows with observed resource demand, not a fixed host count',()=>{
 const s=new SizingSimulation({initialVnodes:64,reservePct:15});
 const low=s.reserveNeeded(s.resources());s.t=600;
 const high=s.reserveNeeded(s.resources());assert.ok(high>low);
 assert.equal(new SizingSimulation({reservePct:0}).reserveNeeded(s.resources()),0);
});

test('shared dashboard conserves vnode rates through split and copy',async()=>{
 const {dashboardPoints}=await import('./model.mjs');
 const s=new SizingSimulation({initialVnodes:1,warmHosts:1,insertFraction:0});
 s.start(s.vnodes[0],{host:1,projection:{cpu:40,ram:20,disk:20,io:40}},true,2000,'fixture');
 s.advanceCopies();s.snapshot();
 const p=dashboardPoints(s.result()).at(-1);
 assert.ok(Math.abs(p.fleet.vnodes.reduce((n,v)=>n+v.estimated_s,0)-p.offered)<1e-6);
 assert.equal(p.acked,undefined);assert.equal(p.p99_ms,undefined);
});

test('interactive trace samples transient transfers instead of hiding all moves',()=>{
 const r=simulate({trigger:45});
 assert.ok(r.summary.moves>0);
 assert.ok(r.snapshots.some(s=>s.jobs.some(j=>j.fraction>0&&j.fraction<1)),
  'a ten-second dashboard cadence can miss every small copy');
});

test('64k EBS clock includes the idle tail and conserves offered volume', () => {
  const rate = t => demandAt(t, 'aws64k');
  assert.deepEqual([0,70,90,110,220,330,399,400,550].map(rate),
    [8000,8000,12000,16000,32000,64000,64000,0,0]);
  let offered = 0;
  for (let t = 0; t < 400; t++) offered += rate(t + 0.5);
  assert.equal(offered, 11760000);
});

test('AWS inventory starts four vnodes per host independently of future packing',()=>{
  const s = new SizingSimulation({scenario:'aws64k',initialHosts:12,initialVnodes:48,
    slots:24,maxHosts:44,warmHosts:0,splitRate:50,bootPolicy:'forecast'});
  assert.deepEqual(s.resources().map(r=>r.slots),Array(12).fill(4));
  assert.equal(s.stats.boots,0);
});

test('zone installation can limit an otherwise fast copy',()=>{
  const s=new SizingSimulation({initialVnodes:1,warmHosts:1,zonesPerSecond:20,dirtyFraction:0});
  s.start(s.vnodes[0],{host:1,projection:{}},false,2000,'fixture');
  const job=s.jobs[0],before=job.leftMiB;
  s.advanceCopies();
  assert.ok(Math.abs(before-job.leftMiB-20*s.c.dt*s.c.bytesPerItem/1048576)<1e-8);
});
