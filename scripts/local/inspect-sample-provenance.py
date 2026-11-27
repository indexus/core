#!/usr/bin/env python3
"""Record local metadata evidence without downloading or modifying sample data."""
import argparse, ast, hashlib, json, runpy, tarfile
from pathlib import Path

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('--root',type=Path,required=True)
p.add_argument('--sample',type=Path,required=True,help='Prepared sample directory containing batches/')
p.add_argument('--out',type=Path,required=True)
p.add_argument('--weather-sample',type=Path,help='Earlier prepared sample containing the unchanged AROME batches')
p.add_argument('--dvf-source',type=Path,help='Prepared DVF NDJSON used by the crop builder; checked without downloading')
a=p.parse_args();root=a.root.resolve();sample=a.sample.resolve()
def digest(file):
 h=hashlib.sha256()
 with file.open('rb') as f:
  for block in iter(lambda:f.read(1024*1024),b''):h.update(block)
 return h.hexdigest()
def evidence(relative):
 file=root/relative
 if not file.exists():return {'file':relative,'present':False},None
 return {'file':relative,'present':True,'sha256':digest(file),'bytes':file.stat().st_size},json.loads(file.read_text())
items=[]
def read(relative):
 meta,data=evidence(relative);items.append(meta);return data or {}
recipe=read('experiences/data/world/anglet-1km/recipe.json')
raw=read('experiences/data/world/anglet-1km/manifest.json')
lidar=read('experiences/public/world/anglet-1km/points-ign/source-manifest-4098c2dc10422c39.json')
photo=read('experiences/public/world/anglet-1km/points-imagery/manifest.json')
footprints=read('experiences/data/world/anglet-1km/context/footprints-source.json')
prepared=read('_run/.data/prep/work/weather/weather.json')
current_weather=read('experiences/public/data/indexus/arome/manifest.json')
legacy_path='_run/.data/launch/weather-ortho.ndjson';file=root/legacy_path;legacy=None
if file.exists():
 items.append({'file':legacy_path,'present':True,'sha256':digest(file),'bytes':file.stat().st_size})
 with file.open() as source:
  for line in source:
   record=json.loads(line);payload=record.get('Payload')
   if isinstance(payload,dict) and payload.get('schema')=='indexus-weather-tiles-v1':legacy=payload;break
if not legacy:raise SystemExit('Archived weather manifest not found')
source_scripts=[]
for name in ['index-arome.py','arome_atmosphere.py']:
 relative='experiences/scripts/research/arome/'+name;file=root/relative
 constants={}
 for node in ast.parse(file.read_text()).body:
  if isinstance(node,ast.Assign) and any(isinstance(t,ast.Name) and t.id=='SOURCE' for t in node.targets):constants['SOURCE']=ast.literal_eval(node.value)
 source_scripts.append({'file':relative,'sha256':digest(file),**constants})
metadata=json.loads((sample/'batches/sample.json').read_text())
counts={}
for manifest in sorted((sample/'batches').glob('*/manifest.json')):
 m=json.loads(manifest.read_text());counts[m['collection']]={'records':m['records'],'manifestSha256':digest(manifest),'batchId':m.get('id')}
assert {name:c['records'] for name,c in counts.items()}==metadata['collections'],'Counts differ from sample metadata'
archive=sample/(metadata['name']+'.tar.xz')
files={str(f.relative_to(sample/'batches')):digest(f) for f in sorted((sample/'batches').rglob('*')) if f.is_file()}
with tarfile.open(archive,'r:xz') as tar:
 archived={};archived_files={}
 for member in tar:
  parts=Path(member.name).parts
  assert not Path(member.name).is_absolute() and '..' not in parts,'Unsafe archive member'
  if member.isdir():continue
  assert member.isfile() and len(parts)>=2,'Unsupported archive member'
  relative=str(Path(*parts[1:]));assert relative not in archived_files,'Duplicate archive member'
  h=hashlib.sha256()
  with tar.extractfile(member) as source:
   for block in iter(lambda:source.read(1024*1024),b''):h.update(block)
  archived_files[relative]=h.hexdigest()
  if len(parts)==3 and parts[-1]=='manifest.json':
   raw_manifest=tar.extractfile(member).read();m=json.loads(raw_manifest)
   archived[m['collection']]=hashlib.sha256(raw_manifest).hexdigest()
 assert archived=={name:c['manifestSha256'] for name,c in counts.items()},'Archive manifests differ from prepared batch directory'
 assert archived_files==files,'Archive files differ from prepared batch directory'

