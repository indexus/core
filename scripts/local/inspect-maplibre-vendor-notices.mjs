// Trace upstream source files; matching hashes do not certify whole package versions.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
const root=path.resolve(process.env.INDEXUS_WEB_ROOT??'/opt/indexus/experiences');
const packageRoot=path.join(root,'node_modules/maplibre-gl');
const metadata=JSON.parse(fs.readFileSync(path.join(packageRoot,'package.json'),'utf8'));
const sha=bytes=>crypto.createHash('sha256').update(bytes).digest('hex');
const packages=new Map(),maps=[];
function traceEmbeddedSource(source,sourceSHA256){
 // Preserve the first node_modules path too: style-spec ships generated modules
 // under dist/node_modules, whose own maps point back to original package files.
 const offset=source.indexOf('node_modules/');
 const relative=source.slice(offset+'node_modules/'.length);
 if(relative.split('/').includes('..'))return null;
 const file=path.join(root,'node_modules',relative);
 if(!fs.existsSync(file)||!fs.statSync(file).isFile())return null;
 const bytes=fs.readFileSync(file),matchesEmbeddedSource=sha(bytes)===sourceSHA256;
 const reference=/\/\/# sourceMappingURL=([^\s]+)/.exec(bytes.toString('utf8'))?.[1];
 if(!reference||reference.includes('/')||reference.includes('..'))return null;
 const mapFile=path.join(path.dirname(file),reference);
 if(!fs.existsSync(mapFile))return null;
 const mapBytes=fs.readFileSync(mapFile),map=JSON.parse(mapBytes);
 if(map.sources.length!==map.sourcesContent?.length)throw Error('Incomplete nested source map: '+mapFile);
 const originals=map.sources.map((original,i)=>{
  const start=original.lastIndexOf('node_modules/');
  const relative=start<0?null:original.slice(start+'node_modules/'.length);
  const candidate=relative&&!relative.split('/').includes('..')?path.join(root,'node_modules',relative):null;
  const sourceHash=typeof map.sourcesContent[i]==='string'?sha(Buffer.from(map.sourcesContent[i])):null;
  const installedHash=candidate&&fs.existsSync(candidate)&&fs.statSync(candidate).isFile()?sha(fs.readFileSync(candidate)):null;
  return {source:original,sourceSHA256:sourceHash,installedFile:candidate?path.relative(root,candidate):null,installedSHA256:installedHash,matchesInstalled:sourceHash!==null&&sourceHash===installedHash};
 });
 return {file:path.relative(root,file),sha256:sha(bytes),matchesEmbeddedSource,sourceMap:path.relative(root,mapFile),sourceMapSHA256:sha(mapBytes),originals,traced:matchesEmbeddedSource&&originals.length>0&&originals.every(s=>s.matchesInstalled)};
}

