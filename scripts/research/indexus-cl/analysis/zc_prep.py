"""Builds the zonecheck input (class char + 7-char spatial address, x in k
projected dims) and, after the Go run, compares Indexus totals with numpy.
Usage: python zc_prep.py make data.npz N k out.bin
       python zc_prep.py check out.bin totals.jsonl"""
import sys, json
import numpy as np
from cl import Space, Stats, lda, predict

ALPHA = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"


def load_bin(p):
    raw = open(p, "rb").read()
    n, k, C = np.frombuffer(raw[:12], np.int32)
    rec = np.dtype([("c", "<u2"), ("loc", "S8"), ("x", "<f8", (k,))])
    a = np.frombuffer(raw[12:], rec, count=n)
    return a, int(k), int(C)


def metrics_of(x):
    k = x.shape[1]
    iu = np.triu_indices(k)
    return np.hstack([x, (x[:, :, None] * x[:, None, :])[:, iu[0], iu[1]]])


if sys.argv[1] == "make":
    D = np.load(sys.argv[2]); N, k = int(sys.argv[3]), int(sys.argv[4])
    X = D["emb"].astype(np.float64)[:N]; y = D["label"].astype(np.int64)[:N]
    C = int(D["label"].max()) + 1
    rng = np.random.default_rng(5)
    P = rng.standard_normal((X.shape[1], k)) / np.sqrt(k)
    Z = X @ P
    sp = Space(X, 6, seed=9)
    codes = sp.codes(X, 42)[:, 42]   # 42 bits = 7 characters of 6 bits
    locs = []
    for c, code in zip(y, codes):
        s = ALPHA[c] + "".join(ALPHA[(int(code) >> (6 * (6 - i))) & 63] for i in range(7))
        locs.append(s.encode())
    rec = np.dtype([("c", "<u2"), ("loc", "S8"), ("x", "<f8", (k,))])
    a = np.zeros(N, rec); a["c"] = y; a["loc"] = locs; a["x"] = Z
    with open(sys.argv[5], "wb") as f:
        f.write(np.array([N, k, C], np.int32).tobytes()); f.write(a.tobytes())
    print("wrote", N, k, C, len(set(locs)), "distinct locations")
else:
    a, k, C = load_bin(sys.argv[2])
    rows = [json.loads(l) for l in open(sys.argv[3])]
    T = {(r["store"], r["zone"]): (r["count"], np.array(r["metrics"])) for r in rows}
    M = metrics_of(a["x"])
    ids = np.arange(len(a))
    sel = {"A": ids, "B": ids, "C": ids[ids % 10 != 0], "D": ids[ids % 10 != 0], "E": ids}
    def exact(idx, c=None):
        m = idx if c is None else idx[a["c"][idx] == c]
        # reference: math.fsum per column (correctly rounded sum)
        import math
        return len(m), np.array([math.fsum(col) for col in M[m].T]) if len(m) else np.zeros(M.shape[1])
    report = {}
    for st, idx in sel.items():
        cnt, ref = exact(idx)
        got_cnt, got = T[(st, "@")]
        e = {"root_count_ok": int(got_cnt) == cnt, "root_max_rel": float(np.max(np.abs(got - ref) / np.maximum(np.abs(ref), 1e-300))),
             "root_bitwise_equal_fsum": int(np.sum(got == ref)), "cols": len(ref)}
        worst = 0.0; cnt_ok = True
        for c in range(C):
            ccount, cref = exact(idx, c)
            if (st, ALPHA[c]) not in T or T[(st, ALPHA[c])][0] == 0:
                cnt_ok &= ccount == 0; continue
            gc, gm = T[(st, ALPHA[c])]
            cnt_ok &= int(gc) == ccount
            if ccount:
                worst = max(worst, float(np.max(np.abs(gm[:k] - cref[:k]) / np.maximum(np.abs(cref[:k]), 1e-300))))
        e["class_counts_ok"], e["class_sum_max_rel"] = bool(cnt_ok), worst
        report[st] = e
    # same model? LDA in the k projected dims from Indexus totals vs from fsum
    def stats_from(st):
        s = Stats(k, C); s.n = float(T[(st, "@")][0])
        iu = np.triu_indices(k); G = np.zeros((k, k)); G[iu] = T[(st, "@")][1][k:]; G = G + G.T - np.diag(np.diag(G))
        s.G = G
        for c in range(C):
            if (st, ALPHA[c]) in T and T[(st, ALPHA[c])][0] > 0:
                s.Y[c] = T[(st, ALPHA[c])][0]; s.B[:, c] = T[(st, ALPHA[c])][1][:k]
        return s
    X = a["x"]
    pr = {st: predict(lda(stats_from(st), 0.05), X) for st in sel}
    ref_C = Stats.of(X[sel["C"]], a["c"][sel["C"]].astype(int), C)
    report["pred_equal"] = {"A_vs_B": float((pr["A"] == pr["B"]).mean()), "C_vs_D": float((pr["C"] == pr["D"]).mean()),
                            "E_vs_A": float((pr["E"] == pr["A"]).mean()),
                            "C_vs_numpy": float((pr["C"] == predict(lda(ref_C, 0.05), X)).mean())}
    print(json.dumps(report, indent=1))
