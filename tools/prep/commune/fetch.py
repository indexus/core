"""Ce que la préparation d'une commune télécharge (réseau : sur le Mac, pas dans les tests).

  boundary   le contour officiel (geo.api.gouv.fr, contour de la commune, Etalab 2.0)
  dalles     les dalles LiDAR HD de 1 km qui le touchent : nom, URL COPC, nombre de points
             (WFS IGNF_LIDAR-HD_METADONNEE:metadata, celui de fetch-scenes.py)
  ortho      les tuiles Web Mercator z18 de 512 px de BD ORTHO (WMS, comme
             prepare-ortho-tiles.py) sur le contour + marge, opaques ; le manifest de
             points-imagery que lisent returns.py, buildings et ortho.py
  bdtopo     les emprises BD TOPO (batiment) de l'emprise, par pages
  quarter    les retours LiDAR d'un quart (COPC, requêtes par boîte, reprise par boîte) :
             un fichier .lasrecords.gz par tuile de 32 m (les enregistrements LAS d'origine,
             échelle 0.01 m et décalage 0, ce que returns.py lit), classes 2 à 6 seulement ;
             la végétation (3 à 5) dans le contour, le sol (2) et les bâtiments (6) à
             BAND m près ; les bâtiments (6) hors du pavé à part (margin), pour les toits
             des bâtiments du bord

Tout est idempotent : un fichier présent n'est pas refait, un fichier s'écrit en .part
puis se renomme. HTTP : web.py (bibliothèque standard) ; laspy[lazrs] : tools/prep/requirements.txt.
"""

import gzip
import hashlib
import io
import json
import math
import os
import threading
import time
from collections import OrderedDict
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from pathlib import Path

import numpy as np

import lambert
import plan
import web
import zone as zones

GEO_API = "https://geo.api.gouv.fr/communes/{code}?format=geojson&geometry=contour"
WFS = "https://data.geopf.fr/wfs/ows"
WMS = "https://data.geopf.fr/wms-r"
WORLD = 40075016.68557849
KEEP = (2, 3, 4, 5, 6)


def atomic_write(path, data):
    path = Path(path)
    tmp = path.with_name(path.name + ".part")
    tmp.write_bytes(data if isinstance(data, bytes) else data.encode())
    os.replace(tmp, path)


def session(retries=8):
    """Connexion, coupure avant les en-têtes, 429/5xx : refaits `retries` fois (web.Session, la politique
    de urllib3.Retry d'avant : pause 2 x 2^(n-1) s, au plus 120 s, Retry-After respecté)."""
    return web.Session(retries=retries, backoff_factor=2, backoff_max=120, status_forcelist=(429, 500, 502, 503, 504))


# --- le contour ------------------------------------------------------------------
def boundary(code, out):
    """Le contour de la commune `code` (INSEE) dans `out` (GeoJSON Feature), s'il n'y est pas."""
    out = Path(out)
    if out.exists():
        return json.loads(out.read_text())
    r = session().get(GEO_API.format(code=code), timeout=60)
    r.raise_for_status()
    d = r.json()
    if d.get("type") != "Feature" or d.get("geometry", {}).get("type") not in ("Polygon", "MultiPolygon"):
        raise ValueError(f"geo.api.gouv.fr : réponse inattendue {str(d)[:200]}")
    out.parent.mkdir(parents=True, exist_ok=True)
    atomic_write(out, json.dumps(d, ensure_ascii=False))
    return d


# --- les dalles ------------------------------------------------------------------
def wfs_pages(params, page=1000):
    """Toutes les entités d'une requête WFS GetFeature (par pages)."""
    s, features, urls = session(), [], []
    while True:
        q = dict(params, COUNT=page, STARTINDEX=len(features))
        r = s.get(WFS, params=q, timeout=120)
        r.raise_for_status()
        d = r.json()
        if d.get("type") != "FeatureCollection":
            raise ValueError(str(d)[:300])
        batch = d["features"]
        features.extend(batch)
        urls.append(r.url)
        matched = d.get("numberMatched")
        if not batch or (isinstance(matched, int) and len(features) >= matched) or len(batch) < page:
            return features, urls


