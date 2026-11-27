"""Continual learning from additive sufficient statistics (the Indexus primitive).

Every model here is a function of sums only:
  n, Y = sum y (class counts), B = sum x y^T (class sums), G = sum x x^T.
So adding data is +, removing it is -, and any order or partition gives the
same sums (up to floating-point rounding, which is what we measure).
"""
import numpy as np


class Stats:
    """Additive statistics over (x, one-hot y). A commutative group."""

    def __init__(self, d, C, dtype=np.float64):
        self.d, self.C, self.dtype = d, C, dtype
        self.n = dtype(0)
        self.Y = np.zeros(C, dtype)
        self.B = np.zeros((d, C), dtype)
        self.G = np.zeros((d, d), dtype)

    @classmethod
    def of(cls, X, y, C, dtype=np.float64):
        s = cls(X.shape[1], C, dtype)
        s.add(X, y)
        return s

    def _parts(self, X, y):
        X = X.astype(self.dtype, copy=False)
        Yh = np.zeros((len(y), self.C), self.dtype)
        Yh[np.arange(len(y)), y] = 1
        return X, Yh

    def add(self, X, y, sign=1):
        if len(y) == 0:
            return self
        X, Yh = self._parts(X, y)
        self.n = self.dtype(self.n + sign * len(y))
        self.Y += sign * Yh.sum(0)
        self.B += sign * (X.T @ Yh)
        self.G += sign * (X.T @ X)
        return self

    def remove(self, X, y):
        return self.add(X, y, sign=-1)

    def __add__(self, o):
        s = Stats(self.d, self.C, self.dtype)
        s.n, s.Y, s.B, s.G = self.n + o.n, self.Y + o.Y, self.B + o.B, self.G + o.G
        return s

    def scaled(self, w):
        s = Stats(self.d, self.C, self.dtype)
        s.n, s.Y, s.B, s.G = self.n * w, self.Y * w, self.B * w, self.G * w
        return s

    def as64(self):
        s = Stats(self.d, self.C, np.float64)
        s.n, s.Y, s.B, s.G = np.float64(self.n), self.Y.astype(np.float64), self.B.astype(np.float64), self.G.astype(np.float64)
        return s


# ---------------------------------------------------------------- models
# All take Stats (float64) and return (W, b) with scores = X @ W + b.

def ridge(s, lam):
    """Least squares on one-hot targets with an unregularised intercept."""
    n = max(float(s.n), 1.0)
    mx = s.B.sum(1) / n            # sum x / n  (each x counted once over classes)
    my = s.Y / n
    Gc = s.G - n * np.outer(mx, mx)
    Bc = s.B - n * np.outer(mx, my)
    W = np.linalg.solve(Gc + lam * n * np.eye(s.d), Bc)
    b = my - mx @ W
    return W, b


def ridge_prior(s, lam, W0, b0):
    """Ridge shrunk towards a parent model (W0, b0): the hierarchical prior."""
    n = max(float(s.n), 1.0)
    # residual targets relative to the parent: y - (x W0 + b0)
    # sums needed: G, B - G W0 - mx n b0 ...
    mx = s.B.sum(1) / n
    R_B = s.B - s.G @ W0 - np.outer(s.B.sum(1), b0)           # sum x r^T
    R_Y = s.Y - (s.B.sum(1) @ W0) - n * b0                    # sum r
    Gc = s.G - n * np.outer(mx, mx)
    Bc = R_B - np.outer(mx, R_Y)
    dW = np.linalg.solve(Gc + lam * np.eye(s.d), Bc)
    db = R_Y / n - mx @ dW
    return W0 + dW, b0 + db


def lda(s, shrink, prior=True):
    """Linear discriminant with a shared covariance shrunk towards its trace."""
    n = float(s.n)
    cnt = s.Y
    seen = cnt > 0
    mu = np.zeros((s.C, s.d))
    mu[seen] = (s.B[:, seen] / cnt[seen]).T
    Sw = s.G - (s.B[:, seen] / cnt[seen]) @ s.B[:, seen].T
    Sw /= max(n - seen.sum(), 1.0)
    t = np.trace(Sw) / s.d
    S = (1 - shrink) * Sw + shrink * t * np.eye(s.d)
    W = np.linalg.solve(S, mu.T)
    b = -0.5 * np.einsum("cd,dc->c", mu, W)
    if prior:
        b = b + np.log(np.maximum(cnt, 1e-12) / max(n, 1.0))
    b[~seen] = -np.inf
    return W, b


def ncm(s):
    cnt = s.Y
    seen = cnt > 0
    mu = np.zeros((s.C, s.d))
    mu[seen] = (s.B[:, seen] / cnt[seen]).T
    mu /= np.maximum(np.linalg.norm(mu, axis=1, keepdims=True), 1e-12)
    b = np.zeros(s.C)
    b[~seen] = -np.inf
    return mu.T, b


def predict(Wb, X):
    W, b = Wb
    return np.argmax(X.astype(np.float64) @ W + b, axis=1)


def acc(Wb, X, y):
    return float((predict(Wb, X) == y).mean()) if len(y) else float("nan")


# ---------------------------------------------------------------- zones
# Indexus "variant 1": fixed random projection to k axes, then interleaved
# bisection. Either linear bounds or Gaussian quantiles (Phi) per axis.

class Space:
    def __init__(self, X_ref, k, seed=0, quantile=True):
        rng = np.random.default_rng(seed)
        self.P = rng.standard_normal((X_ref.shape[1], k)) / np.sqrt(X_ref.shape[1])
        Z = X_ref.astype(np.float64) @ self.P
        self.mu, self.sd = Z.mean(0), Z.std(0) + 1e-12
        self.lo, self.hi = Z.min(0), Z.max(0)
        self.k, self.quantile = k, quantile

    def unit(self, X):
        Z = X.astype(np.float64) @ self.P
        if self.quantile:
            from scipy.special import ndtr
            U = ndtr((Z - self.mu) / self.sd)
        else:
            U = (Z - self.lo) / (self.hi - self.lo)
        return np.clip(U, 0, 1 - 1e-12)

    def codes(self, X, depth):
        """Zone id of every row at every depth 0..depth (bits interleaved)."""
        U = self.unit(X)
        per = int(np.ceil(depth / self.k)) + 1
        cell = np.floor(U * (1 << per)).astype(np.int64)  # per-axis cells
        out = np.zeros((len(X), depth + 1), np.int64)
        code = np.zeros(len(X), np.int64)
        for lvl in range(1, depth + 1):
            a = (lvl - 1) % self.k
            cut = (lvl - 1) // self.k          # how many times axis a was cut before
            bit = (cell[:, a] >> (per - 1 - cut)) & 1
            code = code * 2 + bit
            out[:, lvl] = code
        return out
