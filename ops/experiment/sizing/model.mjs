// Resource planning model, not an execution of the Indexus protocol.
// Units: seconds, MiB, cores. No network, filesystem, clock or randomness outside seed.
export const DEFAULTS = Object.freeze({
  seed: 7, trace: true, seconds: 1200, dt: 2, traceEvery: 2, controlEvery: 10, initialVnodes: 64,
  slots: 5, maxHosts: 96, maxVnodes: 512, cores: 4, ramGiB: 16, diskGiB: 900,
  dataGiB: 0.25, diskAmplification: 2, bytesPerItem: 870,
  vnodeRamMiB: 192, cacheFraction: 0.025, zoneMemoryBytes: 2048, itemsPerZone: 1, hostRamMiB: 512,
  vnodeCores: 0.035, hostCores: 0.08, cpuUsPerWrite: 1300,
  ioMiB: 500, networkMiB: 500, bandwidthMiB: 100, copyMiBPerCore: 250,
  ceiling: 60, trigger: 50, cold: 35, copyCpuPct: 10, bootSeconds: 45,
  reservePct: 15, cooldown: 90, gcSeconds: 20, gcMode: 'granular', idleSeconds: 120,
  dirtyFraction: 0.3, insertFraction: 0.7, hotspotFactor: 6,
  scenario: 'ramp', policy: 'adaptive', hotFirst: false, itemLimit: 500000,
});

const clamp = (x, lo, hi) => Math.max(lo, Math.min(hi, x));
const peak = r => Math.max(r.cpu, r.ram, r.disk, r.io);
const quantile = (xs, q) => xs.length ? [...xs].sort((a,b)=>a-b)[Math.ceil(q*xs.length)-1] : null;

export function demandAt(t, scenario = 'ramp') {
  if (scenario === 'aws64k') {
    if (t >= 400) return 0;
    const stage = Math.floor(t / 110);
    const rate = 8000 * 2 ** stage;
    return rate * (1 + Math.max(0, t % 110 - 70) / 40);
  }
  const stages = [[0,120,2000,2000], [120,180,2000,4000], [180,300,4000,4000],
    [300,360,4000,8000], [360,480,8000,8000], [480,540,8000,16000],
    [540,900,16000,16000], [900,1020,16000,4000], [1020,Infinity,4000,4000]];
  for (const [lo,hi,a,b] of stages) if (t >= lo && t < hi) {
    return scenario === 'double' && hi-lo === 60 ? b : a+(b-a)*(t-lo)/(hi-lo || 1);
  }
  return 2000;
}

export function configWith(overrides = {}) {
  const c = {...DEFAULTS, ...overrides};
  for (const k of ['dt','traceEvery','seconds','dataGiB','itemsPerZone','cores','ramGiB','diskGiB','bytesPerItem','copyMiBPerCore','bandwidthMiB','ioMiB','networkMiB']) {
    if (!Number.isFinite(c[k]) || c[k] <= 0) throw Error(`Paramètre positif requis : ${k}`);
  }
  for (const k of ['initialVnodes','slots','maxHosts','maxVnodes','controlEvery']) {
    if (!Number.isInteger(c[k]) || c[k] < 1) throw Error(`Entier positif requis : ${k}`);
  }
  if (!(c.cold < c.trigger && c.trigger < c.ceiling && c.ceiling <= 100)) throw Error('Seuil bas < déclenchement < plafond ≤ 100 requis.');
  if (c.copyCpuPct <= 0 || c.copyCpuPct >= c.ceiling) throw Error('Budget de copie incompatible avec le plafond.');
  if (!Number.isFinite(c.reservePct)||c.reservePct<0||c.reservePct>100)throw Error('Réserve entre 0 et 100 % requise.');
  if (!['ramp','double','hotspot','volume','aws64k'].includes(c.scenario)) throw Error('Scénario inconnu.');
  if (!['adaptive','fixed','items'].includes(c.policy)) throw Error('Politique inconnue.');
  if (c.initialHosts !== undefined && (!Number.isInteger(c.initialHosts) || c.initialHosts < 1 ||
      c.initialHosts > c.maxHosts || c.initialVnodes > c.initialHosts*c.slots)) throw Error('Initial host inventory cannot fit the vnodes');
  if (c.bootPolicy !== undefined && !['serial','forecast'].includes(c.bootPolicy)) throw Error('Unknown boot policy');
  if (c.zonesPerSecond !== undefined && !(Number.isFinite(c.zonesPerSecond) && c.zonesPerSecond > 0)) throw Error('Invalid installation rate');
  if (c.splitRate !== undefined && !(Number.isFinite(c.splitRate) && c.splitRate > 0)) throw Error('Invalid split rate');
  if (c.initialVnodes > c.maxVnodes) throw Error('Inventaire initial supérieur à la borne du modèle.');
  return c;
}

