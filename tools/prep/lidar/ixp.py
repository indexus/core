"""Write a prep batch (docs/ingest/pipeline.md, section 5) from numpy arrays.

The Python twin of internal/prep: `go run ./tools/prep verify <dir>` checks
what this writes. Records arrive sorted; keys are location bits left aligned
in a uint64 (locations of at most 10 characters) with one depth for all.
"""

import base64, hashlib, json, os, struct, zlib
import numpy as np

ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
MAX_ROWS = 1 << 16


def space_tables(collection, dims, levels=12):
    """The SDK's Space tables of a collection of `dims` linear dimensions: one bit
    per dimension in turn, and the per-character offset of the collection name."""
    return {"mask": list(range(dims)) * (6 * levels // dims),
            "offset": [ALPHABET.index(collection[l % len(collection)]) for l in range(levels)]}


def locations(tables, cells, depth):
    """sdk Space.encode of integer cells (one column per dimension, cells of
    the finest grid of `depth` characters), as the builders compute it."""
    mask, offset = tables["mask"], tables["offset"]
    cells = np.asarray(cells, np.int64)
    dims = cells.shape[1]
    per = 6 * depth // dims  # bits per dimension
    lo = np.zeros(cells.shape, np.int64)
    size = np.full(dims, 2**per, np.int64)
    out = []
    for level in range(depth):
        idx = np.zeros(len(cells), np.int64)
        for bit in range(6):
            d = mask[level * 6 + bit]
            size[d] //= 2
            upper = cells[:, d] >= lo[:, d] + size[d]
            idx = idx * 2 + upper
            lo[:, d] += np.where(upper, size[d], 0)
        out.append(np.array(list(ALPHABET))[(offset[level] + idx) % 64])
    return ["".join(c) for c in np.stack(out, 1)] if len(cells) else []


def crc32(data):
    """CRC-32 (IEEE, zlib's): the checksum of blocks and of the index."""
    return zlib.crc32(data) & 0xFFFFFFFF


def location(key, depth):
    out = []
    for l in range(depth):
        out.append(ALPHABET[(int(key) >> (58 - 6 * l)) & 63])
    return "".join(out) or "@"


def heads(keys, depth):
    """[n][13] heads: 12 bytes of bits (big-endian, left aligned) and the depth."""
    n = len(keys)
    h = np.zeros((n, 13), np.uint8)
    h[:, :8] = keys.astype(">u8").view(np.uint8).reshape(n, 8)
    h[:, 12] = depth
    return h


def pad8(b):
    return b + b"\0" * (-len(b) % 8)


class PartWriter:
    def __init__(self, path, m, payloads):
        self.f = open(path, "wb")
        self.sha = hashlib.sha256()
        self.m, self.payloads, self.off, self.rows, self.index = m, payloads, 0, 0, []
        self.write(b"IXP1" + struct.pack("<HHII", 1, m, 1 if payloads else 0, 0))

    def write(self, b):
        self.f.write(b)
        self.sha.update(b)
        self.off += len(b)

    def block(self, head, ids, weight, metrics, pay_end, payload):
        """head [n][13] u8, ids list of bytes, weight [n] f64, metrics [n][m] f64, pay_end [n] u64 (cumulative), payload bytes."""
        n = len(head)
        id_bytes = b"".join(ids)
        id_end = np.cumsum([len(i) for i in ids], dtype=np.uint64).astype("<u4")
        parts = [struct.pack("<IIQ", n, len(id_bytes), len(payload)), pad8(head.tobytes()), pad8(id_end.tobytes())]
        if self.payloads:
            parts.append(pay_end.astype("<u8").tobytes())
        parts.append(weight.astype("<f8").tobytes())
        parts.append(np.ascontiguousarray(metrics.T).astype("<f8").tobytes())
        parts.append(pad8(id_bytes))
        parts.append(pad8(bytes(payload)))
        body = b"".join(parts)
        body += struct.pack("<II", crc32(body), 0)
        self.index.append(bytes(head[0]) + b"\0\0\0" + struct.pack("<QII", self.off, len(body), n))
        self.rows += n
        self.write(body)

    def close(self):
        at = self.off
        ix = b"".join(self.index)
        self.write(ix)
        self.write(struct.pack("<QQII", at, self.rows, len(self.index), crc32(ix)) + b"IXP1" + b"\0" * 4)
        self.f.close()
        return self.sha.hexdigest()


def write_batch(out, collection, keys, depth, rid, metrics, pay_off, payload, split_depth=2, part_bytes=64 << 20,
                zone_depths=(0, 4), inputs=None, metric_names=None, payload_encoding="binary"):
    """keys [n] uint64 sorted strictly, depth int, rid bytes (one id for all records), metrics [n][m] f64,
    pay_off [n+1] offsets into payload (bytes-like, numpy uint8)."""
    os.makedirs(out, exist_ok=True)
    n, m = metrics.shape
    assert np.all(keys[1:] > keys[:-1]), "keys must be strictly increasing"
    head = heads(keys, depth)
    weight = np.ones(n)
    sizes = 13 + len(rid) + 8 * (1 + m) + np.diff(pay_off)
    # parts end at the first boundary of a split-depth zone past part_bytes
    zone = keys >> np.uint64(64 - 6 * split_depth) if split_depth else np.zeros(n, np.uint64)
    cuts, acc, start = [], 0, 0
    csum = np.cumsum(sizes)
    while start < n:
        target = (csum[start - 1] if start else 0) + part_bytes
        end = int(np.searchsorted(csum, target, side="left")) + 1
        if end >= n:
            cuts.append((start, n))
            break
        end = int(np.searchsorted(zone, zone[end - 1], side="right"))  # finish the zone
        cuts.append((start, end))
        start = end
    parts = []
    for p, (a, b) in enumerate(cuts):
        name = f"part-{p:05d}.ixp"
        w = PartWriter(os.path.join(out, name), m, True)
        for s in range(a, b, MAX_ROWS):
            e = min(b, s + MAX_ROWS)
            o0 = int(pay_off[s])
            w.block(head[s:e], [rid] * (e - s), weight[s:e], metrics[s:e], (pay_off[s + 1:e + 1] - o0),
                    payload[o0:int(pay_off[e])])
        sha = w.close()
        parts.append({"file": name, "records": b - a, "bytes": os.path.getsize(os.path.join(out, name)),
                      "first": location(keys[a], depth) + "/" + rid.decode(), "last": location(keys[b - 1], depth) + "/" + rid.decode(),
                      "sha256": sha})
    man = {"format": "indexus-prep-v1", "collection": collection, "metrics": m}
    if metric_names:
        man["metricNames"] = metric_names
    man.update({"records": n, "payloadBytes": int(pay_off[-1]), "payloadEncoding": payload_encoding, "recordDepths": [depth, depth], "splitDepth": split_depth, "parts": parts})
    if zone_depths and zone_depths[1] >= zone_depths[0]:
        zh, zw, zm = zones(keys, depth, metrics, zone_depths)
        w = PartWriter(os.path.join(out, "zones.ixp"), m + 1, False)
        for s in range(0, len(zh), MAX_ROWS):
            e = min(len(zh), s + MAX_ROWS)
            w.block(zh[s:e], [b""] * (e - s), zw[s:e], zm[s:e], None, b"")
        man["zones"] = {"file": "zones.ixp", "depths": list(zone_depths), "records": len(zh), "sha256": w.close()}
    inputs = inputs or {"tool": "ixp.py", "sources": {}}
    man["inputs"] = inputs
    man["id"] = hashlib.sha256(json.dumps(inputs, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    with open(os.path.join(out, "manifest.json"), "w") as fh:
        fh.write(json.dumps(man, indent=1) + "\n")
    return man


def json_strings(payload, pay_off):
    """Each payload as a JSON string of base64 (what the node stores today for a
    base64 payload). Payload sizes must be multiples of 3 bytes: then the base64
    of the whole is the concatenation of the base64 of each, with no padding."""
    assert np.all(np.diff(pay_off) % 3 == 0)
    b64 = np.frombuffer(base64.b64encode(np.ascontiguousarray(payload).tobytes()), np.uint8)
    n = len(pay_off) - 1
    off = (pay_off.astype(np.int64) // 3) * 4 + 2 * np.arange(n + 1)
    out = np.full(int(off[-1]), ord('"'), np.uint8)
    keep = np.ones(len(out), bool)
    keep[off[:-1]] = False
    keep[off[1:] - 1] = False
    out[keep] = b64
    return out, off.astype(np.uint64)


def zones(keys, depth, metrics, zone_depths):
    """Sums of every zone of the given depths above records of one depth, sorted by head."""
    n, m = metrics.shape
    allk, alld, allw, allm = [], [], [], []
    vals = np.concatenate([np.ones((n, 1)), metrics], 1)
    for d in range(zone_depths[0], min(zone_depths[1], depth) + 1):
        p = keys & ~np.uint64((1 << (64 - 6 * d)) - 1) if d else np.zeros(n, np.uint64)
        starts = np.flatnonzero(np.r_[True, p[1:] != p[:-1]])
        allk.append(p[starts])
        alld.append(np.full(len(starts), d))
        s = np.add.reduceat(vals, starts, axis=0)
        allw.append(s[:, 0])
        allm.append(s)
    k, d = np.concatenate(allk), np.concatenate(alld)
    order = np.lexsort((d, k))
    h = heads(k[order], 0)
    h[:, 12] = d[order]
    return h, np.concatenate(allw)[order], np.concatenate(allm)[order]
