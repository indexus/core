"""The crowns collection AngVegCrowns0001 from the prepared returns: one record
per tree of the scene, plus one small crown per 8 m cell of the low vegetation
the segmentation leaves out. The steps and the arithmetic of
the former builder build_crowns.py (experiences history; its docstring had the
19 metrics); what changes is where the work runs:

  points + chm   per tile, 8 processes: the vegetation group of each prepared
                 tile, its height above the ground (corners of ground.py) and
                 the maximum per 0.5 m cell
  segment        canopy height model, pits, smoothing (numpy); local maxima
                 and region growing on the GPU (prep-metal --peaks, --grow: one
                 thread per cell, one dispatch per growing step) or in numpy
                 (--grow numpy, the code of build_crowns.py)
  stats          per strip of tiles, 4 processes (each holds partial sums of
                 every crown), partial sums added
  write          the NDJSON of today's builder (for `prep ndjson`)

Usage: python3 crowns.py <experiences dir> <work dir> --ground <ground work dir> [--prep DIR] [--metal prep-metal]
Skipped when <work dir>/crowns.key holds the key of the same inputs.
"""

import argparse, json, math, os, struct, subprocess, sys, time
from multiprocessing import Pool
from pathlib import Path
import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ixp  # noqa: E402
import returns  # noqa: E402

COLLECTION = "AngVegCrowns0001"
SIDE, MIN, CELL = 2048.0, -1024.0, 0.5
N = int(SIDE / CELL)
SHRUB = 8.0
NS = int(SIDE / SHRUB)
MIN_TREE, MIN_TREE_RETURNS, MIN_SHRUB, MIN_SHRUB_RETURNS = 3.0, 12, 0.5, 10
HB, HMAX = 0.25, 48.0
NB = int(HMAX / HB)
W = {}  # per process: ground, label, tops, ortho


def ground_at(g, x, y):
    u = np.clip((x - MIN) / CELL, 0, N - 1e-3)
    v = np.clip((y - MIN) / CELL, 0, N - 1e-3)
    i, j = u.astype(np.int64), v.astype(np.int64)
    fu, fv = u - i, v - j
    return (g[j, i] * (1 - fu) * (1 - fv) + g[j, i + 1] * fu * (1 - fv)
            + g[j + 1, i] * (1 - fu) * fv + g[j + 1, i + 1] * fu * fv)


def cell_of(x, y, size=CELL, n=N):
    return (np.clip(((y - MIN) / size).astype(np.int64), 0, n - 1) * n
            + np.clip(((x - MIN) / size).astype(np.int64), 0, n - 1))


def points(path):
    """The vegetation returns of a tile as build_crowns.py keeps them: float32 x, y, z."""
    r = returns.read_tile(path, "vegetation", )
    return r["x"].astype(np.float32), r["y"].astype(np.float32), (r["Z"] * 0.01 - r["z0"]).astype(np.float32)


def load_ground(work):
    if "g" not in W:
        W["g"] = np.load(work / "ground.npy", mmap_mode="r")
    return W["g"]


def chm_job(job):
    """The maximum height above the ground per 0.5 m cell over a band of tile rows."""
    paths, work = job
    us, ms = [], []
    for path in paths:
        x, y, z = points(path)
        if not len(x):
            continue
        h = (z - ground_at(load_ground(work), x, y)).astype(np.float32)
        us.append(cell_of(x, y)), ms.append(h)
    if not us:
        return np.zeros(0, np.int64), np.zeros(0, np.float32)
    c, h = np.concatenate(us), np.concatenate(ms)
    u, inv = np.unique(c, return_inverse=True)
    m = np.full(len(u), -np.inf, np.float32)
    np.maximum.at(m, inv, h)
    return u, m


def bands(tiles, n, size=20):
    """Groups of neighbouring tiles, row by row (a worker then touches few rows
    of the row-major grids it reads); about `size` tiles each."""
    order = sorted(tiles, key=lambda t: (t["bounds"][1], t["bounds"][0]))
    return [order[i:i + size] for i in range(0, len(order), size)]


