# Research scripts

Benchmarks that produced the measured tables of the storage docs. Not
needed to run, build or deploy a node; kept to reproduce those numbers.

| Script | Reproduces |
| --- | --- |
| `storebench.sh` | the store comparison of [docs/storage/zonelog.md](../../docs/storage/zonelog.md) (zonelog against the former bbolt layouts; about 40 min, 6 GB) |
| `bulkbench.sh` | the bulk-ingest measurements of [docs/storage/bulk.md](../../docs/storage/bulk.md) (a local node, a generated batch, bulk against record-by-record writes) |
