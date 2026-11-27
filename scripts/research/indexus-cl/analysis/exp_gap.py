"""Can heads computed from sums close the gap with a trained logistic probe?
Wider grids, then random Fourier features (still additive sums, larger G).
Usage: python exp_gap.py data.npz"""
import sys, json, time
import numpy as np
from cl import Stats, ridge, lda, acc

path = sys.argv[1]
D = np.load(path)
X = D["emb"].astype(np.float64); y = D["label"].astype(np.int64); split = D["split"]
C = int(y.max()) + 1; n, d = X.shape
rng = np.random.default_rng(7)
if split.max() > 0:
    tr_all, te = np.where(split == 0)[0], np.where(split == 1)[0]
else:
    p = rng.permutation(n); te, tr_all = p[: n // 5], p[n // 5 :]
p = rng.permutation(tr_all); va, tr = p[: len(p) // 10], p[len(p) // 10 :]
out = {"data": path.split("/")[-1]}


def pick(make, grid, s_tr, s_all, Fva, Fte):
    scores = {}
    for g in grid:
        try:
            scores[g] = acc(make(s_tr, g), Fva, y[va])
        except np.linalg.LinAlgError:
            scores[g] = float("nan")
    best = max((g for g in grid if scores[g] == scores[g]), key=lambda g: scores[g])
    return best, scores, acc(make(s_all, best), Fte, y[te])


def run(tag, feat):
    t = time.time()
    dd = feat(tr[:2]).shape[1]
    s_tr = Stats(dd, C)
    for ch in np.array_split(tr, 40):
        s_tr.add(feat(ch), y[ch])
    s_va_only = Stats(dd, C)
    for ch in np.array_split(va, 4):
        s_va_only.add(feat(ch), y[ch])
    s_all = s_tr + s_va_only          # tr_all = tr + va
    Fva, Fte = feat(va), feat(te)
    r = {}
    r["ridge"] = pick(ridge, (1e-9, 1e-8, 1e-7, 1e-6, 1e-5), s_tr, s_all, Fva, Fte)
    r["lda"] = pick(lda, (1e-6, 1e-5, 1e-4, 1e-3, 1e-2), s_tr, s_all, Fva, Fte)
    out[tag] = {k: {"best": v[0], "val": v[1], "test": v[2]} for k, v in r.items()}
    print(tag, dd, {k: (v[0], round(v[2], 4)) for k, v in r.items()}, f"{time.time()-t:.0f}s", flush=True)


run("linear", lambda i: X[i])
for D_rff, sig in ((2048, 0.7), (2048, 1.0), (4096, 0.7)):
    W = rng.standard_normal((d, D_rff)) / sig
    b = rng.uniform(0, 2 * np.pi, D_rff)
    run(f"rff{D_rff}_s{sig}", lambda i, W=W, b=b, D_rff=D_rff: np.hstack([X[i], np.sqrt(2.0 / D_rff) * np.cos(X[i] @ W + b)]))
json.dump(out, open(out["data"].replace(".npz", "") + "__gap.json", "w"), indent=1)
print("WROTE")
