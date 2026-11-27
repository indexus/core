"""Quality and continual learning: heads computed from sums vs trained probes.
Usage: python exp_cl.py data.npz [static|stream|classinc|zones|all]"""
import sys, time, json, warnings
import numpy as np
from sklearn.linear_model import LogisticRegression, SGDClassifier
from cl import Stats, ridge, ridge_prior, lda, ncm, predict, acc, Space

warnings.filterwarnings("ignore")
path = sys.argv[1]
what = sys.argv[2] if len(sys.argv) > 2 else "all"
D = np.load(path)
X = D["emb"].astype(np.float64)
y = D["label"].astype(np.int64)
year = D["year"].astype(np.int64)
split = D["split"].astype(np.int64)
C = int(y.max()) + 1
n, d = X.shape
name = path.split("/")[-1].replace(".npz", "")
rng = np.random.default_rng(7)
out = {"data": name, "n": n, "d": d, "C": C}
print(out, flush=True)

if split.max() > 0:           # AG News: official split
    tr_all, te = np.where(split == 0)[0], np.where(split == 1)[0]
else:                          # HuffPost: 80/20 random
    p = rng.permutation(n); te, tr_all = p[: n // 5], p[n // 5 :]
# validation carved from train for hyper-parameters
p = rng.permutation(tr_all); va, tr = p[: len(p) // 10], p[len(p) // 10 :]


_warm = {}
def logreg(idx, Cr=10.0, max_iter=200, warm=None):
    m = _warm.get(warm) or LogisticRegression(C=Cr, max_iter=max_iter, tol=1e-4, warm_start=warm is not None)
    try:
        m.fit(X[idx], y[idx])
    except ValueError:  # a new class appeared: the warm start no longer fits
        m = LogisticRegression(C=Cr, max_iter=max_iter, tol=1e-4, warm_start=True)
        m.fit(X[idx], y[idx])
    if warm is not None:
        _warm[warm] = m
    W = np.zeros((d, C)); b = np.full(C, -np.inf)
    W[:, m.classes_] = m.coef_.T; b[m.classes_] = m.intercept_
    return W, b


def timed(f, *a, **k):
    t = time.time(); r = f(*a, **k); return r, time.time() - t


# ------------------------------------------------------------------ static
if what in ("static", "all", "zones"):
    t0 = time.time()
    s_tr = Stats.of(X[tr], y[tr], C)
    grid = {}
    for lam in (1e-6, 1e-5, 1e-4, 1e-3, 1e-2):
        grid[f"ridge_{lam}"] = acc(ridge(s_tr, lam), X[va], y[va])
    for sh in (1e-3, 0.01, 0.05, 0.2, 0.5):
        grid[f"lda_{sh}"] = acc(lda(s_tr, sh), X[va], y[va])
    grid["ncm"] = acc(ncm(s_tr), X[va], y[va])
    best_lam = max((1e-6, 1e-5, 1e-4, 1e-3, 1e-2), key=lambda l: grid[f"ridge_{l}"])
    best_sh = max((1e-3, 0.01, 0.05, 0.2, 0.5), key=lambda s: grid[f"lda_{s}"])
    lr_grid = {}
    for Cr in (1.0, 10.0, 100.0):
        lr_grid[Cr] = acc(logreg(tr[:30000], Cr), X[va], y[va])
    best_C = max(lr_grid, key=lr_grid.get)
    s_all = Stats.of(X[tr_all], y[tr_all], C)
    (m_lr, t_lr) = timed(logreg, tr_all, best_C)
    (m_r, t_r) = timed(ridge, s_all, best_lam)
    (m_l, t_l) = timed(lda, s_all, best_sh)
    res = {
        "val_grid": grid, "val_logreg": {str(k): v for k, v in lr_grid.items()},
        "best": {"ridge_lambda": best_lam, "lda_shrink": best_sh, "logreg_C": best_C},
        "test_acc": {"logreg_trained": acc(m_lr, X[te], y[te]), "ridge_sums": acc(m_r, X[te], y[te]),
                     "lda_sums": acc(m_l, X[te], y[te]), "ncm_sums": acc(ncm(s_all), X[te], y[te])},
        "fit_seconds": {"logreg": t_lr, "ridge_solve": t_r, "lda_solve": t_l},
    }
    out["static"] = res
    print("STATIC", json.dumps(res["test_acc"]), json.dumps(res["best"]), json.dumps(res["fit_seconds"]), f"{time.time()-t0:.0f}s", flush=True)

# ------------------------------------------------------------------ zones
# Local models: Indexus variant-1 zones (random projection, Gaussian
# quantiles), each zone a ridge shrunk towards its parent zone's model.
if what in ("zones", "all"):
    t0 = time.time()
    best_lam = out["static"]["best"]["ridge_lambda"]
    s_all_tr = Stats.of(X[tr], y[tr], C)
    root = ridge(s_all_tr, best_lam)
    zres = {}
    for k_axes in (8,):
        sp = Space(X[tr], k_axes, seed=3, quantile=True)
        DEP = 10
        ctr, cva = sp.codes(X[tr], DEP), sp.codes(X[va], DEP)
        for tau in (30.0, 300.0, 3000.0):
            levels = [{0: root}]  # levels[l][zone code] -> model
            accs = {0: acc(root, X[va], y[va])}
            prev = {0: root}
            for lvl in range(1, DEP + 1):
                cur = {}
                for z in np.unique(ctr[:, lvl]):
                    m = ctr[:, lvl] == z
                    par = prev[z >> 1]
                    if m.sum() < 5:
                        cur[z] = par; continue
                    s = Stats.of(X[tr[m]], y[tr[m]], C)
                    cur[z] = ridge_prior(s, tau, *par)
                # predict validation with the deepest available zone model
                pred = np.empty(len(va), np.int64)
                for z in np.unique(cva[:, lvl]):
                    m = cva[:, lvl] == z
                    mdl, zz, l = cur.get(z), z, lvl
                    while mdl is None:  # walk up to the nearest trained ancestor
                        zz >>= 1; l -= 1
                        mdl = levels[l].get(zz)
                    pred[m] = predict(mdl, X[va[m]])
                accs[lvl] = float((pred == y[va]).mean())
                levels.append(cur)
                prev = cur
            zres[f"k{k_axes}_tau{tau}"] = accs
            print("ZONES", f"k={k_axes} tau={tau}", json.dumps({l: round(a, 4) for l, a in accs.items()}), flush=True)
    out["zones_val_acc_by_depth"] = zres
    print(f"ZONES {time.time()-t0:.0f}s", flush=True)

# ------------------------------------------------------------------ stream
# Year by year (HuffPost). Each year split 80/20. Prequential accuracy
# (predict year t before learning it), accuracy matrix R[t, j], BWT, FWT.
if what in ("stream", "all") and year.max() > 0:
    t0 = time.time()
    yrs = sorted(np.unique(year))
    YTR, YTE = {}, {}
    for t in yrs:
        idx = rng.permutation(np.where(year == t)[0]); k = len(idx) // 5
        YTE[t], YTR[t] = idx[:k], idx[k:]
    lam = out.get("static", {}).get("best", {}).get("ridge_lambda", 1e-4)
    sh = out.get("static", {}).get("best", {}).get("lda_shrink", 0.05)
    Cr = out.get("static", {}).get("best", {}).get("logreg_C", 10.0)
    methods = ["lda_sums", "ridge_sums", "lda_decay0.5", "lda_window2", "sgd_online", "logreg_retrain", "logreg_first_year"]
    R = {m: np.full((len(yrs), len(yrs)), np.nan) for m in methods}
    preq = {m: [] for m in methods}
    cost = {m: 0.0 for m in methods}
    per_year = {}
    S = Stats(d, C); Sdec = Stats(d, C)
    sgd = SGDClassifier(loss="log_loss", alpha=1e-5, random_state=0)
    first = None
    current = {}
    for i, t in enumerate(yrs):
        Xt, yt = X[YTR[t]], y[YTR[t]]
        # prequential: models from previous step on this year's test set
        if current:
            for m in methods:
                preq[m].append(acc(current[m], X[YTE[t]], y[YTE[t]]))
        # updates
        tt = time.time(); S.add(Xt, yt); current["lda_sums"] = lda(S, sh); cost["lda_sums"] += time.time() - tt
        tt = time.time(); current["ridge_sums"] = ridge(S, lam); cost["ridge_sums"] += time.time() - tt
        tt = time.time(); Sdec = Sdec.scaled(0.5) + Stats.of(Xt, yt, C); current["lda_decay0.5"] = lda(Sdec, sh); cost["lda_decay0.5"] += time.time() - tt
        tt = time.time()
        Sw = Stats(d, C)
        for u in yrs[max(0, i - 1): i + 1]:
            Sw = Sw + Stats.of(X[YTR[u]], y[YTR[u]], C)  # window as a read of 2 year zones
        current["lda_window2"] = lda(Sw, sh); cost["lda_window2"] += time.time() - tt
        tt = time.time()
        for b in np.array_split(rng.permutation(len(yt)), max(1, len(yt) // 256)):
            sgd.partial_fit(Xt[b], yt[b], classes=np.arange(C))
        W = np.zeros((d, C)); W[:, sgd.classes_] = sgd.coef_.T
        current["sgd_online"] = (W, sgd.intercept_.copy()); cost["sgd_online"] += time.time() - tt
        tt = time.time()
        idx_so_far = np.concatenate([YTR[u] for u in yrs[: i + 1]])
        current["logreg_retrain"] = logreg(idx_so_far, Cr, warm="stream"); cost["logreg_retrain"] += time.time() - tt
        if first is None:
            first = logreg(YTR[t], Cr)
        current["logreg_first_year"] = first
        for m in methods:
            for j, u in enumerate(yrs):
                R[m][i, j] = acc(current[m], X[YTE[u]], y[YTE[u]])
        per_year[int(t)] = {"train": int(len(yt)), "classes_seen": int((S.Y > 0).sum())}
        print("YEAR", t, len(yt), {m: round(R[m][i, i], 3) for m in methods}, flush=True)
    T = len(yrs)
    summ = {}
    for m in methods:
        r = R[m]
        summ[m] = {
            "prequential_acc": float(np.mean(preq[m])),
            "final_acc_all_years": float(np.mean(r[T - 1, :])),
            "avg_acc_seen": float(np.mean([np.mean(r[i, : i + 1]) for i in range(T)])),
            "BWT": float(np.mean([r[T - 1, j] - r[j, j] for j in range(T - 1)])),
            "FWT": float(np.mean([r[j - 1, j] for j in range(1, T)])),
            "update_seconds_total": cost[m],
        }
    out["stream"] = {"years": [int(t) for t in yrs], "per_year": per_year, "summary": summ, "R": {m: R[m].round(4).tolist() for m in methods}}
    for m in methods:
        print("STREAM", m, json.dumps({k: round(v, 4) for k, v in summ[m].items()}), flush=True)
    print(f"STREAM {time.time()-t0:.0f}s", flush=True)

# ------------------------------------------------------------------ class-incremental
# Classes arrive one task at a time (no replay). Sums vs SGD.
if what in ("classinc", "all"):
    t0 = time.time()
    order = rng.permutation(C)
    ntask = min(C, 4) if C <= 8 else 6
    tasks = np.array_split(order, ntask)
    trte = (tr_all, te)
    sh = out.get("static", {}).get("best", {}).get("lda_shrink", 0.05)
    lam = out.get("static", {}).get("best", {}).get("ridge_lambda", 1e-4)
    methods = ["lda_sums", "ridge_sums", "ncm_sums", "sgd_online"]
    R = {m: np.full((ntask, ntask), np.nan) for m in methods}
    S = Stats(d, C)
    sgd = SGDClassifier(loss="log_loss", alpha=1e-5, random_state=0)
    for i, cls in enumerate(tasks):
        itr = trte[0][np.isin(y[trte[0]], cls)]
        S.add(X[itr], y[itr])
        cur = {"lda_sums": lda(S, sh), "ridge_sums": ridge(S, lam), "ncm_sums": ncm(S)}
        for ep in range(1):
            for b in np.array_split(rng.permutation(itr), max(1, len(itr) // 256)):
                sgd.partial_fit(X[b], y[b], classes=np.arange(C))
        W = np.zeros((d, C)); W[:, sgd.classes_] = sgd.coef_.T
        cur["sgd_online"] = (W, sgd.intercept_.copy())
        for m in methods:
            for j, cj in enumerate(tasks):
                ite = trte[1][np.isin(y[trte[1]], cj)]
                R[m][i, j] = acc(cur[m], X[ite], y[ite])
    summ = {}
    for m in methods:
        r = R[m]
        summ[m] = {"final_avg_acc": float(np.mean(r[-1])), "BWT": float(np.mean([r[-1, j] - r[j, j] for j in range(ntask - 1)]))}
        print("CLASSINC", m, json.dumps({k: round(v, 4) for k, v in summ[m].items()}), flush=True)
    out["classinc"] = {"tasks": [list(map(int, t)) for t in tasks], "summary": summ, "R": {m: R[m].round(4).tolist() for m in methods}}
    print(f"CLASSINC {time.time()-t0:.0f}s", flush=True)

json.dump(out, open(f"{name}__cl_{what}.json", "w"), indent=1)
print("WROTE", flush=True)
