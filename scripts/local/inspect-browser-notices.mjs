// Run inside the quickstart web-build stage: inspect the build, not installed-package guesses.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import {pathToFileURL} from 'node:url';
const root=path.resolve(process.env.INDEXUS_WEB_ROOT??'/opt/indexus/experiences');
const output=process.env.INDEXUS_NOTICE_OUTPUT;
const {build}=await import(pathToFileURL(path.join(root,'node_modules/vite/dist/node/index.js')).href);
const modules=new Set(),scopes={main:0,worker:0};
function collector(scope){return {name:'indexus-browser-notice-inventory-'+scope,generateBundle(_options,bundle){scopes[scope]++;for(const chunk of Object.values(bundle))if(chunk.type==='chunk')for(const id of Object.keys(chunk.modules))modules.add(id);}};}
process.env.VITE_MAP_STYLE_URL??='';process.env.VITE_DEFAULT_DATASET??='weather';process.env.VITE_DEMO_MODE??='1';
const built=await build({root,configFile:path.join(root,'vite.config.js'),logLevel:'warn',plugins:[collector('main')],worker:{plugins:()=>[collector('worker')]},build:{write:false}});
const sha=bytes=>crypto.createHash('sha256').update(bytes).digest('hex');
const assets=[];
for(const result of Array.isArray(built)?built:[built])for(const asset of result.output){
 const bytes=Buffer.from(asset.type==='chunk'?asset.code:asset.source);
 const file=path.join(root,'dist',asset.fileName);
 if(!fs.existsSync(file))throw Error('Rebuild emitted an asset absent from the original stage: '+asset.fileName);
 const expected=sha(fs.readFileSync(file)),actual=sha(bytes);
 if(expected!==actual)throw Error('Rebuild differs from original stage: '+asset.fileName);
 assets.push({file:asset.fileName,bytes:bytes.length,sha256:actual});
}
const packages=new Map(),unresolved=[],virtual=[];
function owner(file){
 let dir=path.dirname(file);
 while(dir!==path.dirname(dir)){
  const json=path.join(dir,'package.json');
  if(fs.existsSync(json)){const meta=JSON.parse(fs.readFileSync(json,'utf8'));if(meta.name&&meta.version)return {dir,meta};}
  dir=path.dirname(dir);
 }
 return null;
}
for(const id of modules){
 if(id.startsWith('\0')||!path.isAbsolute(id)){virtual.push(id);continue;}
 const file=id.split('?')[0];
 if(!fs.existsSync(file)){unresolved.push(id);continue;}
 const found=owner(file);if(!found){unresolved.push(id);continue;}
 const {dir,meta}=found;
 let entry=packages.get(dir);
 if(!entry){
  const notices=[];
  for(const folder of [dir,path.join(dir,'dist')]){
   if(!fs.existsSync(folder)||!fs.statSync(folder).isDirectory())continue;
   for(const name of fs.readdirSync(folder)){
    const target=path.join(folder,name);
    if(!/(license|licence|copying|notice|legal|third.?party)/i.test(name)||!fs.statSync(target).isFile())continue;
    const bytes=fs.readFileSync(target);
    notices.push({file:path.relative(dir,target),sha256:sha(bytes),bytes:bytes.length,text:bytes.toString('utf8')});
   }
  }
  entry={name:meta.name,version:meta.version,declaredLicense:meta.license??meta.licenses??null,firstParty:dir===root||['../sdk-js','../sdk'].some(sdk=>dir===path.resolve(root,sdk)),packageRoot:dir,modules:[],notices};packages.set(dir,entry);
 }
 entry.modules.push(path.relative(dir,file));
}
const entries=[...packages.values()].map(e=>({...e,modules:[...new Set(e.modules)].sort()})).sort((a,b)=>a.name.localeCompare(b.name));
const result={format:'indexus-browser-notice-inventory-v1',scope:'Rollup chunk module metadata from main and worker builds; all emitted assets byte-identical to the existing web-build stage. Not a complete SBOM or legal clearance.',limits:['Upstream prebundled/vendored code requires separate review of aggregate notices.','Copied public assets, sample data, remote maps/fonts and runtime/OS binaries are outside this module inventory.','Only package-root and dist notice filenames are collected; absence requires investigation, not a conclusion of no license.','First-party licence files are copied separately into the runtime image; the web-build stage does not retain all of them.'],builds:scopes,assets:assets.sort((a,b)=>a.file.localeCompare(b.file)),packages:entries,unresolvedModules:unresolved.sort(),virtualModules:virtual.sort()};
if(unresolved.length)throw Error('Unresolved file modules: '+unresolved.join(', '));
const json=JSON.stringify(result,null,2)+'\n';
if(output)fs.writeFileSync(output,json);else process.stdout.write(json);
console.error(JSON.stringify({packages:entries.length,thirdParty:entries.filter(e=>!e.firstParty).length,modules:modules.size,assets:assets.length,builds:scopes,missingDeclarations:entries.filter(e=>!e.declaredLicense).map(e=>e.name),withoutCollectedNotices:entries.filter(e=>!e.notices.length).map(e=>e.name)}));
