#!/bin/bash
# LIRE step 4: Cohere Wikipedia (embed-multilingual-v3, 1024 dim.), the same
# 1,000 queries against 1 M then 10 M vectors. Results on the console.
shutdown -h +${TTL:-240}
log() { echo "[LIRE] $*" > /dev/console; }
emit() { sed 's/^/[LIRE] /' "$1" > /dev/console; }
log "boot $(date -u +%FT%TZ) $(nproc) cpu $(free -g | awk '/Mem/{print $2}') GiB"
export HOME=/root DEBIAN_FRONTEND=noninteractive
apt-get update -y -q >/dev/null 2>&1; apt-get install -y -q python3-pip >/dev/null 2>&1
pip3 install -q --break-system-packages pyarrow numpy >/dev/null 2>&1 || log "pip failed"
curl -sSL https://go.dev/dl/go1.24.7.linux-amd64.tar.gz | tar xz -C /usr/local || log "go download failed"
mkdir -p /opt/lire/src /data/pq && cd /opt/lire/src && echo "$SRC" | base64 -d | tar xz
/usr/local/go/bin/go build -o /opt/lire/bench . 2>/tmp/build.log || { log "build failed"; emit /tmp/build.log; }
cd /data
log "download $(date -u +%T)"
seq 0 100 | xargs -P 8 -I{} sh -c 'f=$(printf "%04d" {}); curl -sSL --retry 3 --max-time 1800 -o pq/$f.parquet "https://huggingface.co/datasets/Cohere/wikipedia-2023-11-embed-multilingual-v3/resolve/main/en/$f.parquet"'
log "convert $(date -u +%T) $(du -sh pq | cut -f1)"
python3 - <<'PY' > convert.txt 2>&1
import pyarrow.parquet as pq, numpy as np, glob
files = sorted(glob.glob("pq/*.parquet"))
base, queries, n = open("base.fvecs", "wb"), open("queries.fvecs", "wb"), 0
for i, f in enumerate(files):
    c = pq.read_table(f, columns=["emb"]).column("emb").combine_chunks()
    m = c.flatten().to_numpy().astype(np.float32).reshape(len(c), -1)
    head = np.full((m.shape[0], 1), m.shape[1], dtype=np.int32).view(np.float32)
    rows = np.hstack([head, m])
    if i < 100:
        base.write(rows.tobytes()); n += m.shape[0]
    else:
        queries.write(rows[:1000].tobytes())
base.close(); queries.close()
print("base vectors", n, "dims", m.shape[1], "queries 1000 from", files[-1])
PY
emit convert.txt
rm -rf pq
log "run 1M $(date -u +%T)"
/opt/lire/bench -base base.fvecs -queries queries.fvecs -n 1000000 -q 1000 -only tree,ivf -trees 16d,64d > r1m.txt 2>&1
emit r1m.txt
log "run 10M $(date -u +%T)"
/opt/lire/bench -base base.fvecs -queries queries.fvecs -n 0 -q 1000 -only tree -trees 16d,64d > r10m.txt 2>&1
emit r10m.txt
log "DONE $(date -u +%T)"
sleep 120
shutdown -h now
