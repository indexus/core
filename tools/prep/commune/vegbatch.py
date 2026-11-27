"""La végétation de la commune comme un seul lot (docs/ingest/pipeline.md, section 5),
fait pavé par pavé.

L'espace de la végétation d'une commune : est, nord, haut dans un cube de
32 768 m (min [-17408, -17408, -1024]) et 7 caractères, au lieu du cube de
2 048 m ([-1024, 1024]^3) et 5 caractères d'AngVegetation001. Les cellules de
profondeur 2 font 2 048 m et tombent sur les pavés de la préparation ([-1024,
1024] + 2 048 m * (i, j)) ; en dessous, tout est l'espace d'avant : mêmes
voxels de 2 m, mêmes bornes, même code. Un pavé est donc préparé avec le code
d'avant (vegetation.py, ses quarts de 1 024 m), avec les tables de l'espace
(mask, offset) des niveaux 2 à 6 de la nouvelle collection, puis :

  clé    = préfixe du pavé (niveaux 0 et 1) | clé du pavé >> 12, profondeur + 2
  sommes = moments décalés de shift = (2 048 i, 2 048 j, 0) : Σx += n dx,
           Σxx += 2 dx Σx + n dx², Σxy += dx Σy + dy Σx + n dx dy, ...
  charge = la même (6 octets par retour, relatifs au voxel)

Les quarts d'un pavé ont des clés disjointes ; chacun de ses cubes de 512 m
(un caractère sous le pavé) est entier dans un quart : on fusionne par ces
groupes. Chaque pavé écrit ses parts (part-<pavé>-<n>.ixp, l'ordre des noms est
celui des clés) et les sommes de ses zones (profondeurs 2 à 6) ; la fin écrit
zones.ixp (avec les profondeurs 0 et 1) et manifest.json. Reprise par pavé.
"""

import hashlib
import heapq
import json
import os
import struct
import sys
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "lidar"))
import ixp  # noqa: E402

SPACE = {"min": [-17408.0, -17408.0, -1024.0], "side": 32768.0, "depth": 7}
ABOVE = 2  # niveaux au-dessus des pavés
INNER = 5  # profondeur des voxels dans un pavé (l'espace d'avant)
PAVE = 2048.0
METRICS = ["n", "sum_x", "sum_y", "sum_z", "sum_xx", "sum_xy", "sum_xz", "sum_yy", "sum_yz", "sum_zz", "sum_r", "sum_g", "sum_b"]


def tables(collection):
    """Les tables de l'espace de la collection (3 dimensions, 12 niveaux), et celles des niveaux sous les pavés."""
    t = ixp.space_tables(collection, 3, levels=12)
    inner = {"mask": t["mask"][6 * ABOVE:], "offset": t["offset"][ABOVE:]}
    return t, inner


def pave_cell(i, j):
    """Cellule entière (profondeur 2, 16 par axe) du pavé (i, j) : x, y, z (z : [-1024, 1024] est la première)."""
    cx = (2048 * i - 1024 - SPACE["min"][0]) / PAVE
    cy = (2048 * j - 1024 - SPACE["min"][1]) / PAVE
    if cx != int(cx) or cy != int(cy) or not (0 <= cx < 16 and 0 <= cy < 16):
        raise ValueError(f"pavé {i},{j} hors de l'espace")
    return int(cx), int(cy), 0


def pave_prefix(collection, i, j):
    """Les bits (alignés à gauche dans un u64) des deux premiers caractères du pavé."""
    t, _ = tables(collection)
    loc = ixp.locations(t, np.array([pave_cell(i, j)]), ABOVE)[0]
    key = 0
    for l, ch in enumerate(loc):
        key |= ixp.ALPHABET.index(ch) << (58 - 6 * l)
    return key, loc