function random(seed) {
  let s = seed >>> 0;
  return () => { s = (Math.imul(s,1664525)+1013904223)>>>0; return s/4294967296; };
}

export class SizingSimulation {
  constructor(overrides = {}) {
    this.c = configWith(overrides); this.t = 0; this.hosts = []; this.vnodes = [];
    this.jobs = []; this.retained = []; this.events = []; this.snapshots = []; this.admissions = [];
    this.stats = {moves:0,splits:0,boots:0,released:0,copyMiB:0,stalledCopySeconds:0,
      machineSeconds:0,excessPctSeconds:0,peakExcessPct:0,overAnySeconds:0,overCpuSeconds:0,overRamSeconds:0,overDiskSeconds:0,overIoSeconds:0,
      peakCpuDemand:0,peakRam:0,peakDisk:0,peakIo:0,peakHosts:0,peakVnodes:0,
      modelLimitHit:false,peakReadyHosts:0};
    const rng = random(this.c.seed);
    for (let i=0;i<(this.c.initialHosts || 0);i++) this.addHost(0,false);
    for (let i=0;i<this.c.initialVnodes;i++) {
      const v = {id:i, root:i, host:-1, weight:0.7+0.6*rng(),
        dataMiB:this.c.dataGiB*1024*(0.8+0.4*rng()), coolUntil:0, job:null};
      this.vnodes.push(v);
      const rs = this.resources(0);
      let h = this.c.initialHosts ? this.hosts[i % this.c.initialHosts] : this.hosts.find(h=>this.initialFits(rs[h.id],v));
      if (!h) h = this.addHost(0,false);
      if (!h || !this.initialFits(this.resources(0)[h.id],v)) throw Error('Un vnode ne tient pas dans le budget initial : réduire son volume ou augmenter la machine.');
      v.host=h.id;
    }
    const reserve=this.reserveNeeded(this.resources());
    for (let i=0;i<reserve;i++) this.addHost(0,false);
    this.snapshot();
  }

  reserveNeeded(rs) {
    // Empty, ready hosts provide reception capacity; boots are not ready.
    // Conservative reference policy: spare space on occupied hosts is excluded.
    if(Number.isFinite(this.c.warmHosts))return this.c.warmHosts; // explicit fixed-policy experiment only
    const used=rs.filter(r=>!r.retired&&r.slots).reduce((n,r)=>n+r.cpu*this.c.cores/100,0);
    const perHost=Math.max(.01,(this.c.ceiling-this.c.copyCpuPct)/100*this.c.cores-this.c.hostCores);
    return Math.ceil(used*this.c.reservePct/100/perHost);
  }

  memoryFraction() {return this.c.cacheFraction+this.c.zoneMemoryBytes/(this.c.bytesPerItem*this.c.itemsPerZone);}

  initialFits(r,v) {
    const c=this.c;
    return r && r.slots<c.slots &&
      r.ram+(c.vnodeRamMiB+v.dataMiB*this.memoryFraction())/(c.ramGiB*1024)*100<=c.ceiling &&
      r.disk+v.dataMiB*c.diskAmplification/(c.diskGiB*1024)*100<=c.ceiling;
  }

