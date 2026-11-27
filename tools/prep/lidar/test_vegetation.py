"""Tests of the vegetation prep: keys equal the SDK's Space encoding, sums equal
a brute-force reference. python3 -m unittest discover -s tools/prep/lidar"""

import json, os, struct, sys, tempfile, unittest
from pathlib import Path
import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))
import vegetation as v  # noqa: E402

ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
# sdk Space tables of AngVegetation001 (written by the SDK's Space; see gps.py)
MASK = [0, 1, 2] * 24
OFFSET = [10, 49, 42, 31, 40, 42, 40, 55, 36, 55, 44, 50]
# Space.encode(point, 6) of the SDK, from the same file
SDK = [([0.1, 0.1, 0.1], "2ngVeg"), ([12.3, -45.6, 7.8], "q3wpJp"), ([-999.9, 999.1, 30.1], "a3yC3w")]


def location_string(key, depth):
    return "".join(ALPHABET[(int(key) >> (58 - 6 * l)) & 63] for l in range(depth))


def sdk_bisect(p, depth):
    """Space._encode of sdk, literally: bisection with `coordinate > mid`."""
    src = [-1024.0, 1024.0] * 3
    out = ""
    for l in range(depth):
        idx = 0
        for n in range(6):
            a = MASK[l * 6 + n]
            mid = (src[2 * a] + src[2 * a + 1]) / 2
            if p[a] > mid:
                idx, src[2 * a] = idx * 2 + 1, mid
            else:
                idx, src[2 * a + 1] = idx * 2, mid
        out += ALPHABET[(OFFSET[l] + idx) % 64]
    return out


def pack(cells):
    return ((cells[:, 0] << 20) | (cells[:, 1] << 10) | cells[:, 2]).astype(np.uint32)


class Tables2D(unittest.TestCase):
    def test_2d_tables_and_locations_equal_the_sdk(self):
        # space-tables.json of the builders: AngTerrain000001 [0.1, 0.2] -> 'wngT' (depth 4, 0.5 m cells), offsets
        t = v.ixp.space_tables("AngTerrain000001", 2)
        self.assertEqual(t["offset"][:6], [10, 49, 42, 29, 40, 53])
        cell = np.floor((np.array([[0.1, 0.2]]) + 1024) / 0.5).astype(int)
        self.assertEqual(v.ixp.locations(t, cell, 4), ["wngT"])
        c = v.ixp.space_tables("AngVegCrowns0001", 2)
        self.assertEqual(c["offset"], [10, 49, 42, 31, 40, 42, 12, 53, 50, 58, 49, 54])
        self.assertEqual(v.ixp.locations(c, cell, 4), ["wngV"])


class Keys(unittest.TestCase):
    def test_tables_from_the_name_equal_the_sdk(self):
        t = v.ixp.space_tables("AngVegetation001", 3)
        self.assertEqual(t["offset"], OFFSET)
        self.assertEqual(t["mask"], MASK[:72])

    def test_sdk_vectors(self):
        for p, want in SDK:
            cells = np.floor((np.array([p]) + 1024) / v.VOXEL).astype(np.int64)
            key = v.location_keys(pack(cells), 10, MASK, OFFSET, 5)[0]
            self.assertEqual(location_string(key, 5), want[:5], p)

    def test_random_points_equal_the_sdk_bisection(self):
        rng = np.random.default_rng(3)
        pts = rng.uniform(-1023.9, 1023.9, (2000, 3))
        cells = np.floor((pts + 1024) / v.VOXEL).astype(np.int64)
        keys = v.location_keys(pack(cells), 10, MASK, OFFSET, 5)
        for p, k in zip(pts, keys):
            self.assertEqual(location_string(k, 5), sdk_bisect(p, 5))


class Payload(unittest.TestCase):
    def test_json_strings_equal_base64_per_record(self):
        import base64, sys
        sys.path.insert(0, str(Path(__file__).resolve().parent))
        import ixp
        rng = np.random.default_rng(1)
        sizes = rng.integers(1, 20, 50) * 6
        off = np.r_[0, np.cumsum(sizes)].astype(np.uint64)
        pay = rng.integers(0, 256, int(off[-1]), dtype=np.uint8)
        out, o2 = ixp.json_strings(pay, off)
        for i in range(50):
            want = b'"' + base64.b64encode(pay[int(off[i]):int(off[i + 1])].tobytes()) + b'"'
            self.assertEqual(out[int(o2[i]):int(o2[i + 1])].tobytes(), want)


