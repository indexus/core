#!/bin/bash
# Indexus continual learning: embeddings of two public datasets with two frozen
# encoders on CPU. Uploads via presigned PUT URLs only (no key, no role, no
# inbound rule on this machine). Shuts down (terminate) after TTL minutes.
shutdown -h +${TTL:-90}
log() { echo "[CL] $*" > /dev/console; }
log "boot $(date -u +%FT%TZ) $(nproc) cpu $(free -g | awk '/Mem/{print $2}') GiB"
export HOME=/root DEBIAN_FRONTEND=noninteractive
apt-get update -y -q >/dev/null 2>&1; apt-get install -y -q python3-pip python3-venv >/dev/null 2>&1
python3 -m venv /opt/v && . /opt/v/bin/activate
pip install -q torch --index-url https://download.pytorch.org/whl/cpu >/tmp/pip.log 2>&1 || log "torch pip failed"
pip install -q sentence-transformers pyarrow numpy >>/tmp/pip.log 2>&1 || log "pip failed"
log "pip done $(date -u +%T) $(python -c 'import torch,sentence_transformers as s; print(torch.__version__, s.__version__)' 2>&1 | tail -1)"
mkdir -p /data && cd /data && echo "$SRC" | base64 -d > embed.py
H=https://huggingface.co/datasets
curl -sSL --retry 3 -o huff.json "$H/heegyu/news-category-dataset/resolve/main/data.json" || log "huff download failed"
curl -sSL --retry 3 -o ag_train.parquet "$H/fancyzhx/ag_news/resolve/main/data/train-00000-of-00001.parquet" || log "ag download failed"
curl -sSL --retry 3 -o ag_test.parquet "$H/fancyzhx/ag_news/resolve/main/data/test-00000-of-00001.parquet" || log "ag test download failed"
log "data $(ls -la huff.json ag_*.parquet | awk '{print $5, $9}' | tr '\n' ' ')"
OUT=out python embed.py huff.json ag_train.parquet ag_test.parquet > embed.log 2>&1 || { log "embed failed"; tail -20 embed.log | sed 's/^/[CL] /' > /dev/console; }
for f in out/*.npz; do
  b=$(basename "$f"); u=$(grep "^$b " urls.txt 2>/dev/null | cut -d' ' -f2-)
  [ -n "$u" ] || u=$(echo "$URLS" | base64 -d | grep "^$b " | cut -d' ' -f2-)
  code=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT -T "$f" "$u")
  log "upload $b $(sha256sum "$f" | cut -c1-16) $(stat -c %s "$f") http=$code"
done
log "DONE $(date -u +%T)"
sleep 60
shutdown -h now