# --- lecture d'une part IXP1 (l'écriture : ixp.PartWriter) ---
def read_part(path):
    """Les blocs d'une part : dicts head [n][13] u8, ids [bytes], weight [n], metrics [n][m], pay_end [n] u64, payload u8."""
    raw = np.fromfile(path, np.uint8)
    b = raw.tobytes()
    if b[:4] != b"IXP1":
        raise ValueError(f"{path}: pas une part IXP1")
    _, m, has_pay, _ = struct.unpack("<HHII", b[4:16])
    at, rows, count, crc = struct.unpack("<QQII", b[-32:-8])
    if b[-8:-4] != b"IXP1":
        raise ValueError(f"{path}: fin de part illisible")
    index = b[at:at + 32 * count]
    if ixp.crc32(index) != crc:
        raise ValueError(f"{path}: index corrompu")
    pad = lambda k: k + (-k % 8)  # noqa: E731
    for e in range(count):
        off, length, n = struct.unpack("<QII", index[32 * e + 16:32 * e + 32])
        body = b[off:off + length]
        if ixp.crc32(body[:-8]) != struct.unpack("<I", body[-8:-4])[0]:
            raise ValueError(f"{path}: bloc {e} corrompu")
        n2, id_len, pay_len = struct.unpack("<IIQ", body[:16])
        assert n2 == n
        p = 16
        head = np.frombuffer(body, np.uint8, 13 * n, p).reshape(n, 13)
        p += pad(13 * n)
        id_end = np.frombuffer(body, "<u4", n, p).astype(np.int64)
        p += pad(4 * n)
        pay_end = None
        if has_pay:
            pay_end = np.frombuffer(body, "<u8", n, p)
            p += 8 * n
        weight = np.frombuffer(body, "<f8", n, p)
        p += 8 * n
        metrics = np.frombuffer(body, "<f8", m * n, p).reshape(m, n).T
        p += 8 * m * n
        idb = body[p:p + id_len]
        p += pad(id_len)
        payload = np.frombuffer(body, np.uint8, pay_len, p)
        starts = np.r_[0, id_end[:-1]]
        ids = [idb[s:e] for s, e in zip(starts, id_end)]
        yield {"head": head, "ids": ids, "weight": weight, "metrics": metrics, "pay_end": pay_end, "payload": payload}


def head_keys(head):
    """Les 8 premiers octets de chaque tête, en u64 (big-endian)."""
    return np.ascontiguousarray(head[:, :8]).view(">u8").reshape(-1).astype(np.uint64)


def shift_moments(m, dx, dy):
    """Les 13 sommes d'un voxel (n, Σx, Σy, Σz, Σxx, Σxy, Σxz, Σyy, Σyz, Σzz, Σr, Σg, Σb) dans le repère décalé de (dx, dy, 0)."""
    m = np.array(m, np.float64, copy=True)
    n, sx, sy, sz = m[:, 0].copy(), m[:, 1].copy(), m[:, 2].copy(), m[:, 3].copy()
    m[:, 1] = sx + n * dx
    m[:, 2] = sy + n * dy
    m[:, 4] += 2 * dx * sx + n * dx * dx
    m[:, 5] += dx * sy + dy * sx + n * dx * dy
    m[:, 6] += dx * sz
    m[:, 7] += 2 * dy * sy + n * dy * dy
    m[:, 8] += dy * sz
    return m


def rebased_groups(batch_dir, prefix, shift):
    """Les enregistrements d'un lot de quart (clés du pavé, profondeur 5), dans l'espace de la commune,
    par groupe d'un caractère sous le pavé (un cube de 512 m) : (clé du groupe, arrays)."""
    man = json.loads((Path(batch_dir) / "manifest.json").read_text())
    if man["recordDepths"] != [INNER, INNER]:
        raise ValueError(f"{batch_dir}: profondeur {man['recordDepths']}, {INNER} attendue")
    group_mask = np.uint64(((1 << 6 * (ABOVE + 1)) - 1) << (64 - 6 * (ABOVE + 1)))
    held = []

    def flush():
        keys = np.concatenate([h[0] for h in held])
        out = {
            "keys": keys,
            "ids": [i for h in held for i in h[1]],
            "weight": np.concatenate([h[2] for h in held]),
            "metrics": np.concatenate([h[3] for h in held]),
            "sizes": np.concatenate([h[4] for h in held]),
            "payload": np.concatenate([h[5] for h in held]),
        }
        return int(keys[0] & group_mask), out

    current = None
    for part in man["parts"]:
        for blk in read_part(Path(batch_dir) / part["file"]):
            keys = np.uint64(prefix) | (head_keys(blk["head"]) >> np.uint64(6 * ABOVE))
            groups = keys & group_mask
            sizes = np.diff(np.r_[np.uint64(0), blk["pay_end"]]).astype(np.int64)
            metrics = shift_moments(blk["metrics"], *shift)
            cuts = np.flatnonzero(groups[1:] != groups[:-1]) + 1
            bounds = np.r_[0, cuts, len(keys)]
            pay_at = np.r_[0, np.cumsum(sizes)]
            for a, e in zip(bounds[:-1], bounds[1:]):
                g = int(groups[a])
                if current is not None and g != current:
                    yield flush()
                    held.clear()
                current = g
                held.append((keys[a:e], blk["ids"][a:e], blk["weight"][a:e], metrics[a:e], sizes[a:e],
                             blk["payload"][pay_at[a]:pay_at[e]]))
    if held:
        yield flush()