derivatives={}
if a.weather_sample:
 baseline=a.weather_sample.resolve()/'batches'
 weather={name:counts[name] for name in counts if 'AROME' in name}
 assert len(weather)==7,'Expected seven AROME collections'
 for name in weather:
  earlier={str(f.relative_to(baseline/name)):digest(f) for f in (baseline/name).rglob('*') if f.is_file()}
  current={str(f.relative_to(sample/'batches'/name)):digest(f) for f in (sample/'batches'/name).rglob('*') if f.is_file()}
  assert earlier==current,'Weather batch changed: '+name
 derivatives['weather']={'baseline':str(baseline),'unchangedCollections':sorted(weather),'allBatchFilesIdentical':True}
if a.dvf_source:
 crop=runpy.run_path(str(Path(__file__).with_name('build-dvf-scene.py')))['local_sales']
 source_hash=digest(a.dvf_source)
 assert source_hash==metadata['dvf']['source_sha256'],'DVF source hash differs'
 with a.dvf_source.open() as source:expected=list(crop(source,radius=metadata['radius_metres']))
 export=sample/'dvf-local.ndjson';actual=[json.loads(line) for line in export.read_text().splitlines()]
 assert actual==expected,'DVF crop changed records or order'
 assert len(actual)==counts['DvFMV2020idx0001']['records'],'DVF crop count differs'
 assert digest(export)==json.loads((sample/'batches/DvFMV2020idx0001/manifest.json').read_text())['inputs']['sources'][export.name]
 derivatives['dvf']={'sourceSha256':source_hash,'cropSha256':digest(export),'records':len(actual),'radiusMetres':metadata['radius_metres'],'recordsUnchanged':True,'note':'Confirms the local crop from a prepared export, not its upstream acquisition lineage or roof association.'}

assert legacy['meta']['firstTimestamp']==prepared['firstTimestamp']==1738022400
assert legacy['meta']['hours']==prepared['hours']==72
assert raw['sourceSha256']==lidar['source']['subsetSha256']
result={'format':'indexus-sample-provenance-review-v2','status':'Evidence draft; release attribution and complete transformation lineage not yet approved','sample':{'archive':archive.name,'sha256':digest(archive),'bytes':archive.stat().st_size,'collections':counts,'records':sum(c['records'] for c in counts.values()),'archiveManifestsMatch':True,'archiveFilesMatch':True,'fileHashes':files,'derivatives':derivatives},'evidenceFiles':items,'lidar':{'sourceURLs':recipe.get('sources',[]),'subsetSha256':raw['sourceSha256'],'attribution':lidar['source'].get('attribution'),'licenceRecorded':lidar['source'].get('licence'),'note':'A product path date is not proof of each acquisition or update date.'},'buildings':{k:footprints.get(k) for k in ['url','sha256','retrievedAt','attribution','features']},'orthophoto':{k:photo.get(k) for k in ['source','sourceURL','attribution','sha256','retrievedAt','acquisitionDate']},'weather':{'archivedVersion':legacy['version'],'firstTimestamp':prepared['firstTimestamp'],'hours':prepared['hours'],'atmosphereSource':legacy.get('atmosphere',{}).get('source'),'acquisitionScriptEvidence':source_scripts,'currentPublicManifestExcluded':{'version':current_weather.get('version'),'firstTimestamp':current_weather.get('meta',{}).get('firstTimestamp'),'reason':'Different June archive; not evidence of the January sample origin.'}},'remaining':['Confirm immutable acquisition/update dates for each exact upstream product.','Complete the links from retained upstream evidence through prepared exports to all cropped batches.','Credit Open-Meteo as distributor as well as Météo-France; keep data licence separate from API/service terms.','Verify town-label/land-mask source provenance independently.','Reconcile data notice with archive metadata before a public release; do not relabel all data Apache-2.0.']}
a.out.parent.mkdir(parents=True,exist_ok=True);a.out.write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
print(json.dumps({'collections':len(counts),'records':result['sample']['records'],'archiveSha256':result['sample']['sha256'],'weatherVersion':legacy['version'],'weatherSource':result['weather']['atmosphereSource'],'lidarSubsetHashMatched':True}))
