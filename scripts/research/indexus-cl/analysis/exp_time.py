"""Drift without forgetting: keep one statistics zone per year (as Indexus would)
and pick the model at read time.
  - new, undated-yet traffic (prequential): a model over the last w years
  - a document of a known past year j: that year's zone, shrunk towards the
    cumulative model (ridge with a prior), so nothing learnt is lost.
Usage: python exp_time.py data.npz"""
import sys, json, time
import numpy as np
from cl import Stats, ridge, ridge_prior, lda, acc

path = sys.argv[1]
D = np.load(path)
X = D["emb"].astype(np.float64); y = D["label"].astype(np.int64); year = D["year"].astype(np.int64)
C = int(y.max()) + 1; d = X.shape[1]
rng = np.random.default_rng(7)          # same seed and draw order as exp_cl.py
p = rng.permutation(len(y)); p = rng.permutation(p[len(y) // 5:])  # consume the same draws
yrs = sorted(np.unique(year))
YTR, YTE = {}, {}
for t in yrs:
    idx = rng.permutation(np.where(year == t)[0]); k = len(idx) // 5
    YTE[t], YTR[t] = idx[:k], idx[k:]
S_year = {t: Stats.of(X[YTR[t]], y[YTR[t]], C) for t in yrs}
out = {}
for lam, tau, w in ((1e-6, 30.0, 2), (1e-6, 300.0, 2), (1e-6, 3000.0, 2), (1e-6, 300.0, 1), (1e-6, 300.0, 3)):
    T = len(yrs); R = np.full((T, T), np.nan); preq = []
    t0 = time.time()
    for i, t in enumerate(yrs):
        if i > 0:  # prequential on year t with the window model of the previous step
            preq.append(acc(win, X[YTE[t]], y[YTE[t]]))
        cum = Stats(d, C)
        for u in yrs[: i + 1]:
            cum = cum + S_year[u]
        base = ridge(cum, lam)
        Sw = Stats(d, C)
        for u in yrs[max(0, i - w + 1): i + 1]:
            Sw = Sw + S_year[u]
        win = ridge(Sw, lam)
        for j, u in enumerate(yrs):
            if j <= i:
                m = ridge_prior(S_year[u], tau, *base)   # the year's zone, prior = cumulative
            else:
                m = win                                   # future year: best guess is the window
            R[i, j] = acc(m, X[YTE[u]], y[YTE[u]])
    r = {"prequential_acc": float(np.mean(preq)), "final_acc_all_years": float(np.mean(R[T - 1])),
         "avg_acc_seen": float(np.mean([np.mean(R[i, : i + 1]) for i in range(T)])),
         "BWT": float(np.mean([R[T - 1, j] - R[j, j] for j in range(T - 1)])), "seconds": time.time() - t0}
    key = f"lam{lam}_tau{tau}_w{w}"
    out[key] = r
    print("TIMEZONES", key, json.dumps({k: round(v, 4) for k, v in r.items()}), flush=True)
json.dump(out, open(path.split("/")[-1].replace(".npz", "") + "__time.json", "w"), indent=1)
print("WROTE")
