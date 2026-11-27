"""Tests de la préparation d'une commune (sans réseau) :
python3 -m unittest discover -s tools/prep/commune

Le contour de test est celui d'Anglet (testdata/anglet-64024.geojson, geo.api.gouv.fr,
Etalab 2.0) ; un `prep` construit depuis tools/prep vérifie le lot de végétation quand
Go est là (sinon ce test est sauté)."""

import json
import math
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import numpy as np

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent / "lidar"))
import lambert  # noqa: E402
import plan  # noqa: E402
import terrain  # noqa: E402
import vegbatch  # noqa: E402
import zone as zones  # noqa: E402
import ixp  # noqa: E402
import vegetation as veg  # noqa: E402

ORIGIN = (-1.520707, 43.484986)
BOUNDARY = HERE / "testdata" / "anglet-64024.geojson"
ALPHABET = ixp.ALPHABET


def anglet():
    return zones.Polygon.from_geometry(zones.read_geometry(BOUNDARY), zones.Frame(ORIGIN))


class Zone(unittest.TestCase):
    def test_the_commune_in_scene_metres(self):
        z = anglet()
        self.assertAlmostEqual(z.area() / 1e6, 27.0, delta=0.1)  # geo.api.gouv.fr : 2 700,38 ha
        self.assertTrue(z.contains([0.0], [0.0])[0])  # les Quatre Cantons sont dans Anglet
        x0, y0, x1, y1 = z.bounds
        self.assertLess(x0, -2000), self.assertGreater(y1, 4800)
        self.assertGreater(z.radius, 4900)

    def test_mask_agrees_with_the_exact_test(self):
        z = anglet()
        m = zones.Mask(z, (z.bounds[0] - 100, z.bounds[1] - 100, z.bounds[2] + 100, z.bounds[3] + 100), cell=4.0, margin=48)
        rng = np.random.default_rng(2)
        p = rng.uniform(z.bounds[:2], z.bounds[2:], (50000, 2))
        np.testing.assert_array_equal(m.contains(p[:, 0], p[:, 1]), z.contains(p[:, 0], p[:, 1]))
        # near : tout ce qui est à moins de la marge (dilatation carrée : un peu plus)
        d = z.distance(p[:2000, 0], p[:2000, 1])
        near = m.near(p[:2000, 0], p[:2000, 1])
        self.assertTrue(np.all(near[d <= 48]))
        self.assertFalse(np.any(near[d > 48 * math.sqrt(2) + 4 * 2]))

    def test_rect_distance_and_inside(self):
        square = zones.Polygon([np.array([[0, 0], [100, 0], [100, 100], [0, 100.0]])])
        self.assertEqual(square.rect_distance(10, 10, 20, 20), 0.0)  # dedans
        self.assertEqual(square.rect_distance(-10, -10, 200, 200), 0.0)  # le contient
        self.assertAlmostEqual(square.rect_distance(110, 0, 120, 10), 10.0)
        self.assertAlmostEqual(square.rect_distance(103, 104, 120, 120), 5.0)
        self.assertTrue(zones.rect_inside(square, 10, 10, 20, 20, guard=5))
        self.assertFalse(zones.rect_inside(square, 10, 10, 20, 20, guard=15))

    def test_a_hole_is_outside(self):
        outer = np.array([[0, 0], [100, 0], [100, 100], [0, 100.0]])
        hole = np.array([[40, 40], [60, 40], [60, 60], [40, 60.0]])
        z = zones.Polygon([outer, hole])
        self.assertEqual(z.contains([10, 50], [10, 50]).tolist(), [True, False])
        self.assertAlmostEqual(z.area(), 100 * 100 - 20 * 20)
        g = {"type": "MultiPolygon", "coordinates": [[[[0, 0], [1, 0], [1, 1], [0, 0]]], [[[2, 2], [3, 2], [3, 3], [2, 2]]]]}
        self.assertEqual(len(zones.geometry_rings(g)), 2)

    def test_the_disc_is_the_former_scene(self):
        d = zones.scene_zone({"id": "a", "origin": ORIGIN, "radius": 1000})
        self.assertEqual(d.kind, "disc")
        self.assertEqual(d.bounds, (-1000, -1000, 1000, 1000))
        self.assertTrue(d.contains([999.0], [0.0])[0])
        self.assertFalse(d.contains([800.0], [800.0])[0])
        self.assertAlmostEqual(d.rect_distance(1100, -10, 1200, 10), 100)
        self.assertEqual(d.rect_distance(900, -10, 1200, 10), 0)
        m = zones.Mask(d, (-1100, -1100, 1100, 1100), cell=2.0)
        p = np.random.default_rng(1).uniform(-1100, 1100, (20000, 2))
        np.testing.assert_array_equal(m.contains(p[:, 0], p[:, 1]), d.contains(p[:, 0], p[:, 1]))
        p = zones.scene_zone({"id": "a", "origin": ORIGIN, "boundary": BOUNDARY.name}, root=BOUNDARY.parent)
        self.assertEqual(p.kind, "polygon")

    def test_frame_shift(self):
        f, g = zones.Frame(ORIGIN), zones.Frame(ORIGIN, (2048.0, -4096.0))
        lat, lng = f.to_degrees(np.array([3000.0]), np.array([-3500.0]))
        x, y = g.to_metres(lat, lng)
        np.testing.assert_allclose([x[0], y[0]], [3000 - 2048, -3500 + 4096], atol=1e-6)
        np.testing.assert_allclose(g.to_degrees(x, y), (lat, lng), atol=1e-12)


