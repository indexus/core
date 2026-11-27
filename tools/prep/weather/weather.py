#!/usr/bin/env python3
"""AROME weather as native collections of the `gps` Space, then IXP1 batches.

One collection per variable for the whole run; a record is one place, its
metrics are additive, so a zone holds the exact area-weighted mean of what is
below:

    [area km², v·area for each hour of the run]       (73 metrics for 72 hours)

The client reads hour h as metric 1 + h over metric 0, a column of the block
on the GPU, and asks the node for only the columns it draws (`/sets?columns=0,
h+1, h+2`). A node keeps up to 128 metrics in a row.

Names (16 characters, experiences/docs/collections.md 4.3):
type (5) + `AROME` + vintage `MMDD` (the run's first day, UTC) + `v2` (v1 were
the collections of one day each).

- TempeAROME<day>v2  2 m temperature in kelvin, one record per zone of
  `--depth` (default 16: every native 0.01° point alone in its zone of
  0.0027° x 0.0055°, as the former pyramid's finest level); weight = the
  native points it sums (1 at depth 16). The node keeps the sums of every
  zone above, so a far view reads coarse zones and a near one the native
  grid (about 645 000 records for France, 9.5 times depth 13).
- RainfAROME, CloudAROME, SolarAROME, WindUAROME, WindVAROME  rain (mm/h),
  cloud cover (%), shortwave radiation (W/m²), 10 m wind east and north
  (m/s): one record per cell of the 128 x 128 Mercator grid of the animated
  layer, over land and one cell around it; ID `m<size>-<row>-<column>`.
- TownsAROME  one record per city label, at the city, with the series of the
  native point the former labels showed; payload {name, minZoom}.

Inputs, one of:
  --content FILE  the export of the former content (`localdata -export` of
                  LocalAssetData01 with the AromeWinter00001 manifest record):
                  the `indexus-weather-tiles-v1` pyramid, its atmosphere and
                  wind grids and its city series;
  --records DIR   the run-wide NDJSON of the first weather lane
                  (weather-<variable>.ndjson, [area, v·area per hour]) and
                  --start, the day of hour 0.

Writes DIR/<collection>.ndjson and, with --batch, one batch per collection
(`prep ndjson -out BATCH`, docs/ingest/pipeline.md). --check FILE compares
the zone sums with the pyramid's rows (tools/prep/weather/check.py).

  weather.py --content weather-ortho.ndjson --out work/weather --batch batches --prep bin/prep
"""

import argparse
import base64
import datetime
import gzip
import json
import math
import subprocess
import sys
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "lidar"))
import gps  # noqa: E402

RADIUS_KM = 6371.0088
ZERO_CELSIUS = 273.15
NATIVE_DEPTH = 16
NATIVE_DEGREES = 0.01
TILE_ROW = np.dtype([("x", "<u2"), ("y", "<u2"), ("count", "<u4"), ("sum", "<f4"), ("area", "<f4")])
VERSION = "v2"
MAX_METRICS = 128
# variable: (former collection, grid file, channel)
ATMOSPHERE = {
    "Rainf": ("AromeRainfall001", "atmosphere", 0),
    "Cloud": ("AromeCloudCover1", "atmosphere", 1),
    "Solar": ("AromeShortwave01", "atmosphere", 3),
    "WindU": ("AromeWindEast001", "wind", 0),
    "WindV": ("AromeWindNorth01", "wind", 1),
}
TEMPERATURE = ("Tempe", "AromeTemperature")
TOWNS = ("Towns", "AromeCityLabels1")
FORMER_FILES = {
    "Tempe": "weather-temperature",
    "Rainf": "weather-rainfall",
    "Cloud": "weather-cloudcover",
    "Solar": "weather-shortwave",
    "WindU": "weather-windeast",
    "WindV": "weather-windnorth",
    "Towns": "weather-cities",
}


def name(kind, day):
    """TempeAROME0128v2: type, source, the run's first day (MMDD, UTC), version."""
    return f"{kind}AROME{day:%m%d}{VERSION}"


