"""Embeddings for the Indexus continual-learning experiment.
Public data only: HuffPost News Category (heegyu/news-category-dataset, CC BY 4.0)
and AG News (fancyzhx/ag_news). Two frozen encoders, normalised, float16.
Writes one .npz per (dataset, encoder): emb, label, year, split, and classes."""
import json, os, sys, time
import numpy as np

OUT = os.environ.get("OUT", "out")
SMOKE = os.environ.get("SMOKE") == "1"
os.makedirs(OUT, exist_ok=True)

def log(*a):
    m = "[CL] " + " ".join(str(x) for x in a)
    print(m, flush=True)
    try:
        open("/dev/console", "w").write(m + "\n")
    except Exception:
        pass

def huffpost(path):
    rows = []
    with open(path) as f:
        head = f.read(1); f.seek(0)
        it = json.load(f) if head == "[" else (json.loads(l) for l in f if l.strip())
        for r in it:
            d = str(r.get("date", ""))[:4]
            if not d.isdigit():
                continue
            t = (r.get("headline") or "") + ". " + (r.get("short_description") or "")
            rows.append((t.strip(), r["category"], int(d)))
    cats = sorted({c for _, c, _ in rows})
    idx = {c: i for i, c in enumerate(cats)}
    texts = [t for t, _, _ in rows]
    lab = np.array([idx[c] for _, c, _ in rows], np.int16)
    year = np.array([y for _, _, y in rows], np.int16)
    split = np.zeros(len(rows), np.int8)
    return texts, lab, year, split, cats

def agnews(train, test):
    import pyarrow.parquet as pq
    texts, lab, split = [], [], []
    for s, p in ((0, train), (1, test)):
        t = pq.read_table(p).to_pydict()
        texts += t["text"]; lab += t["label"]; split += [s] * len(t["text"])
    return texts, np.array(lab, np.int16), np.zeros(len(texts), np.int16), np.array(split, np.int8), ["World", "Sports", "Business", "Sci/Tech"]

def main():
    sets = {}
    sets["huffpost"] = huffpost(sys.argv[1])
    sets["agnews"] = agnews(sys.argv[2], sys.argv[3])
    for k, v in sets.items():
        log(k, "rows", len(v[0]), "classes", len(v[4]), "years", int(v[2].min()), int(v[2].max()))
    if SMOKE:
        sets = {k: (v[0][:64], v[1][:64], v[2][:64], v[3][:64], v[4]) for k, v in sets.items()}
    from sentence_transformers import SentenceTransformer
    import torch
    torch.set_num_threads(os.cpu_count())
    for enc in os.environ.get("ENCODERS", "sentence-transformers/all-MiniLM-L6-v2,BAAI/bge-small-en-v1.5").split(","):
        m = SentenceTransformer(enc, device="cpu")
        m.max_seq_length = 128
        short = enc.split("/")[-1]
        for k, (texts, lab, year, split, cats) in sets.items():
            t0 = time.time()
            e = m.encode(texts, batch_size=256, normalize_embeddings=True, convert_to_numpy=True, show_progress_bar=False)
            dt = time.time() - t0
            p = f"{OUT}/{k}__{short}.npz"
            np.savez(p, emb=e.astype(np.float16), label=lab, year=year, split=split, classes=np.array(cats))
            log("encoded", k, short, e.shape, f"{dt:.0f}s", f"{len(texts)/dt:.0f}/s", "->", p)
    log("ALLDONE")

if __name__ == "__main__":
    main()
