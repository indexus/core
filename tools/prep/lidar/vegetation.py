"""prep for the vegetation collection AngVegetation001, from the prepared
returns (returns.py: the raw LiDAR HD decoded once, one file per 32 m tile).
Same records as the former builder build_vegetation.py (experiences history),
written as a prep batch (docs/ingest/pipeline.md).

Two stages:
  points   parallel over tiles: the vegetation returns (classes 3-5) of each
           prepared tile -> chunks of points ready for the GPU (IXPT): the
           integer cell of each return (the voxel, packed 3 x 10 bits), its
           offset from the voxel centre (float64 as two f32: a GPU has no
           float64) and its 6-byte payload.
  records  key, sort, sums and payload per voxel, zones above them, parts:
           --backend metal (tools/prep/metal) or numpy.

Usage: python3 vegetation.py <experiences dir> <work dir> [--tables space-tables.json]
       [--prep DIR] [--batch DIR] [--backend metal|numpy] [--metal prep-metal] [--half 64]
Defaults fit a memory budget of min(2 GiB, RAM / 8) for the whole process tree.

IXPT chunk, little-endian: "IXPT" | u32 version 1 | u64 n | u32 payload width w |
u32 bits per axis b | u64 0, then [n]u32 cell (x << 2b | y << b | z),
[3][n]f32 offset from the cell centre (high part), [3][n]f32 the rest
(offset - high, so high + low is the float64 offset to 2^-48), [n][w]u8 payload.
"""

import argparse, json, math, os, shutil, struct, sys, time
from multiprocessing import Pool
from pathlib import Path
import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ixp  # noqa: E402
import returns  # noqa: E402

COLLECTION = "AngVegetation001"
DEPTH = 5
VOXEL = 2048 / 4**DEPTH
BITS = 10  # cells per axis at depth 5: 1024
METRICS = ["n", "sum_x", "sum_y", "sum_z", "sum_xx", "sum_xy", "sum_xz", "sum_yy", "sum_yz", "sum_zz", "sum_r", "sum_g", "sum_b"]
PAYLOAD = "json"  # payload bytes as the node stores them today: a JSON string of base64 ("binary": the 6 bytes per return)


def voxel_points(xyz, rgb):
    """Packed cell, offset from the cell centre (two f32) and payload of each return."""
    cells = np.floor((xyz + 1024) / VOXEL).astype(np.int64)
    q = np.clip(np.floor((xyz - (cells * VOXEL - 1024)) / VOXEL * 256), 0, 255).astype(np.uint8)
    d = xyz - (cells * VOXEL - 1024 + VOXEL / 2)
    hi = d.astype(np.float32)
    d = np.concatenate([hi, (d - hi).astype(np.float32)], 1)
    packed = ((cells[:, 0] << (2 * BITS)) | (cells[:, 1] << BITS) | cells[:, 2]).astype(np.uint32)
    return packed, d, np.concatenate([q, rgb], 1)


def chunk_job(job):
    """Prepared tiles -> one IXPT chunk."""
    path, tiles, half = job
    tm = np.zeros(3)  # read, cells, write
    xyz, rgb = [], []
    for t in tiles:
        t0 = time.perf_counter()
        r = returns.read_tile(t)
        z = r["Z"] * 0.01 - r["z0"]  # the float64 operation of build_vegetation.py
        x, y = r["x"], r["y"]
        if half is not None:
            keep = (np.abs(x) <= half) & (np.abs(y) <= half)
            x, y, z, r["rgb"] = x[keep], y[keep], z[keep], r["rgb"][keep]
        xyz.append(np.stack([x, y, z], 1)), rgb.append(r["rgb"])
        tm[0] += time.perf_counter() - t0
    t0 = time.perf_counter()
    c, d, p = voxel_points(np.concatenate(xyz), np.concatenate(rgb))
    del xyz, rgb
    t1 = time.perf_counter()
    with open(path, "wb") as fh:
        fh.write(b"IXPT" + struct.pack("<IQIIQ", 1, len(c), p.shape[1], BITS, 0))
        fh.write(c.tobytes())
        fh.write(np.ascontiguousarray(d.T).tobytes())
        fh.write(p.tobytes())
    tm[1] += t1 - t0
    tm[2] += time.perf_counter() - t1
    return path, len(c), list(tm)


