#!/usr/bin/env python3
"""La préparation d'une scène « commune » d'Explore : tout le contour, en lots pour le nœud.

  commune.py plan  <catalogue.json> <scène> --data DATA [--exp EXP]   ce qui sera fait, volumes, place libre
  commune.py run   <catalogue.json> <scène> --data DATA --exp EXP [--prep BIN/prep] [--metal BIN/prep-metal]

L'entrée du catalogue porte `boundary` (le contour, GeoJSON à côté du catalogue ;
récupéré de geo.api.gouv.fr par son code `commune` s'il manque), `origin`,
`altitude`, `orthoLevels`, `vegetationSpace` et ses collections.

Étapes, chacune reprise où elle s'est arrêtée (un fichier .done par unité) :
  1. contour, dalles LiDAR HD, orthophoto z18, emprises BD TOPO
  2. pour chaque pavé de 2 048 m (dans l'ordre des clés de la végétation), chaque quart de
     1 024 m : retours (COPC) -> retours préparés (returns.py) -> végétation (vegetation.py,
     clés du pavé), sommes du sol, toits par colonne -> effacement du brut et des retours
     préparés ; puis le pavé : parts de la végétation (vegbatch.write_pave), bâtiments
     (experiences buildings.py) ; effacement des toits
  3. sol (terrain.py), orthophoto (ortho.py), fin du lot de végétation, bâtiments :
     $DATA/batches/<collection> (prep ndjson pour les NDJSON ; un lot inchangé est sauté)

Disque : le brut et les retours préparés d'un seul quart à la fois (2 à 3 Go), les
toits d'un pavé, les sommes du sol (25 Mo par quart), les lots.
"""

import argparse
import json
import math
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent / "lidar"))
import numpy as np  # noqa: E402

import plan  # noqa: E402
import vegbatch  # noqa: E402
import zone as zones  # noqa: E402

GB = 1 << 30
# Mesures du disque de 1 km (3,27 km², 2026-11-14, docs/ingest/pipeline.md), par km² :
PER_KM2 = {
    "returns": 65e6,  # retours LiDAR HD
    "copc_bytes": 10.0 * 65e6,  # ~10 octets par retour dans les COPC (LAZ 1.4)
    "raw_gz": 2.83e9 / 3.27,  # .lasrecords.gz (gzip 1) de toutes les classes
    "ixrt": 27 * 65e6 * 0.85,  # retours préparés, classes 2 à 6 (~85 %)
    "chunks": 34 * 17.4e6,  # points de végétation pour le GPU (IXPT)
    "vegetation": 621e6 / 3.27,  # parts + zones du lot de végétation
    "roofs": 12 * 65e6 * 0.2,  # toits (float32 x 3), ~20 % des retours
    "ground_npz": 25e6,  # sommes du sol d'un quart (1 km²), compressées
    "buildings": 42e6 / 3.14,  # deux collections de bâtiments (NDJSON ; le lot fait autant)
    "ortho": 36e6 / 3.14,
    "terrain": 11.4e6 / 3.14,
}


def log(**kw):
    print(json.dumps(kw, ensure_ascii=False), flush=True)


def load_entry(catalogue, scene_id):
    entries = json.loads(Path(catalogue).read_text())
    for e in entries:
        if e["id"] == scene_id:
            return e
    raise SystemExit(f"scène {scene_id} absente de {catalogue}")


