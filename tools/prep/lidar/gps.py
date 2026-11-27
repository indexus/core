"""The shared `gps` Space (latitude, longitude; 3 bits each per character) and
the scene frame, the parts the prep builders need. Twin of sdk `Space` and of
experiences/scripts/world/{space,frame}.py; checked against the SDK's vectors
(testdata/space-vectors.json) in test_vegetation.py."""

import math

import numpy as np

ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
INDEX = {c: i for i, c in enumerate(ALPHABET)}
MAX_DEPTH = 16
EARTH = 6378137.0
METRES_PER_DEGREE = math.pi * EARTH / 180


def offsets(collection, levels=MAX_DEPTH):
    return [INDEX[collection[l % len(collection)]] for l in range(levels)]


def _bisect_index(val, lo0, hi0, q):
    """Repeated midpoint bisection matching JS Space.encode (strict '>' goes upper)."""
    arr = np.asarray(val, np.float64)
    lo = np.full(arr.shape, lo0, dtype=np.float64)
    hi = np.full(arr.shape, hi0, dtype=np.float64)
    idx = np.zeros(arr.shape, dtype=np.int64)
    for _ in range(q):
        mid = (lo + hi) * 0.5
        upper = arr > mid
        idx = (idx << 1) | upper.astype(np.int64)
        lo = np.where(upper, mid, lo)
        hi = np.where(upper, hi, mid)
    return idx if arr.ndim > 0 else np.int64(idx.item())


def cell_index(lat, lng, q):
    """(row from the south, column from the west) at quad level q; on a boundary the SDK keeps the cell below."""
    return _bisect_index(lat, -90.0, 90.0, q), _bisect_index(lng, -180.0, 180.0, q)


def location_of_cell(row, col, depth, collection):
    off = offsets(collection, depth)
    out = []
    for l in range(depth):
        shift = 3 * (depth - 1 - l)
        r, c = (int(row) >> shift) & 7, (int(col) >> shift) & 7
        idx = 0
        for b in (2, 1, 0):
            idx = idx * 4 + ((r >> b) & 1) * 2 + ((c >> b) & 1)
        out.append(ALPHABET[(off[l] + idx) % 64])
    return "".join(out)


def encode(lat, lng, depth, collection):
    row, col = cell_index(lat, lng, 3 * depth)
    return location_of_cell(row, col, depth, collection)


def bounds(row, col, q):
    dlat, dlng = 180 / 2.0**q, 360 / 2.0**q
    return -90 + row * dlat, -90 + (row + 1) * dlat, -180 + col * dlng, -180 + (col + 1) * dlng


def cell_metres(q, lat):
    return 180 / 2.0**q * METRES_PER_DEGREE, 360 / 2.0**q * METRES_PER_DEGREE * math.cos(math.radians(lat))


def tile_anchor(t, row, col, collection):
    """16-character location of the centre of tile (t, row, col)."""
    q = 3 * MAX_DEPTH
    shift = q - t - 1
    r = ((2 * int(row) + 1) << shift) - 1
    c = ((2 * int(col) + 1) << shift) - 1
    return location_of_cell(r, c, MAX_DEPTH, collection)


class Scene:
    """Scene metres (east, north from the origin, as the builders write them) <-> degrees."""

    def __init__(self, origin, shift=(0.0, 0.0)):
        # shift: a pavé of a commune works in the scene frame less shift (core tools/prep/commune)
        self.lon0, self.lat0 = float(origin[0]), float(origin[1])
        self.shift = (float(shift[0]), float(shift[1]))
        self.cos0 = math.cos(math.radians(self.lat0))
        self.ox = EARTH * math.radians(self.lon0)
        self.oy = EARTH * math.log(math.tan(math.pi / 4 + math.radians(self.lat0) / 2))

    def to_degrees(self, x, y):
        x, y = np.asarray(x, np.float64) + self.shift[0], np.asarray(y, np.float64) + self.shift[1]
        lng = np.degrees((x / self.cos0 + self.ox) / EARTH)
        lat = np.degrees(np.arctan(np.sinh((y / self.cos0 + self.oy) / EARTH)))
        return lat, lng

    def to_metres(self, lat, lng):
        mx = EARTH * np.radians(np.asarray(lng, np.float64))
        my = EARTH * np.log(np.tan(np.pi / 4 + np.radians(np.asarray(lat, np.float64)) / 2))
        return (mx - self.ox) * self.cos0 - self.shift[0], (my - self.oy) * self.cos0 - self.shift[1]
