"""Le sol de la commune (TerraLidHD, tuiles du Space `gps`, niveaux 13 à 19), quart par quart
puis pavé par pavé, mémoire bornée.

  quart   les sommes et comptes des hauteurs de classe 2 par cellule de 0,5 m
          (lidar/ground.tile_sums), un .npz de 2 048 x 2 048 cellules
  fenêtre un carré du repère de la scène : les quarts qui le couvrent, trous remplis
          (push-pull de lidar/ground.fill, pour toute taille), coins quantifiés comme
          lidar/ground.legacy les écrit
  pavé    la fenêtre du pavé et `margin` m autour ; le réseau de niveau 25 et les
          niveaux 19 à 16 de lidar/ground.terra, sur la fenêtre ; on écrit les tuiles
          dont le centre est dans le pavé (la fenêtre les contient), et on garde les
          nœuds du niveau 22 (coins des tuiles de niveau 16) qui sont dans le pavé
  fin     les niveaux 15 à 13 du réseau de niveau 22 assemblé (petit : 5 x 7 m)

Les valeurs dans le pavé sont celles d'un calcul d'un seul tenant tant que leur support
(le remplissage des bords, 96 nœuds, et les filtres des niveaux) reste dans la fenêtre.
Les trous très grands (plus que la marge) se remplissent depuis la fenêtre seule.
"""

import gzip
import json
import math
import sys
from pathlib import Path

import numpy as np

LIDAR = Path(__file__).resolve().parent.parent / "lidar"
sys.path.insert(0, str(LIDAR))
import gps  # noqa: E402
import importlib.util  # noqa: E402

# lidar/ground.py par son chemin : experiences/scripts/world a aussi un ground.py
_spec = importlib.util.spec_from_file_location("lidar_ground", LIDAR / "ground.py")
lg = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(lg)

CELL = 0.5
QN = 2048  # cellules d'un quart par axe
CELLS = 64


def quarter_sums(rdir, q):
    """Sommes (float32) et comptes (uint16) des hauteurs de sol par cellule de 0,5 m d'un quart, de ses tuiles IXRT."""
    man = json.loads((Path(rdir) / "manifest.json").read_text())
    total = np.zeros((QN, QN), np.float32)
    count = np.zeros((QN, QN), np.uint16)
    qx, qy = q & 1, q >> 1
    for t in man["tiles"]:
        row, col, s, n = lg.tile_sums(str(Path(rdir) / t["file"]))
        r, c = (row - 32 * qy) * 64, (col - 32 * qx) * 64
        if not (0 <= r < QN and 0 <= c < QN):
            raise ValueError(f"tuile {t['file']} hors du quart {q}")
        total[r:r + 64, c:c + 64] = s.reshape(64, 64)
        count[r:r + 64, c:c + 64] = n.reshape(64, 64)
    return total, count


def save_quarter(path, i, j, q, total, count):
    """x0, y0 : coin sud-ouest du quart dans le repère de la scène."""
    qx, qy = q & 1, q >> 1
    x0, y0 = 2048.0 * i - 1024 + 1024 * qx, 2048.0 * j - 1024 + 1024 * qy
    tmp = Path(str(path) + ".tmp.npz")
    np.savez_compressed(tmp, total=total, count=count, origin=np.array([x0, y0]))
    tmp.replace(path)


class Quarters:
    """Les sommes de sol des quarts faits, par coin (repère de la scène)."""

    def __init__(self, files):
        self.files = {}
        for f in files:
            with np.load(f) as d:
                self.files[tuple(d["origin"].tolist())] = f

    def window(self, x0, y0, n):
        """Hauteur moyenne (float64) et mesure (bool) des n x n cellules depuis (x0, y0)."""
        total = np.zeros((n, n), np.float32)
        count = np.zeros((n, n), np.uint16)
        for (qx0, qy0), f in self.files.items():
            # chevauchement en cellules
            c0, r0 = int(round((qx0 - x0) / CELL)), int(round((qy0 - y0) / CELL))
            a0, a1 = max(r0, 0), min(r0 + QN, n)
            b0, b1 = max(c0, 0), min(c0 + QN, n)
            if a0 >= a1 or b0 >= b1:
                continue
            with np.load(f) as d:
                total[a0:a1, b0:b1] = d["total"][a0 - r0:a1 - r0, b0 - c0:b1 - c0]
                count[a0:a1, b0:b1] = d["count"][a0 - r0:a1 - r0, b0 - c0:b1 - c0]
        measured = count > 0
        z = np.zeros((n, n), np.float64)
        z[measured] = total[measured] / count[measured]  # le calcul de lidar/ground : float32 / uint16
        return z, measured