class Commune:
    def __init__(self, catalogue, scene_id, data, exp=None, fetch_boundary=True):
        self.catalogue = Path(catalogue).resolve()
        self.entry = load_entry(catalogue, scene_id)
        e = self.entry
        if not e.get("boundary"):
            raise SystemExit(f"scène {scene_id} : pas de contour (`boundary`) dans le catalogue")
        self.data = Path(data)
        self.exp = Path(exp) if exp else None
        self.work = self.data / "prep" / "commune" / e["id"]
        self.work.mkdir(parents=True, exist_ok=True)
        self.boundary = self.catalogue.parent / e["boundary"]
        if not self.boundary.exists():
            if not fetch_boundary or not e.get("commune"):
                raise SystemExit(f"contour absent : {self.boundary}")
            import fetch
            fetch.boundary(e["commune"], self.boundary)
            log(stage="boundary", file=str(self.boundary))
        self.geometry = zones.read_geometry(self.boundary)
        self.frame = zones.Frame(e["origin"])
        self.zone = zones.Polygon.from_geometry(self.geometry, self.frame)
        self.collections = e["collections"]
        space = e.get("vegetationSpace") or {}
        if space and (space.get("min") != vegbatch.SPACE["min"] or space.get("side") != vegbatch.SPACE["side"] or space.get("depth") != vegbatch.SPACE["depth"]):
            raise SystemExit(f"vegetationSpace {space} : la préparation fait {vegbatch.SPACE}")
        prefix = lambda i, j: vegbatch.pave_prefix(self.collections["vegetation"], i, j)[0]  # noqa: E731
        self.paves = plan.order(plan.paves(self.zone), prefix)
        self.quarters = {p: plan.quarters(self.zone, *p) for p in self.paves}
        self._mask = None

    @property
    def mask(self):
        if self._mask is None:
            x0, y0, x1, y1 = self.zone.bounds
            m = plan.BAND + 64
            self._mask = zones.Mask(self.zone, (x0 - m, y0 - m, x1 + m, y1 + m), cell=2.0, margin=plan.BAND)
        return self._mask

    # --- chemins ---
    def pave_dir(self, i, j):
        return self.work / "paves" / f"{i}_{j}"

    def quarter_root(self, i, j, q):
        return self.pave_dir(i, j) / f"q{q}"

    # --- volumes ---
    def estimate(self):
        area = self.zone.area() / 1e6
        boxes = [b for p in self.paves for q in self.quarters[p] for b in plan.boxes(self.zone, *p, q)]
        read = sum((b[2] - b[0]) * (b[3] - b[1]) for b in boxes) / 1e6
        nq = sum(len(v) for v in self.quarters.values())
        k = PER_KM2
        final = area * (k["vegetation"] + k["buildings"] + k["ortho"] + k["terrain"])
        quarter_peak = 1.1 * (k["raw_gz"] + k["ixrt"])  # le brut et les retours préparés d'un quart (1 km² + bande)
        peak = (area * k["vegetation"] + nq * k["ground_npz"] + 4 * 1.1 * k["roofs"] + quarter_peak + 4 * 0.3 * k["vegetation"]
                + area * (k["buildings"] + 1.4 * k["ortho"] + k["terrain"]) + 0.3e9)
        return {"areaKm2": round(area, 2), "paves": len(self.paves), "quarters": nq, "boxes": len(boxes),
                "readKm2": round(read, 2), "returns": int(read * k["returns"]), "downloadGB": round(read * k["copc_bytes"] / 1e9, 1),
                "finalBatchesGB": round(final / 1e9, 1), "peakDiskGB": round(peak / 1e9, 1)}

    # --- étape 1 ---
    def sources(self):
        import fetch
        d = fetch.dalles(self.geometry, self.work / "dalles.json")
        if d["missing"]:
            log(stage="dalles", warning="dalles sans URL au WFS (hors couverture LiDAR HD ?)", missing=d["missing"])
        self.dalles = d["dalles"]
        log(stage="dalles", count=len(self.dalles), points=sum(x["points"] for x in self.dalles))
        imagery = self.work / "points-imagery"
        if not (imagery / "manifest.json").exists() or not (self.work / "points-imagery.done").exists():
            m = fetch.ortho(self.zone, self.frame, imagery, log=log)
            (self.work / "points-imagery.done").write_text(str(len(m["tiles"]["files"])))
            log(stage="ortho", tiles=len(m["tiles"]["files"]), bytes=sum(f["bytes"] for f in m["tiles"]["files"]))
        fetch.bdtopo(self.geometry, self.work / "context" / "batiment.geojson")
        log(stage="bdtopo", file=str(self.work / "context" / "batiment.geojson"))

    def scene_root(self, root, i, j):
        """La disposition d'experiences pour un pavé ou un quart : recette, contexte, orthophoto."""
        e = self.entry
        data = root / "data/world/anglet-1km"
        public = root / "public/world/anglet-1km"
        data.mkdir(parents=True, exist_ok=True)
        public.mkdir(parents=True, exist_ok=True)
        recipe = {"origin": e["origin"], "verticalOrigin": e["altitude"], "frameShift": [plan.PAVE * i, plan.PAVE * j],
                  "radiusMetres": 1024 * math.sqrt(2), "scene": e["id"], "pave": [i, j]}
        path = data / "recipe.json"
        if not path.exists() or json.loads(path.read_text()) != recipe:
            path.write_text(json.dumps(recipe, indent=1))
        (data / "context").mkdir(exist_ok=True)
        link = data / "context" / "batiment.geojson"
        if not link.exists():
            link.symlink_to(self.work / "context" / "batiment.geojson")
        if not (public / "points-imagery").exists():
            (public / "points-imagery").symlink_to(self.work / "points-imagery")
        return data, public

    # --- étape 2 : un quart ---
    def quarter(self, i, j, q, cache, args):
        import fetch
        import returns
        import terrain
        root = self.quarter_root(i, j, q)
        done = root / "done.json"
        if done.exists():
            return json.loads(done.read_text())
        t0 = time.time()
        data, public = self.scene_root(root, i, j)
        pave = self.pave_dir(i, j)
        ground = self.work / "ground" / f"{i}_{j}_q{q}.npz"
        roofs = pave / "buildings" / "parts" / f"roofs-q{q}.done"
        vb = pave / "veg" / f"q{q}"
        products = lambda: ground.exists() and roofs.exists() and (vb / "manifest.json").exists()  # noqa: E731
        prepared = sorted(d for d in (root / "prep" / "lidar").glob("*") if (d / "manifest.json").exists()) if (root / "prep").exists() else []
        if not products() and not prepared and not (data / "blocks").exists():
            # rien de réutilisable : les boîtes sont à relire
            for f in data.glob("box-*.json"):
                f.unlink()
        check_space(self.data, need=3 * GB, what=f"quart {i},{j},{q}")
        files = fetch.quarter(root, self.entry, self.dalles, self.zone, self.mask, i, j, q, cache, log=log) if not products() else []
        t1 = time.time()
        if not files and not products():  # rien dans ce quart (la mer, l'Adour)
            out = {"returns": 0, "seconds": round(t1 - t0, 1)}
            self.collect_band(root, pave, q)
            shutil.rmtree(data / "blocks", ignore_errors=True)
            done.write_text(json.dumps(out))
            return out
        count = 0
        if not products():
            header = json.loads((data / "header.json").read_text())
            import hashlib
            source = hashlib.sha256(json.dumps([(f["url"], f["sha256"]) for f in files], separators=(",", ":")).encode()).hexdigest()
            man = {"schema": "indexus-raw-las-v1", "sourceSha256": source, "dtype": header["dtype"], "pointFormat": header["pointFormat"],
                   "scales": header["scales"], "offsets": header["offsets"], "count": sum(f["count"] for f in files), "files": files,
                   "horizontalCRS": "EPSG:2154", "verticalCRS": "IGN69"}
            count = man["count"]
            (data / "manifest.json").write_text(json.dumps(man, separators=(",", ":")))
            raw = public / "points-raw"
            raw.mkdir(exist_ok=True)
            if not (raw / source[:16]).exists():
                (raw / source[:16]).symlink_to(data / "blocks")
            rdir = returns.prepare(root, root / "prep", args.workers, log)
            shutil.rmtree(data / "blocks", ignore_errors=True)  # le brut ne sert plus : les retours préparés suffisent
            t2 = time.time()
            if not ground.exists():  # le sol : sommes par cellule de 0,5 m
                total, count_ = terrain.quarter_sums(rdir, q)
                (self.work / "ground").mkdir(exist_ok=True)
                terrain.save_quarter(ground, i, j, q, total, count_)
                del total, count_
            if not roofs.exists():  # les toits, par colonne de tuiles du pavé (buildings.py)
                self.roofs(rdir, pave / "buildings" / "parts", q)
                self.collect_band(root, pave, q)
                roofs.write_text("1")
            t3 = time.time()
            vegetation = sum(sum(t["classes"].get(k, 0) for k in "345") for t in json.loads((rdir / "manifest.json").read_text())["tiles"])
            if vegetation and not (vb / "manifest.json").exists():
                # la végétation : clés du pavé, tables des niveaux sous les pavés
                cmd = [sys.executable, str(HERE.parent / "lidar" / "vegetation.py"), str(root), str(root / "work" / "vegetation"),
                       "--prep", str(root / "prep"), "--batch", str(vb), "--collection", self.collections["vegetation"],
                       "--levels-above", str(vegbatch.ABOVE), "--backend", args.backend]
                if args.backend == "metal":
                    cmd += ["--metal", args.metal]
                subprocess.run(cmd, check=True)
        else:
            t2 = t3 = t1
        shutil.rmtree(root / "prep", ignore_errors=True)
        shutil.rmtree(root / "work", ignore_errors=True)
        out = {"returns": count, "vegetation": (vb / "manifest.json").exists(), "seconds": {"lidar": round(t1 - t0, 1), "returns": round(t2 - t1, 1),
               "ground_roofs": round(t3 - t2, 1), "vegetation": round(time.time() - t3, 1)}}
        done.write_text(json.dumps(out))
        log(stage="quarter", pave=[i, j], quarter=q, **out)
        return out

    def roofs(self, rdir, parts, q):
        """La classe 6 des tuiles préparées, par colonne : parts/<colonne>-q<q>.npz (roof : x, y, z float32, repère du pavé)."""
        import struct
        parts.mkdir(parents=True, exist_ok=True)
        man = json.loads((Path(rdir) / "manifest.json").read_text())
        columns = {}
        for t in man["tiles"]:
            path = Path(rdir) / t["file"]
            with open(path, "rb") as fh:
                head = fh.read(84)
                z0 = struct.unpack("<d", head[48:56])[0]
                start = np.frombuffer(head, "<u4", 5, 64)
                a, k = int(start[2]), int(start[3] - start[2])
                fh.seek(84 + 27 * a)
                raw = np.fromfile(fh, np.uint8, 27 * k)
            x, y = raw[:8 * k].view("<f8"), raw[8 * k:16 * k].view("<f8")
            z = raw[16 * k:20 * k].view("<i4") * 0.01 - z0
            columns.setdefault(int(t["file"].split("-")[0]), []).append(np.stack([x, y, z], 1).astype(np.float32))
        for c, arrays in columns.items():
            np.savez(parts / f"{c}-q{q}.npz", roof=np.concatenate(arrays))

    def collect_band(self, root, pave, q):
        """Les toits hors du pavé de ce quart, pour parts/margin.npz."""
        bands = sorted((root / "data/world/anglet-1km").glob("band-*.npy"))
        (pave / "buildings" / "parts").mkdir(parents=True, exist_ok=True)
        if bands:
            np.save(pave / "buildings" / f"band-q{q}.npy", np.concatenate([np.load(b) for b in bands]))
            for b in bands:
                b.unlink()

    # --- étape 2 : un pavé ---
    def pave(self, index, i, j, cache, args):
        pave = self.pave_dir(i, j)
        for q in self.quarters[(i, j)]:
            self.quarter(i, j, q, cache, args)
        out = self.work / "vegetation" / (self.collections["vegetation"] + ".new")
        if not (out / f"pave-{index:03d}.json").exists():
            missing = [q for q in self.quarters[(i, j)] if not (pave / "veg" / f"q{q}" / "manifest.json").exists()
                       and json.loads((self.quarter_root(i, j, q) / "done.json").read_text()).get("vegetation")]
            if missing:
                raise SystemExit(f"pavé {i},{j} : lots de végétation absents pour les quarts {missing} (supprimer leur done.json)")
            batches = [pave / "veg" / f"q{q}" for q in self.quarters[(i, j)] if (pave / "veg" / f"q{q}" / "manifest.json").exists()]
            info = vegbatch.write_pave(out, index, batches, self.collections["vegetation"], i, j)
            log(stage="vegetation", pave=[i, j], records=info["records"], parts=len(info["parts"]))
        shutil.rmtree(pave / "veg", ignore_errors=True)
        self.buildings(i, j, args)

    def buildings(self, i, j, args):
        import terrain
        pave = self.pave_dir(i, j)
        b = pave / "buildings"
        near, fine = self.work / "buildings" / f"{i}_{j}.near.ndjson", self.work / "buildings" / f"{i}_{j}.fine.ndjson"
        if (self.work / "buildings" / f"{i}_{j}.done").exists():
            return
        if not args.buildings:
            return
        (self.work / "buildings").mkdir(exist_ok=True)
        self.scene_root(pave, i, j)
        b.mkdir(parents=True, exist_ok=True)
        # le contour dans le repère du pavé
        rings = [(r - np.array([plan.PAVE * i, plan.PAVE * j])).tolist() for r in self.zone.rings]
        (b / "zone.json").write_text(json.dumps({"rings": rings}))
        bands = sorted(b.glob("band-q*.npy"))
        roof = np.concatenate([np.load(f) for f in bands]) if bands else np.zeros((0, 3), np.float32)
        np.savez(b / "parts" / "margin.npz", roof=roof)
        # le sol du pavé (et 256 m autour, ce qui en est fait)
        quarters = terrain.Quarters(sorted((self.work / "ground").glob("*.npz")))
        margin = 256.0
        n = int((2048 + 2 * margin) / 0.5)
        d, _ = terrain.dtm(quarters, plan.PAVE * i - 1024 - margin, plan.PAVE * j - 1024 - margin, n)
        k = int(margin / 0.5)
        np.save(b / "dtm.npy", d[k:k + 4096, k:k + 4096])
        del d
        exp = self.exp / "scripts/world/buildings"
        tables = exp / "space-tables.json"
        python = args.python_buildings or sys.executable
        rc = subprocess.run([python, str(exp / "buildings.py"), str(pave), str(b), str(tables), "0:64"], cwd=exp).returncode
        if rc == 3:  # no building footprint in this pavé (sea, river, beach)
            near.write_bytes(b"")
            fine.write_bytes(b"")
            shutil.rmtree(b)
            (self.work / "buildings" / f"{i}_{j}.done").write_text("0")
            log(stage="buildings", pave=[i, j], bytes=0, note="no building")
            return
        if rc != 0:
            raise subprocess.CalledProcessError(rc, "buildings.py")
        subprocess.run([python, str(exp / "buildings.py"), str(pave), str(b), str(tables), "--assemble", str(near), str(fine),
                        "--collections", self.collections["buildings"], self.collections["fine"]], check=True, cwd=exp)
        shutil.rmtree(b)
        (self.work / "buildings" / f"{i}_{j}.done").write_text("1")
        log(stage="buildings", pave=[i, j], bytes=near.stat().st_size + fine.stat().st_size)

    # --- étape 3 ---
    def ground(self):
        import terrain
        out = self.work / "terrain.ndjson"
        key = self.work / "terrain.key"
        files = sorted((self.work / "ground").glob("*.npz"))
        stamp = json.dumps([f.name for f in files] + [returns_sha(HERE / "terrain.py")])
        if out.exists() and key.exists() and key.read_text() == stamp:
            return out
        quarters = terrain.Quarters(files)
        kept, heights, n = [], [], 0
        tmp = Path(str(out) + ".tmp")
        with tmp.open("w") as fh:
            for i, j in self.paves:
                written, k, h = terrain.pave(quarters, i, j, self.entry["origin"], self.entry["altitude"], fh, self.collections["terrain"])
                n += written
                if k is not None:
                    kept.append(k)
                    heights.append(h)
                log(stage="terrain", pave=[i, j], tiles=written)
            n += terrain.coarse_levels(kept, fh, self.collections["terrain"])
        os.replace(tmp, out)
        key.write_text(stamp)
        lo, hi = min(h[0] for h in heights), max(h[1] for h in heights)
        (self.work / "heights.json").write_text(json.dumps({"ground": [round(lo, 2), round(hi, 2)]}))
        log(stage="terrain", tiles=n, ground=[round(lo, 2), round(hi, 2)])
        return out

    def ortho(self):
        sys.path.insert(0, str(self.exp / "scripts/world"))
        import ortho
        import space
        out = self.work / "ortho.ndjson"
        if out.exists() and (self.work / "ortho.done").exists():
            return out
        levels = tuple(self.entry.get("orthoLevels", [12, 18]))
        fine = levels[1]
        tiles = level_cells(self.zone, self.frame, fine, space)
        source = ZoneTiles(ortho.MercatorTiles(self.work / "points-imagery"), self.zone, self.mask, self.frame, space)
        stats = ortho.build(source, *self.entry["origin"], 0.0, out, levels, 92, self.collections["ortho"], tiles=tiles)
        (self.work / "ortho.done").write_text(json.dumps(stats))
        log(stage="ortho", **{k: v for k, v in stats.items() if k != "levels"})
        return out


