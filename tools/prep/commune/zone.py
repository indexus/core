"""La zone d'une scène : un disque (le cas d'avant) ou un contour (Polygon /
MultiPolygon GeoJSON en WGS84, par exemple la commune d'Anglet), en mètres de
la scène, et les tests dont la préparation a besoin.

Repère de la scène (celui des builders et d'Explore) : mètres est et nord de
l'origin, Web Mercator multiplié par cos(lat0). Un « pavé » de la préparation
commune garde ce repère, décalé de `shift` (des multiples de 2048 m) : local =
scène - shift, pour que le code fait pour le carré [-1024, 1024] serve tel quel.

Règle pair-impair pour les anneaux (un trou est un anneau de plus). Tout est
en float64 ; les tests vectorisés vont par paquets pour borner la mémoire.
"""

import json
import math
from pathlib import Path

import numpy as np

EARTH = 6378137.0


class Frame:
    """Mètres de la scène (moins `shift`) <-> degrés, comme frame.py / gps.Scene."""

    def __init__(self, origin, shift=(0.0, 0.0)):
        self.lon0, self.lat0 = float(origin[0]), float(origin[1])
        self.shift = (float(shift[0]), float(shift[1]))
        self.cos0 = math.cos(math.radians(self.lat0))
        self.ox = EARTH * math.radians(self.lon0)
        self.oy = EARTH * math.log(math.tan(math.pi / 4 + math.radians(self.lat0) / 2))

    def to_metres(self, lat, lng):
        mx = EARTH * np.radians(np.asarray(lng, np.float64))
        my = EARTH * np.log(np.tan(np.pi / 4 + np.radians(np.asarray(lat, np.float64)) / 2))
        return (mx - self.ox) * self.cos0 - self.shift[0], (my - self.oy) * self.cos0 - self.shift[1]

    def to_degrees(self, x, y):
        x = np.asarray(x, np.float64) + self.shift[0]
        y = np.asarray(y, np.float64) + self.shift[1]
        lng = np.degrees((x / self.cos0 + self.ox) / EARTH)
        lat = np.degrees(np.arctan(np.sinh((y / self.cos0 + self.oy) / EARTH)))
        return lat, lng


def read_geometry(path):
    """La géométrie (Polygon ou MultiPolygon) d'un fichier GeoJSON : Feature, FeatureCollection d'une entité, ou géométrie."""
    d = json.loads(Path(path).read_text())
    if d.get("type") == "FeatureCollection":
        if len(d["features"]) != 1:
            raise ValueError(f"{path}: une seule entité attendue, {len(d['features'])} trouvées")
        d = d["features"][0]
    if d.get("type") == "Feature":
        d = d["geometry"]
    if d.get("type") not in ("Polygon", "MultiPolygon"):
        raise ValueError(f"{path}: Polygon ou MultiPolygon attendu, pas {d.get('type')}")
    return d


def geometry_rings(geometry):
    """Les anneaux [[lng, lat], ...] d'un Polygon / MultiPolygon."""
    polys = [geometry["coordinates"]] if geometry["type"] == "Polygon" else geometry["coordinates"]
    return [np.asarray(ring, np.float64)[:, :2] for poly in polys for ring in poly]


class Disc:
    """Le disque de rayon `radius` autour de l'origine (la scène d'avant)."""

    kind = "disc"

    def __init__(self, radius):
        self.radius = float(radius)
        r = self.radius
        self.bounds = (-r, -r, r, r)

    def contains(self, x, y):
        x, y = np.asarray(x, np.float64), np.asarray(y, np.float64)
        return x * x + y * y <= self.radius**2

    def distance(self, x, y):
        """Distance signée au bord (négative dedans)."""
        return np.hypot(x, y) - self.radius

    def rect_distance(self, x0, y0, x1, y1):
        dx, dy = max(x0, 0.0, -x1), max(y0, 0.0, -y1)
        return max(0.0, math.hypot(dx, dy) - self.radius)

    def area(self):
        return math.pi * self.radius**2


