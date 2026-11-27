# Public vector sets for the LIRE benchmark, into _work/datasets.
set -eu
D="$1"; mkdir -p "$D"; cd "$D"
if [ ! -f sift/sift_base.fvecs ]; then
  curl -sS --max-time 900 -o sift.tar.gz ftp://ftp.irisa.fr/local/texmex/corpus/sift.tar.gz
  tar xzf sift.tar.gz && rm sift.tar.gz
fi
ls -la sift | head
H=https://huggingface.co/datasets/Qdrant/dbpedia-entities-openai3-text-embedding-3-small-1536-100K/resolve/main/data
mkdir -p openai3
for i in 0 1 2; do
  f=openai3/train-0000$i-of-00004.parquet
  [ -f "$f" ] || curl -sSL --max-time 900 -o "$f" "$H/train-0000$i-of-00004.parquet"
done
ls -la openai3
python3 - <<'PY'
import pyarrow.parquet as pq, numpy as np, glob, struct
files = sorted(glob.glob("openai3/*.parquet"))
schema = pq.read_schema(files[0])
print(schema)
col = [f.name for f in schema if "embedding" in f.name.lower()][0]
out = open("openai3/base.fvecs", "wb")
n = 0
for f in files:
    t = pq.read_table(f, columns=[col])
    m = np.array(t.column(col).to_pylist(), dtype=np.float32)
    d = m.shape[1]
    head = np.full((m.shape[0], 1), d, dtype=np.int32).view(np.float32)
    out.write(np.hstack([head, m]).tobytes())
    n += m.shape[0]
out.close()
print("openai3 vectors", n, "dims", d, "norm of the first", float(np.linalg.norm(m[0])))
PY