class Lambert(unittest.TestCase):
    def test_projection(self):
        x, y = lambert.to_l93(3.0, 46.5)  # l'origine de Lambert-93
        self.assertAlmostEqual(float(x), 700000, places=3)
        self.assertAlmostEqual(float(y), 6600000, places=2)
        lng, lat = lambert.from_l93(*lambert.to_l93(-1.520707, 43.484986))
        self.assertAlmostEqual(float(lng), -1.520707, places=9)
        self.assertAlmostEqual(float(lat), 43.484986, places=9)
        # le repère de la scène : l'origine est (0, 0) ; un pavé le décale
        x, y = lambert.to_l93(*ORIGIN)
        np.testing.assert_allclose(lambert.l93_to_local(x, y, ORIGIN), (0, 0), atol=1e-6)
        np.testing.assert_allclose(lambert.l93_to_local(x, y, ORIGIN, (2048, 0)), (-2048, 0), atol=1e-6)

    def test_the_dalles_of_anglet(self):
        x, y = lambert.to_l93(*ORIGIN)
        self.assertEqual((int(x // 1000), int(y // 1000) + 1), (334, 6276))  # LHD_FXX_0334_6276, la dalle du disque
        self.assertEqual(lambert.dalle_bounds(334, 6276), (334000.0, 6275000.0, 335000.0, 6276000.0))
        rings = zones.geometry_rings(zones.read_geometry(BOUNDARY))
        l93 = zones.Polygon([np.stack(lambert.to_l93(r[:, 0], r[:, 1]), 1) for r in rings])
        d = lambert.dalles_of_zone(l93)
        self.assertIn((334, 6276), d)
        self.assertTrue(30 <= len(d) <= 45, len(d))  # 27 km² d'une forme allongée
        # chaque point du contour est dans une dalle de la liste
        for r in rings:
            X, Y = lambert.to_l93(r[:, 0], r[:, 1])
            for a, b in zip(X, Y):
                self.assertIn((int(a // 1000), int(b // 1000) + 1), d)
        self.assertLessEqual(len(d), len(lambert.dalles_of_zone(l93, 200)))


class Plan(unittest.TestCase):
    def test_paves_quarters_and_boxes_cover_the_commune(self):
        z = anglet()
        paves = plan.paves(z)
        self.assertEqual(len(paves), 14)
        rng = np.random.default_rng(7)
        p = rng.uniform(z.bounds[:2], z.bounds[2:], (4000, 2))
        p = p[z.contains(p[:, 0], p[:, 1])]
        boxes = {pv: {q: plan.boxes(z, *pv, q) for q in plan.quarters(z, *pv)} for pv in paves}
        for x, y in p:
            i, j = int(math.floor((x + 1024) / 2048)), int(math.floor((y + 1024) / 2048))
            self.assertIn((i, j), boxes)
            lx, ly = x - 2048 * i, y - 2048 * j
            q = int(lx >= 0) + 2 * int(ly >= 0)
            self.assertIn(q, boxes[(i, j)])
            self.assertTrue(any(b[0] <= lx < b[2] and b[1] <= ly < b[3] for b in boxes[(i, j)][q]), (x, y))
        for pv in boxes.values():
            for q, bs in pv.items():
                qx0, qy0, qx1, qy1 = plan.quarter_square(q)
                for b in bs:  # dans le quart : sur la grille des tuiles de 32 m (une tuile, une boîte)
                    inner = (max(b[0], qx0), max(b[1], qy0), min(b[2], qx1), min(b[3], qy1))
                    if inner[0] < inner[2] and inner[1] < inner[3]:
                        self.assertTrue(all(abs(v / 32 - round(v / 32)) < 1e-9 for v in inner), b)
                        self.assertTrue(b[0] >= qx0 or b[2] <= qx0, b)

    def test_paves_go_in_key_order(self):
        z = anglet()
        prefix = lambda i, j: vegbatch.pave_prefix("VoxelLidHD2025v1", i, j)[0]  # noqa: E731
        order = plan.order(plan.paves(z), prefix)
        keys = [prefix(*p) for p in order]
        self.assertEqual(keys, sorted(keys))
        self.assertEqual(len(set(keys)), len(keys))


def sdk_bisect(p, depth, mins, side, mask, offset):
    """Space._encode du SDK, à la lettre, sur un cube de côté `side` depuis `mins`."""
    src = []
    for a in range(3):
        src += [mins[a], mins[a] + side]
    out = ""
    for l in range(depth):
        idx = 0
        for n in range(6):
            a = mask[l * 6 + n]
            mid = (src[2 * a] + src[2 * a + 1]) / 2
            if p[a] > mid:
                idx, src[2 * a] = idx * 2 + 1, mid
            else:
                idx, src[2 * a + 1] = idx * 2, mid
        out += ALPHABET[(offset[l] + idx) % 64]
    return out


def location_string(key, depth):
    return "".join(ALPHABET[(int(key) >> (58 - 6 * l)) & 63] for l in range(depth))


class Vegetation(unittest.TestCase):
    NAME = "VoxelLidHD2025v1"

    def test_pave_keys_are_the_sdk_keys_of_the_commune_space(self):
        t, inner = vegbatch.tables(self.NAME)
        rng = np.random.default_rng(4)
        for i, j in [(0, 0), (-1, 2), (1, -2)]:
            prefix, loc = vegbatch.pave_prefix(self.NAME, i, j)
            local = np.stack([rng.uniform(-1023.9, 1023.9, 300), rng.uniform(-1023.9, 1023.9, 300), rng.uniform(-20, 90, 300)], 1)
            cells = np.floor((local + 1024) / veg.VOXEL).astype(np.int64)
            packed = ((cells[:, 0] << 20) | (cells[:, 1] << 10) | cells[:, 2]).astype(np.uint32)
            keys = veg.location_keys(packed, 10, inner["mask"], inner["offset"], 5)
            keys = np.uint64(prefix) | (keys >> np.uint64(12))
            for p, k in zip(local, keys):
                g = [p[0] + 2048 * i, p[1] + 2048 * j, p[2]]
                self.assertEqual(location_string(k, 7), sdk_bisect(g, 7, vegbatch.SPACE["min"], vegbatch.SPACE["side"], t["mask"], t["offset"]))
        # sans niveau au-dessus, l'espace d'avant (AngVegetation001) : mêmes voxels
        self.assertEqual(vegbatch.SPACE["side"] / 4**vegbatch.SPACE["depth"], veg.VOXEL)

    def test_a_pave_of_quarters_is_the_commune_batch(self):
        """Des points de deux pavés, en quarts : le lot fusionné a les clés du SDK et les sommes d'un calcul direct."""
        rng = np.random.default_rng(9)
        _, inner = vegbatch.tables(self.NAME)
        t, _ = vegbatch.tables(self.NAME)
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            out = tmp / "batch.new"
            want = {}
            for index, (i, j) in enumerate([(0, -1), (1, 2)]):
                batches = []
                for q in (0, 3):
                    x0, y0, x1, y1 = plan.quarter_square(q)
                    n = 3000
                    local = np.stack([rng.uniform(x0 + 1, x0 + 60, n), rng.uniform(y0 + 1, y0 + 60, n), rng.uniform(-5, 40, n)], 1)
                    local[:40] = local[0]
                    rgb = rng.integers(0, 256, (n, 3), dtype=np.uint8)
                    cell, d, pay = veg.voxel_points(local, rgb)
                    path = tmp / f"c-{i}-{j}-{q}.ixpt"
                    import struct
                    with open(path, "wb") as fh:
                        fh.write(b"IXPT" + struct.pack("<IQIIQ", 1, n, 6, 10, 0))
                        fh.write(cell.tobytes()), fh.write(np.ascontiguousarray(d.T).tobytes()), fh.write(pay.tobytes())
                    veg.PAYLOAD = "json"
                    b = tmp / f"q-{i}-{j}-{q}"
                    veg.records_numpy([str(path)], inner, str(b), {"tool": "test", "sources": {}}, lambda **k: None)
                    batches.append(b)
                    g = local + [2048 * i, 2048 * j, 0]
                    for p, c in zip(g, rgb):
                        k = sdk_bisect(p, 7, vegbatch.SPACE["min"], vegbatch.SPACE["side"], t["mask"], t["offset"])
                        x, y, z = p
                        r, gg, bb = c / 255
                        s = want.setdefault(k, np.zeros(13))
                        s += [1, x, y, z, x * x, x * y, x * z, y * y, y * z, z * z, r, gg, bb]
                vegbatch.write_pave(out, index, batches, self.NAME, i, j, part_bytes=20000)
            man = vegbatch.finish(out, self.NAME, {"tool": "test", "sources": {}})
            self.assertEqual(man["records"], len(want))
            self.assertEqual(man["recordDepths"], [7, 7])
            got = {}
            for part in man["parts"]:
                for blk in vegbatch.read_part(out / part["file"]):
                    for k, m in zip(vegbatch.head_keys(blk["head"]), blk["metrics"]):
                        got[location_string(k, 7)] = m
                    self.assertTrue(np.all(blk["head"][:, 12] == 7))
            self.assertEqual(sorted(got), sorted(want))
            for k in want:
                np.testing.assert_allclose(got[k], want[k], rtol=1e-9, atol=1e-6)
            self.assertGreater(len(man["parts"]), 2)
            names = [p["file"] for p in man["parts"]]
            self.assertEqual(names, sorted(names))
            go = shutil.which("go")
            if go:
                env = dict(os.environ, GOTOOLCHAIN="local", GOPROXY="off", GOFLAGS="-mod=mod")
                r = subprocess.run([go, "run", "./tools/prep", "verify", str(out)], cwd=HERE.parents[2], capture_output=True, text=True, env=env)
                self.assertEqual(r.returncode, 0, r.stderr + r.stdout)

    def test_shifted_moments(self):
        rng = np.random.default_rng(1)
        p = rng.uniform(-10, 10, (50, 3))
        dx, dy = 2048.0, -4096.0

        def sums(q):
            x, y, z = q.T
            return np.array([[len(q), x.sum(), y.sum(), z.sum(), (x * x).sum(), (x * y).sum(), (x * z).sum(), (y * y).sum(), (y * z).sum(), (z * z).sum(), 1, 2, 3]])

        np.testing.assert_allclose(vegbatch.shift_moments(sums(p), dx, dy), sums(p + [dx, dy, 0]), rtol=1e-12)


class Terrain(unittest.TestCase):
    def test_fill_of_any_size_is_the_power_of_two_fill(self):
        rng = np.random.default_rng(3)
        z = rng.uniform(0, 10, (64, 64))
        w = (rng.random((64, 64)) > 0.3).astype(np.float64)
        np.testing.assert_allclose(terrain.fill(z, w), terrain.lg.fill(z, w))
        z2 = rng.uniform(0, 10, (37, 50))
        w2 = (rng.random((37, 50)) > 0.5).astype(np.float64)
        out = terrain.fill(z2, w2)
        self.assertEqual(out.shape, (37, 50))
        np.testing.assert_array_equal(out[w2 > 0], z2[w2 > 0])
        self.assertTrue(np.all(np.isfinite(out)))

    def test_a_plane_through_quarters_windows_and_levels(self):
        """Un sol plan mesuré sur deux pavés : les tuiles des niveaux 13 à 19 sont ce plan (+ l'altitude)."""
        a, b, c = 0.004, -0.002, 3.0
        altitude = 19.91
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            files = []
            for i, j in [(0, 0), (1, 0)]:
                for q in range(4):
                    qx, qy = q & 1, q >> 1
                    x0, y0 = 2048 * i - 1024 + 1024 * qx, 2048 * j - 1024 + 1024 * qy
                    xs = x0 + (np.arange(2048) + 0.5) * 0.5
                    ys = y0 + (np.arange(2048) + 0.5) * 0.5
                    h = a * xs[None, :] + b * ys[:, None] + c
                    count = np.full((2048, 2048), 2, np.uint16)
                    count[100:140, 300:420] = 0  # un trou (un bâtiment)
                    f = tmp / f"{i}_{j}_q{q}.npz"
                    terrain.save_quarter(f, i, j, q, (h * count).astype(np.float32), count)
                    files.append(f)
            qs = terrain.Quarters(files)
            z, m = qs.window(-1024 + 100, -1024 + 50, 64)
            np.testing.assert_allclose(z[m], (a * (-924 + (np.arange(64) + 0.5) * 0.5)[None, :] + b * (-974 + (np.arange(64) + 0.5) * 0.5)[:, None] + c)[m], atol=1e-3)
            kept = []
            out = tmp / "t.ndjson"
            with out.open("w") as fh:
                for i, j in [(0, 0), (1, 0)]:
                    n, k, heights = terrain.pave(qs, i, j, ORIGIN, altitude, fh, "TerraLidHD2025v2", margin=640.0)
                    self.assertGreater(n, 100)
                    kept.append(k)
                terrain.coarse_levels(kept, fh, "TerraLidHD2025v2")
            import gps
            scene = gps.Scene(ORIGIN)
            levels, ids = {}, set()
            for line in out.read_text().splitlines():
                r = json.loads(line)
                self.assertNotIn(r["ID"], ids)
                ids.add(r["ID"])
                t, row, col = (int(v) for v in r["ID"].split("/"))
                levels[t] = levels.get(t, 0) + 1
                p = r["Payload"]
                import base64
                import gzip
                q = np.frombuffer(gzip.decompress(base64.b64decode(p["data"])), "<u2").reshape(65, 65)
                hgt = np.cumsum(q, axis=1, dtype=np.uint16) * p["scale"] + p["offset"]
                s, nn, w, e = gps.bounds(row, col, t)
                # le milieu de la tuile, loin des bords des données
                lat, lng = (s + nn) / 2, (w + e) / 2
                x, y = scene.to_metres(lat, lng)
                if -900 < x < 2900 and -900 < y < 900:
                    self.assertAlmostEqual(float(hgt[32, 32]), a * float(x) + b * float(y) + c + altitude, delta=0.05 if t >= 17 else 0.3)
            self.assertEqual(sorted(levels), list(range(13, 20)))


class Scenes(unittest.TestCase):
    def test_the_scene_record_carries_its_contour(self):
        sys.path.insert(0, str(HERE.parent / "scenes"))
        import scenes
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            (tmp / "boundaries").mkdir()
            shutil.copy(BOUNDARY, tmp / "boundaries" / "anglet-64024.geojson")
            entry = {"id": "anglet", "label": "Anglet", "origin": list(ORIGIN), "boundary": "boundaries/anglet-64024.geojson"}
            (tmp / "catalogue.json").write_text(json.dumps([entry]))
            [r] = scenes.records(scenes.load(tmp / "catalogue.json"))
            self.assertEqual(r["Payload"]["zone"]["type"], "Polygon")
            self.assertEqual(len(r["Payload"]["zone"]["coordinates"][0]), 428)
            self.assertNotIn("radius", r["Payload"])


if __name__ == "__main__":
    unittest.main()


DTYPE = [("X", "<i4"), ("Y", "<i4"), ("Z", "<i4"), ("intensity", "<u2"), ("bit_fields", "u1"), ("classification_flags", "u1"),
         ("classification", "u1"), ("user_data", "u1"), ("scan_angle", "<i2"), ("point_source_id", "<u2"), ("gps_time", "<f8")]


class Pipeline(unittest.TestCase):
    """Un pavé de deux quarts, sans réseau : le relevé LiDAR est fabriqué (fetch.quarter remplacé) ;
    tout le reste est le code du Mac : retours préparés, végétation (numpy), sol, toits, lot."""

    def test_two_quarters_to_a_verified_batch(self):
        import gzip
        import hashlib
        import fetch
        import commune
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            (tmp / "boundaries").mkdir()
            ring = [[-1.5225, 43.4835], [-1.5170, 43.4835], [-1.5170, 43.4875], [-1.5225, 43.4875], [-1.5225, 43.4835]]
            (tmp / "boundaries" / "b.geojson").write_text(json.dumps({"type": "Feature", "properties": {}, "geometry": {"type": "Polygon", "coordinates": [ring]}}))
            entry = {"id": "t", "label": "T", "boundary": "boundaries/b.geojson", "origin": list(ORIGIN), "altitude": 19.91,
                     "orthoLevels": [12, 18], "vegetationSpace": {"min": [-17408, -17408, -1024], "side": 32768, "depth": 7},
                     "collections": {"ortho": "OrthoBDORT2024v2", "terrain": "TerraLidHD2025v2", "buildings": "BuildLidHD2025v3",
                                     "fine": "BuildLidHD2025f3", "vegetation": "VoxelLidHD2025v1"}}
            (tmp / "catalogue.json").write_text(json.dumps([entry]))
            data = tmp / "data"
            c = commune.Commune(tmp / "catalogue.json", "t", data)
            self.assertEqual(c.paves, [(0, 0)])
            self.assertEqual(sorted(c.quarters[(0, 0)]), [0, 1, 2, 3])
            (c.work / "points-imagery").mkdir(parents=True)
            (c.work / "points-imagery" / "manifest.json").write_text(json.dumps({"tiles": {"tileSize": 512, "files": []}}))
            (c.work / "context").mkdir()
            (c.work / "context" / "batiment.geojson").write_text('{"type":"FeatureCollection","features":[]}')
            c.dalles = []
            rng = np.random.default_rng(5)
            made = {}

            def fake_quarter(root, scene, dalles, zone, mask, i, j, q, cache, log=print):
                d = Path(root) / "data/world/anglet-1km"
                (d / "blocks").mkdir(parents=True, exist_ok=True)
                (d / "header.json").write_text(json.dumps({"dtype": DTYPE, "pointFormat": 6, "scales": [0.01] * 3, "offsets": [0.0] * 3}))
                x0, y0, x1, y1 = plan.quarter_square(q)
                n = 6000
                x, y = rng.uniform(x0, x0 + 200, n), rng.uniform(y0, y0 + 200, n)
                cls = rng.choice([2, 3, 4, 5, 6], n)
                z = np.where(cls == 2, 1.0, np.where(cls == 6, 9.0, rng.uniform(2, 15, n)))
                lat, lng = zones.Frame(ORIGIN, (2048.0 * i, 2048.0 * j)).to_degrees(x, y)
                X, Y = lambert.to_l93(lng, lat)
                p = np.zeros(n, DTYPE)
                p["X"], p["Y"], p["Z"] = np.round(X / 0.01), np.round(Y / 0.01), np.round((z + 19.91) / 0.01)
                p["classification"] = cls
                lx, ly = lambert.l93_to_local(p["X"] * 0.01, p["Y"] * 0.01, ORIGIN, (2048.0 * i, 2048.0 * j))
                keep = mask.contains(lx, ly)  # la végétation dans le contour, comme fetch.quarter
                p = p[keep | (cls == 2) | (cls == 6)]
                lx, ly = lambert.l93_to_local(p["X"] * 0.01, p["Y"] * 0.01, ORIGIN, (2048.0 * i, 2048.0 * j))
                tx, ty = plan.tile_of(lx, ly)
                files = []
                for k in np.unique(tx * 64 + ty):
                    a, b = divmod(int(k), 64)
                    chunk = p[(tx == a) & (ty == b)]
                    body = chunk.tobytes()
                    (d / "blocks" / f"{a}-{b}.lasrecords.gz").write_bytes(gzip.compress(body, mtime=0))
                    cl, ct = np.unique(chunk["classification"], return_counts=True)
                    files.append({"url": f"{a}-{b}.lasrecords.gz", "bounds": [a * 32 - 1024, b * 32 - 1024, (a + 1) * 32 - 1024, (b + 1) * 32 - 1024],
                                  "count": len(chunk), "sha256": hashlib.sha256(body).hexdigest(),
                                  "classificationCounts": dict(zip(map(str, cl.tolist()), map(int, ct)))})
                made[q] = p
                return sorted(files, key=lambda f: f["url"])

            original = fetch.quarter
            fetch.quarter = fake_quarter
            try:
                args = type("A", (), {"workers": 2, "backend": "numpy", "metal": None, "buildings": False, "python_buildings": None})()
                c.pave(0, 0, 0, None, args)
                # une reprise ne refait rien
                c.pave(0, 0, 0, None, args)
            finally:
                fetch.quarter = original
            for q in range(4):
                done = json.loads((c.quarter_root(0, 0, q) / "done.json").read_text())
                self.assertGreater(done["returns"], 0)
                self.assertFalse((c.quarter_root(0, 0, q) / "prep").exists())  # les retours préparés sont effacés
                self.assertFalse((c.quarter_root(0, 0, q) / "data/world/anglet-1km/blocks").exists())
                self.assertTrue((c.work / "ground" / f"0_0_q{q}.npz").exists())
            parts = sorted((c.pave_dir(0, 0) / "buildings" / "parts").glob("*-q*.npz"))
            roofs = sum(len(np.load(f)["roof"]) for f in parts)
            self.assertEqual(roofs, sum(int((p["classification"] == 6).sum()) for p in made.values()))
            out = tmp / "batches"
            commune.vegetation_batch(c, out)
            man = json.loads((out / "VoxelLidHD2025v1" / "manifest.json").read_text())
            n = 0
            veg = []
            for q, p in made.items():
                lx, ly = lambert.l93_to_local(p["X"] * 0.01, p["Y"] * 0.01, ORIGIN)
                m = (p["classification"] >= 3) & (p["classification"] <= 5)
                veg.append(np.stack([lx[m], ly[m], p["Z"][m] * 0.01 - 19.91], 1))
            veg = np.concatenate(veg)
            got = 0
            for part in man["parts"]:
                for blk in vegbatch.read_part(out / "VoxelLidHD2025v1" / part["file"]):
                    got += blk["metrics"][:, 0].sum()
                    n += len(blk["head"])
            self.assertEqual(int(got), len(veg))  # chaque retour de végétation, une fois
            self.assertEqual(n, man["records"])
            # le lot est le même à la reprise : sauté
            commune.vegetation_batch(c, out)
            go = shutil.which("go")
            if go:
                env = dict(os.environ, GOTOOLCHAIN="local", GOPROXY="off")
                r = subprocess.run([go, "run", "./tools/prep", "verify", str(out / "VoxelLidHD2025v1")], cwd=HERE.parents[2], capture_output=True, text=True, env=env)
                self.assertEqual(r.returncode, 0, r.stderr + r.stdout)
            # le sol de ce pavé : des tuiles de 13 à 19 à la hauteur du sol (1 m au-dessus du zéro de la scène)
            quarters = terrain.Quarters(sorted((c.work / "ground").glob("*.npz")))
            with (tmp / "t.ndjson").open("w") as fh:
                written, kept, heights = terrain.pave(quarters, 0, 0, ORIGIN, 19.91, fh, "TerraLidHD2025v2")
                terrain.coarse_levels([kept], fh, "TerraLidHD2025v2")
            self.assertGreater(written, 10)
            self.assertAlmostEqual(heights[0], 1.0, delta=0.02)
