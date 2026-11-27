"""Le découpage de la préparation d'une commune.

  pavé    carré de 2 048 m du repère de la scène, [-1024, 1024]² + 2 048 m * (i, j) :
          le carré de la scène d'avant, décalé (shift) ; le code d'avant y travaille tel
          quel (32 m par tuile, 64 x 64 tuiles, végétation dans le cube [-1024, 1024]³)
  quart   carré de 1 024 m d'un pavé : l'unité de travail du LiDAR (téléchargé, préparé,
          réduit en végétation, sol et toits, puis effacé : le disque reste borné)
  boîte   carré de 256 m (8 x 8 tuiles) d'un quart : une requête COPC, un point de reprise

On ne garde que ce qui touche le contour (à `margin` près). Les pavés vont dans l'ordre
des clés de la végétation (vegbatch.pave_prefix) : le lot s'écrit pavé après pavé.
"""

import math

import numpy as np

PAVE = 2048.0
QUARTER = 1024.0
TILE = 32.0
BOX = 256.0
BAND = 48.0  # m lus hors du pavé (toits des bâtiments du bord : buildings.py lit 64 m, assez pour presque tous) et sol hors du contour


def pave_square(i, j):
    """[x0, y0, x1, y1] du pavé dans le repère de la scène."""
    return (PAVE * i - 1024, PAVE * j - 1024, PAVE * i + 1024, PAVE * j + 1024)


def paves(zone, margin=BAND):
    """Les pavés (i, j) qui touchent le contour (à margin près)."""
    x0, y0, x1, y1 = zone.bounds
    out = []
    for i in range(int(math.floor((x0 - margin + 1024) / PAVE)), int(math.floor((x1 + margin + 1024) / PAVE)) + 1):
        for j in range(int(math.floor((y0 - margin + 1024) / PAVE)), int(math.floor((y1 + margin + 1024) / PAVE)) + 1):
            if zone.rect_distance(*pave_square(i, j)) <= margin:
                out.append((i, j))
    return out


def quarter_square(q):
    """[x0, y0, x1, y1] du quart q (0 sud-ouest, 1 sud-est, 2 nord-ouest, 3 nord-est) dans le repère du pavé."""
    qx, qy = q & 1, q >> 1
    return (-1024 + QUARTER * qx, -1024 + QUARTER * qy, QUARTER * qx, QUARTER * qy)


def quarter_region(q, band=BAND):
    """Ce qu'on lit pour un quart (repère du pavé) : le quart, et `band` m au-delà des bords du pavé."""
    x0, y0, x1, y1 = quarter_square(q)
    qx, qy = q & 1, q >> 1
    return (x0 - (band if qx == 0 else 0), y0 - (band if qy == 0 else 0), x1 + (band if qx == 1 else 0), y1 + (band if qy == 1 else 0))


def quarters(zone, i, j, margin=BAND):
    """Les quarts du pavé (i, j) qui touchent le contour (à margin près)."""
    sx, sy = PAVE * i, PAVE * j
    out = []
    for q in range(4):
        x0, y0, x1, y1 = quarter_square(q)
        if zone.rect_distance(x0 + sx, y0 + sy, x1 + sx, y1 + sy) <= margin:
            out.append(q)
    return out


def boxes(zone, i, j, q, margin=BAND, fine=64.0):
    """Les boîtes (repère du pavé) qui couvrent la région du quart et touchent le contour (à margin
    près) : de 256 m sur la grille des tuiles dans le quart, une bande de `BAND` m au-delà du pavé ;
    au bord du contour, coupées en boîtes de `fine` m (on ne télécharge que ce qui sert : les nœuds
    COPC lus deux fois viennent du cache). Une tuile du quart est toujours dans une seule boîte."""
    from zone import rect_inside

    rx0, ry0, rx1, ry1 = quarter_region(q)
    qx0, qy0, qx1, qy1 = quarter_square(q)
    sx, sy = PAVE * i, PAVE * j

    def cuts(r0, r1, a0, a1, step):
        return sorted({r0, r1} | {a0 + step * k for k in range(int((a1 - a0) / step) + 1)})

    xs, ys = cuts(rx0, rx1, qx0, qx1, BOX), cuts(ry0, ry1, qy0, qy1, BOX)
    out = []
    for y0, y1 in zip(ys[:-1], ys[1:]):
        for x0, x1 in zip(xs[:-1], xs[1:]):
            b = (x0, y0, x1, y1)
            if zone.rect_distance(b[0] + sx, b[1] + sy, b[2] + sx, b[3] + sy) > margin:
                continue
            if not fine or rect_inside(zone, b[0] + sx, b[1] + sy, b[2] + sx, b[3] + sy, margin):
                out.append(b)
                continue
            for fy0, fy1 in zip(cuts(y0, y1, y0, y1, fine)[:-1], cuts(y0, y1, y0, y1, fine)[1:]):
                for fx0, fx1 in zip(cuts(x0, x1, x0, x1, fine)[:-1], cuts(x0, x1, x0, x1, fine)[1:]):
                    if fx1 - fx0 <= 0 or fy1 - fy0 <= 0:
                        continue
                    if zone.rect_distance(fx0 + sx, fy0 + sy, fx1 + sx, fy1 + sy) <= margin:
                        out.append((float(fx0), float(fy0), float(fx1), float(fy1)))
    return out


def tile_of(x, y):
    """Indices (colonne, rangée) des tuiles de 32 m du pavé (0..63 dans le pavé ; hors pavé : hors de 0..63)."""
    return np.floor((np.asarray(x) + 1024) / TILE).astype(np.int64), np.floor((np.asarray(y) + 1024) / TILE).astype(np.int64)


def order(paves_list, prefix):
    """Les pavés dans l'ordre des clés de la végétation : prefix(i, j) -> clé."""
    return sorted(paves_list, key=lambda p: prefix(*p))