class Sums(unittest.TestCase):
    def test_records_equal_brute_force(self):
        rng = np.random.default_rng(5)
        n = 20000
        xyz = np.stack([rng.uniform(-40, 40, n), rng.uniform(-40, 40, n), rng.uniform(-3, 30, n)], 1)
        xyz[:50] = xyz[0]  # a dense voxel
        rgb = rng.integers(0, 256, (n, 3), dtype=np.uint8)
        cell, d, pay = v.voxel_points(xyz, rgb)
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "c.ixpt")
            with open(path, "wb") as fh:
                fh.write(b"IXPT" + struct.pack("<IQIIQ", 1, n, 6, 10, 0))
                fh.write(cell.tobytes()), fh.write(np.ascontiguousarray(d.T).tobytes()), fh.write(pay.tobytes())
            v.PAYLOAD = "binary"
            man = v.records_numpy([path], {"mask": MASK, "offset": OFFSET}, os.path.join(tmp, "b"), {"tool": "test", "sources": {}}, lambda **k: None)
            got = read_batch(os.path.join(tmp, "b"), man)
        # brute force: group points by voxel with a dict, sum in float64
        cells = np.floor((xyz + 1024) / v.VOXEL).astype(np.int64)
        keys = v.location_keys(pack(cells), 10, MASK, OFFSET, 5)
        want = {}
        for i in range(n):
            x, y, z = xyz[i]
            r, g, b = rgb[i] / 255
            s = want.setdefault(int(keys[i]), [np.zeros(13), b""])
            s[0] += [1, x, y, z, x * x, x * y, x * z, y * y, y * z, z * z, r, g, b]
            s[1] += pay[i].tobytes()
        self.assertEqual(sorted(want), [k for k, _, _ in got])
        for k, m, p in got:
            np.testing.assert_allclose(m, want[k][0], rtol=1e-12, atol=1e-9)
            self.assertEqual(p, want[k][1])


def read_batch(dir, man):
    """A minimal reader of the parts ixp.py writes (one block per part here)."""
    out = []
    for part in man["parts"]:
        raw = open(os.path.join(dir, part["file"]), "rb").read()
        assert raw[:4] == b"IXP1"
        m = struct.unpack("<H", raw[6:8])[0]
        at = 16
        n, idb, pb = struct.unpack("<IIQ", raw[at:at + 16]); at += 16
        heads = np.frombuffer(raw, np.uint8, 13 * n, at).reshape(n, 13); at += 13 * n; at += -at % 8
        at += 4 * n; at += -at % 8
        pend = np.frombuffer(raw, "<u8", n, at); at += 8 * n
        at += 8 * n  # weights
        met = np.frombuffer(raw, "<f8", m * n, at).reshape(m, n).T; at += 8 * m * n
        at += idb; at += -at % 8
        pay = raw[at:at + pb]
        keys = heads[:, :8].copy().view(">u8").reshape(n)
        start = 0
        for i in range(n):
            out.append((int(keys[i]), met[i], pay[start:int(pend[i])]))
            start = int(pend[i])
    return out


if __name__ == "__main__":
    unittest.main()


class Gps(unittest.TestCase):
    def test_space_vectors_of_the_sdk(self):
        import gps
        d = json.loads((Path(__file__).resolve().parent / "testdata" / "space-vectors.json").read_text())
        for p in d["points"]:
            self.assertEqual(gps.encode(p["point"][0], p["point"][1], p["depth"], p["collection"]), p["location"], p)
        for a in d["anchors"]:
            self.assertEqual(gps.tile_anchor(a["t"], a["row"], a["col"], a["collection"]), a["location"], a)


class ReadTile(unittest.TestCase):
    def test_read_tile_reads_the_documented_layout(self):
        import returns
        rng = np.random.default_rng(3)
        k = [5, 0, 3, 2]  # ground, vegetation (empty), building, other
        cols = [dict(x=rng.random(n) * 32, y=rng.random(n) * 32, Z=rng.integers(-500, 9000, n).astype("<i4"),
                     rgb=rng.integers(0, 256, (n, 3)).astype("u1"), intensity=rng.integers(0, 65535, n).astype("<u2"),
                     bits=rng.integers(0, 255, n).astype("u1"), cls=np.full(n, (2, 4, 6, 1)[g], "u1")) for g, n in enumerate(k)]
        start = np.cumsum([0] + k).astype("<u4")
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "t.ixrt")
            with open(path, "wb") as fh:
                fh.write(b"IXRT" + struct.pack("<III4dd8x", 1, sum(k), 0, 0.0, 32.0, 32.0, 64.0, 12.5))
                fh.write(start.tobytes())
                for c in cols:
                    for name, dt in (("x", "<f8"), ("y", "<f8"), ("Z", "<i4"), ("rgb", "u1"), ("intensity", "<u2"), ("bits", "u1"), ("cls", "u1")):
                        fh.write(np.ascontiguousarray(c[name].astype(dt)).tobytes())
            for g, name in enumerate(("ground", "vegetation", "building", "other")):
                t = returns.read_tile(path, name)
                self.assertEqual(t["bounds"], (0.0, 32.0, 32.0, 64.0))
                self.assertEqual(t["z0"], 12.5)
                for field, key in (("x", "x"), ("y", "y"), ("Z", "Z"), ("rgb", "rgb"), ("intensity", "intensity"), ("bits", "bits"), ("class", "cls")):
                    np.testing.assert_array_equal(t[field], cols[g][key])
