"""Prepared returns: the LiDAR HD source of the scene, decoded once.

One file per 32 m source tile, every class kept, coordinates in the scene
frame (float64, as build_vegetation.py computes them, clipped to the tile:
the source tile assignment is authoritative), the orthophoto colour
resolved. Every later build (vegetation, crowns, gps canopy, ground,
roofs) starts from these files and skips the inflate, the projection and the
colour, which are 80 % of the work.

Usage: python3 returns.py <experiences dir> <prep dir>    (writes <prep dir>/lidar/<key>/)

The directory name is the first 16 hex of the SHA-256 of the inputs (the raw
source hash, the recipe, the orthophoto manifest, the format version), so a
changed input is a new directory and an unchanged one is skipped. Tiles are
written atomically; manifest.json is written last and marks the set complete.

IXRT tile, little-endian:
  header 64 B  "IXRT" | u32 version 1 | u32 n | u32 0 | f64 x0, y0, x1, y1 (tile, scene metres) | f64 z0 | 8 x 0
  [5]u32       start of each group, then n: ground (class 2), vegetation (3-5), building (6), other;
               returns keep the file order inside a group (payload order depends on it)
  then for each group, its k returns as columns, so a group is one read:
  [k]f64 x | [k]f64 y | [k]i32 Z (source cm: z = Z * 0.01 - z0) | [k][3]u8 rgb | [k]u16 intensity
  [k]u8 return bits | [k]u8 classification
27 bytes per return.
"""

import hashlib, json, math, os, struct, subprocess, sys, threading, time, zlib
from multiprocessing import Pool
from pathlib import Path
import numpy as np

GROUPS = {"ground": 0, "vegetation": 1, "building": 2, "other": 3}
VERSION = "2"  # of the arithmetic and the layout: part of the key, bump when either changes
EARTH = 6378137.0
HEADER = 64
G = {}  # per process: frame, orthophoto mosaic


def init(root):
    root = Path(root)
    data, scene = root / "data/world/anglet-1km", root / "public/world/anglet-1km"
    recipe = json.loads((data / "recipe.json").read_text())
    header = json.loads((data / "manifest.json").read_text())
    lon0, lat0 = recipe["origin"]
    ortho = json.loads((scene / "points-imagery/manifest.json").read_text())
    # a pavé of a commune (commune/plan.py): the scene frame less frameShift, so the code of the
    # [-1024, 1024] square holds; the orthophoto is read at the scene's own coordinates
    sx, sy = recipe.get("frameShift", [0.0, 0.0])
    G.update(z0=recipe["verticalOrigin"], cos0=math.cos(math.radians(lat0)), ox=EARTH * math.radians(lon0), sx=float(sx), sy=float(sy),
             oy=EARTH * math.log(math.tan(math.pi / 4 + math.radians(lat0) / 2)),
             dtype=np.dtype([tuple(d) for d in header["dtype"]]), raw=scene / "points-raw" / header["sourceSha256"][:16],
             scene=scene, tiles={f["key"]: f["url"] for f in ortho["tiles"]["files"]}, TS=ortho["tiles"]["tileSize"])
    G["RES"] = 2 * math.pi * EARTH / (G["TS"] * 2**18)