def locations(lat, lng, collection, depth=gps.MAX_DEPTH):
    """gps.encode of many points at once (the SDK's Space.encode)."""
    row, col = gps.cell_index(lat, lng, 3 * depth)
    off = gps.offsets(collection, depth)
    digits = np.zeros((len(row), depth), np.int64)
    for level in range(depth):
        shift = 3 * (depth - 1 - level)
        r, c = (row >> shift) & 7, (col >> shift) & 7
        idx = np.zeros(len(row), np.int64)
        for b in (2, 1, 0):
            idx = idx * 4 + ((r >> b) & 1) * 2 + ((c >> b) & 1)
        digits[:, level] = (off[level] + idx) % 64
    table = np.array(list(gps.ALPHABET))
    return ["".join(chars) for chars in table[digits]]


def cell_centre(x, y, depth):
    size = 2.0**depth
    return -90 + (x + 0.5) * 180 / size, -180 + (y + 0.5) * 360 / size


def native_area_km2(lat):
    """Spherical area of a native 0.01° cell centred at `lat` (index-arome.py)."""
    half = math.radians(NATIVE_DEGREES / 2)
    lat = np.radians(lat)
    return RADIUS_KM**2 * math.radians(NATIVE_DEGREES) * (np.sin(lat + half) - np.sin(lat - half))


# --- the former content -------------------------------------------------------


def lines_of(paths):
    for path in [paths] if isinstance(paths, (str, Path)) else paths:
        with open(path, encoding="utf-8") as source:
            yield from source


def read_content(paths, wanted):
    """The manifest record and the bytes of the wanted content paths, parts joined in order."""
    parts, manifest = {}, None
    for line in lines_of(paths):
        record = json.loads(line)
        payload = record.get("Payload")
        if record.get("ID") == "manifest" or (
            isinstance(payload, dict) and payload.get("schema") == "indexus-weather-tiles-v1"
        ):
            manifest = payload
            continue
        if not isinstance(payload, dict) or not wanted(payload.get("path", "")):
            continue
        if payload.get("encoding") != "gzip-base64":
            raise ValueError(f"{payload['path']}: encoding {payload.get('encoding')}")
        # A part is a slice of one gzip stream (or one gzip member): join, then inflate.
        chunk = base64.b64decode(payload["data"])
        parts.setdefault(payload["path"], {})[payload["part"]] = (chunk, payload["parts"])
    files = {}
    for path_, chunks in parts.items():
        total = next(iter(chunks.values()))[1]
        if len(chunks) != total:
            raise ValueError(f"{path_}: {len(chunks)} of {total} parts")
        files[path_] = gzip.decompress(b"".join(chunks[i][0] for i in range(total)))
    return manifest, files


def pyramid_rows(manifest, files, root, depth, hour):
    rows = [
        np.frombuffer(files[f"{root}{depth}/{tile}/{hour}.bin"], TILE_ROW)
        for tile in manifest["tiles"][str(depth)]
    ]
    rows = np.concatenate(rows) if rows else np.zeros(0, TILE_ROW)
    return rows[rows["area"] > 0]


def native_points(manifest, files, root):
    """Native points (finest level): x, y at depth 16, area km², kelvin per hour."""
    keys, areas, values = None, None, []
    for hour in range(manifest["meta"]["hours"]):
        rows = pyramid_rows(manifest, files, root, NATIVE_DEPTH, hour)
        key = rows["x"].astype(np.int64) << 16 | rows["y"]
        order = np.argsort(key)
        rows, key = rows[order], key[order]
        if keys is None:
            keys, areas = key, rows["area"].astype(np.float64) / 1e6
        elif not np.array_equal(keys, key):
            raise ValueError(f"hour {hour}: native points differ from hour 0")
        values.append(rows["sum"].astype(np.float64) / rows["area"] + ZERO_CELSIUS)
    return keys >> 16, keys & 0xFFFF, areas, np.stack(values, axis=1)


