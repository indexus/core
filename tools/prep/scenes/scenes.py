#!/usr/bin/env python3
"""Explore's scene list as a collection: one record per scene, at the
16-character location of its origin in the `gps` Space, its catalogue entry
as the payload (experiences/scripts/world/catalogue.json). Explore reads the
records of a zone (the root for the whole list) instead of the former
hash-addressed content `world/catalogue.json`.

A scene's zone is a disc (`radius`) or a contour: `boundary`, a GeoJSON file
beside the catalogue (Polygon or MultiPolygon, WGS84), whose geometry goes
into the payload as `zone` (Explore draws and reads within it).

  scenes.py catalogue.json out.ndjson [--batch DIR --prep bin/prep]
"""

import argparse
import json
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "lidar"))
import gps  # noqa: E402

COLLECTION = "SceneExplo2026v1"


def contour(path):
    """The geometry of a GeoJSON file (Feature, one-feature FeatureCollection or geometry)."""
    d = json.loads(Path(path).read_text())
    if d.get("type") == "FeatureCollection":
        if len(d["features"]) != 1:
            raise ValueError(f"{path}: one feature expected")
        d = d["features"][0]
    if d.get("type") == "Feature":
        d = d["geometry"]
    if d.get("type") not in ("Polygon", "MultiPolygon"):
        raise ValueError(f"{path}: Polygon or MultiPolygon expected")
    return {"type": d["type"], "coordinates": d["coordinates"]}


def load(path):
    """The catalogue, each `boundary` read into `zone`."""
    path = Path(path)
    out = []
    for entry in json.loads(path.read_text()):
        if entry.get("boundary"):
            entry = {**entry, "zone": contour(path.parent / entry["boundary"])}
        if not entry.get("zone") and not entry.get("radius"):
            raise ValueError(f"scene {entry.get('id')}: neither a radius nor a boundary")
        out.append(entry)
    return out


def records(catalogue):
    out = []
    for entry in catalogue:
        lng, lat = entry["origin"]
        out.append({
            "Collection": COLLECTION,
            "Location": gps.encode(lat, lng, gps.MAX_DEPTH, COLLECTION),
            "ID": entry["id"],
            "Metrics": [1],
            "Payload": entry,
        })
    ids = [r["ID"] for r in out]
    if len(set(ids)) != len(ids):
        raise ValueError("two scenes share an id")
    return out


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    parser.add_argument("catalogue", type=Path)
    parser.add_argument("out", type=Path)
    parser.add_argument("--batch", type=Path, help="also build the batch in this directory")
    parser.add_argument("--prep", default="prep")
    args = parser.parse_args()
    lines = records(load(args.catalogue))
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text("".join(json.dumps(r, ensure_ascii=False, separators=(",", ":")) + "\n" for r in lines))
    print(f"{COLLECTION}: {len(lines)} scenes", flush=True)
    if args.batch:
        subprocess.run([args.prep, "ndjson", "-out", str(args.batch), str(args.out)], check=True)


if __name__ == "__main__":
    main()
