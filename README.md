# Indexus Core

<a href="LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0--only-blue" alt="License"/></a> <a href="https://discord.gg/EXuGtcDgaS"><img src="https://img.shields.io/discord/1235219716738519080?logo=discord&logoColor=white" alt="Discord"/></a>

Indexus Core implements the proposed **Distributed Prefix Index Protocol (DPIP)** in Go.
It includes a peer, an identity issuer, data preparation tools and a Docker demo.
To explore archived weather, LiDAR and historical house sales around Anglet, start
with the [Docker quickstart](quickstart/README.md). A fresh volume starts with one
peer. You can allow more local peers to join as you add data.

## What it is

A collection maps records into a binary tree. A prefix identifies a zone of
that tree. A reader can ask for its count and sums, then follow longer prefixes
for more detail. Each record contributes a count and additive metrics.

Peers share the zones. XOR distance indicates where a zone should be placed.
The peer that has installed the write permission for a zone can acknowledge its
writes. During a handoff, a receiver may start serving its installed zones while
the rest are still moving. See [local permissions](docs/protocol/local-permissions.md).

This repository holds the bottom layers of the stack:

| Layer | Here |
| --- | --- |
| tree | `internal/tree`: IDs, zone keys, View/Owner, XOR trie |
| authority | `internal/causal`, `internal/protocol`: convergent routing membership, local write permissions, handoff plans |
| store | `internal/zonelog` (the only engine), `internal/storage`: records, control state, receipts |
| wire | `indexus`: `/sets` as IXB2 blocks, `/records` as JSON or IXRB v2 |
| ingest | `internal/prep`, `tools/prep`, `tools/localdata`: IXP1 batches built offline, installed in bulk |

