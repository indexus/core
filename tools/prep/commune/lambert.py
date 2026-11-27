"""Lambert-93 (RGF93, ellipsoïde GRS80), les formules de l'IGN (ALG0003 et
ALG0004), et la grille des dalles LiDAR HD de 1 km.

Une dalle LiDAR HD s'appelle LHD_FXX_<x>_<y>_PTS_..._LAMB93_IGN69 : x est le
kilomètre de son bord ouest, y celui de son bord nord (la dalle 0334_6276
couvre [334 000, 335 000] x [6 275 000, 6 276 000]).

Le repère de la scène : returns.l93_to_local (la même arithmétique, ici avec
le décalage des pavés).

Précision (test_lambert.py) : à moins de 0,15 mm des formules de Snyder (USGS PP 1395)
aux constantes exactes, sur 10 000 points de la France ; l'origine (3° E, 46,5° N)
tombe à 0,12 mm de (700 000, 6 600 000). header_epsg remplace
LasHeader.parse_crs().to_epsg() (pyproj) pour vérifier qu'une dalle est en Lambert-93.
"""

import math
import re
import struct

import numpy as np

N, C, XS, YS, E = 0.7256077650, 11754255.426, 700000.0, 12655612.050, 0.0818191910428158
LON0 = math.radians(3.0)
EARTH = 6378137.0


def to_l93(lng, lat):
    """(x, y) Lambert-93 de degrés WGS84 (RGF93 : la même chose à moins d'un mètre)."""
    lam, phi = np.radians(np.asarray(lng, np.float64)), np.radians(np.asarray(lat, np.float64))
    s = np.sin(phi)
    iso = np.log(np.tan(math.pi / 4 + phi / 2) * ((1 - E * s) / (1 + E * s)) ** (E / 2))
    r = C * np.exp(-N * iso)
    g = N * (lam - LON0)
    return XS + r * np.sin(g), YS - r * np.cos(g)


def from_l93(x, y):
    """(lng, lat) degrés de Lambert-93 (latitude itérée, comme returns.l93_to_local)."""
    dx, dy = np.asarray(x, np.float64) - XS, YS - np.asarray(y, np.float64)
    lon = LON0 + np.arctan2(dx, dy) / N
    iso = -np.log(np.hypot(dx, dy) / C) / N
    lat = 2 * np.arctan(np.exp(iso)) - math.pi / 2
    for _ in range(6):
        s = np.sin(lat)
        lat = 2 * np.arctan(((1 + E * s) / (1 - E * s)) ** (E / 2) * np.exp(iso)) - math.pi / 2
    return np.degrees(lon), np.degrees(lat)


def l93_to_local(x, y, origin, shift=(0.0, 0.0)):
    """Mètres de la scène (moins shift) de Lambert-93 : l'arithmétique de returns.l93_to_local, pas à pas."""
    lon0, lat0 = origin
    cos0 = math.cos(math.radians(lat0))
    ox = EARTH * math.radians(lon0)
    oy = EARTH * math.log(math.tan(math.pi / 4 + math.radians(lat0) / 2))
    dx, dy = x - XS, YS - y
    lon = math.radians(3) + np.arctan2(dx, dy) / N
    iso = -np.log(np.hypot(dx, dy) / C) / N
    lat = 2 * np.arctan(np.exp(iso)) - math.pi / 2
    for _ in range(6):
        s = np.sin(lat)
        lat = 2 * np.arctan(((1 + E * s) / (1 - E * s)) ** (E / 2) * np.exp(iso)) - math.pi / 2
    return (EARTH * lon - ox) * cos0 - shift[0], (EARTH * np.log(np.tan(math.pi / 4 + lat / 2)) - oy) * cos0 - shift[1]


def dalle_name(kx, ky):
    """Nom court d'une dalle : kilomètre ouest, kilomètre nord."""
    return f"{kx:04d}_{ky:04d}"


def dalle_bounds(kx, ky):
    """[x0, y0, x1, y1] Lambert-93 de la dalle (ouest kx km, nord ky km)."""
    return (kx * 1000.0, (ky - 1) * 1000.0, (kx + 1) * 1000.0, ky * 1000.0)


def dalles_of_zone(zone_l93, margin=0.0):
    """Les dalles de 1 km qui touchent le contour (en Lambert-93) ou en sont à moins de `margin` m, triées."""
    x0, y0, x1, y1 = zone_l93.bounds
    out = []
    for kx in range(int(math.floor((x0 - margin) / 1000)), int(math.floor((x1 + margin) / 1000)) + 1):
        for ky in range(int(math.floor((y0 - margin) / 1000)) + 1, int(math.floor((y1 + margin) / 1000)) + 2):
            if zone_l93.rect_distance(*dalle_bounds(kx, ky)) <= margin:
                out.append((kx, ky))
    return sorted(out)


# --- le système de coordonnées d'une dalle (sans pyproj) ---------------------------
_TOKEN = re.compile(r'\s*(?:("(?:[^"]|"")*")|([\[\](),])|([^\s\[\](),"]+))')
_COMPOUND = ("COMPD_CS", "COMPOUNDCRS")
_PROJECTED = ("PROJCS", "PROJCRS", "PROJECTEDCRS")