class ZoneSums:
    """Sommes des zones de profondeurs d0..d1 au-dessus d'enregistrements triés (comme prep.BatchWriter)."""

    def __init__(self, m, d0, d1):
        self.m, self.d0, self.d1 = m, d0, d1
        self.cur = [None] * (d1 - d0 + 1)
        self.acc = [np.zeros(m + 1) for _ in self.cur]
        self.w = [0.0] * len(self.cur)
        self.keys, self.depths, self.weights, self.sums = [], [], [], []

    def add(self, keys, weight, metrics):
        """Un groupe d'enregistrements triés."""
        vals = np.concatenate([np.ones((len(keys), 1)), metrics], 1)
        for k, d in enumerate(range(self.d0, self.d1 + 1)):
            p = keys & ~np.uint64((1 << (64 - 6 * d)) - 1) if d else np.zeros(len(keys), np.uint64)
            starts = np.flatnonzero(np.r_[True, p[1:] != p[:-1]])
            s = np.add.reduceat(vals, starts, axis=0)
            ws = np.add.reduceat(weight, starts)
            for z, (zk, zs, zw) in enumerate(zip(p[starts], s, ws)):
                zk = int(zk)
                if self.cur[k] is not None and zk != self.cur[k]:
                    self._emit(k, d)
                if self.cur[k] != zk:
                    self.cur[k] = zk
                self.acc[k] += zs
                self.w[k] += zw

    def _emit(self, k, d):
        self.keys.append(self.cur[k])
        self.depths.append(d)
        self.weights.append(self.w[k])
        self.sums.append(self.acc[k].copy())
        self.acc[k][:] = 0
        self.w[k] = 0.0

    def close(self):
        for k, d in enumerate(range(self.d0, self.d1 + 1)):
            if self.cur[k] is not None:
                self._emit(k, d)
                self.cur[k] = None
        m = self.m + 1
        return (np.asarray(self.keys, np.uint64), np.asarray(self.depths, np.int64),
                np.asarray(self.weights, np.float64), np.asarray(self.sums, np.float64).reshape(-1, m))


def write_pave(out_dir, index, quarter_batches, collection, i, j, part_bytes=64 << 20):
    """Fusionne les lots des quarts du pavé (i, j) en parts du lot de la commune ; écrit pave-<index>.json
    (ses parts) et pave-<index>-zones.npz (ses zones de profondeurs 2 à 6). Rien n'est écrit à moitié :
    les fichiers .tmp sont renommés à la fin."""
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    prefix, _ = pave_prefix(collection, i, j)
    shift = (PAVE * i, PAVE * j)
    streams = [rebased_groups(b, prefix, shift) for b in quarter_batches]
    merged = heapq.merge(*[((g, q) + (r,) for g, r in s) for q, s in enumerate(streams)], key=lambda t: (t[0], t[1]))
    zones = ZoneSums(len(METRICS), ABOVE, ABOVE + INNER - 1)
    parts, writer, size, n_part, rows, first, last = [], None, 0, 0, 0, None, None
    payload_bytes = 0
    depth = ABOVE + INNER

    def close():
        nonlocal writer
        sha = writer.close()
        name = writer.name
        os.replace(out_dir / (name + ".tmp"), out_dir / name)
        parts.append({"file": name, "records": rows, "bytes": (out_dir / name).stat().st_size,
                      "first": ixp.location(first, depth) + "/" + firstid, "last": ixp.location(last, depth) + "/" + lastid,
                      "sha256": sha})
        writer = None

    prev = -1
    firstid = lastid = ""
    for g, _q, r in merged:
        keys = r["keys"]
        if int(keys[0]) <= prev:
            raise ValueError("clés hors d'ordre entre quarts")
        prev = int(keys[-1])
        if writer is not None and size >= part_bytes:  # une part finit entre deux groupes (des zones de profondeur 3)
            close()
        if writer is None:
            name = f"part-{index:03d}-{n_part:03d}.ixp"
            n_part += 1
            writer = ixp.PartWriter(str(out_dir / (name + ".tmp")), len(METRICS), True)
            writer.name = name
            size, rows, first, firstid = 0, 0, int(keys[0]), r["ids"][0].decode()
        head = ixp.heads(keys, depth)
        pay_end = np.cumsum(r["sizes"]).astype(np.uint64)
        n = len(keys)
        payload_bytes += int(pay_end[-1]) if n else 0
        for s in range(0, n, ixp.MAX_ROWS):
            e = min(n, s + ixp.MAX_ROWS)
            o0 = int(pay_end[s - 1]) if s else 0
            writer.block(head[s:e], r["ids"][s:e], r["weight"][s:e], r["metrics"][s:e], pay_end[s:e] - np.uint64(o0),
                         r["payload"][o0:int(pay_end[e - 1])])
        rows += n
        size += 13 * n + sum(len(x) for x in r["ids"]) + 8 * (1 + len(METRICS)) * n + int(pay_end[-1] if n else 0)
        last, lastid = int(keys[-1]), r["ids"][-1].decode()
        zones.add(keys, r["weight"], r["metrics"])
    if writer is not None:
        close()
    zk, zd, zw, zs = zones.close()
    np.savez(out_dir / f"pave-{index:03d}-zones.tmp.npz", keys=zk, depths=zd, weights=zw, sums=zs)
    os.replace(out_dir / f"pave-{index:03d}-zones.tmp.npz", out_dir / f"pave-{index:03d}-zones.npz")
    info = {"pave": [i, j], "index": index, "parts": parts, "records": sum(p["records"] for p in parts),
            "payloadBytes": payload_bytes}
    tmp = out_dir / f"pave-{index:03d}.json.tmp"
    tmp.write_text(json.dumps(info, indent=1) + "\n")
    os.replace(tmp, out_dir / f"pave-{index:03d}.json")
    return info


