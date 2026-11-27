"""The LiDAR ground from the prepared returns (returns.py), as two collections:

  AngTerrain000001  32 m tiles of 65 x 65 corner heights every 0.5 m, 256 m
                    tiles every 4 m (scene frame) - experiences/scripts/world/
                    buildings/{extract,terrain}.py, the same arithmetic.
  TerraLidHD2025v1  raster tiles of the gps Space, levels 13 to 19 - scripts/
                    world/ground.py, read from the AngTerrain000001 tiles as it
                    reads them.

Steps: per tile (8 processes), the class-2 height sum and count of each 0.5 m
cell (a bincount of 4 096 bins: memory-bound, so on the CPU, beside the read);
holes filled by push-pull; corners; tiles. Output: two NDJSON files in the
format of today's builders (for `prep ndjson`), and corners-q.npy and
corners-known.npy (the 0.5 m corner grid as the tiles hold it, for crowns.py).

Usage: python3 ground.py <experiences dir> <work dir> [--prep DIR]
Skipped when <work dir>/ground.key holds the key of the same inputs.
"""

import argparse, base64, gzip, json, math, os, sys, time
from multiprocessing import Pool
from pathlib import Path
import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))
import gps  # noqa: E402
import ixp  # noqa: E402
import returns  # noqa: E402

SIDE, MIN, CELL = 2048.0, -1024.0, 0.5
N = int(SIDE / CELL)
LEGACY, TERRA = "AngTerrain000001", "TerraLidHD2025v1"


def tile_sums(path):
    """extract.py for one tile: class-2 height sums (float32) and counts (uint16) of its 64 x 64 cells."""
    r = returns.read_tile(path, "ground")
    b = r["bounds"]
    x, y = r["x"], r["y"]
    z = r["Z"] * 0.01 - r["z0"]
    k = ((y - b[1]) / CELL).astype(int) * 64 + ((x - b[0]) / CELL).astype(int)
    row, col = int(round((b[1] - MIN) / 32)), int(round((b[0] - MIN) / 32))
    return row, col, np.bincount(k, z, 4096).astype(np.float32), np.bincount(k, None, 4096).astype(np.uint16)