def returns_sha(path):
    import hashlib
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def level_cells(zone, frame, t, space):
    """Les cellules (row, col) du niveau t du Space gps qui touchent la zone (le même test qu'orthoCoverage dans Explore)."""
    x0, y0, x1, y1 = zone.bounds
    lat_s, lng_w = frame.to_degrees(x0, y0)
    lat_n, lng_e = frame.to_degrees(x1, y1)
    r0, c0 = (int(v) for v in space.cell_index(float(lat_s), float(lng_w), t))
    r1, c1 = (int(v) for v in space.cell_index(float(lat_n), float(lng_e), t))
    out = []
    for row in range(r0, r1 + 1):
        s, n, _, _ = space.bounds(row, 0, t)
        _, ys = frame.to_metres(s, frame.lon0)
        _, yn = frame.to_metres(n, frame.lon0)
        for col in range(c0, c1 + 1):
            _, _, w, e = space.bounds(row, col, t)
            xw, _ = frame.to_metres(frame.lat0, w)
            xe, _ = frame.to_metres(frame.lat0, e)
            if zone.rect_distance(float(xw), float(ys), float(xe), float(yn)) <= 0:
                out.append((row, col))
    return out


class ZoneTiles:
    """La source de l'orthophoto (ortho.MercatorTiles) coupée au contour : alpha nul hors de la zone."""

    def __init__(self, source, zone, mask, frame, space):
        self.source, self.zone, self.mask, self.frame, self.space = source, zone, mask, frame, space

    def read(self, t, row, col):
        rgba = self.source.read(t, row, col)
        if rgba is None:
            return None
        s, n, w, e = self.space.bounds(row, col, t)
        h, wd = rgba.shape[:2]
        lat = n - (np.arange(h) + 0.5) / h * (n - s)
        lng = w + (np.arange(wd) + 0.5) / wd * (e - w)
        _, y = self.frame.to_metres(lat, np.full(h, self.frame.lon0))
        x, _ = self.frame.to_metres(np.full(wd, self.frame.lat0), lng)
        if zones.rect_inside(self.zone, float(x.min()), float(y.min()), float(x.max()), float(y.max())):
            return rgba
        X, Y = np.meshgrid(x, y)
        inside = self.mask.contains(X.ravel(), Y.ravel()).reshape(h, wd)
        if not inside.any():
            return None
        out = rgba.copy()
        out[..., 3] = np.where(inside, out[..., 3], 0)
        return out