for(const basename of ['maplibre-gl.mjs','maplibre-gl-worker.mjs']){
 const bundleFile=path.join(packageRoot,'dist',basename),mapFile=bundleFile+'.map';
 const bundle=fs.readFileSync(bundleFile),mapBytes=fs.readFileSync(mapFile);
 if(!bundle.toString('utf8').includes('sourceMappingURL='+basename+'.map'))throw Error('Missing expected source-map reference: '+basename);
 const map=JSON.parse(mapBytes);
 if(map.sources.length!==map.sourcesContent?.length)throw Error('Incomplete source contents: '+basename);
 maps.push({file:path.relative(packageRoot,mapFile),sha256:sha(mapBytes),bundleSHA256:sha(bundle),sources:map.sources.length});
 for(let i=0;i<map.sources.length;i++){
  const source=map.sources[i],offset=source.lastIndexOf('node_modules/');
  if(offset<0)continue;
  const relative=source.slice(offset+'node_modules/'.length),parts=relative.split('/');
  const name=parts.splice(0,relative.startsWith('@')?2:1).join('/'),file=parts.join('/');
  if(!file||!name||file.split('/').includes('..'))throw Error('Invalid source path: '+source);
  let entry=packages.get(name);
  if(!entry){
   const dir=path.join(root,'node_modules',name),json=path.join(dir,'package.json');
   const meta=fs.existsSync(json)?JSON.parse(fs.readFileSync(json,'utf8')):null;
   const notices=[];
   for(const folder of [dir,path.join(dir,'dist')])if(fs.existsSync(folder))for(const filename of fs.readdirSync(folder)){
    const target=path.join(folder,filename);
    if(!/(license|licence|copying|notice|legal|third.?party)/i.test(filename)||!fs.statSync(target).isFile())continue;
    const bytes=fs.readFileSync(target);notices.push({file:path.relative(dir,target),sha256:sha(bytes),bytes:bytes.length,text:bytes.toString('utf8')});
   }
   if(!notices.length&&fs.existsSync(path.join(dir,'README.md'))){
    const bytes=fs.readFileSync(path.join(dir,'README.md')),text=bytes.toString('utf8');
    const match=/^#{1,6}\s+licen[cs]e[^\n]*\n[\s\S]*?(?=^#{1,6}\s|$(?![\s\S]))/im.exec(text);
    if(match){const section=Buffer.from(match[0]);notices.push({file:'README.md#license',sourceFileSHA256:sha(bytes),sha256:sha(section),bytes:section.length,text:match[0]});}
   }
   entry={name,installedVersion:meta?.version??null,declaredLicense:meta?.license??null,notices,sources:[]};packages.set(name,entry);
  }
  const target=path.join(root,'node_modules',name,file),content=map.sourcesContent[i];
  const sourceSHA256=typeof content==='string'?sha(Buffer.from(content)):null;
  const installedSHA256=fs.existsSync(target)&&fs.statSync(target).isFile()?sha(fs.readFileSync(target)):null;
  const matchesInstalled=sourceSHA256!==null&&sourceSHA256===installedSHA256;
  const embeddedTrace=matchesInstalled?null:traceEmbeddedSource(source,sourceSHA256);
  entry.sources.push({upstreamSourcePath:source,map:basename+'.map',file,sourceSHA256,installedSHA256,matchesInstalled,embeddedTrace,tracedToInstalledSources:matchesInstalled||embeddedTrace?.traced===true});
 }
}
const upstreamNotice=fs.readFileSync(path.join(packageRoot,'LICENSE.txt'));
const entries=[...packages.values()].sort((a,b)=>a.name.localeCompare(b.name)).map(entry=>({...entry,allMappedSourcesMatch:entry.sources.every(source=>source.matchesInstalled),allMappedSourcesTraced:entry.sources.every(source=>source.tracedToInstalledSources)}));
const report={format:'indexus-upstream-source-map-notice-review-v2',scope:'MapLibre upstream main/worker source-map candidates. Not a final bundle SBOM or legal clearance.',limits:['Source-map presence does not establish survival through final app tree shaking.','Installed dependency versions are not assumed to be the versions used to build MapLibre; per-file hash matches/mismatches are retained.','Matching all mapped files does not certify unlisted files or establish an entire upstream package version.','Nested prebundled code, assets, data, remote services and OS/runtime components need separate review.','Collected notice texts are preparation evidence; publication and licence decisions remain separate.'],upstream:{name:metadata.name,version:metadata.version,license:{file:'LICENSE.txt',sha256:sha(upstreamNotice),bytes:upstreamNotice.length,containsMapbox:upstreamNotice.includes('Contains code from mapbox-gl-js'),containsGlfx:upstreamNotice.includes('Contains code from glfx.js'),containsD3Color:upstreamNotice.includes('Contains a portion of d3-color')}},maps,packages:entries};
if(process.env.INDEXUS_NOTICE_REQUIRE_TRACED==='1'){
 const untraced=entries.filter(entry=>!entry.allMappedSourcesTraced);
 const missing=entries.filter(entry=>!entry.notices.length);
 if(untraced.length||missing.length)throw Error('Incomplete upstream notice provenance: '+JSON.stringify({untraced:untraced.map(e=>e.name),withoutNotices:missing.map(e=>e.name)}));
}
const output=process.env.INDEXUS_NOTICE_OUTPUT;
if(output)fs.writeFileSync(output,JSON.stringify(report,null,2)+'\n');else process.stdout.write(JSON.stringify(report,null,2)+'\n');
console.error(JSON.stringify({upstream:metadata.version,packages:entries.length,allMappedSourcesMatch:entries.filter(e=>e.allMappedSourcesMatch).map(e=>e.name),allMappedSourcesTraced:entries.filter(e=>e.allMappedSourcesTraced).map(e=>e.name),sourceMismatches:entries.filter(e=>!e.allMappedSourcesMatch).map(e=>({name:e.name,files:e.sources.filter(s=>!s.matchesInstalled).length})),withoutCollectedNotices:entries.filter(e=>!e.notices.length).map(e=>e.name)}));
