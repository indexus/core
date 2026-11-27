#!/usr/bin/env python3
"""Independent reference encoder for docs/protocol/wire-block.md (IXB2).

It exists only to produce golden files that the Go encoder and the JS decoder
are both tested against. It is deliberately written from the document, not
from either implementation. Regenerate with:

    python3 reference.py            # writes <case>.bin and <case>.json here
"""
import json
import math
import os
import struct

ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"


def pad4(b):
    return b + b"\0" * (-len(b) % 4)


def key_words(suffix):
    k = math.ceil(6 * len(suffix) / 32)
    bits = 0
    for ch in suffix:
        bits = (bits << 6) | ALPHABET.index(ch)
    bits <<= 32 * k - 6 * len(suffix)
    return [(bits >> (32 * (k - 1 - w))) & 0xFFFFFFFF for w in range(k)], k


def f32(x):
    return struct.unpack("<f", struct.pack("<f", x))[0]


def rows_sections(parent, rows, offset, f64):
    """rows: {location: (count, [metrics])}; offset: where the first section
    starts in the body. Returns encoded sections + expectation."""
    groups = {}
    for loc, (count, metrics) in rows.items():
        loc = loc.split(":", 1)[0][:32]
        p = "" if parent == "@" else parent
        if not loc.startswith(p):
            p = ""
        suffix = loc[len(p):]
        g = groups.setdefault((p, len(suffix)), {})
        if suffix in g:  # fold collisions
            c, m = g[suffix]
            width = max(len(m), len(metrics))
            m = [(m[i] if i < len(m) else 0) + (metrics[i] if i < len(metrics) else 0) for i in range(width)]
            g[suffix] = (c + count, m)
        else:
            g[suffix] = (count, list(metrics))
    out, expect = [], []
    for (p, s) in sorted(groups, key=lambda t: (t[0] != ("" if parent == "@" else parent), t[1])):
        g = groups[(p, s)]
        encoded = sorted((key_words(sfx)[0], sfx, v) for sfx, v in g.items())
        n, k = len(encoded), math.ceil(6 * s / 32)
        m = max(len(v[1]) for _, _, v in encoded)
        # More than 255 metrics (an embedding): kind 4, whose header ends
        # with a u32 metric count and keeps 0 in the u8 one.
        wide_rows = m > 255
        if wide_rows:
            body = struct.pack("<IBBBBII", n, len(p), s, k, 0, 0, m) + pad4(p.encode())
        else:
            body = struct.pack("<IBBBBI", n, len(p), s, k, m, 0) + pad4(p.encode())
        body += b"".join(struct.pack("<%dI" % k, *w) for w, _, _ in encoded)
        body += struct.pack("<%dI" % n, *[v[0] for _, _, v in encoded])
        if f64 and m:
            # The f64 column starts on a multiple of 8 from the start of the body.
            body += b"\0" * (-(offset + 8 + len(body)) % 8)
        for j in range(m):
            body += struct.pack("<%d%s" % (n, "d" if f64 else "f"), *[(v[1][j] if j < len(v[1]) else 0.0) for _, _, v in encoded])
        out.append(struct.pack("<IHH", 8 + len(body), 4 if wide_rows else 1, 0) + body)
        offset += 8 + len(body)
        wide = (lambda x: float(x)) if f64 else f32
        expect.append({
            "parent": p, "suffixChars": s, "keyWords": k, "metricCount": m, "revision": 0,
            "metricBytes": 8 if f64 else 4,
            "suffixes": [sfx for _, sfx, _ in encoded],
            "keys": [x for w, _, _ in encoded for x in w],
            "count": [v[0] for _, _, v in encoded],
            "metrics": [[wide(v[1][j] if j < len(v[1]) else 0.0) for _, _, v in encoded] for j in range(m)],
        })
    return out, expect


def redirects_section(redirects):
    body = struct.pack("<I", len(redirects))
    for r in redirects:
        loc, name, ip = (r[x].encode() for x in ("location", "name", "ip"))
        body += pad4(struct.pack("<HHHH", len(loc), len(name), len(ip), r["port"]) + loc + name + ip)
    return struct.pack("<IHH", 8 + len(body), 2, 0) + body


def columns_section(columns):
    body = pad4(struct.pack("<I%dH" % len(columns), len(columns), *columns))
    return struct.pack("<IHH", 8 + len(body), 3, 0) + body


def encode(requests, redirects, f64=False, columns=None):
    """columns: the metric positions to send (columns=...), or None for all."""
    sections, expect = [], []
    offset = 16
    if columns is not None:
        sections.append(columns_section(columns))
        offset += len(sections[0])
        requests = [(parent, {loc: (count, [m[c] if c < len(m) else 0.0 for c in columns]) for loc, (count, m) in rows.items()})
                    for parent, rows in requests]
    for parent, rows in requests:
        s, e = rows_sections(parent, rows, offset, f64)
        sections += s
        expect += e
        offset += sum(len(x) for x in s)
    flags = 2 if f64 else 0
    if columns is not None:
        flags |= 4
    if any(e["metricCount"] > 255 for e in expect):
        flags |= 8
    if redirects:
        sections.append(redirects_section(redirects))
        flags |= 1
    head = b"IXB2" + struct.pack("<HHII", 16, flags, len(sections), 0)
    out = {"sections": expect, "redirects": redirects}
    if columns is not None:
        out["columns"] = columns
    return head + b"".join(sections), out


