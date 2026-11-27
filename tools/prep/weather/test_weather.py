"""Tests of the weather prep: locations equal the SDK's Space encoding, and the
zone sums of the collections equal every row of a pyramid built as the former
index-arome.py built it. python3 -m unittest discover -s tools/prep/weather"""

import base64
import datetime
import gzip
import json
import math
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import weather as w  # noqa: E402
import check  # noqa: E402

VECTORS = HERE.parent / "lidar" / "testdata" / "space-vectors.json"
START = 1738022400  # 2025-01-28 00:00 UTC
HOURS = 30
TILE = 256


def fake_export(path, seed=7):
    """A small run in the former content format: native 0.01° points with a
    temperature per hour, the pyramid of depths 6 to 16 (index-arome.py),
    an 8 x 8 atmosphere and wind grid, two city series."""
    rng = np.random.default_rng(seed)
    lat, lon = np.meshgrid(np.round(43.20 + np.arange(40) / 100, 2), np.round(-1.75 + np.arange(50) / 100, 2),
                           indexing="ij")
    land = rng.random(lat.shape) < 0.8
    lat, lon = lat[land], lon[land]
    area = 6371008.8**2 * np.deg2rad(0.01) * (np.sin(np.deg2rad(lat + 0.005)) - np.sin(np.deg2rad(lat - 0.005)))
    celsius = 6 + 3 * np.sin(np.arange(HOURS) / 4)[None, :] + rng.normal(0, 2, (len(lat), HOURS))
    root = "data/indexus/arome/test-v1/"
    files, tiles = {}, {}
    for depth in range(6, 17):
        size = 2**depth
        x = np.ceil((lat + 90) * size / 180).astype(np.int64) - 1
        y = np.ceil((lon + 180) * size / 360).astype(np.int64) - 1
        cells, inverse = np.unique(x * size + y, return_inverse=True)
        cx, cy = np.divmod(cells, size)
        keys = (cx // TILE) * size + cy // TILE
        names = []
        for key in np.unique(keys):
            tx, ty = divmod(int(key), size)
            names.append(f"{tx}-{ty}")
            pick = keys == key
            for hour in range(HOURS):
                rows = np.zeros(int(pick.sum()), check.w.TILE_ROW)
                rows["x"], rows["y"] = cx[pick], cy[pick]
                rows["count"] = np.bincount(inverse, minlength=len(cells))[pick]
                rows["sum"] = np.bincount(inverse, weights=celsius[:, hour] * area, minlength=len(cells))[pick]
                rows["area"] = np.bincount(inverse, weights=area, minlength=len(cells))[pick]
                files[f"{root}{depth}/{names[-1]}/{hour}.bin"] = rows.tobytes()
        tiles[str(depth)] = names
    size = 8
    files[root + "atmosphere.f32"] = rng.random((HOURS, size * size, 4)).astype("<f4").tobytes()
    files[root + "wind.f32"] = rng.normal(0, 5, (HOURS, size * size, 2)).astype("<f4").tobytes()
    # the native point of a city: its series as the former labels kept it (0.01 °C)
    x16 = np.ceil((lat + 90) * 2**16 / 180).astype(np.int64) - 1
    y16 = np.ceil((lon + 180) * 2**16 / 360).astype(np.int64) - 1
    clat, clon = w.cell_centre(x16, y16, 16)
    cities = [{"name": n, "coordinates": [float(clon[i]) + 0.001, float(clat[i])], "minZoom": z,
               "temperatures": [round(float(t), 2) for t in celsius[i]]} for n, i, z in [("Anglet", 3, 0), ("Biarritz", 77, 7)]]
    files[root + "cities-v2.json"] = json.dumps(cities).encode()
    manifest = {"schema": "indexus-weather-tiles-v1", "version": "test-v1", "tileSize": TILE, "tiles": tiles,
                "meta": {"firstTimestamp": START, "hours": HOURS},
                "atmosphere": {"file": "atmosphere.f32", "size": size, "bounds": [-1.8, 43.1, -1.2, 43.7],
                               "wind": {"file": "wind.f32"}}}
    with open(path, "w") as out:
        out.write(json.dumps({"Collection": "AromeWinter00001", "Location": "@", "ID": "manifest",
                              "Payload": manifest}) + "\n")
        for name, data in files.items():
            payload = {"path": name, "encoding": "gzip-base64", "part": 0, "parts": 1,
                       "data": base64.b64encode(gzip.compress(data)).decode()}
            out.write(json.dumps({"Collection": "LocalAssetData01", "ID": "part00000", "Payload": payload}) + "\n")
    return lat, lon, area, celsius


class Locations(unittest.TestCase):
    def test_locations_equal_the_sdk(self):
        vectors = [v for v in json.loads(VECTORS.read_text())["points"] if len(v["point"]) == 2]
        self.assertTrue(vectors)
        for v in vectors:
            got = w.locations(np.array([v["point"][0]]), np.array([v["point"][1]]), v["collection"], v["depth"])
            self.assertEqual(got[0], v["location"], v)

    def test_decode_is_the_centre_of_the_zone(self):
        lat, lng = np.array([43.484986, -12.5, 51.0]), np.array([-1.520707, 100.25, 2.0])
        for i, loc in enumerate(w.locations(lat, lng, "TempeAROME0128v1")):
            a, b = w.decode(loc, "TempeAROME0128v1")
            self.assertLess(abs(a - lat[i]), 180 / 2**48)
            self.assertLess(abs(b - lng[i]), 360 / 2**48)
            self.assertEqual(w.locations(np.array([a]), np.array([b]), "TempeAROME0128v1")[0], loc)

    def test_names_have_sixteen_characters_of_the_alphabet(self):
        for kind in ["Tempe", "Rainf", "Cloud", "Solar", "WindU", "WindV", "Towns"]:
            n = w.name(kind, datetime.date(2025, 1, 28))
            self.assertEqual(len(n), 16)
            self.assertTrue(all(c in w.gps.ALPHABET for c in n), n)


class Width(unittest.TestCase):
    def test_a_run_of_72_hours_is_one_row_a_node_keeps(self):
        self.assertLessEqual(1 + 72, w.MAX_METRICS)
        self.assertEqual(w.name("Tempe", datetime.date(2025, 1, 28)), "TempeAROME0128v2")


class Depth(unittest.TestCase):
    def test_the_records_depth_is_read_from_their_ids(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "weather-temperature.ndjson"
            path.write_text(json.dumps({"Collection": "AromeTemperature", "ID": "13-5978-4304"}) + "\n")
            self.assertEqual(w.records_depth(tmp), 13)

    def test_native_records_are_one_point_each(self):
        holder = tempfile.TemporaryDirectory()
        self.addCleanup(holder.cleanup)
        export = Path(holder.name) / "content.ndjson"
        lat, _, area, _ = fake_export(export)
        _, fields = w.from_content(export, w.NATIVE_DEPTH)
        _, _, ids, weights, metrics, _ = fields["Tempe"]
        self.assertEqual(len(ids), len(lat))
        self.assertTrue(all(i.startswith("16-") for i in ids))
        self.assertTrue((weights == 1).all())
        self.assertAlmostEqual(metrics[:, 0].sum(), area.sum() / 1e6, delta=1e-9 * area.sum())


class Pyramid(unittest.TestCase):
    def run_depth(self, depth):
        holder = tempfile.TemporaryDirectory()
        self.addCleanup(holder.cleanup)
        tmp = holder.name
        export = Path(tmp) / "content.ndjson"
        lat, lon, area, celsius = fake_export(export)
        start, fields = w.from_content(export, depth)
        self.assertEqual(start, datetime.datetime(2025, 1, 28, tzinfo=datetime.timezone.utc))
        out = Path(tmp) / "out"
        written = w.write_collections(fields, start, out)
        descriptor = w.describe(fields, start, depth)
        self.assertEqual(sorted(descriptor["collections"]), ["Cloud", "Rainf", "Solar", "Tempe", "Towns", "WindU", "WindV"])
        self.assertEqual(len(written), 7)

        def lines(collection):
            with open(out / f"{collection}.ndjson") as f:
                return [json.loads(line) for line in f]

        rlat, rlng, rarea, values = check.temperature_series(descriptor, lines)
        self.assertEqual(values.shape[1], HOURS)
        # weights are native points, metric 0 their area
        temp = lines(descriptor["collections"]["Tempe"])
        self.assertEqual(sum(l.get("Weight", 1) for l in temp), len(lat))
        self.assertAlmostEqual(sum(l["Metrics"][0] for l in temp), area.sum() / 1e6, delta=1e-9 * area.sum())
        self.atmosphere = check.atmosphere_check(descriptor, lines, export)
        levels = check.pyramid_from_content(export, depth)
        return check.compare(rlat, rlng, rarea, values, levels, depth), fields, lines, descriptor

    def test_sums_equal_every_level_at_the_native_depth(self):
        result, _, _, _ = self.run_depth(16)
        self.assertEqual(sorted(result), list(range(6, 17)))
        for depth, r in result.items():
            self.assertTrue(r["compared"], depth)
            self.assertEqual(r["missing"], 0, depth)
            self.assertLess(r["worstCelsius"], 1e-4, depth)  # the pyramid is float32
            self.assertLess(r["worstAreaRelative"], 1e-6, depth)

    def test_atmosphere_equals_the_grids_at_every_cell_and_zone_depth(self):
        self.run_depth(13)
        self.assertEqual(sorted(self.atmosphere), ["Cloud", "Rainf", "Solar", "WindU", "WindV"])
        for kind, worst in self.atmosphere.items():
            self.assertEqual(sorted(k for k in worst if k != "cells"), list(range(1, 17)), kind)
            self.assertLess(max(worst.values()), 1e-12, kind)

    def test_sums_equal_the_levels_above_the_records(self):
        result, fields, lines, descriptor = self.run_depth(13)
        for depth, r in result.items():
            self.assertEqual(r["compared"], depth <= 13, depth)
            if r["compared"]:
                self.assertEqual(r["missing"], 0, depth)
                self.assertLess(r["worstCelsius"], 1e-4, depth)
        # atmosphere: a cell's mean is the grid value of the hour
        rain = lines(descriptor["collections"]["Rainf"])
        cell = rain[5]
        lat, lng, ids, weights, metrics, _ = fields["Rainf"]
        i = ids.index(cell["ID"])
        self.assertEqual(cell["Metrics"][0], metrics[i, 0])
        self.assertAlmostEqual(cell["Metrics"][27] / cell["Metrics"][0], metrics[i, 27] / metrics[i, 0])
        # towns: the label's series, at the city, with its payload
        town = lines(descriptor["collections"]["Towns"])[1]
        self.assertEqual(town["Payload"], {"name": "Biarritz", "minZoom": 7})


if __name__ == "__main__":
    unittest.main()