def temperature(x, y, area, kelvin, depth):
    """One record per zone of `depth`: weight (native points), [area, K·area per hour]."""
    shift = NATIVE_DEPTH - depth
    zone, inverse = np.unique((x >> shift) << 32 | (y >> shift), return_inverse=True)
    metrics = np.zeros((len(zone), 1 + kelvin.shape[1]))
    metrics[:, 0] = np.bincount(inverse, weights=area)
    for hour in range(kelvin.shape[1]):
        metrics[:, 1 + hour] = np.bincount(inverse, weights=kelvin[:, hour] * area)
    zx, zy = zone >> 32, zone & 0xFFFFFFFF
    lat, lng = cell_centre(zx, zy, depth)
    ids = [f"{depth}-{a}-{b}" for a, b in zip(zx, zy)]
    return lat, lng, ids, np.bincount(inverse).astype(np.float64), metrics


def mercator_grid(bounds, size):
    """Edges (lat of rows from the north, lng of columns) and km² of the animated layer's cells."""
    west, south, east, north = bounds
    top, bottom = (math.asinh(math.tan(math.radians(v))) for v in (north, south))
    rows = np.degrees(np.arctan(np.sinh(top + (bottom - top) * np.arange(size + 1) / size)))
    columns = west + (east - west) * np.arange(size + 1) / size
    band = np.sin(np.radians(rows[:-1])) - np.sin(np.radians(rows[1:]))
    area = RADIUS_KM**2 * np.radians((east - west) / size) * band
    return rows, columns, np.repeat(area, size)


def atmosphere(manifest, files, root, x, y):
    """Per variable: the Mercator grid cells over land (and one around it)."""
    meta = manifest["atmosphere"]
    size, hours = meta["size"], manifest["meta"]["hours"]
    rows, columns, area = mercator_grid(meta["bounds"], size)
    lat, lng = cell_centre(x, y, NATIVE_DEPTH)
    row = np.searchsorted(-rows, -lat) - 1
    column = np.searchsorted(columns, lng) - 1
    inside = (row >= 0) & (row < size) & (column >= 0) & (column < size)
    land = np.zeros((size, size), bool)
    land[row[inside], column[inside]] = True
    grown = land.copy()
    for dr in (-1, 0, 1):
        for dc in (-1, 0, 1):
            grown |= np.roll(np.roll(land, dr, 0), dc, 1)
    cells = np.flatnonzero(grown)
    r, c = np.divmod(cells, size)
    mercator = np.arcsinh(np.tan(np.radians(rows)))
    centre_lat = np.degrees(np.arctan(np.sinh((mercator[r] + mercator[r + 1]) / 2)))
    centre_lng = (columns[c] + columns[c + 1]) / 2
    grids = {
        "atmosphere": np.frombuffer(files[root + meta["file"]], "<f4").reshape(hours, size * size, 4),
        "wind": np.frombuffer(files[root + meta["wind"]["file"]], "<f4").reshape(hours, size * size, 2),
    }
    ids = [f"m{size}-{a}-{b}" for a, b in zip(r, c)]
    out = {}
    for kind, (_, file, channel) in ATMOSPHERE.items():
        values = grids[file][:, cells, channel].T.astype(np.float64)
        metrics = np.concatenate([area[cells, None], values * area[cells, None]], axis=1)
        out[kind] = (centre_lat, centre_lng, ids, np.ones(len(cells)), metrics, None)
    return out