def fill(z, w):
    """lidar/ground.fill pour toute taille : les dimensions impaires reçoivent une rangée sans mesure."""
    if z.shape[0] == 1 and z.shape[1] == 1:
        return z
    ph, pw = z.shape[0] % 2, z.shape[1] % 2
    if ph or pw:
        h, wd = z.shape
        zp, wp = np.pad(z, ((0, ph), (0, pw))), np.pad(w, ((0, ph), (0, pw)))
        return fill(zp, wp)[:h, :wd]
    if z.shape[0] == 1 or z.shape[1] == 1:  # une seule rangée ou colonne : on double l'autre
        a = 2 if z.shape[0] == 1 else 1
        b = 2 if z.shape[1] == 1 else 1
        return fill(np.tile(z, (a, b)), np.tile(w, (a, b)))[:z.shape[0], :z.shape[1]]
    zw = (z * w).reshape(z.shape[0] // 2, 2, -1, 2).sum((1, 3))
    w2 = w.reshape(z.shape[0] // 2, 2, -1, 2).sum((1, 3))
    coarse = fill(zw / np.maximum(w2, 1e-9), np.minimum(w2, 1.0))
    del zw, w2
    hj, hi = np.nonzero(w == 0)
    out = z.copy()
    out[hj, hi] = coarse[hj // 2, hi // 2]
    del coarse
    nj, ni = z.shape[0] - 1, z.shape[1] - 1
    up, down = np.maximum(hj - 1, 0), np.minimum(hj + 1, nj)
    left, right = np.maximum(hi - 1, 0), np.minimum(hi + 1, ni)
    for _ in range(3):
        out[hj, hi] = (out[up, hi] + out[down, hi] + out[hj, left] + out[hj, right]) / 4
    return out


def dtm(quarters, x0, y0, n):
    """Le sol rempli (float32) de la fenêtre, et ses cellules mesurées."""
    z, measured = quarters.window(x0, y0, n)
    return fill(z, measured.astype(np.float32)).astype(np.float32), measured


def corners(dtm_):
    """Hauteurs aux coins (n + 1)², quantifiées comme les tuiles de lidar/ground.legacy, en float32."""
    p = np.pad(dtm_, 1, mode="edge")
    fine = (p[:-1, :-1] + p[1:, :-1] + p[:-1, 1:] + p[1:, 1:]) / 4
    return (lg.quantise(fine) * 0.01 + -100).astype(np.float32)


def pave(quarters, i, j, origin, altitude, fh, collection, margin=384.0, levels=(16, 19), keep_level=22):
    """Les tuiles des niveaux levels du pavé (i, j) dans fh (NDJSON) ; les nœuds du niveau keep_level
    dans le pavé : (rows, cols, z, ok). Retourne (tuiles écrites, nœuds, fourchette des hauteurs)."""
    coarse, finest = levels
    side = 2048.0 + 2 * margin
    n = int(side / CELL)
    x0 = -1024.0 - margin  # repère du pavé
    d, measured = dtm(quarters, 2048.0 * i + x0, 2048.0 * j + x0, n)
    if not measured.any():
        return 0, None, None
    heights = (float(d[measured].min()), float(d[measured].max()))
    known = np.zeros((n + 1, n + 1), bool)
    # un coin est connu quand une tuile de 32 m (alignée sur le pavé) qui le touche a une mesure
    t = int(32 / CELL)
    k = int(round(margin / 32))  # tuiles de marge (margin multiple de 32 m)
    nt = n // t
    m = measured[:nt * t, :nt * t].reshape(nt, t, nt, t).any((1, 3))
    for a in range(nt):
        for b in range(nt):
            if m[a, b]:
                known[a * t:a * t + t + 1, b * t:b * t + t + 1] = True
    grid = corners(d)
    del d, measured
    scene = gps.Scene(origin, (2048.0 * i, 2048.0 * j))
    q = finest + 6
    lat_s, lng_w = scene.to_degrees(x0, x0)
    lat_n, lng_e = scene.to_degrees(x0 + side, x0 + side)
    r0, c0 = (int(v) // CELLS * CELLS for v in gps.cell_index(float(lat_s), float(lng_w), q))
    r1, c1 = ((int(v) // CELLS + 1) * CELLS for v in gps.cell_index(float(lat_n), float(lng_e), q))
    z, ok = lg.lattice(grid, known, x0, CELL, scene, q, (r0, r1 + 1), (c0, c1 + 1))
    del grid, known
    z = lg.fill_out(z + altitude, ok)
    inside = lambda lat, lng: -1024 <= float(scene.to_metres(lat, lng)[0]) < 1024 and -1024 <= float(scene.to_metres(lat, lng)[1]) < 1024  # noqa: E731
    written, kept = 0, None
    for t_ in range(finest, coarse - 1, -1):
        if t_ + 6 == keep_level:
            # les nœuds du niveau keep_level (coins des tuiles de t_) dans le pavé
            rows = np.arange(r0, r0 + z.shape[0])
            cols = np.arange(c0, c0 + z.shape[1])
            lat = -90 + rows * 180 / 2.0**keep_level
            lng = -180 + cols * 360 / 2.0**keep_level
            _, y = scene.to_metres(lat, np.full(len(lat), scene.lon0))
            x, _ = scene.to_metres(np.full(len(lng), scene.lat0), lng)
            rs, cs = (y >= -1024) & (y < 1024), (x >= -1024) & (x < 1024)
            kept = (rows[rs], cols[cs], z[np.ix_(rs, cs)].astype(np.float64), ok[np.ix_(rs, cs)])
        cell_level = t_ + 6
        for row in range(r0 // CELLS, r1 // CELLS):
            for col in range(c0 // CELLS, c1 // CELLS):
                s, nn, w, e = gps.bounds(row, col, t_)
                if not inside((s + nn) / 2, (w + e) / 2):
                    continue
                jj, ii = row * CELLS - r0, col * CELLS - c0
                valid = ok[jj:jj + CELLS + 1, ii:ii + CELLS + 1]
                if valid.shape != (CELLS + 1, CELLS + 1):
                    continue
                cells = valid[:-1, :-1] & valid[1:, :-1] & valid[:-1, 1:] & valid[1:, 1:]
                if not cells.any():
                    continue
                cz = z[jj:jj + CELLS + 1, ii:ii + CELLS + 1]
                metrics = [0.0, 0.0, 0.0]
                if t_ == finest:
                    ns, ew = gps.cell_metres(cell_level, (s + nn) / 2)
                    mean = (cz[:-1, :-1] + cz[1:, :-1] + cz[:-1, 1:] + cz[1:, 1:]) / 4
                    wgt = cells * ns * ew
                    metrics = [float(wgt.sum()), float((wgt * mean).sum()), float((wgt * mean * mean).sum())]
                fh.write(json.dumps({"Collection": collection, "Location": gps.tile_anchor(t_, row, col, collection), "ID": f"{t_}/{row}/{col}",
                                     "Metrics": metrics, "Payload": lg.terra_payload(cz, t_, row, col)}, separators=(",", ":")) + "\n")
                written += 1
        if t_ == coarse:
            break
        a0, b0 = r0 // (2 * CELLS) * 2 * CELLS, c0 // (2 * CELLS) * 2 * CELLS
        a1, b1 = -(-r1 // (2 * CELLS)) * 2 * CELLS, -(-c1 // (2 * CELLS)) * 2 * CELLS
        pad = ((r0 - a0, a1 - r1), (c0 - b0, b1 - c1))
        z, ok = lg.halve(np.pad(z, pad, mode="edge"), np.pad(ok, pad))
        r0, r1, c0, c1 = a0 // 2, a1 // 2, b0 // 2, b1 // 2
    return written, kept, heights


def coarse_levels(kept, fh, collection, levels=(13, 15), keep_level=22):
    """Les niveaux 15 à 13 du réseau de niveau 22 assemblé des pavés (même suite que lidar/ground.terra)."""
    rows = np.concatenate([k[0] for k in kept])
    cols = np.concatenate([k[1] for k in kept])
    a = CELLS * 2 ** (keep_level - 6 - levels[0] + 1)  # aligné pour chaque demi jusqu'au niveau le plus grossier
    r_lo, r_hi = rows.min() // a * a, -(-(rows.max() + 1) // a) * a
    c_lo, c_hi = cols.min() // a * a, -(-(cols.max() + 1) // a) * a
    z = np.zeros((r_hi - r_lo, c_hi - c_lo))
    ok = np.zeros(z.shape, bool)
    seen = np.zeros(z.shape, bool)
    for kr, kc, kz, kok in kept:
        z[np.ix_(kr - r_lo, kc - c_lo)] = kz
        ok[np.ix_(kr - r_lo, kc - c_lo)] = kok
        seen[np.ix_(kr - r_lo, kc - c_lo)] = True
    z = lg.fill_out(z, seen)  # ce qu'aucun pavé ne voit est hors de la commune : rempli depuis les bords, comme lidar/ground.terra
    coarse, fine = levels
    r0, r1, c0, c1 = r_lo, r_hi, c_lo, c_hi
    # le niveau keep_level porte les coins des tuiles de keep_level - 6 : un demi pour la première d'ici
    z, ok = lg.halve(z, ok)
    r0, r1, c0, c1 = r0 // 2, r1 // 2, c0 // 2, c1 // 2
    written = 0
    for t_ in range(fine, coarse - 1, -1):
        for row in range(r0 // CELLS, r1 // CELLS):
            for col in range(c0 // CELLS, c1 // CELLS):
                jj, ii = row * CELLS - r0, col * CELLS - c0
                valid = ok[jj:jj + CELLS + 1, ii:ii + CELLS + 1]
                if valid.shape != (CELLS + 1, CELLS + 1):
                    continue
                cells = valid[:-1, :-1] & valid[1:, :-1] & valid[:-1, 1:] & valid[1:, 1:]
                if not cells.any():
                    continue
                cz = z[jj:jj + CELLS + 1, ii:ii + CELLS + 1]
                fh.write(json.dumps({"Collection": collection, "Location": gps.tile_anchor(t_, row, col, collection), "ID": f"{t_}/{row}/{col}",
                                     "Metrics": [0.0, 0.0, 0.0], "Payload": lg.terra_payload(cz, t_, row, col)}, separators=(",", ":")) + "\n")
                written += 1
        if t_ == coarse:
            break
        z, ok = lg.halve(z, ok)
        r0, r1, c0, c1 = r0 // 2, r1 // 2, c0 // 2, c1 // 2
    return written
