#!/bin/bash
# LIRE benchmark instance: downloads public vector sets, runs, prints results
# on the console ([LIRE] lines), then shuts down (and is destroyed).
shutdown -h +200
log() { echo "[LIRE] $*" > /dev/console; }
emit() { sed 's/^/[LIRE] /' "$1" > /dev/console; }
log "boot $(date -u +%FT%TZ) $(nproc) cpu $(free -g | awk '/Mem/{print $2}') GiB"
export HOME=/root DEBIAN_FRONTEND=noninteractive
apt-get update -y -q >/dev/null 2>&1; apt-get install -y -q python3-pip >/dev/null 2>&1
pip3 install -q --break-system-packages pyarrow numpy >/dev/null 2>&1 || log "pip failed"
curl -sSL https://go.dev/dl/go1.24.7.linux-amd64.tar.gz | tar xz -C /usr/local || log "go download failed"
mkdir -p /opt/lire/src /data && cd /opt/lire/src && echo "$SRC" | base64 -d | tar xz
/usr/local/go/bin/go build -o /opt/lire/bench . 2>/tmp/build.log || { log "build failed"; emit /tmp/build.log; }
cd /data
if [ "${SIFT:-1}" = 1 ]; then
log "sift download $(date -u +%T)"
curl -sS --max-time 1200 -o sift.tar.gz ftp://ftp.irisa.fr/local/texmex/corpus/sift.tar.gz && tar xzf sift.tar.gz && rm sift.tar.gz
log "sift run $(date -u +%T)"
/opt/lire/bench -base sift/sift_base.fvecs -queries sift/sift_query.fvecs -n 0 -q 1000 ${SIFT_ARGS:--only tree,lire,ivf,fixed} > sift.txt 2>&1
emit sift.txt
fi
log "$DATASET download $(date -u +%T)"
H=https://huggingface.co/datasets/$DATASET
curl -sSL "https://huggingface.co/api/datasets/$DATASET/tree/main/data" | python3 -c "import json,sys; [print(f['path']) for f in json.load(sys.stdin)]" > files.txt
log "$(wc -l < files.txt) parquet files"
mkdir -p pq && xargs -P 8 -I{} sh -c 'curl -sSL --retry 3 --max-time 1800 -o "pq/$(basename {})" "'"$H"'/resolve/main/{}"' < files.txt
log "convert $(date -u +%T) $(du -sh pq | cut -f1)"
COLUMN="$COLUMN" python3 - <<'PY' > convert.txt 2>&1
import pyarrow.parquet as pq, numpy as np, glob, pyarrow as pa
files = sorted(glob.glob("pq/*.parquet"))
schema = pq.read_schema(files[0])
import os
cols = [f.name for f in schema if "embedding" in f.name.lower() and pa.types.is_list(f.type)]
print("embedding columns", cols)
col = [c for c in cols if os.environ.get("COLUMN", "") in c][0]
out = open("big.fvecs", "wb"); n = 0
for f in files:
    c = pq.read_table(f, columns=[col]).column(col).combine_chunks()
    m = c.flatten().to_numpy().astype(np.float32).reshape(len(c), -1)
    head = np.full((m.shape[0], 1), m.shape[1], dtype=np.int32).view(np.float32)
    out.write(np.hstack([head, m]).tobytes()); n += m.shape[0]
out.close(); print("column", col, "vectors", n, "dims", m.shape[1])
PY
emit convert.txt
rm -rf pq
log "big run $(date -u +%T)"
/opt/lire/bench -base big.fvecs -n 0 -q 1000 ${BIG_ARGS:--only tree,ivf,fixed} > big.txt 2>&1
emit big.txt
log "DONE $(date -u +%T)"
sleep 120
shutdown -h now