  addHost(readyAt, count=true) {
    if (this.hosts.filter(h=>!h.retired).length>=this.c.maxHosts) {this.stats.modelLimitHit=true;return null;}
    const h={id:this.hosts.length,readyAt,retired:false,idleSince:null};this.hosts.push(h);
    if (count) {this.stats.boots++;this.event('boot',{host:h.id,readyAt});}
    return h;
  }

  event(kind,fields={}) {this.events.push({t:this.t,kind,...fields});}

  rates(time=this.t) {
    // Forecasts use only observed demand and current heat, never future events.
    const observedAt=Math.min(time,this.t);
    const hot=this.c.scenario==='hotspot' && observedAt>=600 && observedAt<720;
    const current=demandAt(this.t,this.c.scenario);
    const past=demandAt(Math.max(0,this.t-20),this.c.scenario);
    const total=time<=this.t?demandAt(time,this.c.scenario):Math.min(2*current,current+Math.max(0,(current-past)/20)*(time-this.t));
    const weights=this.vnodes.map(v=>v.weight*(hot && v.root<4 ? this.c.hotspotFactor:1));
    const sum=weights.reduce((a,b)=>a+b,0);
    return weights.map(w=>total*w/sum);
  }

  resources(time=this.t, withCopy=true, detail=false) {
    const c=this.c, rates=this.rates(time);
    const out=this.hosts.map(h=>({id:h.id,ready:h.readyAt<=this.t&&!h.retired,retired:h.retired,
      cpu:c.hostCores/c.cores*100,ram:c.hostRamMiB/(c.ramGiB*1024)*100,disk:0,io:0,net:0,
      nodeStats:detail?{}:undefined,slots:0,incoming:0,outgoing:0,vnodes:[],copied:[],rate:0,copyMiB:0}));
    const add=(host,v,rate,base,data,cache,slot)=>{
      const r=out[host];if(!r)return;
      r.cpu+=(base*c.vnodeCores+rate*c.cpuUsPerWrite/1e6)/c.cores*100;
      r.ram+=(base*c.vnodeRamMiB+cache*this.memoryFraction())/(c.ramGiB*1024)*100;
      r.disk+=data*c.diskAmplification/(c.diskGiB*1024)*100;
      r.io+=rate*c.bytesPerItem*c.diskAmplification/1048576/c.ioMiB*100;
      r.net+=rate*c.bytesPerItem/1048576/c.networkMiB*100;
      if(detail){const id=v.job?.born&&host===v.job.src?v.job.sourceVnode:v.id;
        r.nodeStats[id]??={id,rate:0};r.nodeStats[id].rate+=rate;}
      r.rate+=rate;r.slots+=slot;if(slot)r.vnodes.push(v.id);
    };
    for (let i=0;i<this.vnodes.length;i++) {
      const v=this.vnodes[i],j=v.job,rate=rates[i];
      const data=v.dataMiB+rate*Math.max(0,time-this.t)*c.bytesPerItem/1048576*c.insertFraction;
      if (!j) add(v.host,v,rate,1,data,data,1);
      else {
        add(j.src,v,rate*(1-j.share),j.born?0:1,data*(1-j.freedFraction),data*(1-j.freedFraction),j.born?0:1);
        add(j.dst,v,rate*j.share,1,data*j.fraction,data*j.fraction,1);
        out[j.src].outgoing++;out[j.dst].incoming++;
        out[j.dst].copied.push({vnode:v.id,from:j.src,fraction:j.fraction,share:j.share});
        if (withCopy) {
          const b=j.bandwidth||0;
          out[j.src].cpu+=b/c.copyMiBPerCore/c.cores*100;
          out[j.dst].cpu+=b/c.copyMiBPerCore/c.cores*100;
          out[j.src].io+=b/c.ioMiB*100;
          out[j.dst].io+=2*b/c.ioMiB*100;
          out[j.src].copyMiB+=b;out[j.dst].copyMiB+=b;
          if(j.src!==j.dst){out[j.src].net+=b/c.networkMiB*100;out[j.dst].net+=b/c.networkMiB*100;}
        }
      }
    }
    for(const r of this.retained) out[r.host].disk+=r.dataMiB*c.diskAmplification/(c.diskGiB*1024)*100;
    return out;
  }

