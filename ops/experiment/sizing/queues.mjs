// Bounded execution counterexamples, not an implementation of Indexus.
// CPU is core-ms; I/O is blocking ms; each vnode has one serial writer.
// Installation schedules must come from a validated protocol trace: this
// model neither transfers records nor proves single-writer/ACK durability.
class Queue {
  values = []; head = 0;
  get length() { return this.values.length - this.head; }
  push(value) { this.values.push(value); }
  pop() {
    const value = this.values[this.head++];
    if (this.head > 1024 && this.head * 2 > this.values.length) {
      this.values = this.values.slice(this.head); this.head = 0;
    }
    return value;
  }
}
class Histogram {
  bins = new Map(); count = 0;
  observe(ms) {
    const bin = Math.ceil(Math.log2(Math.max(1, ms * 1000)));
    this.bins.set(bin, (this.bins.get(bin) || 0) + 1); this.count++;
  }
  quantile(q) {
    let seen = 0;
    for (const [bin, n] of [...this.bins].sort((a,b) => a[0]-b[0])) {
      seen += n; if (seen >= Math.ceil(this.count*q)) return 2**bin/1000;
    }
    return null;
  }
  result() { return {samples:this.count,p50:this.quantile(.5),p95:this.quantile(.95),p99:this.quantile(.99)}; }
}
function positive(value, label) {
  if (!Number.isFinite(value) || value <= 0) throw Error(`${label} must be positive`);
}
function cost(value, label) {
  if (!Number.isFinite(value) || value < 0) throw Error(`${label} must be nonnegative`);
}

// Workload clock for MODE=steps; defaults match the two AWS comparisons.
// After the last plateau there are no new writes (drain/audit are separate).
export function stepsRate(timeMs, {baseRate=2000,rampSeconds=120,plateauSeconds=120,levels=4} = {}) {
  positive(rampSeconds,'rampSeconds');positive(plateauSeconds,'plateauSeconds');
  positive(baseRate,'baseRate');
  if (!Number.isInteger(levels)||levels<1)throw Error('invalid levels');
  let t=Math.max(0,timeMs/1000),rate=baseRate;
  if(t<plateauSeconds)return rate;t-=plateauSeconds;
  for(let level=1;level<levels;level++) {
    if(t<rampSeconds)return rate*(1+t/rampSeconds);
    t-=rampSeconds;rate*=2;
    if(t<plateauSeconds)return rate;t-=plateauSeconds;
  }
  return 0;
}

