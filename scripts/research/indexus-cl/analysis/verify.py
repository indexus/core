"""Independent re-computation of the headline numbers with scikit-learn only
(no code from cl.py): static accuracies, exact unlearning, probe convergence."""
import sys, json, time, warnings
import numpy as np
from sklearn.linear_model import Ridge, LogisticRegression
from sklearn.discriminant_analysis import LinearDiscriminantAnalysis as LDA
warnings.filterwarnings("ignore")
path, lam, sh = sys.argv[1], float(sys.argv[2]), float(sys.argv[3])
D = np.load(path); X = D["emb"].astype(np.float64); y = D["label"].astype(np.int64); split = D["split"]
C = int(y.max()) + 1; n = len(y)
rng = np.random.default_rng(7)
if split.max() > 0:
    tr, te = np.where(split == 0)[0], np.where(split == 1)[0]
else:
    p = rng.permutation(n); te, tr = p[: n // 5], p[n // 5:]
Y = np.eye(C)[y]
r = {}
t = time.time()
rd = Ridge(alpha=lam * len(tr)).fit(X[tr], Y[tr])
r["ridge_test_acc"] = float((rd.predict(X[te]).argmax(1) == y[te]).mean())
ld = LDA(solver="lsqr", shrinkage=sh).fit(X[tr], y[tr])
r["lda_test_acc_sklearn_shrinkage"] = float(ld.score(X[te], y[te]))
# exact unlearning: retrain on the kept 90 % vs the same model from the full fit
keep = tr[rng.random(len(tr)) >= 0.10]
rk = Ridge(alpha=lam * len(keep)).fit(X[keep], Y[keep])
r["unlearn_check_ridge_keep_acc"] = float((rk.predict(X[te]).argmax(1) == y[te]).mean())
from cl import Stats, ridge, predict          # subtraction path, compared with the sklearn retrain
s = Stats.of(X[tr], y[tr], C); gone = np.setdiff1d(tr, keep); s.remove(X[gone], y[gone])
r["subtraction_vs_sklearn_retrain_pred_agree"] = float((predict(ridge(s, lam), X[te]) == rk.predict(X[te]).argmax(1)).mean())
lr = LogisticRegression(C=1.0, max_iter=1000, tol=1e-6).fit(X[tr], y[tr])
r["logreg_C1_maxiter1000_test_acc"] = float(lr.score(X[te], y[te]))
r["logreg_n_iter"] = int(np.max(lr.n_iter_))
r["seconds"] = time.time() - t
print(json.dumps(r))