  projected(r,v,rate,local=false) {
    const c=this.c;
    const foreground=(c.vnodeCores+(local?0:rate*c.cpuUsPerWrite/1e6))/c.cores*100;
    const reserve=c.policy==='fixed'?1.75/c.cores*100:c.copyCpuPct;
    const foreIo=r.io+(local?0:rate*c.bytesPerItem*c.diskAmplification/1048576/c.ioMiB*100);
    const copyIoBudget=Math.max(0,c.ceiling-foreIo)/200*c.ioMiB;
    const expectedCopy=Math.min(c.bandwidthMiB,copyIoBudget,c.policy==='fixed'?Infinity:c.copyCpuPct/100*c.cores*c.copyMiBPerCore);
    return {cpu:c.policy==='fixed'?Math.max(r.cpu+reserve,r.cpu+foreground):r.cpu+foreground+reserve,
      ram:r.ram+(c.vnodeRamMiB+v.dataMiB*this.memoryFraction())/(c.ramGiB*1024)*100,
      disk:r.disk+v.dataMiB*c.diskAmplification/(c.diskGiB*1024)*100,
      io:foreIo+expectedCopy/c.ioMiB*200};
  }

  target(v,rate,src,rs,local=false) {
    const c=this.c;let best=null;
    for(const h of this.hosts) {
      const r=rs[h.id];if(!r.ready||r.incoming||r.slots>=c.slots||(!local&&h.id===src))continue;
      const p=this.projected(r,v,rate,h.id===src);
      if(peak(p)>c.ceiling+1e-8)continue;
      // Pack within the resource envelope; the fixed-budget comparison
      // instead prefers an empty machine, as the current Place order does.
      const score=c.policy==='fixed' ? (r.slots===0?1000:0)-peak(p) : peak(p);
      if(!best||score>best.score)best={host:h.id,score,projection:p};
    }
    return best;
  }

  start(v,target,split,rate,reason) {
    const c=this.c,src=v.host,sourceVnode=v.id;
    if(split) {
      if(this.vnodes.length>=c.maxVnodes){this.stats.modelLimitHit=true;return false;}
      v.weight/=2;v.dataMiB/=2;
      v={...v,id:this.vnodes.length,coolUntil:0};this.vnodes.push(v);this.stats.splits++;
    } else this.stats.moves++;
    const j={id:this.stats.moves+this.stats.splits,vnode:v.id,sourceVnode,src,dst:target.host,born:split,
      initialMiB:v.dataMiB,leftMiB:v.dataMiB,fraction:0,share:0,freedFraction:0,history:[],bandwidth:0,start:this.t};
    v.job=j;this.jobs.push(j);
    this.admissions.push({t:this.t,vnode:v.id,src,dst:target.host,projection:target.projection,ceiling:c.ceiling});
    this.event(split?'split':'move',{vnode:v.id,src,dst:target.host,dataMiB:v.dataMiB,reason});
    return true;
  }