def read_chunk(path):
    raw = open(path, "rb").read()
    assert raw[:4] == b"IXPT"
    _, n, w, bits, _ = struct.unpack("<IQIIQ", raw[4:32])
    at = 32
    cell = np.frombuffer(raw, np.uint32, n, at); at += 4 * n
    d = np.frombuffer(raw, np.float32, 6 * n, at).reshape(6, n); at += 24 * n
    pay = np.frombuffer(raw, np.uint8, w * n, at).reshape(n, w)
    return cell, d, pay, bits


def location_keys(cell, bits, mask, offset, depth):
    """Location bits, left aligned in a uint64, of packed cells: sdk Space._encode on integer cells."""
    axes = [(cell >> np.uint32(2 * bits - a * bits)) & np.uint32((1 << bits) - 1) for a in range(3)]
    used = [bits] * 3
    key = np.zeros(len(cell), np.uint64)
    for l in range(depth):
        idx = np.zeros(len(cell), np.uint32)
        for n in range(6):
            a = mask[l * 6 + n]
            used[a] -= 1
            idx = (idx << np.uint32(1)) | ((axes[a] >> np.uint32(used[a])) & np.uint32(1))
        key |= ((idx + np.uint32(offset[l])) & np.uint32(63)).astype(np.uint64) << np.uint64(58 - 6 * l)
    return key


def absolute(n, cx, cy, cz, s):
    """Sums about the voxel centre (s: [k][9]: dx dy dz dxx dxy dxz dyy dyz dzz) -> absolute moments."""
    sx, sy, sz, sxx, sxy, sxz, syy, syz, szz = s.T
    return np.stack([n * cx + sx, n * cy + sy, n * cz + sz,
                     n * cx * cx + 2 * cx * sx + sxx, n * cx * cy + cx * sy + cy * sx + sxy, n * cx * cz + cx * sz + cz * sx + sxz,
                     n * cy * cy + 2 * cy * sy + syy, n * cy * cz + cy * sz + cz * sy + syz, n * cz * cz + 2 * cz * sz + szz], 1)


