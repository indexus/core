"""Fair baseline for exp_time: a trained logistic probe that also knows the year
(one probe per year, trained on that year only), final step of the stream."""
import sys, json, time, warnings
import numpy as np
from sklearn.linear_model import LogisticRegression
warnings.filterwarnings("ignore")
D = np.load(sys.argv[1]); X = D["emb"].astype(np.float64); y = D["label"].astype(np.int64); year = D["year"].astype(np.int64)
rng = np.random.default_rng(7); p = rng.permutation(len(y)); p = rng.permutation(p[len(y) // 5:])
yrs = sorted(np.unique(year)); YTR, YTE = {}, {}
for t in yrs:
    idx = rng.permutation(np.where(year == t)[0]); k = len(idx) // 5
    YTE[t], YTR[t] = idx[:k], idx[k:]
accs, t0 = [], time.time()
for t in yrs:
    m = LogisticRegression(C=1.0, max_iter=200, tol=1e-4).fit(X[YTR[t]], y[YTR[t]])
    accs.append(float(m.score(X[YTE[t]], y[YTE[t]])))
print(json.dumps({"logreg_per_year_final_acc_all_years": float(np.mean(accs)), "per_year": dict(zip(map(int, yrs), [round(a, 4) for a in accs])), "seconds": time.time() - t0}))
