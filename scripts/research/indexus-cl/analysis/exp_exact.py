"""Exactness: order/partition invariance, exact unlearning, numerical drift.
Usage: python exp_exact.py data.npz [n_max]"""
import sys, time, json
import numpy as np
from sklearn.metrics import roc_auc_score
from cl import Stats, ridge, lda, predict

path = sys.argv[1]
NMAX = int(sys.argv[2]) if len(sys.argv) > 2 else 10**9
D = np.load(path)
X = D["emb"].astype(np.float64)[:NMAX]
y = D["label"].astype(np.int64)[:NMAX]
C = int(y.max()) + 1
n, d = X.shape
rng = np.random.default_rng(1)
out = {"data": path.split("/")[-1], "n": n, "d": d, "C": C}
print(out, flush=True)
perm = rng.permutation(n)
te = perm[: n // 5]
tr = perm[n // 5 :]
Xte, yte = X[te], y[te]


def rel(a, b):
    return float(np.linalg.norm(a - b) / max(np.linalg.norm(b), 1e-300))


def models(s):
    s = s.as64()
    return {"ridge": ridge(s, 1e-4), "lda": lda(s, 0.05)}


def compare(sa, sb):
    ma, mb = models(sa), models(sb)
    r = {}
    for k in ma:
        pa, pb = predict(ma[k], Xte), predict(mb[k], Xte)
        r[k] = {"W_rel": rel(ma[k][0], mb[k][0]), "pred_diff": float((pa != pb).mean())}
    r["G_rel"] = rel(sa.as64().G, sb.as64().G)
    return r


def stats_in_order(idx, parts, dtype):
    """Split idx into `parts` contiguous chunks in this order, build each
    chunk's stats by batches of 1000, then fold the parts as a binary tree."""
    chunks = np.array_split(idx, parts)
    ps = []
    for ch in chunks:
        s = Stats(d, C, dtype)
        for b in np.array_split(ch, max(1, len(ch) // 1000)):
            s.add(X[b], y[b])
        ps.append(s)
    while len(ps) > 1:
        ps = [ps[i] + ps[i + 1] if i + 1 < len(ps) else ps[i] for i in range(0, len(ps), 2)]
    return ps[0]


# ---------------------------------------------------------------- T1
t0 = time.time()
ref = stats_in_order(tr, 1, np.float64)
res = []
for trial in range(12):
    idx = rng.permutation(tr)
    parts = int(rng.choice([1, 2, 7, 16, 64]))
    for dt in (np.float64, np.float32):
        s = stats_in_order(idx, parts, dt)
        c = compare(s, ref)
        res.append({"parts": parts, "dtype": np.dtype(dt).name, **c})
summ = {}
for dt in ("float64", "float32"):
    rr = [r for r in res if r["dtype"] == dt]
    summ[dt] = {
        "G_rel_max": max(r["G_rel"] for r in rr),
        "ridge_W_rel_max": max(r["ridge"]["W_rel"] for r in rr),
        "ridge_pred_diff_max": max(r["ridge"]["pred_diff"] for r in rr),
        "lda_W_rel_max": max(r["lda"]["W_rel"] for r in rr),
        "lda_pred_diff_max": max(r["lda"]["pred_diff"] for r in rr),
    }
out["T1_order_partition"] = summ
print("T1", json.dumps(summ), f"{time.time()-t0:.0f}s", flush=True)

# ---------------------------------------------------------------- T2
# Delete 10 % of the training set, a whole class, and the oldest year when
# there are years, by subtraction; compare with recomputation without them.
t0 = time.time()
full = stats_in_order(tr, 1, np.float64)
cases = {"random10pct": tr[rng.random(len(tr)) < 0.10], "one_class": tr[y[tr] == np.bincount(y[tr]).argmax()]}
if "year" in D.files and D["year"][:NMAX].max() > 0:
    yr = D["year"][:NMAX]
    cases["oldest_year"] = tr[yr[tr] == yr[tr].min()]
t2 = {}
for k, delidx in cases.items():
    sub = Stats(d, C)
    sub.G, sub.B, sub.Y, sub.n = full.G.copy(), full.B.copy(), full.Y.copy(), full.n
    for b in np.array_split(delidx, max(1, len(delidx) // 1000)):
        sub.remove(X[b], y[b])
    keep = np.setdiff1d(tr, delidx)
    fresh = stats_in_order(rng.permutation(keep), 3, np.float64)
    t2[k] = {"deleted": int(len(delidx)), **compare(sub, fresh)}
out["T2_unlearning"] = t2
print("T2", json.dumps(t2), f"{time.time()-t0:.0f}s", flush=True)

# Membership inference on a model that memorises: ridge with tiny lambda on a
# small training set. Attack score = margin of the true class.
small = tr[:1500]
mem_del, mem_keep = small[:500], small[500:]
nonmem = te[:500]
s_small = Stats.of(X[small], y[small], C)
def margin(Wb, idx):
    S = X[idx] @ Wb[0] + Wb[1]
    t = S[np.arange(len(idx)), y[idx]]
    S2 = S.copy(); S2[np.arange(len(idx)), y[idx]] = -np.inf
    return t - S2.max(1)
def auc(a, b):
    return float(roc_auc_score(np.r_[np.ones(len(a)), np.zeros(len(b))], np.r_[a, b]))
m_before = ridge(s_small, 1e-6)
s_after = s_small.as64(); s_after.remove(X[mem_del], y[mem_del])
m_after = ridge(s_after, 1e-6)
m_retrain = ridge(Stats.of(X[mem_keep], y[mem_keep], C), 1e-6)
mia = {
    "auc_members_vs_nonmembers_before": auc(margin(m_before, mem_del), margin(m_before, nonmem)),
    "auc_deleted_vs_nonmembers_after": auc(margin(m_after, mem_del), margin(m_after, nonmem)),
    "auc_kept_members_after": auc(margin(m_after, mem_keep[:500]), margin(m_after, nonmem)),
    "after_vs_retrain_W_rel": rel(m_after[0], m_retrain[0]),
}
out["T2_membership_inference"] = mia
print("MIA", json.dumps(mia), flush=True)

# ---------------------------------------------------------------- T3
# Churn: a live set of N items; each step adds 1000 new items and deletes
# 1000 random live ones. Incremental stats vs recomputation, three ways of
# accumulating: float64, float32, float64 with Neumaier compensation.
t0 = time.time()
N = min(20000, len(tr) // 2)
pool = rng.permutation(tr)
live = list(pool[:N]); nxt = N
class Neu:
    def __init__(self, s):
        self.s = s.as64(); self.c = Stats(d, C)
    def apply(self, delta):
        for f in ("G", "B", "Y"):
            a, b = getattr(self.s, f), getattr(delta, f)
            t = a + b
            comp = np.where(np.abs(a) >= np.abs(b), (a - t) + b, (b - t) + a)
            setattr(self.c, f, getattr(self.c, f) + comp)
            setattr(self.s, f, t)
        self.s.n += delta.n
    def value(self):
        v = self.s.as64()
        v.G, v.B, v.Y = v.G + self.c.G, v.B + self.c.B, v.Y + self.c.Y
        return v
s64 = Stats.of(X[live], y[live], C, np.float64)
s32 = Stats.of(X[live], y[live], C, np.float32)
neu = Neu(s64)
curve = []
steps = int(sys.argv[3]) if len(sys.argv) > 3 else 400
for step in range(1, steps + 1):
    if nxt + 1000 > len(pool):
        nxt = N  # reuse the pool: items come back, that is fine for drift
    new = pool[nxt : nxt + 1000]; nxt += 1000
    gone_pos = rng.choice(len(live), 1000, replace=False)
    gone = np.array(live)[gone_pos]
    keep_mask = np.ones(len(live), bool); keep_mask[gone_pos] = False
    live = list(np.array(live)[keep_mask]) + list(new)
    delta = Stats.of(X[new], y[new], C) .__add__(Stats.of(X[gone], y[gone], C).scaled(-1))
    s64 = s64 + delta
    s32.add(X[new], y[new]); s32.remove(X[gone], y[gone])
    neu.apply(delta)
    if step in (1, 10, 50, 100, 200, 400, 1000, 2000, 4000) or step == steps:
        fresh = Stats.of(X[live], y[live], C)
        curve.append({"step": step, "f64": compare(s64, fresh), "f32": compare(s32, fresh), "neumaier": compare(neu.value(), fresh)})
        print("T3", step, json.dumps({k: (v["G_rel"], v["ridge"]["pred_diff"]) for k, v in curve[-1].items() if k != "step"}), flush=True)
out["T3_churn"] = curve
print(f"T3 {time.time()-t0:.0f}s", flush=True)
json.dump(out, open(path.split("/")[-1].replace(".npz", "") + "__exact.json", "w"), indent=1)
print("WROTE", flush=True)
