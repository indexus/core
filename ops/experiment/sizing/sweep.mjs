// Usage: node ops/experiment/sizing/sweep.mjs /absolute/output-directory
import {mkdir,writeFile,readFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import {resolve} from 'node:path';
import {simulate} from './model.mjs';

const out=resolve(process.argv[2]||'sizing-results');await mkdir(out,{recursive:true});
const source=await readFile(new URL('./model.mjs',import.meta.url));
const rows=[];
const run=(group,config)=>{const r=simulate({...config,trace:false});rows.push({group,config:r.config,summary:r.summary});};
for(const scenario of ['ramp','hotspot','double','volume'])for(const seed of [7,19,41])
  for(const slots of [1,2,4,5,6,8])for(const policy of ['adaptive','fixed'])
    run('packing',{scenario,seed,slots,policy,trigger:45,...(scenario==='volume'?{dataGiB:64,itemsPerZone:128}:{})});
console.log('packing scenarios:',rows.length);
for(const scenario of ['ramp','hotspot','double'])for(const seed of [7,19,41])
  for(const slots of [4,5])for(const trigger of [40,45,50,55])for(const copyCpuPct of [5,10,15])
    run('thresholds',{scenario,seed,slots,trigger,copyCpuPct});
console.log('threshold scenarios:',rows.length);
for(const scenario of ['hotspot','double'])for(const reservePct of [0,10,20,30])
  for(const seed of [7,19,41])run('warm',{scenario,reservePct,seed,slots:5,trigger:45});
for(const dataGiB of [.125,8,64])for(const itemLimit of [20000,500000])
  for(const seed of [7,19,41])run('items',{dataGiB,itemsPerZone:dataGiB>1?128:1,itemLimit,seed,slots:5,policy:'items',trigger:45});
for(const cpuUsPerWrite of [650,1300,2600])for(const bandwidthMiB of [25,100,200])
  for(const dataGiB of [.25,32,128])run('sensitivity',{cpuUsPerWrite,bandwidthMiB,dataGiB,itemsPerZone:dataGiB>1?128:1,slots:5,trigger:45});
for(const gcMode of ['granular','end'])for(const gcSeconds of [0,20,60])for(const seed of [7,19,41])
  run('gc',{gcMode,gcSeconds,seed,scenario:'volume',dataGiB:64,itemsPerZone:128,slots:5,trigger:45});
for(const vnodeRamMiB of [192,512,1024])for(const zoneMemoryBytes of [512,2048,4096])
  for(const itemsPerZone of [1,128])run('memory',{vnodeRamMiB,zoneMemoryBytes,itemsPerZone,slots:5,trigger:45});
await writeFile(resolve(out,'sweep.json'),JSON.stringify({model_sha256:createHash('sha256').update(source).digest('hex'),rows},null,2)+'\n');
const fields=['group','scenario','seed','slots','policy','trigger','copyCpuPct','reservePct','dataGiB','cpuUsPerWrite','bandwidthMiB','itemsPerZone','zoneMemoryBytes','vnodeRamMiB','gcMode','gcSeconds',
  'machineMinutes','moves','splits','overAnySeconds','excessPctSeconds','peakExcessPct','overCpuSeconds','overRamSeconds','overDiskSeconds','overIoSeconds','peakCpuDemand','peakHosts','finalHosts','unfinishedCopies','modelLimitHit'];
await writeFile(resolve(out,'sweep.csv'),fields.join(',')+'\n'+rows.map(r=>fields.map(k=>r.config[k]??r.summary[k]??r[k]??'').join(',')).join('\n')+'\n');
for(const [name,cfg] of Object.entries({ramp:{},hotspot:{scenario:'hotspot'},doubling:{scenario:'double'},volume:{scenario:'volume',dataGiB:64,itemsPerZone:128}})) {
  await writeFile(resolve(out,name+'.json'),JSON.stringify(simulate({...cfg,trigger:45}))+'\n');
}
console.log('total scenarios:',rows.length,'output:',out);
