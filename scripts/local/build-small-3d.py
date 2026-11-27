#!/usr/bin/env python3
"""Prepare a small Explore scene from existing local source exports, without downloads.

Usage: build-small-3d.py --sources DIR --prep BIN --out DIR [--radius 150]
The local source exports must use the Anglet frame of the supplied catalogue.
"""
import argparse
import base64
import gzip
import hashlib
import json
import math
from pathlib import Path
import subprocess
import tarfile

R = 6378137.0

def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--sources", type=Path, required=True)
    ap.add_argument("--prep", type=Path, required=True)
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--radius", type=float, default=150)
    args = ap.parse_args()
    if not 50 <= args.radius <= 500:
        ap.error("radius must be between 50 and 500 metres")
    if args.out.exists():
        ap.error("output already exists; choose a fresh output directory")
    args.out.mkdir(parents=True)
    exports = args.out / "exports"
    exports.mkdir()
    with (args.sources / "catalogue.ndjson").open() as f:
        source = json.loads(next(f))["Payload"]
    catalogue = json.loads(gzip.decompress(base64.b64decode(source["data"])))
    scene = next(s for s in catalogue if s["id"] == "anglet")
    lon, lat = scene["origin"]
    cos = math.cos(math.radians(lat))
    radius = args.radius
    scene.update(id="anglet-mini", label=f"Anglet · mini 3D · {2*radius:g} m", radius=radius)
    scene.pop("address", None)
    scene["bounds"] = [[lon-math.degrees(radius/(R*cos)),lat-math.degrees(radius/R)],
                       [lon+math.degrees(radius/(R*cos)),lat+math.degrees(radius/R)]]
    scene["collections"].update(buildings="BuildLidHD2025v2", fine="BuildLidHD2025f2")
    counts, building_ids = {}, set()

    def tile_intersects(record):
        z, row, col = map(int, record["ID"].split("/"))
        # These are the shared GPS Space cells, south-origin, not XYZ map tiles.
        dlat, dlon = 180 / 2**z, 360 / 2**z
        xmin, xmax = (math.radians(-180+col*dlon-lon)*R*cos, math.radians(-180+(col+1)*dlon-lon)*R*cos)
        ymin, ymax = (math.radians(-90+row*dlat-lat)*R, math.radians(-90+(row+1)*dlat-lat)*R)
        return xmin <= radius and xmax >= -radius and ymin <= radius and ymax >= -radius

    def building(record):
        a, b = record["Payload"]["origin"][:2]
        x, y = math.radians(b-lon)*R*cos, math.radians(a-lat)*R
        keep = x*x+y*y <= (radius+20)**2
        if keep: building_ids.add(record["ID"].partition(".")[2])
        return keep

    def vegetation(record):
        n,x,y = record["Metrics"][:3]
        return n > 0 and (x/n)**2+(y/n)**2 <= radius**2

    jobs = [("buildings-v2.ndjson",building),
            ("buildings-v2-fine.ndjson",lambda r:r["ID"].partition(".")[2] in building_ids),
            ("terrain.ndjson",tile_intersects),("ortho.ndjson",tile_intersects),
            ("vegetation-final.ndjson",vegetation)]
    for filename, keep in jobs:
        count = 0
        with (args.sources/filename).open() as src, (exports/filename).open("w") as dest:
            for line in src:
                record = json.loads(line)
                if keep(record):
                    dest.write(line)
                    count += 1
        if not count: raise RuntimeError(f"empty scene component: {filename}")
        counts[filename] = count
        print(filename,count,flush=True)
    # Use the existing scene builder and its shared GPS encoder.
    scene_file = args.out / "catalogue.json"
    scene_file.write_text(json.dumps([scene]))
    core = Path(__file__).resolve().parents[2]
    subprocess.run(["python3",str(core/"tools/prep/scenes/scenes.py"),str(scene_file),str(exports/"scenes.ndjson")],check=True)
    batches = args.out / "batches"
    subprocess.run([str(args.prep.resolve()),"ndjson","-out",str(batches),*[str(f) for f in sorted(exports.glob("*.ndjson"))]],check=True)
    for batch in sorted(batches.iterdir()):
        if not batch.is_dir(): continue
        subprocess.run([str(args.prep.resolve()),"verify",str(batch)],check=True)
    metadata = {"name":"indexus-sample-3d-mini-v1", "radius_metres":radius,
                "diameter_metres":2*radius,"source_records":counts,
                "sources":{"buildings":"IGN BD TOPO and LiDAR HD, local prepared exports",
                           "terrain_vegetation":"IGN LiDAR HD, local prepared exports",
                           "orthophoto":"IGN BD ORTHO, local prepared exports"},
                "distribution_status":"Local preview; attribution and source acquisition metadata must be verified before publication"}
    (batches/"sample.json").write_text(json.dumps(metadata,indent=2))
    archive = args.out / "indexus-sample-3d-mini-v1.tar.xz"
    with tarfile.open(archive,"w:xz",preset=6) as tar:
        tar.add(batches,arcname="indexus-sample-3d-mini-v1")
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_suffix(archive.suffix+".sha256").write_text(f"{digest}  {archive.name}\n")
    print(json.dumps({"archive":str(archive),"bytes":archive.stat().st_size,"sha256":digest}),flush=True)

if __name__ == "__main__": main()
