# Research scripts

Benchmarks that produced the measured tables of the storage docs. Not
needed to run, build or deploy a node; kept to reproduce those numbers.

| Script | Reproduces |
| --- | --- |
| `storebench.sh` | the store comparison of [docs/storage/zonelog.md](../../docs/storage/zonelog.md) (zonelog against the former bbolt layouts; about 40 min, 6 GB) |
| `bulkbench.sh` | the bulk-ingest measurements of [docs/storage/bulk.md](../../docs/storage/bulk.md) (a local node, a generated batch, bulk against record-by-record writes) |

The `wirepresence` Go/JS fixture includes 300 original indicator columns,
float32 and float64, reordered projections, a redirect and an out-of-schema
column. Its checker compares the SDK's present, absent and unknown classes
per requested bit to a source-column oracle. Projection of a missing source
column is zero-filled by Go. The caller must verify schema coverage before
setting `complete: true`, including when no redirect is present.

```sh
mkdir -p /tmp/indexus-presence-blocks
go run ./scripts/research/wirepresence /tmp/indexus-presence-blocks
node scripts/research/wirepresence/check.mjs /path/to/sdk /tmp/indexus-presence-blocks
```