def towns(files, root, x, y, area, kelvin):
    """City labels at the city, with the series of their former native point."""
    series = json.loads(files[root + "cities-v2.json"])
    lat, lng = cell_centre(x, y, NATIVE_DEPTH)
    out_lat, out_lng, ids, metrics, payloads = [], [], [], [], []
    for city in series:
        temperatures = np.array([np.nan if t is None else t for t in city["temperatures"]])
        near = np.argsort((lat - city["coordinates"][1]) ** 2 + (lng - city["coordinates"][0]) ** 2)[:64]
        celsius = kelvin - ZERO_CELSIUS
        match = [i for i in near if np.allclose(np.round(celsius[i], 2), temperatures, atol=0.006)]
        if not match:
            raise ValueError(f"{city['name']}: no native point has its series")
        i = match[0]
        metrics.append(np.concatenate([[area[i]], kelvin[i] * area[i]]))
        out_lng.append(city["coordinates"][0])
        out_lat.append(city["coordinates"][1])
        ids.append(city["name"])
        payloads.append({"name": city["name"], "minZoom": city.get("minZoom", 0)})
    n = len(ids)
    return np.array(out_lat), np.array(out_lng), ids, np.ones(n), np.array(metrics), payloads


def from_content(path, depth):
    """Fields of every variable from the former content: kind -> (lat, lng, ids, weights, metrics, payloads)."""
    manifest, _ = read_content(path, lambda p: False)
    if manifest is None or manifest.get("schema") != "indexus-weather-tiles-v1":
        raise ValueError("no indexus-weather-tiles-v1 manifest in the export")
    root = f"data/indexus/arome/{manifest['version']}/"
    keep = (f"{root}{NATIVE_DEPTH}/", root + "atmosphere.f32", root + "wind.f32", root + "cities-v2.json")
    _, files = read_content(path, lambda p: p.startswith(keep))
    x, y, area, kelvin = native_points(manifest, files, root)
    fields = {TEMPERATURE[0]: temperature(x, y, area, kelvin, depth) + (None,)}
    if "atmosphere" in manifest:
        fields.update(atmosphere(manifest, files, root, x, y))
    if root + "cities-v2.json" in files:
        fields[TOWNS[0]] = towns(files, root, x, y, area, kelvin)
    start = datetime.datetime.fromtimestamp(manifest["meta"]["firstTimestamp"], datetime.timezone.utc)
    return start, fields


# --- the first lane's records ---------------------------------------------------


def decode(location, collection):
    """Centre of the zone of a location (the inverse of `locations`), lat and lng."""
    off = gps.offsets(collection, len(location))
    row = col = 0
    for level, char in enumerate(location):
        idx = (gps.INDEX[char] - off[level]) % 64
        r = ((idx >> 5) & 1) << 2 | ((idx >> 3) & 1) << 1 | ((idx >> 1) & 1)
        c = ((idx >> 4) & 1) << 2 | ((idx >> 2) & 1) << 1 | (idx & 1)
        row, col = row << 3 | r, col << 3 | c
    return cell_centre(row, col, 3 * len(location))


def records_depth(directory):
    """The zone depth of the first lane's temperature records (their IDs: depth-x-y)."""
    path = Path(directory) / f"{FORMER_FILES[TEMPERATURE[0]]}.ndjson"
    with open(path, encoding="utf-8") as source:
        return int(json.loads(source.readline())["ID"].split("-")[0])


def from_records(directory, depth):
    """Fields from weather-<variable>.ndjson of the first lane (the same values, one collection a variable)."""
    fields = {}
    for kind, former in [TEMPERATURE, TOWNS, *[(k, v[0]) for k, v in ATMOSPHERE.items()]]:
        path = Path(directory) / f"{FORMER_FILES[kind]}.ndjson"
        if not path.exists():
            print(f"absent: {path}", flush=True)
            continue
        lat, lng, ids, metrics, payloads = [], [], [], [], []
        with open(path, encoding="utf-8") as source:
            for line in source:
                record = json.loads(line)
                if record["Collection"] != former:
                    raise ValueError(f"{path}: collection {record['Collection']}, want {former}")
                a, b = decode(record["Location"], former)
                lat.append(a)
                lng.append(b)
                ids.append(record["ID"])
                metrics.append(record["Metrics"])
                payloads.append(record.get("Payload"))
        lat, lng, metrics = np.array(lat), np.array(lng), np.array(metrics, np.float64)
        weights = np.ones(len(ids))
        if kind == TEMPERATURE[0]:
            if not all(i.startswith(f"{depth}-") for i in ids):
                raise ValueError(f"{path}: records are not zones of depth {depth}")
            # Native points of a zone: its area over the area of one native cell
            # there (they differ by 1e-4 within a zone of depth 13).
            weights = np.round(metrics[:, 0] / native_area_km2(lat))
        fields[kind] = (lat, lng, ids, weights, metrics, payloads if kind == TOWNS[0] else None)
    return fields