BASIC = (
    [
        ("@", {"7": (3, [10.0, 2.5]), "a": (1, [4.0]), "7a": (2, [1.0, 0.25, 7.0]), "_-": (5, [])}),
        ("7", {"7B": (4, [1.5]), "7B:ref1": (1, [0.5]), "7C": (2, [123456.789]), "7Bxyz": (1, [3.0])}),
    ],
    [{"location": "a", "name": "peerA", "ip": "10.0.0.2", "port": 21000}],
)

# Edges of the key and metric rules, one request per kind of case:
# - "7B": a key equal to its parent (zero-character suffix, k = 0, folded with
#   its ":ref" twin), 6-character suffixes whose 6th character straddles words
#   0 and 1 (k = 2), keys outside the parent (sent whole under parent ""), and
#   metric lengths from 0 to 5 in one section;
# - "@": a 32-character key (k = 6) and a 33-character one truncated onto it;
# - "b": large and negative metrics, rounded to float32 once.
# Several parents and no redirects (flags = 0). Every value is exact in
# float64, so the order in which collisions are summed does not matter.
EDGES = (
    [
        ("7B", {
            "7B": (2, [1.0]), "7B:r2": (1, [2.0, 3.0]),
            "7Babcdef": (1, [1.0, 2.0, 3.0, 4.0, 5.0]), "7Bzzzzz_": (3, [0.5]),
            "7C": (1, [9.0]), "c9": (4, [8.0]), "c9:x": (1, []),
        }),
        ("@", {
            "0123456789ABCDEFGHIJKLMNOPQRSTUV": (1, [1.0]),
            "0123456789ABCDEFGHIJKLMNOPQRSTUVW": (2, [2.0, 1.0]),
            "_": (1, [-1.5]),
        }),
        ("b", {"bQ": (1, [1e12, 0.1]), "b-_": (2, [3.0e5, 1.5e7, 16777217.0])}),
    ],
    [],
)

# Aggregates of a DVF collection under one zone, as the node holds them:
# [Σ valeur foncière, Σ surface, Σ €/m², Σ (lat + 90), Σ (lng + 180)], from a
# single sale up to sums of 1e12. sdk checks against this file that what an
# application displays is the same from IXB2 as from v1.
DVF = (
    [
        ("u0", {
            "u0A": (1, [189056.0, 34.0, 5560.470588235294, 135.74761759038387, 183.12291497781908]),
            "u0B": (1, [133365.0, 86.0, 1550.7558139534883, 136.6010415690312, 179.2485833346568]),
            "u0C": (3, [2286068.0, 616.0, 11598.584995319996, 407.7495272404708, 549.9689999152474]),
            "u0D": (250, [98538282.0, 40373.0, 916104.7833720458, 34045.66122428856, 45711.71846422679]),
            "u0E": (40000, [15206604138.0, 6608310.0, 130492857.77623127, 5440081.885141663, 7320566.159935768]),
            "u0F": (200000, [76030951788.0, 33002614.0, 652558071.3831596, 27200705.161940936, 36599995.199505344]),
            "u0G": (290000, [1012345678901.0, 43210987.0, 6672345678.25, 39672000.123456, 52867000.654321]),
        }),
    ],
    [],
)

# metrics=f64. The metrics column of every section is preceded by the zeros
# that put it on a multiple of 8 from the start of the body: parents of 0, 1,
# 2 and 5 characters and keys of 1 and 2 words move the count column's end
# across both residues, and a section without metrics takes no padding.
ALIGN = (
    [
        ("@", {"a": (1, [0.1]), "bC": (2, [1e300, -2.5e-300]), "dEf": (5, [])}),
        ("X", {"X1": (3, []), "XYZ": (1, [16777217.0, 1.0, 2.0])}),
        ("ab", {"abc": (1, [3.0]), "abcdefgh": (2, [123456.789, 0.5])}),
        ("q_-9Z", {"q_-9Z0": (4, [1.0 / 3.0])}),
    ],
    [{"location": "X", "name": "peerX", "ip": "10.0.0.3", "port": 21001}],
)

# A 300-metric embedding beside narrow rows: "7" goes out as a wide
# section (kind 4, flag bit 3), "@" stays a narrow one. Values are exact
# in float32.
EMBED = [(j % 13) * 0.5 - 3.0 for j in range(300)]
WIDE = (
    [
        ("@", {"a": (1, [1.0, 2.0]), "b": (2, [])}),
        ("7", {"7B": (1, EMBED), "7C": (3, EMBED[:7])}),
    ],
    [],
)

CASES = {
    "wide": WIDE, "wide-f64": WIDE + (True,),
    "basic": BASIC, "edges": EDGES, "dvf": DVF,
    "basic-f64": BASIC + (True,), "align-f64": ALIGN + (True,),
    # columns=2,0 (metric 2 first; rows without it send 0), then f64 with
    # one column, whose alignment counts the columns section.
    "columns": BASIC + (False, [2, 0]), "columns-f64": ALIGN + (True, [1]),
}

if __name__ == "__main__":
    here = os.path.dirname(os.path.abspath(__file__))
    for name, case in CASES.items():
        raw, expect = encode(*case)
        with open(os.path.join(here, name + ".bin"), "wb") as f:
            f.write(raw)
        with open(os.path.join(here, name + ".json"), "w") as f:
            json.dump(expect, f, indent=1)
            f.write("\n")
        print(name, len(raw), "bytes")