def lnglat_box(zone_lnglat_rings, margin_deg=0.002):
    a = np.concatenate(zone_lnglat_rings)
    return [float(a[:, 0].min() - margin_deg), float(a[:, 1].min() - margin_deg), float(a[:, 0].max() + margin_deg), float(a[:, 1].max() + margin_deg)]


def dalles(geometry, out, margin=plan.BAND):
    """Les dalles LiDAR HD qui touchent le contour à `margin` m près : [{name, kx, ky, url, points}] dans `out`."""
    out = Path(out)
    if out.exists():
        return json.loads(out.read_text())
    rings = zones.geometry_rings(geometry)
    l93 = zones.Polygon([np.stack(lambert.to_l93(r[:, 0], r[:, 1]), 1) for r in rings])
    wanted = {lambert.dalle_name(kx, ky): (kx, ky) for kx, ky in lambert.dalles_of_zone(l93, margin)}
    box = lnglat_box(rings)
    features, urls = wfs_pages({"SERVICE": "WFS", "VERSION": "2.0.0", "REQUEST": "GetFeature",
                                "TYPENAMES": "IGNF_LIDAR-HD_METADONNEE:metadata", "OUTPUTFORMAT": "application/json",
                                "SRSNAME": "EPSG:4326", "BBOX": ",".join(map(str, box)) + ",urn:ogc:def:crs:OGC::CRS84"})
    found = {}
    for f in features:
        p = f["properties"]
        url = p.get("url_npl") or ""
        name = Path(url).name
        parts = name.split("_")
        if len(parts) < 4 or not parts[2].isdigit():
            continue
        key = f"{parts[2]}_{parts[3]}"
        if key in wanted:
            found[key] = {"name": key, "kx": wanted[key][0], "ky": wanted[key][1], "url": url,
                          "points": int(p.get("nombre_points") or 0), "edition": p.get("date_edition")}
    missing = sorted(set(wanted) - set(found))
    result = {"dalles": [found[k] for k in sorted(found)], "missing": missing, "source": urls,
              "retrievedAt": datetime.now(timezone.utc).isoformat(), "attribution": "LiDAR HD © IGN, Licence Ouverte 2.0"}
    atomic_write(out, json.dumps(result, indent=1))
    return result


