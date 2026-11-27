#!/usr/bin/env python3
"""Sums of the weather collections against the former pyramid, at every level.

Reads the records of the TempeAROME day collections (the NDJSON weather.py
wrote, or the batches through `prep cat`) and the pyramid of the former
content (`--content`, every row of every level and hour) or rows sampled from
it (`--samples`, a JSON list of {depth, x, y, hour, celsius, areaKm2}). For
each pyramid row at depth d (bits per axis) the records whose cell lies in it
are summed: area, and Σ °C·area = Σ K·area − 273.15·Σ area. A level deeper
than the records cannot be compared (the record is the mean of its zone) and
is reported as such.

  check.py --collections work/weather --content weather-ortho.ndjson
  check.py --batches batches --prep bin/prep --samples samples.json
"""

import argparse
import json
import subprocess
import sys
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))
import weather as w  # noqa: E402
import gps  # noqa: E402


def read_lines(args, collection):
    if args.collections:
        path = Path(args.collections) / f"{collection}.ndjson"
        with open(path, encoding="utf-8") as source:
            return [json.loads(line) for line in source]
    out = subprocess.run([args.prep, "cat", str(Path(args.batches) / collection)], check=True, capture_output=True)
    return [json.loads(line) for line in out.stdout.decode().splitlines()]


def temperature_series(descriptor, lines_of):
    """Records of the temperature: lat, lng, area km², K·km² per hour of the run."""
    collection = descriptor["collections"]["Tempe"]
    lat, lng, area, values = [], [], [], []
    for line in lines_of(collection):
        a, b = w.decode(line["Location"], collection)
        lat.append(a)
        lng.append(b)
        area.append(line["Metrics"][0])
        values.append(line["Metrics"][1:])
    values = np.array(values, np.float64)
    if values.shape[1] != descriptor["hours"]:
        raise ValueError(f"{collection}: {values.shape[1]} hours, the run has {descriptor['hours']}")
    return np.array(lat), np.array(lng), np.array(area), values


def compare(lat, lng, area, values, levels, record_depth):
    """levels: depth -> rows of (depth, x, y, hour, celsius, areaKm2), or a function giving them.
    → per depth: rows, rows without records, worst °C, worst area (relative)."""
    out = {}
    for depth in sorted(levels):
        if depth > record_depth:
            out[depth] = {"compared": False}
            continue
        at = levels[depth]() if callable(levels[depth]) else levels[depth]
        x, y = gps.cell_index(lat, lng, depth)
        key = x << 32 | y
        zone, inverse = np.unique(key, return_inverse=True)
        zone_area = np.bincount(inverse, weights=area)
        worst_c = worst_a = 0.0
        missing = 0
        for hour in sorted(set(int(h) for h in at[:, 3])):
            sel = at[at[:, 3] == hour]
            sums = np.bincount(inverse, weights=values[:, hour])
            want = sel[:, 1].astype(np.int64) << 32 | sel[:, 2].astype(np.int64)
            pos = np.searchsorted(zone, want)
            found = (pos < len(zone)) & (zone[np.minimum(pos, len(zone) - 1)] == want)
            missing += int((~found).sum())
            pos = pos[found]
            a = zone_area[pos]
            celsius = sums[pos] / a - w.ZERO_CELSIUS
            worst_c = max(worst_c, float(np.max(np.abs(celsius - sel[found, 4]), initial=0)))
            worst_a = max(worst_a, float(np.max(np.abs(a - sel[found, 5]) / sel[found, 5], initial=0)))
        out[depth] = {"rows": len(at), "compared": True, "missing": missing,
                      "worstCelsius": worst_c, "worstAreaRelative": worst_a}
    return out


