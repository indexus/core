# Getting started with core

From a fresh clone to a node that answers, then to the whole stack. Words
in *italics* are in the [glossary](glossary.md).

## 1. Requirements

| For | Needs |
| --- | --- |
| the node and the Go tools | Go 1.26 (`go.mod`), curl |
| the batch builders (`tools/prep`) | Python 3.12 with numpy |
| the GPU kernels of the builders | macOS with the Xcode command line tools (`xcrun swiftc`, Metal) |
| the whole stack | Node 22 and the three sibling repositories (see [local-experiences.md](local-experiences.md)) |

## 2. Build and test

```sh
go build ./... && go vet ./...
gofmt -l .                                   # must print nothing
go test ./indexus/... ./internal/zonelog/... ./internal/storage/...
go test ./...                                # everything, a few minutes
python3 -m unittest discover -s tools/prep/lidar
```

`internal/architecture` fails the build of the tests when a package imports
across a layer it should not.

## 3. One node

A node needs an *issuer* (it signs the node's certificate and the write
tokens) and a configuration file. Every field is described in
[production-configuration.md](production-configuration.md); unknown fields
stop startup.

```sh
mkdir -p .local/node1
go build -o bin/issuer ./cmd/issuer
go build -o bin/node ./cmd/node
go build -o bin/localdata ./tools/localdata

bin/issuer -addr 127.0.0.1:22000 -network indexus -key .local/issuer.ed25519 \
  -bootstrap 127.0.0.1 -p2pPort 21000 &

cat > .local/node1/node.json <<'JSON'
{
  "storage": {"path": "node", "identity_directory": "."},
  "identity": {"network": "indexus", "cluster": "indexus", "issuer": "http://127.0.0.1:22000"},
  "network": {"listen": "127.0.0.1:21000", "monitoring": "127.0.0.1:19000", "advertise": ["127.0.0.1"]}
}
JSON
cd .local/node1
../../bin/node check -config node.json
../../bin/node init  -config node.json      # once: creates the network
../../bin/node serve -config node.json      # every later start
```

Check it from another terminal:

```sh
curl -s http://127.0.0.1:19000/health
curl -s http://127.0.0.1:19000/status
```

The node keeps its data in `node.causal.db.records/` (a *zonelog* directory) and its
identity beside it. Keep both together: the identity is the node's address
in the tree. A second node joins with `network.bootstrap` set and runs
`serve` directly, without `init`.

## 4. Put data in it

Records are prepared offline as *IXP1 batches* and installed in one pass
(*bulk ingest*):

```sh
go run ./tools/prep ndjson -out .local/batches my-records.ndjson   # one batch per collection
go run ./tools/prep verify .local/batches/<collection>
bin/localdata -bulk .local/batches/<collection> \
  -write http://127.0.0.1:21000 -issuer http://127.0.0.1:22000
```

The NDJSON record format and the batch layout are in
[ingest/pipeline.md](ingest/pipeline.md) (section 5); what the node does
with a batch is in [storage/bulk.md](storage/bulk.md). `localdata -write
<url> -records file.ndjson` writes record by record instead, through the
normal write path.

Read it back with the SDK (`Network.getSets`, `getRecords`) or by hand:
`/sets` answers *IXB2* blocks, `/records` JSON or, with `format=binary`,
*IXRB* ([protocol/wire-block.md](protocol/wire-block.md)).

## 5. The whole stack

`scripts/local/explore.sh` builds the binaries, starts the issuer and one
node, builds and installs every batch Explore reads, then starts the
dashboard's gateway and Explore. Layout of the folders, inputs, URLs and
how to stop or rebuild: [local-experiences.md](local-experiences.md).

## 6. Where to go next

- The protocol and its invariants: [protocol/README.md](protocol/README.md).
- The store: [storage/zonelog.md](storage/zonelog.md).
- A local autoscaling mesh of several nodes: [../ops/local/README.md](../ops/local/README.md).
- Production builds and AWS: [production-build.md](production-build.md).