def records_numpy(chunks, tables, out, inputs, log):
    mask, offset = tables["mask"], tables["offset"]
    keys, mets, pays, offs, t = [], [], [], [], {"read": 0, "key": 0, "sort": 0, "reduce": 0, "gather": 0}
    for path in chunks:
        t0 = time.perf_counter()
        cell, d, pay, bits = read_chunk(path)
        t1 = time.perf_counter()
        key = location_keys(cell, bits, mask, offset, DEPTH)
        t2 = time.perf_counter()
        order = np.argsort(key, kind="stable")
        ks = key[order]
        t3 = time.perf_counter()
        starts = np.flatnonzero(np.r_[True, ks[1:] != ks[:-1]])
        n = np.diff(np.r_[starts, len(ks)]).astype(np.float64)
        dx, dy, dz = (d[i][order].astype(np.float64) + d[i + 3][order] for i in range(3))
        cols = np.stack([dx, dy, dz, dx * dx, dx * dy, dx * dz, dy * dy, dy * dz, dz * dz], 1)
        s = np.add.reduceat(cols, starts, axis=0)
        rgb = np.add.reduceat(pay[order][:, 3:6].astype(np.int64), starts, axis=0) / 255
        c = cell[order[starts]].astype(np.int64)
        centre = [((c >> (2 * bits - a * bits)) & ((1 << bits) - 1)) * VOXEL - 1024 + VOXEL / 2 for a in range(3)]
        m = np.concatenate([n[:, None], absolute(n, *centre, s), rgb], 1)
        t4 = time.perf_counter()
        pays.append(pay[order])
        offs.append(starts)
        keys.append(ks[starts]), mets.append(m)
        t5 = time.perf_counter()
        for k, v in zip(t, (t1 - t0, t2 - t1, t3 - t2, t4 - t3, t5 - t4)):
            t[k] += v
    t0 = time.perf_counter()
    # Voxels nest in source files, so chunk records never share a key: one sort of the records orders them all.
    rec_n = [len(k) for k in keys]
    key = np.concatenate(keys)
    order = np.argsort(key, kind="stable")
    met = np.concatenate(mets)[order]
    # payload of record r: its points, contiguous in its chunk's sorted payload (gathered point by point)
    w = pays[0].shape[1] if pays else 6
    base = np.cumsum([0] + [len(p) for p in pays])[:-1]
    start = np.concatenate([o + b for o, b in zip(offs, base)])
    size = np.concatenate([np.diff(np.r_[o, len(p)]) for o, p in zip(offs, pays)])
    allpay = np.concatenate(pays)
    start, size = start[order], size[order]
    pt_off = np.r_[0, np.cumsum(size)].astype(np.int64)
    idx = np.repeat(start - pt_off[:-1], size)
    idx += np.arange(int(pt_off[-1]), dtype=np.int64)
    payload = allpay[idx].reshape(-1)
    del idx, allpay
    pay_off = (pt_off * w).astype(np.uint64)
    if PAYLOAD == "json":
        payload, pay_off = ixp.json_strings(payload, pay_off)
    t1 = time.perf_counter()
    man = ixp.write_batch(out, COLLECTION, key[order], DEPTH, b"v", met, pay_off, payload, inputs=inputs, metric_names=METRICS,
                          payload_encoding=PAYLOAD)
    t2 = time.perf_counter()
    t.update(merge=t1 - t0, write=t2 - t1)
    log(stage="records", backend="numpy", records=int(man["records"]), seconds={k: round(v, 3) for k, v in t.items()})
    return man


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("root", type=Path)
    ap.add_argument("out", type=Path)
    ap.add_argument("--tables", type=Path, default=None, help="space-tables.json of the SDK (default: the same tables, from the collection name)")
    ap.add_argument("--prep", type=Path, default=None, help="prepared returns live here (default: <out>/../prep)")
    ap.add_argument("--half", type=float, default=None)
    ap.add_argument("--workers", type=int, default=None)
    ap.add_argument("--chunk-points", type=int, default=None, help="returns per chunk (default: from the memory budget)")
    ap.add_argument("--backend", default="metal", choices=["numpy", "metal", "none"])
    ap.add_argument("--metal", default=None, help="prep-metal binary")
    ap.add_argument("--payload", default="json", choices=["json", "binary"], help="payload bytes: a JSON string of base64 as today, or raw")
    ap.add_argument("--batch", type=Path, default=None, help="where the batch goes (default: <out>/batch)")
    ap.add_argument("--force", action="store_true", help="rebuild even when out/batch holds this batch id")
    ap.add_argument("--collection", default=None, help="the collection whose Space tables key the voxels (default AngVegetation001)")
    ap.add_argument("--levels-above", type=int, default=0,
                    help="levels of that Space above the [-1024, 1024] cube of the scene (a commune's paving, commune/vegbatch.py): "
                         "the keys are those of the cube, with the tables of the levels below")
    args = ap.parse_args()
    peak = returns.Peak()
    global PAYLOAD, COLLECTION
    COLLECTION = args.collection or COLLECTION
    tables = json.loads(args.tables.read_text())[COLLECTION] if args.tables else ixp.space_tables(COLLECTION, 3)
    if args.levels_above:
        k = args.levels_above
        tables = {"mask": tables["mask"][6 * k:], "offset": tables["offset"][k:]}
    PAYLOAD = args.payload
    args.out.mkdir(parents=True, exist_ok=True)
    logf = open(Path(str(args.out).rstrip("/") + ".log.jsonl"), "a")

    def log(**kw):
        line = json.dumps(kw)
        print(line, flush=True)
        logf.write(line + "\n"), logf.flush()

    # memory, measured: about 500 B per return held by a worker (arrays and their
    # temporaries), 72 B per return of a GPU batch; the records of the collection
    # (about 1 GB for the 1.12 M voxels) come on top in the records stage
    budget = returns.budget()
    workers = args.workers or min(os.cpu_count(), 8)
    per_job = args.chunk_points or max(100_000, int(budget * 0.5 / workers / 500))
    batch_points = max(per_job, min(8_000_000, int(budget * 0.1 / 72)))

    t0 = time.perf_counter()
    rdir = returns.prepare(args.root, args.prep or args.out.parent / "prep", args.workers, log, args.half)
    t_prep = time.perf_counter() - t0
    peak_returns = peak.phase()
    man = json.loads((rdir / "manifest.json").read_text())
    tiles = [t for t in man["tiles"] if sum(t["classes"].get(k, 0) for k in "345") > 0]
    if args.half is not None:
        h = args.half
        tiles = [t for t in tiles if not (t["bounds"][2] <= -h or t["bounds"][0] >= h or t["bounds"][3] <= -h or t["bounds"][1] >= h)]
    inputs = {"tool": "prep vegetation 2", "sources": {"returns": man["key"], "vegetation.py": returns.file_sha(__file__),
              "ixp.py": returns.file_sha(ixp.__file__), "space-tables": returns.batch_id(tables)},
              "options": {"half": str(args.half), "depth": str(DEPTH), "collection": COLLECTION, "payload": args.payload}}
    if args.levels_above:
        inputs["options"]["levelsAbove"] = str(args.levels_above)
    bid = returns.batch_id(inputs)
    batch = args.batch or args.out / "batch"
    if not args.force and (batch / "manifest.json").exists() and json.loads((batch / "manifest.json").read_text()).get("id") == bid:
        log(stage="skip", collection=COLLECTION, id=bid[:16], reason="same inputs")
        return
    chunks_dir = args.out / "chunks"
    chunks_dir.mkdir(parents=True, exist_ok=True)
    jobs, cur, cnt = [], [], 0
    for t in tiles:
        cur.append(str(rdir / t["file"]))
        cnt += sum(t["classes"].get(k, 0) for k in "345")
        if cnt >= per_job:
            jobs.append(cur)
            cur, cnt = [], 0
    if cur:
        jobs.append(cur)
    jobs = [(str(chunks_dir / f"c-{i:05d}.ixpt"), j, args.half) for i, j in enumerate(jobs)]

    t1 = time.perf_counter()
    tm, npts, done = np.zeros(3), 0, []
    with Pool(workers) as pool:
        for path, n, t in pool.imap_unordered(chunk_job, jobs):
            done.append(path)
            npts += n
            tm += t
    done.sort()
    t2 = time.perf_counter()
    log(stage="points", tiles=len(tiles), chunks=len(done), points=npts, seconds=round(t2 - t1, 3), points_per_s=round(npts / (t2 - t1)),
        cpu_seconds=dict(zip(["read", "cells", "write"], np.round(tm, 2).tolist())), workers=workers, job_points=per_job,
        batch_points=batch_points, budget_mb=budget >> 20, peak_rss_mb_all_processes=peak.phase())
    out = batch.with_name(batch.name + ".new")
    if args.backend == "numpy":
        records_numpy(done, tables, str(out), inputs, log)
    elif args.backend == "metal":
        import subprocess
        spec = {"collection": COLLECTION, "depth": DEPTH, "voxel": VOXEL, "origin": -1024, "bits": BITS,
                "mask": tables["mask"][: 6 * DEPTH], "offset": tables["offset"][:DEPTH], "id": "v",
                "splitDepth": 2, "partBytes": 64 << 20, "zoneDepths": [0, DEPTH - 1], "metricNames": METRICS,
                "inputs": inputs, "batchID": bid, "batchPoints": batch_points, "payloadEncoding": args.payload,
                "chunks": done, "out": str(out)}
        (args.out / "spec.json").write_text(json.dumps(spec))
        r = subprocess.run([args.metal, str(args.out / "spec.json")], capture_output=True, text=True)
        sys.stdout.write(r.stdout), sys.stderr.write(r.stderr)
        for line in r.stdout.splitlines():
            if line.startswith("{"):
                logf.write(line + "\n")
        if r.returncode:
            sys.exit(r.returncode)
    for p in done:
        os.remove(p)
    if args.backend != "none":
        if batch.exists():
            shutil.rmtree(batch)
        os.replace(out, batch)
    t3 = time.perf_counter()
    peak.stop.set()
    log(stage="total", backend=args.backend, seconds=round(t3 - t0, 3), returns_seconds=round(t_prep, 3), points_seconds=round(t2 - t1, 3),
        records_seconds=round(t3 - t2, 3), peak_rss_mb={"returns": peak_returns, "records": peak.mb()})


if __name__ == "__main__":
    main()