def atmosphere_check(descriptor, lines_of, content):
    """Each atmosphere variable against the former grid (atmosphere.f32, wind.f32):
    every cell and hour, then the zone sums at every depth (1 to 16 characters),
    the grid's side placing each cell by its own geometry. → kind -> {depth: worst relative}."""
    manifest, _ = w.read_content(content, lambda p: False)
    root = f"data/indexus/arome/{manifest['version']}/"
    meta = manifest.get("atmosphere")
    if not meta:
        return {}
    _, files = w.read_content(content, lambda p: p in (root + meta["file"], root + meta["wind"]["file"]))
    size, hours = meta["size"], manifest["meta"]["hours"]
    rows, columns, area = w.mercator_grid(meta["bounds"], size)
    mercator = np.arcsinh(np.tan(np.radians(rows)))
    grids = {
        "atmosphere": np.frombuffer(files[root + meta["file"]], "<f4").reshape(hours, size * size, 4),
        "wind": np.frombuffer(files[root + meta["wind"]["file"]], "<f4").reshape(hours, size * size, 2),
    }
    out = {}
    for kind, (_, file, channel) in w.ATMOSPHERE.items():
        collection = descriptor["collections"].get(kind)
        if not collection:
            continue
        cells, sums, locs = {}, {}, {}
        worst_cell = 0.0
        for line in lines_of(collection):
            _, r, c = line["ID"].split("-")
            cell = int(r) * size + int(c)
            a = line["Metrics"][0]
            if abs(a - area[cell]) > 1e-9 * area[cell]:
                raise ValueError(f"{collection}/{line['ID']}: area {a}, grid {area[cell]}")
            for h, value in enumerate(line["Metrics"][1:]):
                want = float(grids[file][h, cell, channel])
                worst_cell = max(worst_cell, abs(value / a - want) / max(1.0, abs(want)))
                sums.setdefault(h, {})[cell] = value
            locs[cell] = line["Location"]
        # zones: records' sums by location prefix, against the grid's cells placed by their centres
        cell_ids = np.array(sorted(locs))
        r, c = np.divmod(cell_ids, size)
        lat = np.degrees(np.arctan(np.sinh((mercator[r] + mercator[r + 1]) / 2)))
        lng = (columns[c] + columns[c + 1]) / 2
        worst = {"cells": worst_cell}
        grid_locs = w.locations(lat, lng, collection)
        record_locs = locs
        for depth in range(1, 17):
            for hour in (0, hours // 2, hours - 1):
                want, got = {}, {}
                for i, cell in enumerate(cell_ids):
                    want[grid_locs[i][:depth]] = want.get(grid_locs[i][:depth], 0.0) + \
                        area[cell] * float(grids[file][hour, cell, channel])
                    key = record_locs[cell][:depth]
                    got[key] = got.get(key, 0.0) + sums[hour][cell]
                if want.keys() != got.keys():
                    raise ValueError(f"{kind} depth {depth}: zones differ")
                rel = max(abs(got[z] - want[z]) / max(1.0, abs(want[z])) for z in want)
                worst[depth] = max(worst.get(depth, 0.0), rel)
        out[kind] = worst
    return out


def pyramid_from_content(path, max_depth=16):
    """depth -> rows of every hour; levels deeper than max_depth are named, not read."""
    manifest, _ = w.read_content(path, lambda p: False)
    root = f"data/indexus/arome/{manifest['version']}/"
    depths = sorted(int(d) for d in manifest["tiles"])
    wanted = tuple(f"{root}{d}/" for d in depths if d <= max_depth)
    _, files = w.read_content(path, lambda p: p.startswith(wanted) and p.endswith(".bin"))

    def level(depth):
        out = []
        for hour in range(manifest["meta"]["hours"]):
            r = w.pyramid_rows(manifest, files, root, depth, hour)
            celsius = r["sum"].astype(np.float64) / r["area"]
            out.append(np.stack([np.full(len(r), depth), r["x"], r["y"], np.full(len(r), hour), celsius,
                                 r["area"].astype(np.float64) / 1e6], 1))
        return np.concatenate(out)

    return {d: (lambda d=d: level(d)) for d in depths}


def pyramid_from_samples(path):
    data = json.loads(Path(path).read_text())
    samples = data["pyramid"] if isinstance(data, dict) else data
    rows = np.array([[s["depth"], s["x"], s["y"], s["hour"], s["celsius"], s["areaKm2"]] for s in samples], np.float64)
    return {int(d): rows[rows[:, 0] == d] for d in np.unique(rows[:, 0])}


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--collections", type=Path, help="weather.py --out directory")
    source.add_argument("--batches", type=Path, help="batch directory (one sub-directory per collection)")
    parser.add_argument("--descriptor", type=Path, help="weather.json (default: beside the collections)")
    parser.add_argument("--prep", default="prep")
    pyramid = parser.add_mutually_exclusive_group(required=True)
    pyramid.add_argument("--content", type=Path, nargs="+")
    pyramid.add_argument("--samples", type=Path)
    parser.add_argument("--celsius", type=float, default=1e-3, help="tolerance of a mean, °C")
    parser.add_argument("--area", type=float, default=1e-5, help="relative tolerance of an area")
    parser.add_argument("--grid", type=float, default=1e-6, help="tolerance of an atmosphere value (relative, or absolute under 1)")
    args = parser.parse_args()
    descriptor_path = args.descriptor or Path(args.collections or args.batches) / "weather.json"
    descriptor = json.loads(Path(descriptor_path).read_text())
    lat, lng, area, values = temperature_series(descriptor, lambda c: read_lines(args, c))
    record_depth = descriptor.get("depth", 13)
    if args.content:
        levels = pyramid_from_content(args.content, record_depth)
    else:
        levels = pyramid_from_samples(args.samples)
    result = compare(lat, lng, area, values, levels, record_depth)
    ok = True
    print(f"{len(lat):,} records, {values.shape[1]} hours")
    for depth, r in result.items():
        if not r["compared"]:
            print(f"depth {depth:2}: below the records (depth {record_depth}): not compared")
            continue
        good = r["missing"] == 0 and r["worstCelsius"] <= args.celsius and r["worstAreaRelative"] <= args.area
        ok &= good
        print(f"depth {depth:2}: {r['rows']:,} rows, {r['missing']} without records, "
              f"mean within {r['worstCelsius']:.2g} °C, area within {r['worstAreaRelative']:.2g} relative"
              f"{'' if good else '  <-- past the tolerance'}")
    if args.content:
        lines = lambda c: read_lines(args, c)  # noqa: E731
        for kind, worst in atmosphere_check(descriptor, lines, args.content).items():
            levels = max(v for k, v in worst.items() if k != "cells")
            good = worst["cells"] <= args.grid and levels <= args.grid
            ok &= good
            print(f"{kind}: every cell and hour within {worst['cells']:.2g} of the grid, zone sums at depths "
                  f"1 to 16 within {levels:.2g} relative{'' if good else '  <-- past the tolerance'}")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