def stats_job(job):
    paths, work, T = job
    g = load_ground(work)
    if "label" not in W:
        W["label"] = np.load(work / "label.npy", mmap_mode="r").ravel()
        W["ortho"] = np.load(work / "ortho.npy", mmap_mode="r").reshape(-1, 3)
        W["top"] = np.load(work / "tops.npy")
    label, ortho, top = W["label"], W["ortho"], W["top"]
    K = T + NS * NS
    acc = {k: np.zeros(K) for k in ("n", "x", "y", "xx", "xy", "yy", "z", "r", "g", "b", "w")}
    hist = np.zeros(T * NB, np.int64)
    shist = np.zeros(NS * NS * 24, np.int64)
    def batches():
        xs, ys, zs, k = [], [], [], 0
        for path in paths:
            x, y, z = points(path)
            xs.append(x), ys.append(y), zs.append(z)
            k += len(x)
            if k >= 200_000:
                yield np.concatenate(xs), np.concatenate(ys), np.concatenate(zs)
                xs, ys, zs, k = [], [], [], 0
        if k:
            yield np.concatenate(xs), np.concatenate(ys), np.concatenate(zs)

    for x, y, z in batches():
        x, y, z = x.astype(np.float64), y.astype(np.float64), z.astype(np.float64)
        hag = z - ground_at(g, x, y)
        c = cell_of(x, y)
        lab = label[c]
        tree = lab > 0
        low = ~tree & (hag >= MIN_SHRUB)
        key = np.where(tree, lab, T + cell_of(x, y, SHRUB, NS))
        keep = tree | low
        key, x, y, z, hag, c, tree = key[keep], x[keep], y[keep], z[keep], hag[keep], c[keep], tree[keep]
        add = lambda name, v: acc[name].__iadd__(np.bincount(key, v, K))
        add("n", None)
        for name, v in (("x", x), ("y", y), ("xx", x * x), ("xy", x * y), ("yy", y * y), ("z", z)):
            add(name, v)
        upper = np.where(tree, hag >= 0.5 * top[np.where(tree, key, 0)], True)
        col = ortho[c].astype(np.float64) / 255
        acc["w"] += np.bincount(key, upper.astype(np.float64), K)
        for k, name in enumerate("rgb"):
            add(name, col[:, k] * upper)
        hb = np.clip((hag / HB).astype(np.int64), 0, NB - 1)
        hist += np.bincount(key[tree] * NB + hb[tree], minlength=T * NB)
        sb = np.clip(hb[~tree], 0, 23)
        shist += np.bincount((key[~tree] - T) * 24 + sb, minlength=NS * NS * 24)
    return acc, hist, shist