# --- output ---------------------------------------------------------------------


def write_collection(path, collection, location, ids, weights, metrics, payloads):
    with open(path, "w", encoding="utf-8") as target:
        for i in range(len(ids)):
            line = {"Collection": collection, "Location": location[i], "ID": ids[i]}
            if weights[i] != 1:
                line["Weight"] = float(weights[i])
            line["Metrics"] = [float(v) for v in metrics[i]]
            if payloads is not None:
                line["Payload"] = payloads[i]
            target.write(json.dumps(line, ensure_ascii=False, separators=(",", ":")) + "\n")


def write_collections(fields, start, out):
    """One NDJSON per variable; returns {collection: (path, records)}."""
    out.mkdir(parents=True, exist_ok=True)
    written = {}
    for kind, (lat, lng, ids, weights, metrics, payloads) in sorted(fields.items()):
        if metrics.shape[1] > MAX_METRICS:
            raise ValueError(f"{kind}: {metrics.shape[1]} metrics, a node keeps {MAX_METRICS}")
        collection = name(kind, start)
        path = out / f"{collection}.ndjson"
        write_collection(path, collection, locations(lat, lng, collection), ids, weights, metrics, payloads)
        written[collection] = (path, len(ids))
    return written


def describe(fields, start, depth):
    """The client's descriptor of the run: hours, the collection of each variable."""
    hours = next(iter(fields.values()))[4].shape[1] - 1
    return {
        "firstTimestamp": int(start.timestamp()),
        "depth": depth,
        "hours": hours,
        "collections": {kind: name(kind, start) for kind in sorted(fields)},
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--content", type=Path, nargs="+", help="export(s) of the former content")
    source.add_argument("--records", type=Path, help="directory of the first lane's weather-*.ndjson")
    parser.add_argument("--start", type=datetime.date.fromisoformat, help="day of hour 0 (with --records)")
    parser.add_argument("--out", type=Path, required=True, help="directory of the NDJSON per collection")
    parser.add_argument("--depth", type=int, default=None,
                        help="temperature zone depth, bits per axis (≤ 16; default 16, with --records the records' own)")
    parser.add_argument("--batch", type=Path, help="also build one batch per collection in this directory")
    parser.add_argument("--prep", default="prep", help="the prep tool (go build ./tools/prep)")
    args = parser.parse_args()
    if args.depth is not None and not 6 <= args.depth <= NATIVE_DEPTH:
        parser.error(f"--depth: 6 to {NATIVE_DEPTH}")
    if args.content:
        depth = args.depth or NATIVE_DEPTH
        start, fields = from_content(args.content, depth)
    else:
        if not args.start:
            parser.error("--records needs --start")
        start = datetime.datetime.combine(args.start, datetime.time(), datetime.timezone.utc)
        depth = args.depth or records_depth(args.records)
        fields = from_records(args.records, depth)
    written = write_collections(fields, start, args.out)
    (args.out / "weather.json").write_text(json.dumps(describe(fields, start, depth), indent=1) + "\n")
    for collection, (path, records) in written.items():
        print(f"{collection}: {records:,} records, {path.stat().st_size / 1e6:.1f} MB", flush=True)
    if args.batch:
        paths = [str(path) for path, _ in written.values()]
        subprocess.run([args.prep, "ndjson", "-out", str(args.batch), *paths], check=True)


if __name__ == "__main__":
    main()