def parse_wkt(text):
    """Un WKT (1 ou 2) en arbre : (MOT, [enfants]) ; un enfant est un nœud, une chaîne ou un nombre."""
    tokens = []
    pos = 0
    text = text.strip().rstrip("\x00").strip()
    while pos < len(text):
        m = _TOKEN.match(text, pos)
        if not m or m.end() == pos:
            raise ValueError(f"WKT illisible près de {text[pos:pos + 20]!r}")
        pos = m.end()
        quoted, punct, word = m.groups()
        if quoted is not None:
            tokens.append(("str", quoted[1:-1].replace('""', '"')))
        elif punct is not None:
            tokens.append(("p", punct))
        elif word is not None:
            tokens.append(("w", word))
    i = 0

    def value():
        nonlocal i
        kind, tok = tokens[i]
        i += 1
        if kind == "str":
            return tok
        if kind != "w":
            raise ValueError(f"WKT : {tok!r} inattendu")
        if i < len(tokens) and tokens[i] in (("p", "["), ("p", "(")):
            i += 1
            children = []
            while tokens[i] not in (("p", "]"), ("p", ")")):
                children.append(value())
                if tokens[i] == ("p", ","):
                    i += 1
            i += 1
            return (tok.upper(), children)
        try:
            return float(tok)
        except ValueError:
            return tok

    tree = value()
    if i != len(tokens):
        raise ValueError("WKT : texte après la fin")
    return tree


def _children(node, *names):
    return [c for c in node[1] if isinstance(c, tuple) and c[0] in names]


def _authority(node):
    for c in _children(node, "AUTHORITY", "ID"):
        if len(c[1]) >= 2 and str(c[1][0]).upper() == "EPSG":
            return int(float(c[1][1]))
    return None


def _is_lambert93(node):
    """Un PROJCS sans code EPSG reconnu à ses paramètres (ce que pyproj fait pour to_epsg)."""
    if node[0] not in _PROJECTED:
        return False
    flat = {}

    def walk(n):
        for c in n[1]:
            if isinstance(c, tuple):
                if c[0] in ("PARAMETER", "PARAMETERFILE") and len(c[1]) >= 2 and isinstance(c[1][1], float):
                    flat[re.sub(r"[^a-z0-9]", "", str(c[1][0]).lower())] = c[1][1]
                elif c[0] in ("SPHEROID", "ELLIPSOID") and len(c[1]) >= 3:
                    flat["_a"], flat["_rf"] = c[1][1], c[1][2]
                walk(c)

    walk(node)
    want = {("standardparallel1", "latitudeof1ststandardparallel"): 49.0,
            ("standardparallel2", "latitudeof2ndstandardparallel"): 44.0,
            ("latitudeoforigin", "latitudeoffalseorigin"): 46.5,
            ("centralmeridian", "longitudeoffalseorigin", "longitudeoforigin"): 3.0,
            ("falseeasting", "eastingatfalseorigin"): 700000.0,
            ("falsenorthing", "northingatfalseorigin"): 6600000.0}
    for names, v in want.items():
        got = [flat[n] for n in names if n in flat]
        if not got or abs(got[0] - v) > 1e-9 * max(1.0, abs(v)):
            return False
    return abs(flat.get("_a", 0) - 6378137.0) < 1e-3 and abs(flat.get("_rf", 0) - 298.257222101) < 1e-6


def wkt_epsg(text):
    """Le code EPSG d'un WKT, comme pyproj.CRS(text).to_epsg() pour ce qui nous concerne : le code
    de l'objet de tête ; sans code, 2154 si c'est un PROJCS Lambert-93 sur GRS80 ; sinon None. Un
    système composé (horizontal + altitude) garde son propre code : ce n'est pas 2154."""
    tree = parse_wkt(text)
    code = _authority(tree)
    if code is not None:
        return code
    return 2154 if _is_lambert93(tree) else None


def _geokeys_epsg(data):
    """ProjectedCSTypeGeoKey (3072), sinon GeographicTypeGeoKey (2048), d'un GeoKeyDirectoryTag brut."""
    words = struct.unpack(f"<{len(data) // 2}H", bytes(data[: len(data) // 2 * 2]))
    if len(words) < 4:
        return None
    keys = {words[4 + 4 * k]: words[4 + 4 * k + 3] for k in range(words[3]) if 4 + 4 * k + 3 < len(words) and words[4 + 4 * k + 1] == 0}
    for key in (3072, 2048):
        if keys.get(key) not in (None, 0, 32767):
            return keys[key]
    return None


def header_epsg(header):
    """Le code EPSG d'un en-tête LAS (laspy.LasHeader), sans pyproj : le WKT (LASF_Projection 2112)
    d'abord, sinon les GeoKeys (34735), comme parse_crs(prefer_wkt=True) ; EVLR puis VLR."""
    groups = [getattr(header, "evlrs", None) or [], getattr(header, "vlrs", None) or []]
    for kind in ("wkt", "geokeys"):
        for vlrs in groups:
            for v in vlrs:
                if getattr(v, "user_id", "").rstrip("\x00") != "LASF_Projection":
                    continue
                rid = getattr(v, "record_id", None)
                if kind == "wkt" and rid == 2112:
                    text = getattr(v, "string", None)
                    if text is None:
                        text = bytes(v.record_data).decode("utf-8", "replace")
                    return wkt_epsg(text)
                if kind == "geokeys" and rid == 34735:
                    raw = getattr(v, "record_data", None)
                    if raw is None and hasattr(v, "record_data_bytes"):
                        raw = v.record_data_bytes()
                    if raw is not None:
                        return _geokeys_epsg(raw)
                    entries = getattr(v, "geo_keys", None) or []
                    keys = {e.id: e.value_offset for e in entries if getattr(e, "tiff_tag_location", 0) == 0}
                    for key in (3072, 2048):
                        if keys.get(key) not in (None, 0, 32767):
                            return keys[key]
    return None