def segment(chm, work, grow, metal, log):
    """build_crowns.py "segment": pits, smoothing, maxima, region growing."""
    empty = chm < 0
    p = np.pad(np.where(empty, np.nan, chm), 1, constant_values=np.nan)
    shifts = [(a, b) for a in range(3) for b in range(3) if (a, b) != (1, 1)]
    count = np.zeros((N, N), np.int8)
    for a, b in shifts:
        count += ~np.isnan(p[a:a + N, b:b + N])
    pit = empty & (count >= 5)
    pj, pi = np.nonzero(pit)  # the 8 neighbours of the pits only (build_crowns.py stacks all cells)
    with np.errstate(all="ignore"):
        chm[pit] = np.nanmedian(np.stack([p[pj + a, pi + b] for a, b in shifts]), axis=0)
    del p, count, pit
    h = np.maximum(chm, 0).astype(np.float32)
    for _ in range(2):
        q = np.pad(h, ((1, 1), (0, 0)), mode="edge")
        h = (q[:-2] + 2 * q[1:-1] + q[2:]) / 4
        q = np.pad(h, ((0, 0), (1, 1)), mode="edge")
        h = (q[:, :-2] + 2 * q[:, 1:-1] + q[:, 2:]) / 4
    h = h.astype(np.float32)
    jitter = ((np.arange(N * N, dtype=np.uint64) * np.uint64(2654435761)) % np.uint64(1000)).astype(np.float32).reshape(N, N)
    hj = h + jitter * 1e-6
    del jitter
    want = np.round((0.6 + 0.12 * h) / CELL).clip(2, 8).astype(np.int8)

    def running_max(a, r, axis):
        out = a.copy()
        for k in range(1, r + 1):
            for s in (k, -k):
                sh = np.roll(a, s, axis=axis)
                if s > 0:
                    sh[(slice(None),) * axis + (slice(0, s),)] = 0
                else:
                    sh[(slice(None),) * axis + (slice(s, None),)] = 0
                np.maximum(out, sh, out=out)
        return out

    t0 = time.perf_counter()
    if grow == "metal":  # one thread per cell: the maximum of its own window
        h.tofile(work / "peaks-h.f32"), hj.tofile(work / "peaks-hj.f32")
        r = subprocess.run([metal, "--peaks", str(work), str(N)], capture_output=True, text=True)
        if r.returncode:
            sys.exit(r.stderr)
        peak = np.fromfile(work / "peaks.u8", np.uint8).reshape(N, N).astype(bool)
        for f in ("peaks-h.f32", "peaks-hj.f32", "peaks.u8"):
            os.remove(work / f)
    else:
        peak = np.zeros((N, N), bool)
        for r in range(2, 9):
            band = (want == r) & (h >= MIN_TREE)
            if not band.any():
                continue
            win = running_max(running_max(hj, r, 0), r, 1)
            peak |= band & (hj >= win)
    del want, hj
    t1 = time.perf_counter()
    pj, pi = np.nonzero(peak)
    label = np.zeros(N * N, np.int32)
    flat_h = h.ravel()
    ids = pj * N + pi
    label[ids] = np.arange(1, len(ids) + 1)
    top = np.r_[0, flat_h[ids]].astype(np.float32)
    if grow == "metal":
        flat_h.tofile(work / "grow-h.f32"), label.tofile(work / "grow-label.i32"), top.tofile(work / "grow-top.f32")
        r = subprocess.run([metal, "--grow", str(work), str(N), str(len(top))], capture_output=True, text=True)
        if r.returncode:
            sys.exit(r.stderr)
        info = json.loads(r.stdout.strip().splitlines()[-1])
        label = np.fromfile(work / "grow-label.i32", np.int32)
        for f in ("grow-h.f32", "grow-label.i32", "grow-top.f32"):
            os.remove(work / f)
        it = info["iterations"]
    else:
        cand = np.flatnonzero((flat_h >= 1.5) & (label == 0))
        for it in range(200):
            best_h = np.full(len(cand), -1.0, np.float32)
            best_l = np.zeros(len(cand), np.int32)
            hc = flat_h[cand]
            cj, ci = cand // N, cand % N
            for dj, di in ((1, 0), (-1, 0), (0, 1), (0, -1)):
                ok = (cj + dj >= 0) & (cj + dj < N) & (ci + di >= 0) & (ci + di < N)
                nbr = np.where(ok, cand + dj * N + di, cand)
                l, hn = label[nbr], flat_h[nbr]
                good = ok & (l > 0) & (hc <= hn + 0.3) & (hc >= np.maximum(1.5, 0.45 * top[l])) & (hn > best_h)
                best_h = np.where(good, hn, best_h)
                best_l = np.where(good, l, best_l)
            g2 = best_l > 0
            if not g2.any():
                break
            label[cand[g2]] = best_l[g2]
            cand = cand[~g2]
    t2 = time.perf_counter()
    log(stage="segment", trees=len(ids), grow=grow, iterations=it, seconds={"maxima": round(t1 - t0, 3), "grow": round(t2 - t1, 3)})
    return label.reshape(N, N), top