  control() {
    const c=this.c,look=this.t+c.bootSeconds;
    let rs=this.resources(),future=this.resources(look,false),rates=this.rates(look);
    const sources=this.hosts.filter(h=>rs[h.id].ready&&rs[h.id].slots)
      .sort((a,b)=>peak(future[b.id])-peak(future[a.id]));
    let shortage=false;
    for(const h of sources) {
      const r=rs[h.id],f=future[h.id];if(r.outgoing||r.incoming)continue;
      const eligible=this.vnodes.filter(v=>v.host===h.id&&!v.job&&v.coolUntil<=this.t);
      const itemHot=c.policy==='items'&&eligible.some(v=>v.dataMiB*1048576/c.bytesPerItem>c.itemLimit);
      const rateHot=eligible.some(v=>rates[v.id] > c.splitRate);
      const hot=peak(f)>=c.trigger || rateHot;
      const cool=r.slots===1&&peak(f)<c.cold&&this.t>c.cooldown;
      if(!itemHot&&!hot&&!cool)continue;
      eligible.sort((a,b)=>rates[b.id]-rates[a.id]);
      let done=false;
      for(const v of eligible) {
        const rate=rates[v.id];
        const split=itemHot||rate > c.splitRate||((c.vnodeCores+rate*c.cpuUsPerWrite/1e6)/c.cores*100+c.copyCpuPct>c.ceiling);
        if(split&&this.vnodes.length>=c.maxVnodes){this.stats.modelLimitHit=true;continue;}
        const forecastData=v.dataMiB+rate*c.bootSeconds*c.bytesPerItem/1048576*c.insertFraction;
        const candidate={...v,dataMiB:forecastData/(split?2:1)};
        const dest=this.target(candidate,split?rate/2:rate,h.id,future,itemHot);
        if(!dest){if(hot||itemHot)shortage=true;continue;}
        const movedRate=split?rate/2:rate;
        const sourceAfter={cpu:f.cpu-(candidate.dataMiB>0?movedRate*c.cpuUsPerWrite/1e6/c.cores*100:0),
          ram:f.ram-(c.vnodeRamMiB+candidate.dataMiB*this.memoryFraction())/(c.ramGiB*1024)*100,
          disk:f.disk-candidate.dataMiB*c.diskAmplification/(c.diskGiB*1024)*100,
          io:f.io-movedRate*c.bytesPerItem*c.diskAmplification/1048576/c.ioMiB*100};
        const destAfter={...dest.projection,cpu:rs[dest.host].cpu+(c.vnodeCores+movedRate*c.cpuUsPerWrite/1e6)/c.cores*100,
          io:rs[dest.host].io+movedRate*c.bytesPerItem*c.diskAmplification/1048576/c.ioMiB*100};
        if(!itemHot&&hot&&Math.max(peak(sourceAfter),peak(destAfter))>peak(f)-3)continue;
        if(cool&&(dest.host===h.id||future[dest.host].slots===0||peak(dest.projection)>c.trigger))continue;
        done=this.start(v,dest,split,rate,itemHot?'items':cool?'consolidation':'resources');
        if(done){rs=this.resources();future=this.resources(look,false);rates=this.rates(look);break;}
      }
      if(!done&&(hot||itemHot))shortage=true;
    }
    rs=this.resources();
    const want=this.reserveNeeded(rs);
    const idle=this.hosts.filter(h=>rs[h.id].ready&&rs[h.id].slots===0&&!rs[h.id].incoming&&!rs[h.id].outgoing);
    const booting=this.hosts.filter(h=>!h.retired&&h.readyAt>this.t).length;
    // One boot per controller round; boot reservations are counted once.
    const needReceiver=shortage && idle.length===0 && booting===0;
    const reserveShortfall=Math.max(0,want-idle.length-booting);
    let starts=Number(needReceiver || reserveShortfall>0);
    if(c.bootPolicy==='forecast' && shortage) {
      const projectedWork=future.filter(r=>r.ready).reduce((sum,r)=>sum+r.cpu,0);
      const fleet=this.hosts.filter(h=>!h.retired).length;
      const capacityShortfall=Math.ceil(projectedWork/c.trigger)-fleet;
      starts=Math.max(starts,capacityShortfall);
    }
    for(let i=0;i<starts;i++)if(!this.addHost(this.t+c.bootSeconds))break;
    for(const h of idle) {
      h.idleSince??=this.t;
      if(idle.length>want&&this.t-h.idleSince>=c.idleSeconds) {
        h.retired=true;idle.splice(idle.indexOf(h),1);this.stats.released++;this.event('release',{host:h.id});
      }
    }
    for(const h of this.hosts)if(rs[h.id]?.slots)h.idleSince=null;
  }