def file_sha(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for b in iter(lambda: fh.read(1 << 20), b""):
            h.update(b)
    return h.hexdigest()


def batch_id(inputs):
    return hashlib.sha256(json.dumps(inputs, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


# --- memory: a budget from the machine, and the peak of the whole process tree ---
def physical_memory():
    try:
        return os.sysconf("SC_PAGE_SIZE") * os.sysconf("SC_PHYS_PAGES")
    except (ValueError, OSError, AttributeError):
        return int(subprocess.run(["sysctl", "-n", "hw.memsize"], capture_output=True, text=True).stdout)


def budget():
    """Bytes the whole pipeline may hold: 2 GiB, or an eighth of the machine if that is less."""
    return min(2 << 30, physical_memory() // 8)


class Peak:
    """Samples the resident memory of this process and all its descendants (ps, every 0.2 s)."""

    def __init__(self):
        self.peak, self.stop = 0, threading.Event()
        threading.Thread(target=self.run, daemon=True).start()

    def run(self):
        me = os.getpid()
        while not self.stop.is_set():
            try:
                out = subprocess.run(["ps", "-A", "-o", "pid=,ppid=,rss="], capture_output=True, text=True).stdout
                kids, rss = {}, {}
                for line in out.splitlines():
                    p, pp, r = map(int, line.split())
                    kids.setdefault(pp, []).append(p)
                    rss[p] = r
                todo, total = [me], 0
                while todo:
                    p = todo.pop()
                    total += rss.get(p, 0)
                    todo += kids.get(p, [])
                self.peak = max(self.peak, total * 1024)
            except Exception:
                pass
            self.stop.wait(0.2)

    def mb(self):
        return round(self.peak / 1e6)

    def phase(self):
        """The peak since the last call, in MB, and start a new phase."""
        p, self.peak = self.peak, 0
        return round(p / 1e6)


# --- the float64 arithmetic of build_vegetation.py, step for step ---
def l93_to_local(x, y):
    n, c, xs, ys, e = 0.7256077650, 11754255.426, 700000.0, 12655612.050, 0.0818191910428158
    dx, dy = x - xs, ys - y
    lon = math.radians(3) + np.arctan2(dx, dy) / n
    iso = -np.log(np.hypot(dx, dy) / c) / n
    lat = 2 * np.arctan(np.exp(iso)) - math.pi / 2
    for _ in range(6):
        s = np.sin(lat)
        lat = 2 * np.arctan(((1 + e * s) / (1 - e * s)) ** (e / 2) * np.exp(iso)) - math.pi / 2
    return (EARTH * lon - G["ox"]) * G["cos0"] - G["sx"], (EARTH * np.log(np.tan(math.pi / 4 + lat / 2)) - G["oy"]) * G["cos0"] - G["sy"]


def ortho_rgb(x, y):
    """The orthophoto colour of each point, read as build_vegetation.py reads it,
    from one mosaic of the z18 tiles decoded once (build_mosaic)."""
    TS, RES, cos0 = G["TS"], G["RES"], G["cos0"]
    x, y = x + G["sx"], y + G["sy"]
    px = (x / cos0 + G["ox"] + math.pi * EARTH) / RES
    py = (math.pi * EARTH - (y / cos0 + G["oy"])) / RES
    tx, ty = (px // TS).astype(np.int64), (py // TS).astype(np.int64)
    u = np.clip((px - tx * TS).astype(int), 0, TS - 1) + (tx - G["tx0"]) * TS
    v = np.clip((py - ty * TS).astype(int), 0, TS - 1) + (ty - G["ty0"]) * TS
    return G["mosaic"][v, u]


def mosaic_bounds(box):
    """z18 tiles over a scene rectangle (x0, y0, x1, y1): every point of the files lies in it."""
    TS, RES, cos0 = G["TS"], G["RES"], G["cos0"]
    px = (np.array([box[0], box[2]]) / cos0 + G["sx"] / cos0 + G["ox"] + math.pi * EARTH) / RES
    py = (math.pi * EARTH - (np.array([box[1], box[3]]) / cos0 + G["sy"] / cos0 + G["oy"])) / RES
    return int(px.min() // TS), int(px.max() // TS), int(py.min() // TS), int(py.max() // TS)


def attach_mosaic(name, shape, tx0, ty0):
    """The mosaic is one block of shared memory (no file): every worker maps it."""
    from multiprocessing import shared_memory
    if G.get("mosaic_name") != name:
        try:  # the creator unlinks it, a worker only maps it
            shm = shared_memory.SharedMemory(name=name, track=False)
        except TypeError:  # Python < 3.13: a mapping registers with a tracker that would unlink it
            from multiprocessing import resource_tracker
            shm = shared_memory.SharedMemory(name=name)
            if name != G.get("created"):  # the creator keeps its registration
                resource_tracker.unregister(shm._name, "shared_memory")
        G.update(mosaic_shm=shm, mosaic_name=name, mosaic=np.ndarray(shape, np.uint8, buffer=shm.buf), tx0=tx0, ty0=ty0)


def decode_tile(job):
    from PIL import Image
    name, shape, tx, ty, tx0, ty0 = job
    url = G["tiles"].get(f"18/{tx}/{ty}")
    if url:
        attach_mosaic(name, shape, tx0, ty0)
        TS = G["TS"]
        G["mosaic"][(ty - ty0) * TS:(ty - ty0 + 1) * TS, (tx - tx0) * TS:(tx - tx0 + 1) * TS] = np.asarray(Image.open(G["scene"] / "points-imagery" / url).convert("RGB"))
    return 1


def build_mosaic(pool, box):
    from multiprocessing import shared_memory
    tx0, tx1, ty0, ty1 = mosaic_bounds(box)
    TS = G["TS"]
    shape = ((ty1 - ty0 + 1) * TS, (tx1 - tx0 + 1) * TS, 3)
    shm = shared_memory.SharedMemory(create=True, size=int(np.prod(shape)))
    G["created"] = shm.name
    np.ndarray(shape, np.uint8, buffer=shm.buf)[:] = 90  # where no tile exists: the grey of build_vegetation.py
    sum(pool.imap_unordered(decode_tile, [(shm.name, shape, tx, ty, tx0, ty0) for tx in range(tx0, tx1 + 1) for ty in range(ty0, ty1 + 1)], chunksize=4))
    return shm, (shm.name, shape, tx0, ty0)


# --- one tile ---
def strip_job(job):
    """A strip of neighbouring source files (one process each: a process touches
    only its part of the shared mosaic, so the memory of the tree stays bounded)."""
    files, mosaic = job
    n, t = 0, np.zeros(4)
    for f, out in files:
        k, tt = tile_job((f, out, mosaic))
        n, t = n + k, t + tt
    return n, t


def tile_job(job):
    """One source file -> one IXRT file (atomically)."""
    f, out, mosaic = job
    attach_mosaic(*mosaic)
    b = f["bounds"]
    t0 = time.perf_counter()
    pts = np.frombuffer(zlib.decompress((G["raw"] / f["url"]).read_bytes(), 31), G["dtype"])
    t1 = time.perf_counter()
    cls = pts["classification"]
    group = np.where(cls == 2, 0, np.where((cls >= 3) & (cls <= 5), 1, np.where(cls == 6, 2, 3))).astype(np.uint8)
    order = np.argsort(group, kind="stable")
    pts, group = pts[order], group[order]
    x, y = l93_to_local(pts["X"] * 0.01, pts["Y"] * 0.01)
    x = np.clip(x, b[0], b[2] - 1e-6)
    y = np.clip(y, b[1], b[3] - 1e-6)
    t2 = time.perf_counter()
    rgb = ortho_rgb(x, y)
    t3 = time.perf_counter()
    n = len(pts)
    start = np.searchsorted(group, np.arange(5)).astype("<u4")
    with open(out + ".tmp", "wb") as fh:
        fh.write(b"IXRT" + struct.pack("<III4dd8x", 1, n, 0, *b, G["z0"]))
        fh.write(start.tobytes())
        for g in range(4):
            a, e = int(start[g]), int(start[g + 1])
            fh.write(x[a:e].astype("<f8").tobytes()), fh.write(y[a:e].astype("<f8").tobytes()), fh.write(pts["Z"][a:e].astype("<i4").tobytes())
            fh.write(np.ascontiguousarray(rgb[a:e]).tobytes()), fh.write(pts["intensity"][a:e].astype("<u2").tobytes())
            fh.write(pts["bit_fields"][a:e].tobytes()), fh.write(pts["classification"][a:e].tobytes())
    os.replace(out + ".tmp", out)
    t4 = time.perf_counter()
    return n, (t1 - t0, t2 - t1, t3 - t2, t4 - t3)


def find(prep_dir):
    """The prepared returns under prep_dir (<prep_dir>/lidar/<key>/, complete:
    manifest.json written last); the newest when there are several. Returns
    (directory, manifest); manifest["tiles"] lists file, bounds, count and
    classes of every tile."""
    sets = sorted((d for d in (Path(prep_dir) / "lidar").glob("*") if (d / "manifest.json").exists()), key=lambda d: (d / "manifest.json").stat().st_mtime)
    if not sets:
        raise FileNotFoundError(f"no prepared returns under {prep_dir}/lidar (python3 returns.py <experiences> <prep dir>)")
    return sets[-1], json.loads((sets[-1] / "manifest.json").read_text())


def read_tile(path, group="vegetation"):
    """The returns of one group of one IXRT tile: one read, then views.

    Stable (the IXRT layout above, header version 1; prepared sets of
    returns.VERSION "2"): group is "ground" (class 2), "vegetation"
    (classes 3-5), "building" (class 6) or "other" (every other class). The
    dict holds, for the k returns of the group, in the order of the source file:
      x, y       float64[k]  scene metres (east, north of the scene origin), clipped to the tile
      Z          int32[k]    source height in cm: z = Z * 0.01 - z0 (scene datum, float64)
      rgb        uint8[k, 3] orthophoto colour at (x, y); 90 where no tile exists
      intensity  uint16[k]   bits uint8[k] (return number | number of returns << 4)   class uint8[k]
      bounds     (x0, y0, x1, y1) of the tile, scene metres     z0  float (recipe verticalOrigin)
    """
    g = GROUPS[group]
    with open(path, "rb") as fh:
        head = fh.read(HEADER + 4 * 5)
        assert head[:4] == b"IXRT", path
        out = {"bounds": struct.unpack("<4d", head[16:48]), "z0": struct.unpack("<d", head[48:56])[0]}
        start = np.frombuffer(head, "<u4", 5, HEADER)
        a, b = int(start[g]), int(start[g + 1])
        k = b - a
        fh.seek(HEADER + 4 * 5 + 27 * a)
        raw = np.fromfile(fh, np.uint8, 27 * k)
    at = 0
    for name, dt, w in (("x", "<f8", 1), ("y", "<f8", 1), ("Z", "<i4", 1), ("rgb", "u1", 3), ("intensity", "<u2", 1), ("bits", "u1", 1), ("class", "u1", 1)):
        size = np.dtype(dt).itemsize * w * k
        col = raw[at:at + size].view(dt)
        out[name] = col.reshape(-1, 3) if w == 3 else col
        at += size
    return out


def prepare(root, prep_dir, workers=None, log=print, half=None):
    """Builds (or finds) the prepared returns of the scene (or of the tiles
    within `half` metres of the origin, for a test); returns their directory."""
    root, prep_dir = Path(root), Path(prep_dir)
    init(root)
    data = root / "data/world/anglet-1km"
    header = json.loads((data / "manifest.json").read_text())
    inputs = {"tool": "prep returns " + VERSION, "sources": {"points-raw": header["sourceSha256"], "recipe.json": file_sha(data / "recipe.json"),
              "points-imagery/manifest.json": file_sha(root / "public/world/anglet-1km/points-imagery/manifest.json")}}
    if half is not None:
        inputs["options"] = {"half": str(half)}
    key = batch_id(inputs)
    out = prep_dir / "lidar" / key[:16]
    if (out / "manifest.json").exists():
        return out
    out.mkdir(parents=True, exist_ok=True)
    files = sorted(header["files"], key=lambda f: f["url"])
    if half is not None:
        files = [f for f in files if not (f["bounds"][2] <= -half or f["bounds"][0] >= half or f["bounds"][3] <= -half or f["bounds"][1] >= half)]
    # measured: a worker peaks at about 240 MB (a tile and its part of the mosaic),
    # the driver at 320 MB (it fills the mosaic)
    workers = workers or max(1, min(os.cpu_count(), 8, (budget() - (320 << 20)) // (300 << 20)))
    t0 = time.perf_counter()
    tm, total = np.zeros(4), 0
    box = [min(f["bounds"][0] for f in files), min(f["bounds"][1] for f in files),
           max(f["bounds"][2] for f in files), max(f["bounds"][3] for f in files)]
    with Pool(workers, initializer=init, initargs=(str(root),)) as pool:
        shm, mosaic = build_mosaic(pool, box)
    shm.close()  # the segment stays (unlinked at the end); this process no longer holds its pages
    tmo = time.perf_counter() - t0
    # One strip of neighbouring columns per worker, with equal numbers of returns:
    # a worker touches only its part of the shared mosaic.
    todo = sorted(((int(f["url"].split("-")[0]), int(f["url"].split("-")[1].split(".")[0])), f) for f in files
                  if not (out / f["url"].replace(".lasrecords.gz", ".ixrt")).exists())
    cum = np.cumsum([t[1]["count"] for t in todo]) if todo else np.zeros(0)
    cut = np.searchsorted(cum, np.linspace(0, cum[-1] if len(cum) else 0, workers + 1)[1:-1])
    strips = [s for s in np.split(np.arange(len(todo)), cut) if len(s)]
    jobs = [([(todo[i][1], str(out / todo[i][1]["url"].replace(".lasrecords.gz", ".ixrt"))) for i in s], mosaic) for s in strips]
    with Pool(workers, initializer=init, initargs=(str(root),)) as pool:
        for n, t in pool.imap_unordered(strip_job, jobs):
            total += n
            tm += t
    try:
        from multiprocessing import shared_memory
        shared_memory.SharedMemory(name=mosaic[0]).unlink()
    except FileNotFoundError:
        pass
    tiles = [{"file": f["url"].replace(".lasrecords.gz", ".ixrt"), "bounds": f["bounds"], "count": f["count"],
              "classes": f["classificationCounts"]} for f in files]
    size = sum((out / t["file"]).stat().st_size for t in tiles)
    man = {"format": "indexus-returns-v1", "key": key, "inputs": inputs, "returns": sum(t["count"] for t in tiles), "bytes": size, "tiles": tiles}
    (out / "manifest.json").write_text(json.dumps(man, indent=1) + "\n")
    log(stage="returns", files=len(files), returns=int(total), bytes=size, seconds=round(time.perf_counter() - t0, 3),
        mosaic_seconds=round(tmo, 3), cpu_seconds=dict(zip(["inflate", "project", "colour", "write"], np.round(tm, 2).tolist())), workers=workers)
    return out


if __name__ == "__main__":
    peak = Peak()
    d = prepare(sys.argv[1], sys.argv[2], log=lambda **k: print(json.dumps(k), flush=True))
    print(json.dumps({"returns_dir": str(d), "peak_rss_mb": peak.mb()}))