def write(work, out, log):
    """build_crowns.py "write", unchanged but for its inputs."""
    tables = ixp.space_tables(COLLECTION, 2)
    g = np.load(work / "ground.npy")
    s = np.load(work / "stats.npz")
    top = np.load(work / "tops.npy")
    T = len(top)
    n = s["n"]
    tree = np.arange(len(n)) < T
    keep = np.where(tree, n >= MIN_TREE_RETURNS, n >= MIN_SHRUB_RETURNS)
    keep[0] = False
    idx = np.flatnonzero(keep)
    nk = n[idx]
    mx, my = s["x"][idx] / nk, s["y"][idx] / nk
    cxx = np.maximum(s["xx"][idx] / nk - mx * mx, 0)
    cxy = s["xy"][idx] / nk - mx * my
    cyy = np.maximum(s["yy"][idx] / nk - my * my, 0)
    tr = cxx + cyy
    det = cxx * cyy - cxy * cxy
    disc = np.sqrt(np.maximum(tr * tr / 4 - det, 0))
    l1, l2 = tr / 2 + disc, np.maximum(tr / 2 - disc, 0)
    floor = np.where(tree[idx], 0.8, 0.5)
    r1, r2 = np.maximum(2 * np.sqrt(l1), floor), np.maximum(2 * np.sqrt(l2), floor)
    ang = 0.5 * np.arctan2(2 * cxy, cxx - cyy)
    ca, sa = np.cos(ang), np.sin(ang)
    s1, s2 = (r1 / 2) ** 2, (r2 / 2) ** 2
    cxx, cxy, cyy = s1 * ca * ca + s2 * sa * sa, (s1 - s2) * ca * sa, s1 * sa * sa + s2 * ca * ca
    area = math.pi * r1 * r2
    ground = ground_at(g, mx, my)

    def pct(hh, q):
        c = np.cumsum(hh, axis=1)
        return (np.argmax(c >= q * c[:, -1:], axis=1) + 0.5)

    hist = s["hist"].reshape(T, NB)
    shist = s["shist"].reshape(-1, 24)
    istree = tree[idx]
    ht = hist[np.where(istree, idx, 0)]
    hs = shist[np.where(istree, 0, idx - T)]
    top_t = pct(ht, 0.99) * HB
    above = ht.copy()
    above[:, : int(1.0 / HB)] = 0
    base_t = pct(np.where(above.sum(1, keepdims=True) > 0, above, ht), 0.10) * HB
    top_s, base_s = pct(hs, 0.95) * HB, pct(hs, 0.05) * HB
    h_top = np.where(istree, top_t, top_s)
    h_base = np.where(istree, base_t, base_s)
    h_base = np.minimum(h_base, h_top - np.where(istree, 1.0, 0.3))
    h_base = np.where(istree, np.maximum(h_base, 0), 0)
    h_top = np.maximum(h_top, h_base + 0.3)
    zmean = s["z"][idx] / nk - ground
    h_waist = np.clip(zmean, h_base + 0.25 * (h_top - h_base), h_base + 0.75 * (h_top - h_base))
    w = np.maximum(s["w"][idx], 1)
    rgb = np.stack([s["r"][idx] / w, s["g"][idx] / w, s["b"][idx] / w], 1)
    cells = np.stack([np.clip(((mx - MIN) / CELL).astype(np.int64), 0, N - 1),
                      np.clip(((my - MIN) / CELL).astype(np.int64), 0, N - 1)], 1)
    locs = ixp.locations(tables, cells, 4)
    R = lambda v, d=3: round(float(v), d)
    nt = ns = 0
    with open(str(out) + ".tmp", "w") as fh:
        for i in range(len(idx)):
            t = bool(istree[i])
            if t:
                nt += 1
                rid = f"t{nt}"
            else:
                ns += 1
                rid = f"s{ns}"
            x, y = float(mx[i]), float(my[i])
            gz = float(ground[i])
            metrics = [1, R(x), R(y), R(x * x, 2), R(x * y, 2), R(y * y, 2), R(cxx[i]), R(cxy[i]), R(cyy[i]), R(area[i], 2),
                       R(gz, 2), R(gz + h_base[i], 2), R(gz + h_top[i], 2), R(gz + h_waist[i], 2),
                       R(rgb[i, 0]), R(rgb[i, 1]), R(rgb[i, 2]), int(nk[i]), int(t)]
            fh.write(json.dumps({"Collection": COLLECTION, "Location": locs[i], "ID": rid, "Metrics": metrics}, separators=(",", ":")) + "\n")
    os.replace(str(out) + ".tmp", out)
    return nt, ns


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("root", type=Path)
    ap.add_argument("work", type=Path)
    ap.add_argument("--ground", type=Path, required=True, help="work dir of ground.py (corners-q.npy, corners-known.npy)")
    ap.add_argument("--prep", type=Path, default=None)
    ap.add_argument("--metal", default=None, help="prep-metal binary (region growing on the GPU)")
    ap.add_argument("--grow", default=None, choices=["metal", "numpy"])
    ap.add_argument("--workers", type=int, default=None)
    ap.add_argument("--half", type=float, default=None)
    args = ap.parse_args()
    grow = args.grow or ("metal" if args.metal else "numpy")
    peak = returns.Peak()
    work = args.work
    work.mkdir(parents=True, exist_ok=True)
    log = lambda **k: print(json.dumps(k), flush=True)
    t0 = time.perf_counter()
    rdir = returns.prepare(args.root, args.prep or work.parent, args.workers, log, args.half)
    man = json.loads((rdir / "manifest.json").read_text())
    key = returns.batch_id({"tool": "prep crowns 1", "sources": {"returns": man["key"], "crowns.py": returns.file_sha(__file__),
                            "ground": returns.file_sha(args.ground / "corners-q.npy"), "ixp.py": returns.file_sha(ixp.__file__)},
                            "options": {"grow": grow}})
    if (work / "crowns.key").exists() and (work / "crowns.key").read_text() == key:
        log(stage="skip", collection=COLLECTION, key=key[:16])
        return
    workers = args.workers or min(os.cpu_count(), 8)
    veg = [t for t in man["tiles"] if sum(t["classes"].get(k, 0) for k in "345") > 0]
    groups = [[str(rdir / t["file"]) for t in b] for b in bands(veg, workers)]
    # ground: build_crowns.py "ground", from the corners of the tiles
    q = np.load(args.ground / "corners-q.npy")
    known = np.load(args.ground / "corners-known.npy")
    g = q.astype(np.float32) * 0.01 + -100
    g = g.astype(np.float32)
    g[~known] = np.nan
    g[~known] = np.nanmedian(g)
    np.save(work / "ground.npy", g)
    del q, known, g
    t1 = time.perf_counter()
    peak.phase()
    # ortho: the colour at each cell centre (build_crowns.py "ortho"), from the mosaic of returns.py
    returns.init(args.root)
    sel = [t for t in man["tiles"] if sum(t["classes"].get(k, 0) for k in "345") > 0]
    box = (min(t["bounds"][0] for t in sel), min(t["bounds"][1] for t in sel), max(t["bounds"][2] for t in sel), max(t["bounds"][3] for t in sel))
    with Pool(workers, initializer=returns.init, initargs=(str(args.root),)) as pool:
        shm, mosaic = returns.build_mosaic(pool, box)
    returns.attach_mosaic(*mosaic)
    out = np.lib.format.open_memmap(work / "ortho.npy", mode="w+", dtype=np.uint8, shape=(N, N, 3))
    out[:] = 90
    c = MIN + (np.arange(N) + 0.5) * CELL
    i0, i1 = int((box[0] - MIN) / CELL), int((box[2] - MIN) / CELL)  # cells the vegetation can reach
    for j0 in range(int((box[1] - MIN) / CELL), int((box[3] - MIN) / CELL), 256):
        j1 = min(j0 + 256, int((box[3] - MIN) / CELL))
        yy, xx = np.meshgrid(c[j0:j1], c[i0:i1], indexing="ij")
        out[j0:j1, i0:i1] = returns.ortho_rgb(xx.ravel(), yy.ravel()).reshape(j1 - j0, i1 - i0, 3)
    out.flush()
    del out
    returns.G.pop("mosaic", None)
    returns.G.pop("mosaic_name", None)
    returns.G["mosaic_shm"].close()
    shm.close()
    shm.unlink()
    t2 = time.perf_counter()
    chm = np.full(N * N, -1.0, np.float32)
    with Pool(workers) as pool:
        for u, m in pool.imap(chm_job, [(b, work) for b in groups]):
            np.maximum.at(chm, u, m)
    peaks = {"ortho+chm": peak.phase()}
    t3 = time.perf_counter()
    label, top = segment(chm.reshape(N, N), work, grow, args.metal, log)
    np.save(work / "label.npy", label)
    np.save(work / "tops.npy", top)
    del label, chm
    peaks["segment"] = peak.phase()
    t4 = time.perf_counter()
    T = len(top)
    # one task per band of rows of tiles: each worker sums its bands, the parent adds the workers
    rows = bands(veg, workers, size=-(-len(veg) // (4 * workers)))
    acc, hist, shist = None, None, None
    # a worker holds partial sums of every crown (about 40 MB) and a batch of
    # returns: 4 workers keep the tree under the budget
    with Pool(max(1, min(workers, returns.budget() // (500 << 20)))) as pool:
        for a, h, sh in pool.imap_unordered(stats_job, [([str(rdir / t["file"]) for t in b], work, T) for b in rows]):
            if acc is None:
                acc, hist, shist = a, h, sh
            else:
                for k in acc:
                    acc[k] += a[k]
                hist += h
                shist += sh
    np.savez(work / "stats.npz", hist=hist, shist=shist.astype(np.int32), **acc)
    t5 = time.perf_counter()
    peaks["stats"] = peak.phase()
    nt, ns = write(work, work / "crowns.ndjson", log)
    t6 = time.perf_counter()
    for f in ("ortho.npy", "label.npy", "stats.npz", "ground.npy", "tops.npy"):
        os.remove(work / f)
    (work / "crowns.key").write_text(key)
    peak.stop.set()
    log(stage="crowns", records=nt + ns, trees=nt, low=ns, seconds={"returns": round(t1 - t0 - 0, 3), "ortho": round(t2 - t1, 3), "chm": round(t3 - t2, 3),
        "segment": round(t4 - t3, 3), "stats": round(t5 - t4, 3), "write": round(t6 - t5, 3), "total": round(t6 - t0, 3)},
        peak_rss_mb=dict(peaks, write=peak.mb()))


if __name__ == "__main__":
    main()