export function replayQueues(options = {}) {
  const c = {durationMs:1000,stepMs:.25,rate:1000,workers:4096,queueCapacity:16384,
    maxOffered:1_000_000,writeCpuMs:.25,writeIoMs:.25,writeHoldMs:0,
    hosts:[{id:'h0',cores:4,ioSlots:1}],nodes:[{id:'n0',host:'h0',weight:1}],
    background:[],installs:[],seed:7,...options};
  for (const k of ['durationMs','stepMs','workers','maxOffered']) positive(c[k], k);
  if (!Number.isInteger(c.workers)) throw Error('workers must be an integer');
  if (!Number.isInteger(c.queueCapacity) || c.queueCapacity < 0) throw Error('invalid queue capacity');
  cost(c.writeCpuMs,'writeCpuMs'); cost(c.writeIoMs,'writeIoMs'); cost(c.writeHoldMs,'writeHoldMs');
  if (c.writeCpuMs+c.writeIoMs+c.writeHoldMs===0) throw Error('a write must do work');
  if (c.stepMs>c.durationMs || c.durationMs/c.stepMs>10_000_000) throw Error('invalid execution horizon');
  if (c.nodes.length>1024 || c.background.length>256) throw Error('execution model scope exceeded');
  const hosts = new Map(c.hosts.map(h => {
    positive(h.cores,'host cores');
    const ioSlots=h.ioSlots??1;
    if (!Number.isInteger(ioSlots)||ioSlots<1) throw Error('invalid I/O slots');
    return [h.id,{...h,ioSlots,cpuMs:0,ioMs:0}];
  }));
  const nodes = new Map(c.nodes.map(n => {
    if (!hosts.has(n.host)) throw Error('unknown host'); cost(n.weight,'weight');
    return [n.id,{...n,queue:new Queue(),writer:null,writerMs:0,acked:0}];
  }));
  if (hosts.size!==c.hosts.length || nodes.size!==c.nodes.length) throw Error('duplicate identity');
  const backgrounds = c.background.map((b,i) => {
    if (!nodes.has(b.node)) throw Error('unknown background node');
    positive(b.everyMs,'background period');
    if(b.everyMs<c.stepMs)throw Error('background period below timestep; refine the step');
    cost(b.cpuMs??0,'background CPU'); cost(b.ioMs??0,'background I/O'); cost(b.holdMs??0,'background writer hold');
    if (!(b.cpuMs||b.ioMs||b.holdMs)) throw Error('background must do work');
    cost(b.atMs??0,'background start');
    return {...b,id:i,next:b.atMs??0,until:b.untilMs??c.durationMs,waiting:0,active:false,released:0,completed:0};
  });
  const installs = c.installs.map(x => {
    if (!nodes.has(x.from)||!nodes.has(x.to)||x.from===x.to || !(x.fraction>0&&x.fraction<=1)) throw Error('invalid install');
    cost(x.atMs,'install time'); return {...x};
  }).sort((a,b)=>a.atMs-b.atMs);
  let route=[],totalWeight=0;
  function refreshRoute() {
    totalWeight=0; route=[];
    for (const n of nodes.values()) if (n.weight>0) {totalWeight+=n.weight;route.push([totalWeight,n]);}
    if (!totalWeight) throw Error('no installed routing share');
  }
  refreshRoute();
  let seed=c.seed>>>0;
  function owner() {
    seed=(Math.imul(seed,1664525)+1013904223)>>>0;
    const at=seed/4294967296*totalWeight;
    let lo=0,hi=route.length-1;
    while(lo<hi){const mid=(lo+hi)>>1;if(at<route[mid][0])hi=mid;else lo=mid+1;}
    return route[lo][1];
  }
  const pending=new Queue(),outside=new Set(),service=new Histogram(),intended=new Histogram();
  let offered=0,acked=0,shed=0,inFlight=0,peakQueued=0,peakInFlight=0,carry=0,nextInstall=0,sequence=0;
  function task(node,cpu,io,extra) {return {node,cpu,io,id:sequence++,...extra};}
  function admit(due,t) {
    const node=owner(); inFlight++;
    node.queue.push(task(node,c.writeCpuMs,c.writeIoMs,{due,started:t,client:true,hold:c.writeHoldMs}));
  }
  function finish(job,t) {
    if (job.client) {acked++;inFlight--;job.node.acked++;service.observe(t-job.started);intended.observe(t-job.due);}
    else {job.background.completed++;job.background.active=false;outside.delete(job);}
    if (job.node.writer===job) job.node.writer=null;
  }
  for (let tick=0;tick<Math.ceil(c.durationMs/c.stepMs);tick++) {
    const t=tick*c.stepMs,dt=Math.min(c.stepMs,c.durationMs-t);
    while(nextInstall<installs.length&&installs[nextInstall].atMs<=t) {
      const x=installs[nextInstall++],from=nodes.get(x.from),to=nodes.get(x.to),weight=from.weight*x.fraction;
      from.weight-=weight;to.weight+=weight;refreshRoute();
    }
    for(const b of backgrounds) {
      while(b.next<=t&&b.next<b.until){b.released++;b.waiting++;b.next+=b.everyMs;}
      // A repeated job adds debt, not unbounded runnable tasks. No work is
      // silently discarded because the previous quantum has not finished.
      if(b.waiting&&!b.active) {
        b.waiting--;b.active=true;
        const n=nodes.get(b.node),job=task(n,b.cpuMs??0,b.ioMs??0,{background:b,hold:b.holdMs??0});
        if(b.writer!==false)n.queue.push(job);else outside.add(job);
      }
    }
    const rate=typeof c.rate==='function'?c.rate(t):c.rate;cost(rate,'offered rate');
    carry+=rate*dt/1000;
    const arrivals=Math.floor(carry+1e-9);carry-=arrivals;
    offered+=arrivals;if(offered>c.maxOffered)throw Error('maxOffered exceeded; explicitly size the experiment');
    // Drain old arrivals before new ones; all latency is measured from the
    // intended arrival, including the external bounded worker queue.
    while(inFlight<c.workers&&pending.length)admit(pending.pop(),t);
    for(let i=0;i<arrivals;i++) {
      if(inFlight<c.workers)admit(t,t);
      else if(pending.length<c.queueCapacity)pending.push(t);
      else shed++;
    }
    peakQueued=Math.max(peakQueued,pending.length);peakInFlight=Math.max(peakInFlight,inFlight);
    const active=[...outside];
    for(const n of nodes.values()) {
      if(!n.writer&&n.queue.length)n.writer=n.queue.pop();
      if(n.writer){active.push(n.writer);n.writerMs+=dt;}
    }
    const cpu=new Map(),io=new Map();
    for(const job of active) {
      job.hadCpu=job.cpu>1e-9;
      // A measured writer wall time is deliberately not attributed to CPU
      // or disk: that split needs separate service measurements.
      if (!job.hadCpu && job.io<=1e-9) { job.hold=Math.max(0,(job.hold??0)-dt); continue; }
      const list=job.hadCpu?cpu:io;
      if(!list.has(job.node.host))list.set(job.node.host,[]);
      list.get(job.node.host).push(job);
    }
    for(const [host,jobs] of cpu) {
      const h=hosts.get(host);let budget=h.cores*dt;
      // Single-threaded quanta share physical cores, not the sum of each
      // process's visible GOMAXPROCS. Redistribute unused short-task shares.
      jobs.sort((a,b)=>a.cpu-b.cpu);
      for(let i=0;i<jobs.length;i++) {
        const work=Math.min(jobs[i].cpu,dt,budget/(jobs.length-i));
        jobs[i].cpu-=work;budget-=work;h.cpuMs+=work;
      }
    }
    for(const [host,jobs] of io) {
      const h=hosts.get(host);jobs.sort((a,b)=>a.id-b.id);
      for(const job of jobs.slice(0,h.ioSlots)){const work=Math.min(job.io,dt);job.io-=work;h.ioMs+=work;}
    }
    for(const job of active)if(job.cpu<=1e-9&&job.io<=1e-9&&(job.hold??0)<=1e-9)finish(job,t+dt);
  }
  return {source:'synthetic-execution-model',calibrated:false,
    scope:'synthetic bounded queue replay; not protocol execution or an AWS latency forecast',
    offered,acked,shed,queued:pending.length,inFlight,peakQueued,peakInFlight,
    unfinishedFraction:offered?(pending.length+inFlight)/offered:0,
    serviceMs:service.result(),intendedMs:intended.result(),
    hosts:[...hosts.values()].map(h=>({id:h.id,cpuPct:100*h.cpuMs/(h.cores*c.durationMs),ioPct:100*h.ioMs/(h.ioSlots*c.durationMs)})),
    nodes:[...nodes.values()].map(n=>({id:n.id,host:n.host,weight:n.weight,acked:n.acked,writerPct:100*n.writerMs/c.durationMs})),
    background:backgrounds.map(b=>({id:b.id,node:b.node,released:b.released,completed:b.completed,pending:b.waiting+Number(b.active)})),
    unresolvedInstalls:installs.length-nextInstall,stepMs:c.stepMs};
}