  advanceCopies() {
    const c=this.c,rs=this.resources(this.t,false),rates=this.rates();
    const degree=rs.map(r=>r.incoming+r.outgoing);
    for(const j of this.jobs) {
      const perHost=(id,write)=>{
        const r=rs[id],n=degree[id]||1;
        // With an unpredicted foreground spike, strictly zero spare CPU
        // would deadlock the very copy that relieves it. A 2% rescue budget
        // is explicit and any resulting >60% demand is counted, not hidden.
        const cpuBudget=c.policy==='fixed'?100:Math.max(2,Math.min(c.copyCpuPct,c.ceiling-r.cpu));
        return Math.max(0,Math.min(cpuBudget/100*c.cores*c.copyMiBPerCore/n,
          Math.max(0,c.ceiling-r.io)/100*c.ioMiB/(write?2:1)/n,
          c.ceiling/100*c.networkMiB/n));
      };
      j.bandwidth=Math.min(c.bandwidthMiB,perHost(j.src,false),perHost(j.dst,true));
      const dirty=rates[j.vnode]*(1-j.share)*c.bytesPerItem/1048576*c.dirtyFraction;
      // Installed zones are a separate bottleneck from bytes copied. The
      // measured service rate includes concurrent writes and per-lot commits.
      const zoneMiB=c.bytesPerItem*c.itemsPerZone/1048576;
      const installMiB=c.zonesPerSecond===undefined?Infinity:c.zonesPerSecond*zoneMiB;
      const net=Math.min(j.bandwidth-dirty,installMiB);
      j.bandwidth=Math.min(j.bandwidth,Math.max(0,net+dirty));
      if(net<=0)this.stats.stalledCopySeconds+=c.dt;
      j.leftMiB=clamp(j.leftMiB-net*c.dt,0,j.initialMiB);
      j.fraction=Math.max(j.fraction,1-j.leftMiB/j.initialMiB);
      const share=c.hotFirst?(j.fraction<0.1?5*j.fraction:0.5+(j.fraction-0.1)*0.5/0.9):j.fraction;
      j.share=Math.max(j.share,clamp(share,0,1));
      if(c.gcMode==='granular') {
        j.history.push({t:this.t,f:j.fraction});
        while(j.history.length&&j.history[0].t<=this.t-c.gcSeconds)j.freedFraction=j.history.shift().f;
      }
      this.stats.copyMiB+=j.bandwidth*c.dt;
    }
  }

  step() {
    const c=this.c;
    if(this.t%c.controlEvery<c.dt)this.control();
    this.advanceCopies();
    const rs=this.resources(),rates=this.rates();
    for(const r of rs)if(!r.retired) {
      this.stats.machineSeconds+=c.dt;
      if(!r.ready)continue;
      if(peak(r)>c.ceiling+1e-7)this.stats.overAnySeconds+=c.dt;
      this.stats.peakExcessPct=Math.max(this.stats.peakExcessPct,peak(r)-c.ceiling);
      this.stats.excessPctSeconds+=Math.max(0,peak(r)-c.ceiling)*c.dt;
      for(const [key,field] of [['Cpu','cpu'],['Ram','ram'],['Disk','disk'],['Io','io']]) {
        this.stats[`peak${key==='Cpu'?'CpuDemand':key}`]=Math.max(this.stats[`peak${key==='Cpu'?'CpuDemand':key}`],r[field]);
        if(r[field]>c.ceiling+1e-7)this.stats[`over${key}Seconds`]+=c.dt;
      }
    }
    this.stats.peakHosts=Math.max(this.stats.peakHosts,this.hosts.filter(h=>!h.retired).length);
    this.stats.peakReadyHosts=Math.max(this.stats.peakReadyHosts,rs.filter(r=>r.ready).length);
    this.stats.peakVnodes=Math.max(this.stats.peakVnodes,this.vnodes.length);
    // Storage growth assumes accepted inserts; no ACK or latency conclusion
    // can be drawn when resource demand exceeds real execution capacity.
    for(const v of this.vnodes)v.dataMiB+=rates[v.id]*c.bytesPerItem/1048576*c.insertFraction*c.dt;
    for(const j of [...this.jobs])if(j.leftMiB<=1e-7) {
      const v=this.vnodes[j.vnode];
      this.retained.push({host:j.src,dataMiB:v.dataMiB*(1-j.freedFraction),until:this.t+c.gcSeconds});
      v.host=j.dst;v.job=null;v.coolUntil=this.t+c.cooldown;
      this.event('complete',{vnode:v.id,src:j.src,dst:j.dst,seconds:this.t+c.dt-j.start});
      this.jobs.splice(this.jobs.indexOf(j),1);
    }
    this.retained=this.retained.filter(r=>r.until>this.t);
    this.t+=c.dt;
    if(this.t%c.traceEvery<c.dt||this.t>=c.seconds)this.snapshot();
  }