# --- l'orthophoto ----------------------------------------------------------------
def z18_tiles(zone, frame, margin, z=18):
    """Les tuiles Web Mercator (x, y) de niveau z qui touchent la zone (repère de la scène) à `margin` m près."""
    span = WORLD / 2**z
    x0, y0, x1, y1 = zone.bounds
    lat_s, lng_w = frame.to_degrees(x0 - margin, y0 - margin)
    lat_n, lng_e = frame.to_degrees(x1 + margin, y1 + margin)
    mx = lambda lng: float(lng) / 360 * WORLD + WORLD / 2  # noqa: E731
    my = lambda lat: WORLD / 2 - math.asinh(math.tan(math.radians(float(lat)))) / (2 * math.pi) * WORLD  # noqa: E731
    out = []
    for tx in range(int(mx(lng_w) // span), int(mx(lng_e) // span) + 1):
        for ty in range(int(my(lat_n) // span), int(my(lat_s) // span) + 1):
            # le carré de la tuile dans le repère de la scène (Mercator x cos0 : linéaire)
            west, east = tx * span - WORLD / 2, (tx + 1) * span - WORLD / 2
            north, south = WORLD / 2 - ty * span, WORLD / 2 - (ty + 1) * span
            k = frame.cos0
            r = ((west - frame.ox) * k - frame.shift[0], (south - frame.oy) * k - frame.shift[1],
                 (east - frame.ox) * k - frame.shift[0], (north - frame.oy) * k - frame.shift[1])
            if zone.rect_distance(*r) <= margin:
                out.append((tx, ty))
    return out


def ortho(zone, frame, directory, margin=96.0, size=512, workers=4, log=print):
    """Tuiles z18 opaques (WebP q92) et points-imagery/manifest.json (indexus-orthophoto-v1 + tiles)."""
    from PIL import Image

    directory = Path(directory)
    (directory / "tiles").mkdir(parents=True, exist_ok=True)
    jobs = z18_tiles(zone, frame, margin)
    span = WORLD / 2**18
    s = session()
    lock = threading.Lock()
    done = [0]

    def fetch(job):
        tx, ty = job
        key = f"18/{tx}/{ty}"
        existing = sorted((directory / "tiles").glob(f"18-{tx}-{ty}-*.webp"))
        if not existing:
            left, top = tx * span - WORLD / 2, WORLD / 2 - ty * span
            params = {"SERVICE": "WMS", "VERSION": "1.3.0", "REQUEST": "GetMap", "LAYERS": "ORTHOIMAGERY.ORTHOPHOTOS",
                      "STYLES": "", "CRS": "EPSG:3857", "BBOX": f"{left},{top - span},{left + span},{top}",
                      "WIDTH": size, "HEIGHT": size, "FORMAT": "image/jpeg"}
            for attempt in range(5):
                try:
                    r = s.get(WMS, params=params, timeout=90)
                    r.raise_for_status()
                    image = Image.open(io.BytesIO(r.content)).convert("RGB")
                    if image.size != (size, size):
                        raise ValueError("dimensions inattendues")
                    break
                except Exception:
                    if attempt == 4:
                        raise
                    time.sleep(2**attempt)
            stream = io.BytesIO()
            image.save(stream, format="WEBP", quality=92, method=4)
            raw = stream.getvalue()
            name = f"18-{tx}-{ty}-{hashlib.sha256(raw).hexdigest()[:16]}.webp"
            atomic_write(directory / "tiles" / name, raw)
            existing = [directory / "tiles" / name]
        raw = existing[-1].read_bytes()
        with lock:
            done[0] += 1
            if done[0] % 200 == 0:
                log(stage="ortho", tiles=done[0], total=len(jobs))
        return {"key": key, "url": f"tiles/{existing[-1].name}", "bytes": len(raw), "sha256": hashlib.sha256(raw).hexdigest()}

    with ThreadPoolExecutor(max_workers=workers) as pool:
        files = list(pool.map(fetch, jobs))
    manifest = {"schema": "indexus-orthophoto-v1", "origin": [frame.lon0, frame.lat0], "source": "IGN BD ORTHO, aerial orthophotography",
                "layer": "ORTHOIMAGERY.ORTHOPHOTOS", "attribution": "Orthophotographie © IGN", "crs": "EPSG:3857",
                "tiles": {"schema": "indexus-orthophoto-tiles-v1", "tileSize": size, "minzoom": 18, "maxzoom": 18,
                          "sampleSpacingMetres": span / size * frame.cos0, "source": "IGN BD ORTHO / WMS", "files": files}}
    atomic_write(directory / "manifest.json", json.dumps(manifest, separators=(",", ":")))
    return manifest


# --- BD TOPO -----------------------------------------------------------------------
def bdtopo(geometry, out, layer="batiment"):
    """Les entités BD TOPO de l'emprise du contour (avec une marge), dans `out`."""
    out = Path(out)
    if out.exists():
        return
    box = lnglat_box(zones.geometry_rings(geometry), 0.003)
    features, urls = wfs_pages({"SERVICE": "WFS", "VERSION": "2.0.0", "REQUEST": "GetFeature", "TYPENAMES": "BDTOPO_V3:" + layer,
                                "SRSNAME": "EPSG:4326", "OUTPUTFORMAT": "application/json",
                                "BBOX": ",".join(map(str, box)) + ",urn:ogc:def:crs:OGC::CRS84"}, page=2000)
    out.parent.mkdir(parents=True, exist_ok=True)
    atomic_write(out, json.dumps({"type": "FeatureCollection", "features": features}, separators=(",", ":")))
    atomic_write(out.with_name(out.stem + "-source.json"), json.dumps({"url": urls, "features": len(features), "attribution": "BD TOPO © IGN",
                                                                       "retrievedAt": datetime.now(timezone.utc).isoformat()}))


# --- COPC ------------------------------------------------------------------------
class RangeCache:
    """Les plages HTTP lues (nœuds COPC), en mémoire, jusqu'à `limit` octets (LRU) : une boîte relit les
    ancêtres et les nœuds au bord de la boîte voisine sans les télécharger. Une pause entre deux
    requêtes ménage le service public."""

    def __init__(self, limit=768 << 20, pause=0.05):
        self.limit, self.pause, self.bytes, self.lock = limit, pause, 0, threading.Lock()
        self.items, self.last = OrderedDict(), 0.0
        self.stats = {"requests": 0, "downloaded": 0, "hits": 0}
        self.session = session()

    def get(self, url, start, end, timeout=(20, 120)):
        """Les octets [start, end] (inclus) de `url`, du cache ou du réseau."""
        key = (url, f"bytes={start}-{end}")
        with self.lock:
            data = self.items.get(key)
            if data is not None:
                self.items.move_to_end(key)
                self.stats["hits"] += 1
                return data
        with self.lock:
            wait = self.pause - (time.time() - self.last)
            self.last = time.time() + max(wait, 0)
        if wait > 0:
            time.sleep(wait)
        # A body cut mid-transfer (connection reset, incomplete read, read timeout) is
        # not retried by the session: retried here, with a growing pause.
        for attempt in range(10):
            try:
                r = self.session.get(url, headers={"Range": key[1]}, timeout=timeout)
                r.raise_for_status()
                break
            except web.TransportError:
                if attempt == 9:
                    raise
                web.sleep(min(120, 2 * 2 ** attempt))
        data = r.content
        with self.lock:
            self.stats["requests"] += 1
            self.stats["downloaded"] += len(data)
            if len(data) <= 64 << 20:
                self.items[key] = data
                self.bytes += len(data)
                while self.bytes > self.limit and self.items:
                    _, old = self.items.popitem(last=False)
                    self.bytes -= len(old)
        return data


class RemoteFile(io.RawIOBase):
    """Une dalle COPC distante comme un fichier (seek, read) : chaque lecture est une plage HTTP,
    passée par le RangeCache. Ce que CopcReader lit d'un fichier local, il le lit ici à distance
    (COMMUNE_REMOTE_COPC=1), sans requests."""

    def __init__(self, url, cache):
        super().__init__()
        self.url, self.cache, self.offset = url, cache, 0
        r = cache.session.head(url, timeout=60, allow_redirects=True)
        r.raise_for_status()
        self.size = int(r.headers["Content-Length"])

    def readable(self):
        return True

    def seekable(self):
        return True

    def tell(self):
        return self.offset

    def seek(self, offset, whence=io.SEEK_SET):
        base = {io.SEEK_SET: 0, io.SEEK_CUR: self.offset, io.SEEK_END: self.size}[whence]
        if base + offset < 0:
            raise ValueError("position négative")
        self.offset = base + offset
        return self.offset

    def read(self, n=-1):
        end = self.size if n is None or n < 0 else min(self.size, self.offset + n)
        if end <= self.offset:
            return b""
        data = self.cache.get(self.url, self.offset, end - 1)
        if len(data) != end - self.offset:
            raise web.TransportError(f"{self.url}: {len(data)} octets au lieu de {end - self.offset}")
        self.offset = end
        return data

    def readinto(self, buffer):
        data = self.read(len(buffer))
        buffer[: len(data)] = data
        return len(data)


TILE_CHUNK = 32 << 20


def local_tile(d, tiles, keep=None, streams=None, log=print):
    """La dalle COPC entière sur le disque (tiles/<nom>.copc.laz), téléchargée en morceaux de
    32 Mo sur plusieurs flux. Lire par requêtes COPC à distance coûtait des milliers de petites
    plages, donc la latence : ~1 Mo/s quand le lien en donne 25 à 100. Une dalle fait ~0,5 Go ;
    on en garde `keep` (COMMUNE_TILES_KEEP, 6 par défaut), les plus anciennes partent."""
    keep = keep or int(os.environ.get("COMMUNE_TILES_KEEP", "6"))
    streams = streams or int(os.environ.get("COMMUNE_TILE_STREAMS", "6"))
    tiles = Path(tiles)
    tiles.mkdir(parents=True, exist_ok=True)
    path = tiles / f"{d['name']}.copc.laz"
    if path.exists():
        os.utime(path)
        return path
    old = sorted(tiles.glob("*.copc.laz"), key=lambda f: f.stat().st_mtime)
    for f in old[: max(0, len(old) - (keep - 1))]:
        f.unlink()
    s = session()
    size = int(s.head(d["url"], timeout=60, allow_redirects=True).headers["Content-Length"])
    part = path.with_suffix(".part")
    with open(part, "wb") as f:
        f.truncate(size)
    t0 = time.time()

    def fetch(offset):
        end = min(size, offset + TILE_CHUNK) - 1
        for attempt in range(10):
            try:
                r = s.get(d["url"], headers={"Range": f"bytes={offset}-{end}"}, timeout=(20, 180))
                r.raise_for_status()
                body = r.content
                if len(body) != end - offset + 1:
                    raise web.TransportError(f"{len(body)} octets au lieu de {end - offset + 1}")
                with open(part, "r+b") as f:
                    f.seek(offset)
                    f.write(body)
                return len(body)
            except web.RequestError:
                if attempt == 9:
                    raise
                web.sleep(min(120, 2 * 2 ** attempt))

    with ThreadPoolExecutor(streams) as pool:
        got = sum(pool.map(fetch, range(0, size, TILE_CHUNK)))
    if got != size:
        raise RuntimeError(f"{d['name']}: {got} octets sur {size}")
    os.replace(part, path)
    seconds = time.time() - t0
    log(stage="tile", name=d["name"], MB=round(size / 1e6), seconds=round(seconds, 1), MBps=round(size / 1e6 / max(seconds, 1e-3), 1))
    return path


def corners_l93(box, frame):
    """La boîte (repère du pavé) en Lambert-93 : [x0, y0, x1, y1] de ses coins, plus 2 m."""
    x0, y0, x1, y1 = box
    lat, lng = frame.to_degrees(np.array([x0, x1, x0, x1]), np.array([y0, y0, y1, y1]))
    X, Y = lambert.to_l93(lng, lat)
    return float(X.min() - 2), float(Y.min() - 2), float(X.max() + 2), float(Y.max() + 2)


def quarter(root, scene, dalles_list, zone, mask, i, j, q, cache, log=print):
    """Les retours du quart q du pavé (i, j) sous `root` (la disposition d'experiences pour un
    scène : data/world/anglet-1km, public/world/anglet-1km/points-raw/<sha>/). Reprise par boîte."""
    import laspy
    from laspy.copc import Bounds, CopcReader

    root = Path(root)
    data = root / "data/world/anglet-1km"
    blocks = data / "blocks"
    blocks.mkdir(parents=True, exist_ok=True)
    shift = (plan.PAVE * i, plan.PAVE * j)
    frame = zones.Frame(scene["origin"], shift)
    todo = plan.boxes(zone, i, j, q)
    qx0, qy0, qx1, qy1 = plan.quarter_square(q)
    readers, header = {}, None

    def reader(d):
        if d["name"] not in readers:
            if os.environ.get("COMMUNE_REMOTE_COPC") == "1":
                r = CopcReader(RemoteFile(d["url"], cache))
            else:
                r = CopcReader.open(str(local_tile(d, root.parents[2] / "tiles", log=log)))
            if lambert.header_epsg(r.header) != 2154:
                raise ValueError(f"{d['url']}: système de coordonnées inattendu")
            readers[d["name"]] = r
        return readers[d["name"]]

    meta = data / "header.json"
    try:
        for n, box in enumerate(todo):
            check = data / f"box-{int(box[0])}-{int(box[1])}.json"
            if check.exists():
                continue
            lx0, ly0, lx1, ly1 = corners_l93(box, frame)
            parts = []
            for d in dalles_list:
                b = lambert.dalle_bounds(d["kx"], d["ky"])
                if b[2] < lx0 or b[0] > lx1 or b[3] < ly0 or b[1] > ly1:
                    continue
                r = reader(d)
                pts = r.query(bounds=Bounds(np.array([lx0, ly0, -1000.0]), np.array([lx1, ly1, 9000.0])))
                if not len(pts):
                    continue
                if header is None:
                    header = laspy.LasHeader(point_format=r.header.point_format, version=r.header.version)
                    header.scales = np.array([0.01, 0.01, 0.01])
                    header.offsets = np.array([0.0, 0.0, 0.0])
                    meta.write_text(json.dumps({"dtype": header.point_format.dtype().descr, "pointFormat": header.point_format.id,
                                                "scales": [0.01] * 3, "offsets": [0.0] * 3}))
                if pts.point_format.id != header.point_format.id:
                    raise RuntimeError("formats de points LAS mélangés")
                normal = laspy.ScaleAwarePointRecord.zeros(len(pts), header=header)
                for name in header.point_format.dimension_names:
                    if name not in ("X", "Y", "Z"):
                        normal[name] = pts[name]
                normal.x, normal.y, normal.z = pts.x, pts.y, pts.z  # sur la grille de 1 cm, décalage 0
                parts.append(normal.array)
            entries, band = [], []
            if parts:
                p = np.concatenate(parts)
                x, y = lambert.l93_to_local(p["X"] * 0.01, p["Y"] * 0.01, scene["origin"], shift)
                cls = p["classification"]
                inbox = (x >= box[0]) & (x < box[2]) & (y >= box[1]) & (y < box[3])
                keep = inbox & np.isin(cls, KEEP)
                gx, gy = x + shift[0], y + shift[1]
                inside = np.zeros(len(p), bool)
                inside[keep] = mask.contains(gx[keep], gy[keep])
                near = np.zeros(len(p), bool)
                near[keep] = mask.near(gx[keep], gy[keep])
                veg = (cls >= 3) & (cls <= 5)
                keep &= np.where(veg, inside, near)
                inq = (x >= qx0) & (x < qx1) & (y >= qy0) & (y < qy1)
                out_pave = (x < -1024) | (x >= 1024) | (y < -1024) | (y >= 1024)
                # les toits hors du pavé : pour les bâtiments du bord (buildings.py, parts/margin.npz)
                b = keep & out_pave & (cls == 6)
                if b.any():
                    band = np.stack([x[b], y[b], p["Z"][b] * 0.01 - scene["altitude"]], 1).astype(np.float32)
                sel = keep & inq
                ix, iy = plan.tile_of(x[sel], y[sel])
                recs = p[sel]
                key = ix * 64 + iy
                for k in np.unique(key):
                    a, c = divmod(int(k), 64)
                    chunk = recs[key == k]
                    body = chunk.tobytes()
                    name = f"{a}-{c}.lasrecords.gz"
                    atomic_write(blocks / name, gzip.compress(body, compresslevel=1, mtime=0))
                    classes, counts = np.unique(chunk["classification"], return_counts=True)
                    entries.append({"url": name, "bounds": [a * 32 - 1024, c * 32 - 1024, (a + 1) * 32 - 1024, (c + 1) * 32 - 1024],
                                    "count": int(len(chunk)), "decodedBytes": len(body), "sha256": hashlib.sha256(body).hexdigest(),
                                    "classificationCounts": dict(zip(map(str, classes.tolist()), map(int, counts)))})
            if len(band):
                np.save(data / f"band-{int(box[0])}-{int(box[1])}.npy", band)
            atomic_write(check, json.dumps({"box": box, "files": entries}, separators=(",", ":")))
            if n % 8 == 0:
                log(stage="lidar", pave=[i, j], quarter=q, box=n + 1, boxes=len(todo), points=sum(e["count"] for e in entries),
                    downloadedMB=round(cache.stats["downloaded"] / 1e6), cacheHits=cache.stats["hits"])
    finally:
        for r in readers.values():
            r.__exit__(None, None, None)
    files = sorted((f for c in data.glob("box-*.json") for f in json.loads(c.read_text())["files"]), key=lambda f: f["url"])
    return files