def finish(out_dir, collection, inputs, payload_encoding="json"):
    """zones.ixp (profondeurs 0 à 6) et manifest.json du lot, des pavés écrits."""
    out_dir = Path(out_dir)
    paves = sorted(out_dir.glob("pave-[0-9][0-9][0-9].json"))
    infos = [json.loads(p.read_text()) for p in paves]
    parts = [p for info in infos for p in info["parts"]]
    keys, depths, weights, sums = [], [], [], []
    for info in infos:
        z = np.load(out_dir / f"pave-{info['index']:03d}-zones.npz")
        keys.append(z["keys"]), depths.append(z["depths"]), weights.append(z["weights"]), sums.append(z["sums"])
    keys, depths = np.concatenate(keys), np.concatenate(depths)
    weights, sums = np.concatenate(weights), np.concatenate(sums)
    # profondeurs 0 et 1 : les sommes des zones de profondeur 2 (une par pavé)
    top = depths == ABOVE
    extra_k, extra_d, extra_w, extra_s = [], [], [], []
    for d in range(0, ABOVE):
        p = keys[top] & ~np.uint64((1 << (64 - 6 * d)) - 1) if d else np.zeros(int(top.sum()), np.uint64)
        for zk in np.unique(p):
            sel = p == zk
            extra_k.append(zk), extra_d.append(d)
            extra_w.append(weights[top][sel].sum()), extra_s.append(sums[top][sel].sum(0))
    keys = np.concatenate([keys, np.asarray(extra_k, np.uint64)])
    depths = np.concatenate([depths, np.asarray(extra_d, np.int64)])
    weights = np.concatenate([weights, np.asarray(extra_w)])
    sums = np.concatenate([sums, np.asarray(extra_s).reshape(-1, sums.shape[1])])
    order = np.lexsort((depths, keys))
    h = ixp.heads(keys[order], 0)
    h[:, 12] = depths[order]
    w = ixp.PartWriter(str(out_dir / "zones.ixp"), len(METRICS) + 1, False)
    zw, zm = weights[order], sums[order]
    for s in range(0, len(h), ixp.MAX_ROWS):
        e = min(len(h), s + ixp.MAX_ROWS)
        w.block(h[s:e], [b""] * (e - s), zw[s:e], zm[s:e], None, b"")
    zsha = w.close()
    man = {"format": "indexus-prep-v1", "collection": collection, "metrics": len(METRICS), "metricNames": METRICS,
           "records": int(sum(p["records"] for p in parts)), "payloadBytes": int(sum(i["payloadBytes"] for i in infos)),
           "payloadEncoding": payload_encoding, "recordDepths": [ABOVE + INNER, ABOVE + INNER], "splitDepth": ABOVE + 1,
           "parts": parts, "zones": {"file": "zones.ixp", "depths": [0, ABOVE + INNER - 1], "records": int(len(h)), "sha256": zsha},
           "inputs": inputs}
    man["id"] = hashlib.sha256(json.dumps(inputs, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    tmp = out_dir / "manifest.json.tmp"
    tmp.write_text(json.dumps(man, indent=1) + "\n")
    os.replace(tmp, out_dir / "manifest.json")
    return man