def free_bytes(path):
    p = Path(path)
    while not p.exists():
        p = p.parent
    return shutil.disk_usage(p).free


def check_space(path, need, what):
    free = free_bytes(path)
    if free < need:
        raise SystemExit(f"place libre insuffisante pour {what} : {free / GB:.1f} Go libres, {need / GB:.1f} Go demandés ({path})")


def vegetation_batch(c, out):
    """Le lot de végétation : zones.ixp et manifest.json des pavés écrits, puis ses fichiers dans out/<collection>
    (les fiches des pavés restent dans le travail : une reprise ne refait aucun pavé)."""
    import hashlib
    name = c.collections["vegetation"]
    work = c.work / "vegetation" / (name + ".new")
    final = Path(out) / name
    infos = [json.loads(p.read_text()) for p in sorted(work.glob("pave-[0-9][0-9][0-9].json"))]
    if len(infos) != len(c.paves):
        raise SystemExit(f"végétation : {len(infos)} pavés sur {len(c.paves)}")
    shas = hashlib.sha256("".join(p["sha256"] for info in infos for p in info["parts"]).encode()).hexdigest()
    inputs = {"tool": "prep commune vegetation 1", "sources": {"parts": shas, "vegbatch.py": returns_sha(HERE / "vegbatch.py")},
              "options": {"collection": name, "space": json.dumps(vegbatch.SPACE, sort_keys=True)}}
    bid = hashlib.sha256(json.dumps(inputs, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    if (final / "manifest.json").exists() and json.loads((final / "manifest.json").read_text()).get("id") == bid:
        log(stage="skip", collection=name, id=bid[:16], reason="same inputs")
        return
    man = vegbatch.finish(work, name, inputs)
    staging = Path(out) / (name + ".new")
    shutil.rmtree(staging, ignore_errors=True)
    staging.mkdir(parents=True)
    for f in [p["file"] for p in man["parts"]] + ["zones.ixp", "manifest.json"]:
        os.replace(work / f, staging / f)
    if final.exists():
        shutil.rmtree(final)
    os.replace(staging, final)
    log(stage="batch", collection=name, records=man["records"], parts=len(man["parts"]), id=man["id"][:16])


def batches(c, prep, out):
    """Les lots : végétation, puis les NDJSON par prep ndjson (un lot inchangé est sauté)."""
    vegetation_batch(c, out)
    files = [c.work / "terrain.ndjson", c.work / "ortho.ndjson"]
    for kind in ("near", "fine"):
        target = c.work / "buildings" / f"{kind}.ndjson"
        parts = sorted(p for p in (c.work / "buildings").glob(f"*_*.{kind}.ndjson"))
        if parts:
            tmp = Path(str(target) + ".tmp")
            with tmp.open("wb") as fh:
                for p in parts:
                    with p.open("rb") as src:
                        shutil.copyfileobj(src, fh)
            os.replace(tmp, target)
            files.append(target)
    files = [f for f in files if f.exists()]
    env = dict(os.environ, GOMEMLIMIT=os.environ.get("PREP_GOMEMLIMIT", "1500MiB"))
    subprocess.run([prep, "ndjson", "-out", str(out), *map(str, files)], check=True, env=env)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    ap.add_argument("command", choices=["plan", "run"])
    ap.add_argument("catalogue", type=Path)
    ap.add_argument("scene")
    ap.add_argument("--data", type=Path, required=True)
    ap.add_argument("--exp", type=Path, help="experiences (buildings.py, ortho.py)")
    ap.add_argument("--prep", default="prep", help="le binaire prep (tools/prep)")
    ap.add_argument("--metal", default=None, help="prep-metal (végétation sur le GPU) ; sans : numpy")
    ap.add_argument("--workers", type=int, default=None)
    ap.add_argument("--python-buildings", default=None, help="le python des bâtiments (matplotlib) ; défaut : celui-ci")
    ap.add_argument("--no-buildings", dest="buildings", action="store_false")
    ap.add_argument("--yes", action="store_true", help="ne pas demander avant de télécharger")
    args = ap.parse_args()
    args.backend = "metal" if args.metal else "numpy"
    c = Commune(args.catalogue, args.scene, args.data, args.exp)
    est = c.estimate()
    free = free_bytes(args.data)
    log(stage="plan", scene=c.entry["id"], label=c.entry.get("label"), **est, freeGB=round(free / 1e9, 1),
        order=[list(p) for p in c.paves])
    if args.command == "plan":
        return
    if free < est["peakDiskGB"] * 1e9 + 2 * GB:
        raise SystemExit(f"place libre : {free / 1e9:.1f} Go ; il en faut environ {est['peakDiskGB'] + 2:.0f} (pic estimé + 2 Go)")
    if not args.yes and sys.stdin.isatty():
        answer = input(f"Télécharger environ {est['downloadGB']} Go (LiDAR HD) et préparer {est['areaKm2']} km² ? [o/N] ")
        if answer.strip().lower() not in ("o", "oui", "y", "yes"):
            raise SystemExit("arrêt demandé")
    import fetch
    c.sources()
    cache = fetch.RangeCache()
    for index, (i, j) in enumerate(c.paves):
        c.pave(index, i, j, cache, args)
    c.ground()
    c.ortho()
    batches(c, args.prep, args.data / "batches")
    log(stage="done", work=str(c.work), batches=str(args.data / "batches"), download=cache.stats)


if __name__ == "__main__":
    main()