class Polygon:
    """Un contour en mètres de la scène : anneaux (n, 2), règle pair-impair."""

    kind = "polygon"

    def __init__(self, rings):
        a, b = [], []
        for ring in rings:
            ring = np.asarray(ring, np.float64)
            if len(ring) > 1 and np.all(ring[0] == ring[-1]):
                ring = ring[:-1]
            if len(ring) < 3:
                continue
            a.append(ring)
            b.append(np.roll(ring, -1, axis=0))
        if not a:
            raise ValueError("contour vide")
        self.rings = a
        A, B = np.concatenate(a), np.concatenate(b)
        self.ax, self.ay, self.bx, self.by = A[:, 0], A[:, 1], B[:, 0], B[:, 1]
        self.bounds = (float(A[:, 0].min()), float(A[:, 1].min()), float(A[:, 0].max()), float(A[:, 1].max()))
        self.radius = float(np.hypot(A[:, 0], A[:, 1]).max())  # le disque autour de l'origine qui le contient

    @classmethod
    def from_geometry(cls, geometry, frame):
        rings = []
        for ring in geometry_rings(geometry):
            x, y = frame.to_metres(ring[:, 1], ring[:, 0])
            rings.append(np.stack([x, y], 1))
        return cls(rings)

    @property
    def edges(self):
        return len(self.ax)

    def area(self):
        """Aire (m², pair-impair : un trou se soustrait s'il est orienté à l'inverse ; on prend |aire| par anneau, trous en moins)."""
        total = 0.0
        for k, ring in enumerate(self.rings):
            x, y = ring[:, 0], ring[:, 1]
            s = abs(float(np.dot(x, np.roll(y, -1)) - np.dot(np.roll(x, -1), y)) / 2)
            # un anneau dans un autre (trou) se soustrait
            inside = sum(bool(Polygon([r]).contains(ring[:1, 0], ring[:1, 1])[0]) for j, r in enumerate(self.rings) if j != k)
            total += -s if inside % 2 else s
        return total

    def contains(self, x, y, chunk=1 << 22):
        """Points dans le contour (exact, nombre de croisements vers +x ; demi-ouvert : un sommet compte d'un côté)."""
        x, y = np.atleast_1d(np.asarray(x, np.float64)), np.atleast_1d(np.asarray(y, np.float64))
        out = np.zeros(len(x), bool)
        per = max(1, chunk // max(1, self.edges))
        ax, ay, bx, by = self.ax, self.ay, self.bx, self.by
        for s in range(0, len(x), per):
            px, py = x[s:s + per, None], y[s:s + per, None]
            spans = (ay > py) != (by > py)
            with np.errstate(divide="ignore", invalid="ignore"):
                cx = ax + (py - ay) * (bx - ax) / (by - ay)
            out[s:s + per] = (np.count_nonzero(spans & (px < cx), axis=1) & 1).astype(bool)
        return out

    def boundary_distance(self, x, y, chunk=1 << 22):
        """Distance (non signée) au bord, exacte."""
        x, y = np.atleast_1d(np.asarray(x, np.float64)), np.atleast_1d(np.asarray(y, np.float64))
        out = np.empty(len(x))
        per = max(1, chunk // max(1, self.edges))
        dx, dy = self.bx - self.ax, self.by - self.ay
        ll = np.maximum(dx * dx + dy * dy, 1e-300)
        for s in range(0, len(x), per):
            px, py = x[s:s + per, None], y[s:s + per, None]
            t = np.clip(((px - self.ax) * dx + (py - self.ay) * dy) / ll, 0, 1)
            out[s:s + per] = np.sqrt(np.min((self.ax + t * dx - px) ** 2 + (self.ay + t * dy - py) ** 2, axis=1))
        return out

    def distance(self, x, y):
        """Distance signée au bord (négative dedans)."""
        d = self.boundary_distance(x, y)
        return np.where(self.contains(x, y), -d, d)

    def rect_distance(self, x0, y0, x1, y1):
        """0 si le rectangle touche le contour (bord ou intérieur), sinon la distance entre eux."""
        if self.contains([x0], [y0])[0]:
            return 0.0
        # un sommet dans le rectangle, ou une arête qui le coupe : distance 0
        ax, ay, bx, by = self.ax, self.ay, self.bx, self.by
        d = segment_rect_distance(ax, ay, bx, by, x0, y0, x1, y1)
        return float(d.min())


def rect_inside(zone, x0, y0, x1, y1, guard=0.0):
    """Le rectangle est dans la zone, à plus de `guard` m de son bord."""
    if isinstance(zone, Disc):
        dx, dy = max(abs(x0), abs(x1)), max(abs(y0), abs(y1))
        return math.hypot(dx, dy) <= zone.radius - guard
    if not zone.contains([x0], [y0])[0]:
        return False
    return bool(segment_rect_distance(zone.ax, zone.ay, zone.bx, zone.by, x0, y0, x1, y1).min() > guard)


def segment_rect_distance(ax, ay, bx, by, x0, y0, x1, y1):
    """Distance de chaque segment (ax, ay)-(bx, by) au rectangle plein [x0, x1] x [y0, y1] (0 s'il le touche)."""
    ax, ay, bx, by = (np.asarray(v, np.float64) for v in (ax, ay, bx, by))
    # le segment touche le rectangle : clip de Liang-Barsky
    dx, dy = bx - ax, by - ay
    t0, t1 = np.zeros_like(ax), np.ones_like(ax)
    ok = np.ones(ax.shape, bool)
    for p, q in ((-dx, ax - x0), (dx, x1 - ax), (-dy, ay - y0), (dy, y1 - ay)):
        par = p == 0
        ok &= ~(par & (q < 0))
        with np.errstate(divide="ignore", invalid="ignore"):
            r = q / p
        t0 = np.where(~par & (p < 0), np.maximum(t0, r), t0)
        t1 = np.where(~par & (p > 0), np.minimum(t1, r), t1)
    hit = ok & (t0 <= t1)
    # sinon : le minimum est entre une extrémité et le rectangle, ou un coin du rectangle et le segment
    def point_rect(px, py):
        return np.hypot(np.maximum(np.maximum(x0 - px, 0), px - x1), np.maximum(np.maximum(y0 - py, 0), py - y1))

    best = np.minimum(point_rect(ax, ay), point_rect(bx, by))
    ll = np.maximum(dx * dx + dy * dy, 1e-300)
    for cx, cy in ((x0, y0), (x0, y1), (x1, y0), (x1, y1)):
        t = np.clip(((cx - ax) * dx + (cy - ay) * dy) / ll, 0, 1)
        best = np.minimum(best, np.hypot(ax + t * dx - cx, ay + t * dy - cy))
    return np.where(hit, 0.0, best)


def scene_zone(entry, frame=None, root=None):
    """La zone d'une entrée du catalogue : `zone` (géométrie WGS84), `boundary` (fichier à côté du catalogue) ou `radius`."""
    frame = frame or Frame(entry["origin"])
    if entry.get("zone"):
        return Polygon.from_geometry(entry["zone"], frame)
    if entry.get("boundary"):
        return Polygon.from_geometry(read_geometry(Path(root or ".") / entry["boundary"]), frame)
    if entry.get("radius"):
        return Disc(entry["radius"])
    raise ValueError(f"scène {entry.get('id')}: ni contour ni rayon")


class Mask:
    """Le contour rastérisé en cellules de `cell` mètres sur `bounds` : 0 dehors, 1 dedans, 2 bord
    (une arête passe par la cellule ou une voisine) ; `near` : à moins de `margin` mètres du contour
    (dilatation carrée : un sur-ensemble du disque de rayon margin). Les points des cellules de bord
    passent par le test exact."""

    def __init__(self, zone, bounds, cell=2.0, margin=0.0):
        self.zone, self.cell, self.margin = zone, float(cell), float(margin)
        x0, y0, x1, y1 = bounds
        self.x0, self.y0 = float(x0), float(y0)
        self.nx, self.ny = int(math.ceil((x1 - x0) / cell)), int(math.ceil((y1 - y0) / cell))
        state = np.zeros((self.ny, self.nx), np.uint8)
        if isinstance(zone, Disc):
            cx = self.x0 + (np.arange(self.nx) + 0.5) * cell
            cy = self.y0 + (np.arange(self.ny) + 0.5) * cell
            d = np.hypot(cx[None, :], cy[:, None]) - zone.radius
            state[d <= 0] = 1
            state[np.abs(d) <= cell * 1.5] = 2
        else:
            # lignes : les croisements au centre de chaque rangée, intervalles pair-impair
            cx = self.x0 + (np.arange(self.nx) + 0.5) * cell
            for j in range(self.ny):
                py = self.y0 + (j + 0.5) * cell
                spans = (zone.ay > py) != (zone.by > py)
                if not spans.any():
                    continue
                a = np.flatnonzero(spans)
                xs = np.sort(zone.ax[a] + (py - zone.ay[a]) * (zone.bx[a] - zone.ax[a]) / (zone.by[a] - zone.ay[a]))
                k = np.searchsorted(xs, cx, side="right")
                state[j, (k & 1).astype(bool)] = 1
            # bord : les cellules traversées par une arête (échantillonnées au quart de cellule) et leurs voisines
            for ax, ay, bx, by in zip(zone.ax, zone.ay, zone.bx, zone.by):
                n = max(2, int(math.ceil(math.hypot(bx - ax, by - ay) / (cell / 4))) + 1)
                t = np.linspace(0, 1, n)
                i = np.floor((ax + t * (bx - ax) - self.x0) / cell).astype(np.int64)
                j = np.floor((ay + t * (by - ay) - self.y0) / cell).astype(np.int64)
                for di in (-1, 0, 1):
                    for dj in (-1, 0, 1):
                        ii, jj = i + di, j + dj
                        ok = (ii >= 0) & (ii < self.nx) & (jj >= 0) & (jj < self.ny)
                        state[jj[ok], ii[ok]] = 2
        self.state = state
        near = state > 0
        k = int(math.ceil(margin / cell))
        if k:
            for _ in range(k):  # dilatation carrée de k cellules, séparable
                grown = near.copy()
                grown[:, 1:] |= near[:, :-1]
                grown[:, :-1] |= near[:, 1:]
                near = grown
            for _ in range(k):
                grown = near.copy()
                grown[1:, :] |= near[:-1, :]
                grown[:-1, :] |= near[1:, :]
                near = grown
        self.near_cells = near

    def _index(self, x, y):
        i = np.floor((np.asarray(x, np.float64) - self.x0) / self.cell).astype(np.int64)
        j = np.floor((np.asarray(y, np.float64) - self.y0) / self.cell).astype(np.int64)
        ok = (i >= 0) & (i < self.nx) & (j >= 0) & (j < self.ny)
        return np.clip(i, 0, self.nx - 1), np.clip(j, 0, self.ny - 1), ok

    def contains(self, x, y):
        """Exact : la cellule décide, sauf au bord (test du contour)."""
        x, y = np.atleast_1d(np.asarray(x, np.float64)), np.atleast_1d(np.asarray(y, np.float64))
        i, j, ok = self._index(x, y)
        s = np.where(ok, self.state[j, i], 0)
        out = s == 1
        edge = np.flatnonzero(s == 2)
        if len(edge):
            out[edge] = self.zone.contains(x[edge], y[edge])
        return out

    def near(self, x, y):
        """À moins de `margin` du contour (sur-ensemble), ou dedans."""
        i, j, ok = self._index(x, y)
        return ok & self.near_cells[j, i]
