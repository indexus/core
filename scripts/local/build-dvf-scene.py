#!/usr/bin/env python3
"""Add only geocoded house sales inside the local Anglet scene to a prepared sample."""
import argparse,hashlib,json,math,shutil,subprocess,tarfile
from pathlib import Path

def local_sales(lines,origin=(-1.520707,43.484986),radius=150):
    lon,lat=origin;scale=math.pi*6378137/180;cos=math.cos(math.radians(lat))
    for line in lines:
        record=json.loads(line);m=record['Metrics']
        if record.get('Deleted') or len(m)<5:continue
        y=(m[3]-90-lat)*scale;x=(m[4]-180-lon)*scale*cos
        if x*x+y*y<=radius*radius and m[0]>0 and m[1]>0:
            yield record

def main():
    ap=argparse.ArgumentParser(description=__doc__)
    for key in ['sample','dvf','prep','out']:ap.add_argument('--'+key,type=Path,required=True)
    ap.add_argument("--weather-sample",type=Path,help="Reuse verified weather batches without reprocessing the source archive")
    a=ap.parse_args()
    if a.out.exists():ap.error('choose a fresh output directory')
    batches=a.out/'batches';shutil.copytree(a.sample,batches)
    meta=json.loads((batches/'sample.json').read_text());radius=meta['radius_metres']
    if a.weather_sample:
        weather_meta=json.loads((a.weather_sample/'sample.json').read_text())
        for directory in a.weather_sample.iterdir():
            if directory.is_dir() and 'AROME' in directory.name:shutil.copytree(directory,batches/directory.name)
        meta['weather']=weather_meta['weather']
    export=a.out/'dvf-local.ndjson';digest=hashlib.sha256()
    def source():
        with a.dvf.open('rb') as f:
            for line in f:digest.update(line);yield line
    rows=list(local_sales(source(),radius=radius))
    if not rows:raise ValueError('no local sales; never fabricate a roof price')
    export.write_text(''.join(json.dumps(row,separators=(',',':'))+'\n' for row in rows))
    subprocess.run([str(a.prep.resolve()),'ndjson','-out',str(batches),str(export)],check=True)
    counts={}
    for directory in sorted(batches.iterdir()):
        if not directory.is_dir():continue
        subprocess.run([str(a.prep.resolve()),'verify',str(directory)],check=True)
        m=json.loads((directory/'manifest.json').read_text());counts[m['collection']]=m['records']
    name='indexus-sample-3d-cross-collection-v2'
    meta.update(name=name,collections=counts,dvf={'year':2020,'scope':'local Anglet scene only','records':len(rows),'source_sha256':digest.hexdigest(),'transformation':'crop only; unchanged IDs, locations and metrics; roof matches are approximate'})
    (batches/'sample.json').write_text(json.dumps(meta,indent=2))
    archive=a.out/(name+'.tar.xz')
    with tarfile.open(archive,'w:xz',preset=6) as t:t.add(batches,arcname=name)
    sha=hashlib.sha256(archive.read_bytes()).hexdigest()
    Path(str(archive)+'.sha256').write_text(sha+'  '+archive.name+'\n')
    print(json.dumps({'bytes':archive.stat().st_size,'sha256':sha,'collections':counts,'dvf':meta['dvf']}))
if __name__=='__main__':main()
