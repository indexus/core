#!/usr/bin/env python3
"""Add France temperature, local detail and coarse weather to a small 3D sample."""
import argparse, hashlib, json, math, re, shutil, subprocess, tarfile
from pathlib import Path
ALPHABET='0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_'
INDEX={c:i for i,c in enumerate(ALPHABET)}

def bounds(location,collection):
    lo,hi=[-90.,-180.],[90.,180.]
    for level,c in enumerate(location):
        word=(INDEX[c]-INDEX[collection[level%len(collection)]])%64
        for bit in range(5,-1,-1):
            d=(level*6+5-bit)%2; mid=(lo[d]+hi[d])/2
            if word & (1<<bit): lo[d]=mid
            else: hi[d]=mid
    return lo,hi

def france_temperature(src, dest, lat, lon, radius, depth):
    """Partition original records: native cells near Anglet, real zone sums elsewhere.

    A whole coarse cell is retained at native resolution if it intersects the
    local window. Never emit both that cell's sum and its native descendants.
    Metrics remain additive area and temperature*area, including all 72 hours.
    """
    collection=src.stem;size=depth//3
    scale=math.pi*6378137/180;cos=math.cos(math.radians(lat))
    fine={};coarse={};native=0;source_records=0;digest=hashlib.sha256()
    source_sums=None
    with src.open('rb') as source,dest.open('w') as output:
        for raw in source:
            digest.update(raw);record=json.loads(raw);source_records+=1
            location=record['Location'];prefix=location[:size];metrics=record['Metrics']
            if source_sums is None:source_sums=[0.]*len(metrics)
            for i,value in enumerate(metrics):source_sums[i]+=value
            if prefix not in fine:
                lo,hi=bounds(prefix,collection)
                x=max(lo[1]-lon,0,lon-hi[1])*scale*cos
                y=max(lo[0]-lat,0,lat-hi[0])*scale
                fine[prefix]=x*x+y*y<=radius*radius
            if fine[prefix]:
                output.write(raw.decode());native+=1
            else:
                sums=coarse.setdefault(prefix,[0.]*len(metrics))
                for i,value in enumerate(metrics):sums[i]+=value
        for prefix,sums in sorted(coarse.items()):
            output.write(json.dumps({'Collection':collection,'Location':prefix,
                'ID':'france-'+prefix,'Metrics':sums},separators=(',',':'))+'\n')
    return {'source_records':source_records,'source_sha256':digest.hexdigest(),
        'source_sums':source_sums,'native_records':native,'coarse_records':len(coarse),
        'coarse_depth_per_axis':depth,'records':native+len(coarse)}

def main():
    ap=argparse.ArgumentParser(description=__doc__)
    for name in ['sample','weather','prep','out']:ap.add_argument('--'+name,type=Path,required=True)
    ap.add_argument('--radius-km',type=float,default=30)
    ap.add_argument('--france-depth',type=int,choices=[0,9,12],default=12,
        help='coarse GPS bits per axis outside Anglet; 0 preserves the old local-only sample')
    a=ap.parse_args()
    if not (a.sample/'sample.json').is_file():ap.error('--sample must contain sample.json and collection batches')
    if a.out.exists():ap.error('choose a fresh output directory')
    a.out.mkdir(parents=True)
    batches=a.out/'batches';shutil.copytree(a.sample,batches)
    exports=a.out/'weather';exports.mkdir()
    # Match the unchanged source scene frame; no coordinate or record re-encoding.
    lon,lat=-1.520707,43.484986
    radius=a.radius_km*1000;scale=math.pi*6378137/180;cos=math.cos(math.radians(lat))
    counts={};temperature_proof=None
    for src in sorted(a.weather.glob('*.ndjson')):
        if not src.name.startswith('Tempe'):
            shutil.copy2(src,exports/src.name);continue
        if a.france_depth:
            temperature_proof=france_temperature(src,exports/src.name,lat,lon,radius,a.france_depth)
            counts[src.stem]=temperature_proof['records']
            print(json.dumps(temperature_proof),flush=True)
            continue
        prefix_cache={};count=0
        with src.open() as f,(exports/src.name).open('w') as dest:
            for line in f:
                m=re.search(r'"Location"\s*:\s*"([^"]+)"',line)
                if not m:raise ValueError('source record without Location')
                location=m[1];prefix=location[:3];collection=src.stem
                if prefix not in prefix_cache:
                    lo,hi=bounds(prefix,collection)
                    prefix_cache[prefix]=(lo[0]<=lat+radius/scale and hi[0]>=lat-radius/scale and lo[1]<=lon+radius/(scale*cos) and hi[1]>=lon-radius/(scale*cos))
                if not prefix_cache[prefix]:continue
                lo,hi=bounds(location,collection)
                x=(((lo[1]+hi[1])/2)-lon)*scale*cos;y=(((lo[0]+hi[0])/2)-lat)*scale
                if x*x+y*y<=radius*radius:dest.write(line);count+=1
        if count==0:raise ValueError('empty temperature window')
        counts[collection]=count;print(collection,count,flush=True)
    subprocess.run([str(a.prep.resolve()),'ndjson','-out',str(batches),*[str(p) for p in sorted(exports.glob('*.ndjson'))]],check=True)
    for directory in sorted(batches.iterdir()):
        if not directory.is_dir():continue
        subprocess.run([str(a.prep.resolve()),'verify',str(directory)],check=True)
        m=json.loads((directory/'manifest.json').read_text());counts[m['collection']]=m['records']
    metadata=json.loads((batches/'sample.json').read_text())
    sample_name='indexus-sample-3d-weather-france-v1' if a.france_depth else 'indexus-sample-3d-weather-v1'
    metadata.update(name=sample_name,weather={'run':'AROME archive beginning 2025-01-28','temperature_native_radius_km':a.radius_km,'temperature_france_depth':a.france_depth,'coarse_variables':'Original prepared France grids; no new acquisition'},collections=counts)
    (batches/'sample.json').write_text(json.dumps(metadata,indent=2))
    if temperature_proof:(a.out/'temperature-proof.json').write_text(json.dumps(temperature_proof,indent=2))
    archive=a.out/(sample_name+'.tar.xz')
    with tarfile.open(archive,'w:xz',preset=6) as t:t.add(batches,arcname='indexus-sample-3d-weather-v1')
    digest=hashlib.sha256(archive.read_bytes()).hexdigest()
    Path(str(archive)+'.sha256').write_text(f'{digest}  {archive.name}\n')
    print(json.dumps({'bytes':archive.stat().st_size,'sha256':digest,'collections':counts}),flush=True)
if __name__=='__main__':main()