  snapshot() {
    if(!this.c.trace)return;
    const hosts=this.resources(this.t,true,true).filter(r=>!r.retired).map(r=>({...r,
      cpu:+r.cpu.toFixed(3),ram:+r.ram.toFixed(3),disk:+r.disk.toFixed(3),io:+r.io.toFixed(3),
      rate:+r.rate.toFixed(1),copyMiB:+r.copyMiB.toFixed(1)}));
    this.snapshots.push({t:this.t,demand:demandAt(this.t,this.c.scenario),hosts,
      jobs:this.jobs.map(j=>({vnode:j.vnode,sourceVnode:j.sourceVnode,born:j.born,src:j.src,dst:j.dst,fraction:j.fraction,share:j.share,
        bandwidth:j.bandwidth,leftMiB:j.leftMiB})),vnodes:this.vnodes.length});
  }

  result() {
    const completed=this.events.filter(e=>e.kind==='complete').map(e=>e.seconds);
    return {config:this.c,summary:{...this.stats,machineMinutes:this.stats.machineSeconds/60,
      finalHosts:this.hosts.filter(h=>!h.retired).length,finalVnodes:this.vnodes.length,
      unfinishedCopies:this.jobs.length,medianMoveSeconds:quantile(completed,.5),p95MoveSeconds:quantile(completed,.95)},
      events:this.events,snapshots:this.snapshots,admissions:this.admissions};
  }
}

export function simulate(overrides={}) {
  const s=new SizingSimulation(overrides);while(s.t<s.c.seconds)s.step();return s.result();
}

// The exact same fleet schema consumed by the AWS dashboard.
export function dashboardPoints(result) {
 return result.snapshots.map(s=>({t:s.t,x:s.t,offered:s.demand,estimated_s:s.demand,vnodes:s.vnodes,open:s.jobs.length,
   fleet:{schema:1,source:'simulation',machines:s.hosts.map(h=>({id:String(h.id),label:'H'+(h.id+1),
    state:!h.ready?'booting':h.slots?'active':'warm',cpu:h.cpu,ram:h.ram,disk:h.disk,io:h.io,cpu_scope:'estimé',slots:result.config.slots})),
   vnodes:s.hosts.flatMap(h=>h.vnodes.map(id=>({id:id+'@'+h.id,label:'V'+(id+1),machine:String(h.id),
    state:h.copied.some(j=>j.vnode===id)?'receiving':'ready',estimated_s:h.nodeStats?.[id]?.rate??null}))),
   transfers:s.jobs.map(j=>({id:String(j.vnode),physical_move:j.born===undefined?null:!j.born,source:j.sourceVnode+'@'+j.src,target:j.vnode+'@'+j.dst,source_machine:String(j.src),target_machine:String(j.dst),
    phase:'copie progressive',progress:j.fraction,share:j.share,age_s:null}))}}));
}