The client layers (store, walk, gpu, map) are in
[sdk](https://github.com/indexus/sdk); the apps are in
[experiences](https://github.com/indexus/experiences); the admin console and
the API gateway in [dashboard](https://github.com/indexus/dashboard).

## Build a node from source

Requirements: Go 1.26, nothing else: the node module requires no module
outside the standard library (`go.mod` has no `require`, there is no
`go.sum`). The optional HTTP/3 edge, `gateway/`, is a module of its own.

```sh
go build ./... && go vet ./...
go test ./indexus/... ./internal/zonelog/...
```

One node by hand (adapt [ops/config/node.json](ops/config/node.json); the
issuer must run first):

```sh
go build -o bin/issuer ./cmd/issuer && go build -o bin/node ./cmd/node
bin/issuer -addr 127.0.0.1:22000 -network indexus -key .local/issuer.ed25519 -bootstrap 127.0.0.1 -p2pPort 21000 &
bin/node check -config node.json
bin/node init  -config node.json   # once: creates the network
bin/node serve -config node.json   # every later start
```

The whole stack (one node, the gateway, Explore with its collections) is
one command, `scripts/local/explore.sh`; see
[docs/local-experiences.md](docs/local-experiences.md). Step by step:
[docs/getting-started.md](docs/getting-started.md).

Compaction is on by default (`"compaction": false` turns it off): cool vnode
pairs fold back with durable handoffs and retirement witnesses, so the
topology shrinks after a spike; see
[cold reconfiguration](docs/protocol/cold-reconfiguration.md).

## Repository map

| Path | Role |
| --- | --- |
| `cmd/node`, `internal/app/node` | Node process: configuration, `check`, `init`, `serve`, `move` |
| `cmd/issuer`, `cmd/provisioner` | Identity issuer and process orchestration |
| `gateway/` | Optional same-origin HTTPS/HTTP3 edge, a separate Go module so the node links no QUIC stack ([docs/http3.md](docs/http3.md)) |
| `quic/` | Optional HTTP/3 build of the node (NET-1), a separate Go module: its `cmd/node` serves and calls peers over HTTP/3 with a TCP fallback |
| `cmd/experiment`, `ops/experiment` | P2P test bench: per-machine agent, open-loop loader, sentinel, live dashboard, local and AWS ladders ([ops/experiment/README.md](ops/experiment/README.md)) |
| `indexus/` | Node runtime: records, zones, reads, writes, summaries, bulk install, maintenance |
| `internal/tree` | Address algebra |
| `internal/protocol`, `internal/causal` | Capacity, plans, handoff rules; topology and authority facts |
| `internal/zonelog`, `internal/storage` | The store and the node's durable state on it |
| `internal/mesh` | Peer exchanges, handoffs, bounded gossip, recovery |
| `internal/metrics` | Process counters behind `GET /metrics` |
| `internal/adapter` | HTTP, mesh transport, TLS, issuer, provisioner, logs |
| `internal/prep` | IXP1 batch format |
| `internal/modelcheck`, `internal/simulation` | Bounded state search, controlled-network runs |
| `internal/architecture` | Tests of the dependency boundaries between these packages |
| `tools/localdata` | Import, bulk install, export of a stopped node |
| `tools/prep` | Batch builder (Go), LiDAR, scenes and weather builders (Python), Metal kernels |
| `scripts/local` | `explore.sh` (whole stack), `prep-all.sh` (every batch Explore reads) |
| `scripts/perf`, `perf/` | Go benchmark suite in the `indexus-perf/1` format and its baseline |
| `scripts/research` | Benchmarks behind the storage docs; not needed to operate |
| `ops/` | Example configuration, local autoscaling mesh, AWS lab ([ops/README.md](ops/README.md)) |
| `.agents/skills/` | Project skills for coding agents (layer charter, simplification method, GPU path, budgets, audit), shared by the four repositories |

### HTTP surface

- Peer and client port (default 21000): `/item`, `/item/delete`, `/set`,
  `/sets`, `/records`, `/content`, `/aggregates`, `/ping`, `/neighbors`,
  `/random`, `/join-hint` (public: a name that halves one of the largest
  cells, JOIN-1), and `/cluster/` between nodes.
- Monitoring port (default 19000, loopback): `/health`, `/status`,
  `/acknowledged`, `/registered`, `/routing`, `/ownership`, `/count`,
  `/autoscale`, `/metrics` (wire, write, gossip and handoff counters, storage
  and process gauges; polled by `ops/experiment/live.py`).

## Tests

```sh
go vet ./...
go test ./...                                   # the whole suite (minutes)
python3 -m unittest discover -s tools/prep/lidar
python3 -m unittest discover -s tools/prep/commune -p 'test_[lw]*.py'   # HTTP and Lambert-93, seconds
```

The protocol, its named invariants and the tests that prove them are in
[docs/protocol](docs/protocol/README.md).

## Documentation

- [docs/README.md](docs/README.md): index of the documentation
- [docs/getting-started.md](docs/getting-started.md): build, one node, the whole stack
- [docs/glossary.md](docs/glossary.md): the words shared by the four repositories
- [docs/protocol/wire-block.md](docs/protocol/wire-block.md): the client wire (IXB2, IXRB)
- [docs/production-configuration.md](docs/production-configuration.md): every configuration field
- [docs/cli.md](docs/cli.md): proposal of one `indexus` command replacing the scripts

Maintained by Rémi Henriot. The [project history](https://github.com/indexus/papers/tree/main/60-history-and-authorship) documents the contributions of AI tools.

## License

Copyright © Indexus. The Indexus Core software in this revision is licensed
under the [GNU Affero General Public License, version 3 only](LICENSE)
(SPDX: `AGPL-3.0-only`). For a separate commercial license from the rights
holder, contact [contact@indexus.io](mailto:contact@indexus.io).

Earlier revisions distributed under MIT retain that grant. Third-party components
retain their respective terms. The [DPIP specification](https://github.com/indexus/papers)
is published separately under CC BY 4.0.