def fill(z, w):
    """terrain.py: average down to one cell, then bring values back up. The same
    arithmetic, with the softening done on the hole cells only (less memory)."""
    if z.shape[0] == 1:
        return z
    zw = (z * w).reshape(z.shape[0] // 2, 2, -1, 2).sum((1, 3))
    w2 = w.reshape(z.shape[0] // 2, 2, -1, 2).sum((1, 3))
    coarse = fill(zw / np.maximum(w2, 1e-9), np.minimum(w2, 1.0))
    del zw, w2
    hj, hi = np.nonzero(w == 0)
    out = z.copy()
    out[hj, hi] = coarse[hj // 2, hi // 2]  # np.repeat(np.repeat(coarse, 2, 0), 2, 1) at the holes
    del coarse
    n = z.shape[0] - 1
    up, down = np.maximum(hj - 1, 0), np.minimum(hj + 1, n)  # edge padding
    left, right = np.maximum(hi - 1, 0), np.minimum(hi + 1, n)
    for _ in range(3):  # every hole from the previous values of its neighbours (np.where on a padded copy)
        out[hj, hi] = (out[up, hi] + out[down, hi] + out[hj, left] + out[hj, right]) / 4
    return out


def quantise(grid):
    return np.clip(np.round((grid + 100) * 100), 0, 65535).astype(np.uint16)


def legacy_payload(grid, tile, step, **more):
    q = quantise(grid)
    q[:, 1:] -= q[:, :-1].copy()
    body = base64.b64encode(gzip.compress(q.astype("<u2").tobytes(), 6, mtime=0)).decode()
    return {"format": "indexus-terrain-tile-v1", "size": 65, "bounds": tile, "step": step,
            "scale": 0.01, "offset": -100, "delta": "x", "data": body, **more}


def legacy(dtm, measured, tables):
    """terrain.py steps 3 and 4: corner heights, fine and coarse tiles, records."""
    p = np.pad(dtm, 1, mode="edge")
    fine = (p[:-1, :-1] + p[1:, :-1] + p[:-1, 1:] + p[1:, 1:]) / 4
    integral = np.zeros((N + 9, N + 9))
    integral[1:, 1:] = np.pad(dtm, 4, mode="edge").astype(np.float64).cumsum(0).cumsum(1)
    c = np.arange(0, N + 1, 8)
    box = lambda a, b: integral[np.ix_(a, b)]
    coarse = (box(c + 8, c + 8) - box(c, c + 8) - box(c + 8, c) + box(c, c)) / 64
    records, exists = [], np.zeros((64, 64), bool)
    corners = np.zeros((N + 1, N + 1), np.uint16)  # the quantised corners, as ground.py and build_crowns.py read the tiles back
    known = np.zeros((N + 1, N + 1), bool)
    for j in range(64):
        for i in range(64):
            cells = dtm[j * 64:j * 64 + 64, i * 64:i * 64 + 64]
            if not measured[j * 64:j * 64 + 64, i * 64:i * 64 + 64].any():
                continue
            exists[j, i] = True
            x0, y0 = MIN + i * 32, MIN + j * 32
            z = cells.astype(np.float64)
            g = fine[j * 64:j * 64 + 65, i * 64:i * 64 + 65]
            records.append({"Collection": LEGACY, "Location": ixp.locations(tables, [[i, j]], 2)[0], "ID": "tile",
                            "Metrics": [float(z.size), float(z.sum()), float((z * z).sum())],
                            "Payload": legacy_payload(g, [x0, y0, x0 + 32, y0 + 32], CELL)})
            corners[j * 64:j * 64 + 65, i * 64:i * 64 + 65] = quantise(g)
            known[j * 64:j * 64 + 65, i * 64:i * 64 + 65] = True
    u = np.arange(65) / 8
    for j in range(8):
        for i in range(8):
            grid = coarse[j * 64:j * 64 + 65, i * 64:i * 64 + 65]
            x0, y0 = MIN + i * 256, MIN + j * 256
            error = 0.0
            for b in range(8):
                for a in range(8):
                    if not exists[j * 8 + b, i * 8 + a]:
                        continue
                    g = grid[b * 8:b * 8 + 9, a * 8:a * 8 + 9]
                    k, f = np.minimum(u.astype(int), 7), u - np.minimum(u.astype(int), 7)
                    rows = g[k] * (1 - f[:, None]) + g[k + 1] * f[:, None]
                    interp = rows[:, k] * (1 - f) + rows[:, k + 1] * f
                    J, I = (j * 8 + b) * 64, (i * 8 + a) * 64
                    error = max(error, float(np.abs(interp - fine[J:J + 65, I:I + 65]).max()))
            centre = [(i * 256 + 128) // 4, (j * 256 + 128) // 4]
            records.append({"Collection": LEGACY, "Location": ixp.locations(tables, [centre], 3)[0], "ID": "coarse",
                            "Metrics": [0.0, 0.0, 0.0],
                            "Payload": legacy_payload(grid, [x0, y0, x0 + 256, y0 + 256], 4.0, error=round(error, 2),
                                                      children="".join("1" if e else "0" for e in exists[j * 8:j * 8 + 8, i * 8:i * 8 + 8].ravel()))})
    return records, corners, known


# --- TerraLidHD2025v1: scripts/world/ground.py on the corners of the fine tiles ---
CELLS = 64


def lattice(grid, known, x0, step, scene, level, rows, cols, block=256):
    """ground.py's lattice, a block of rows at a time (the same values, less memory)."""
    z, ok = np.zeros((rows[1] - rows[0], cols[1] - cols[0])), np.zeros((rows[1] - rows[0], cols[1] - cols[0]), bool)
    for a in range(rows[0], rows[1], block):
        b = min(a + block, rows[1])
        z[a - rows[0]:b - rows[0]], ok[a - rows[0]:b - rows[0]] = lattice_rows(grid, known, x0, step, scene, level, (a, b), cols)
    return z, ok


def lattice_rows(grid, known, x0, step, scene, level, rows, cols):
    lat = -90 + np.arange(rows[0], rows[1]) * 180 / 2.0**level
    lng = -180 + np.arange(cols[0], cols[1]) * 360 / 2.0**level
    _, y = scene.to_metres(lat, np.full(len(lat), scene.lon0))
    x, _ = scene.to_metres(np.full(len(lng), scene.lat0), lng)
    u, v = (x - x0) / step, (y - x0) / step
    n = grid.shape[0] - 1
    inside = ((v >= 0) & (v <= n))[:, None] & ((u >= 0) & (u <= n))[None, :]
    i, j = np.clip(np.floor(u).astype(int), 0, n - 1), np.clip(np.floor(v).astype(int), 0, n - 1)
    a, b = np.clip(u - i, 0, 1)[None, :], np.clip(v - j, 0, 1)[:, None]
    pick = lambda g, dj, di: g[np.ix_(j + dj, i + di)]
    z = (pick(grid, 0, 0) * (1 - a) + pick(grid, 0, 1) * a) * (1 - b) + (pick(grid, 1, 0) * (1 - a) + pick(grid, 1, 1) * a) * b
    ok = inside & pick(known, 0, 0) & pick(known, 0, 1) & pick(known, 1, 0) & pick(known, 1, 1)
    return z.astype(np.float64), ok


def fill_out(z, ok, rounds=96):
    z, done = np.where(ok, z, 0.0), ok.copy()
    for _ in range(rounds):
        if done.all():
            break
        zp, dp = np.pad(z * done, 1), np.pad(done, 1).astype(np.float64)
        total = zp[:-2, 1:-1] + zp[2:, 1:-1] + zp[1:-1, :-2] + zp[1:-1, 2:]
        count = dp[:-2, 1:-1] + dp[2:, 1:-1] + dp[1:-1, :-2] + dp[1:-1, 2:]
        grow = ~done & (count > 0)
        z[grow] = total[grow] / count[grow]
        done |= grow
    return z


def halve(z, ok):
    p = np.pad(z, 1, mode="edge")
    s = (p[:-2] + 2 * p[1:-1] + p[2:]) / 4
    s = (s[:, :-2] + 2 * s[:, 1:-1] + s[:, 2:]) / 4
    return s[::2, ::2], ok[::2, ::2]


def terra_payload(corners, t, row, col):
    floor = math.floor(float(corners.min())) - 1.0
    q = np.clip(np.round((corners - floor) * 100), 0, 65535).astype(np.uint16)
    q[:, 1:] -= q[:, :-1].copy()
    data = base64.b64encode(gzip.compress(q.astype("<u2").tobytes(), 6, mtime=0)).decode()
    return {"format": "indexus-terrain-tile-v2", "level": t, "row": row, "col": col, "size": CELLS + 1,
            "scale": 0.01, "offset": floor, "delta": "x", "data": data}


def terra(grid, known, scene, altitude, fh, levels=(13, 19)):
    x0, step = MIN, CELL
    coarse, fine = levels
    reach = -x0
    lat_s, lng_w = scene.to_degrees(-reach, -reach)
    lat_n, lng_e = scene.to_degrees(reach, reach)
    q = fine + 6
    r0, c0 = (int(v) // CELLS * CELLS for v in gps.cell_index(float(lat_s), float(lng_w), q))
    r1, c1 = ((int(v) // CELLS + 1) * CELLS for v in gps.cell_index(float(lat_n), float(lng_e), q))
    z, ok = lattice(grid, known, x0, step, scene, q, (r0, r1 + 1), (c0, c1 + 1))
    z = fill_out(z + altitude, ok)
    n = 0
    for t in range(fine, coarse - 1, -1):
        cell_level = t + 6
        for row in range(r0 // CELLS, r1 // CELLS):
            for col in range(c0 // CELLS, c1 // CELLS):
                j, i = row * CELLS - r0, col * CELLS - c0
                valid = ok[j:j + CELLS + 1, i:i + CELLS + 1]
                cells = valid[:-1, :-1] & valid[1:, :-1] & valid[:-1, 1:] & valid[1:, 1:]
                if not cells.any():
                    continue
                corners = z[j:j + CELLS + 1, i:i + CELLS + 1]
                metrics = [0.0, 0.0, 0.0]
                if t == fine:
                    s, nn, _, _ = gps.bounds(row, 0, t)
                    ns, ew = gps.cell_metres(cell_level, (s + nn) / 2)
                    mean = (corners[:-1, :-1] + corners[1:, :-1] + corners[:-1, 1:] + corners[1:, 1:]) / 4
                    w = cells * ns * ew
                    metrics = [float(w.sum()), float((w * mean).sum()), float((w * mean * mean).sum())]
                fh.write(json.dumps({"Collection": TERRA, "Location": gps.tile_anchor(t, row, col, TERRA), "ID": f"{t}/{row}/{col}",
                                     "Metrics": metrics, "Payload": terra_payload(corners, t, row, col)}, separators=(",", ":")) + "\n")
                n += 1
        if t == coarse:
            break
        a0, b0 = r0 // (2 * CELLS) * 2 * CELLS, c0 // (2 * CELLS) * 2 * CELLS
        a1, b1 = -(-r1 // (2 * CELLS)) * 2 * CELLS, -(-c1 // (2 * CELLS)) * 2 * CELLS
        pad = ((r0 - a0, a1 - r1), (c0 - b0, b1 - c1))
        z, ok = halve(np.pad(z, pad, mode="edge"), np.pad(ok, pad))
        r0, r1, c0, c1 = a0 // 2, a1 // 2, b0 // 2, b1 // 2
    return n


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("root", type=Path)
    ap.add_argument("work", type=Path)
    ap.add_argument("--prep", type=Path, default=None)
    ap.add_argument("--workers", type=int, default=None)
    ap.add_argument("--half", type=float, default=None, help="tiles within this distance only (a test)")
    args = ap.parse_args()
    peak = returns.Peak()
    args.work.mkdir(parents=True, exist_ok=True)
    t0 = time.perf_counter()
    rdir = returns.prepare(args.root, args.prep or args.work.parent, args.workers, lambda **k: print(json.dumps(k), flush=True), args.half)
    man = json.loads((rdir / "manifest.json").read_text())
    recipe = json.loads((args.root / "data/world/anglet-1km/recipe.json").read_text())
    key = returns.batch_id({"tool": "prep ground 1", "sources": {"returns": man["key"], "ground.py": returns.file_sha(__file__),
                            "gps.py": returns.file_sha(gps.__file__), "ixp.py": returns.file_sha(ixp.__file__)},
                            "options": {"origin": str(recipe["origin"]), "altitude": str(recipe["verticalOrigin"])}})
    if (args.work / "ground.key").exists() and (args.work / "ground.key").read_text() == key:
        print(json.dumps({"stage": "skip", "collection": "ground", "key": key[:16]}))
        return
    t1 = time.perf_counter()
    total = np.zeros((N, N), np.float32)
    count = np.zeros((N, N), np.uint16)
    workers = args.workers or min(os.cpu_count(), 8)
    peak.phase()
    with Pool(workers) as pool:
        for row, col, s, n in pool.imap_unordered(tile_sums, [str(rdir / t["file"]) for t in man["tiles"]], chunksize=16):
            total[row * 64:row * 64 + 64, col * 64:col * 64 + 64] = s.reshape(64, 64)
            count[row * 64:row * 64 + 64, col * 64:col * 64 + 64] = n.reshape(64, 64)
    t2 = time.perf_counter()
    peaks = {"cells": peak.phase()}
    measured = count > 0
    dtm = np.where(measured, total / np.maximum(count, 1), 0).astype(np.float32)
    del total, count
    dtm = fill(dtm.astype(np.float64), measured.astype(np.float64)).astype(np.float32)
    import gc
    gc.collect()
    t3 = time.perf_counter()
    records, corners, known = legacy(dtm, measured, ixp.space_tables(LEGACY, 2))
    with open(args.work / "terrain-1km.ndjson.tmp", "w") as fh:
        for r in records:
            fh.write(json.dumps(r, separators=(",", ":")) + "\n")
    os.replace(args.work / "terrain-1km.ndjson.tmp", args.work / "terrain-1km.ndjson")
    np.save(args.work / "corners-q.npy", corners)  # cm + 100 m, uint16 (the tiles' values)
    np.save(args.work / "corners-known.npy", known)
    t4 = time.perf_counter()
    peaks["legacy"] = peak.phase()
    grid = (corners * 0.01 + -100).astype(np.float32)  # ground.py: cumsum(q) * scale + offset, into a float32 grid
    with open(args.work / "terrain.ndjson.tmp", "w") as fh:
        nterra = terra(grid, known, gps.Scene(recipe["origin"]), recipe["verticalOrigin"], fh)
    os.replace(args.work / "terrain.ndjson.tmp", args.work / "terrain.ndjson")
    t5 = time.perf_counter()
    (args.work / "ground.key").write_text(key)
    peak.stop.set()
    print(json.dumps({"stage": "ground", "records": {LEGACY: len(records), TERRA: nterra}, "measured_share": round(float(measured.mean()), 4),
                      "seconds": {"returns": round(t1 - t0, 3), "cells": round(t2 - t1, 3), "fill": round(t3 - t2, 3), "legacy": round(t4 - t3, 3),
                                  "terra": round(t5 - t4, 3), "total": round(t5 - t0, 3)}, "peak_rss_mb": dict(peaks, terra=peak.mb())}), flush=True)


if __name__ == "__main__":
    main()
