# Proposal: one `indexus` command

Status: proposal, nothing implemented. Today the stack is operated by a
handful of scripts spread over the four repositories. This page proposes
one command, `indexus`, that would replace them, and maps every kept script
to a command so that each can be retired once its command exists.

## Principles

- **One binary, in core** (`cmd/indexus`, Go): it already builds the node,
  the issuer, `localdata` and `prep`, and it knows the configuration format.
  JavaScript and Python steps (the gateway, Vite, the LiDAR builders) are
  run as child processes, as `explore.sh` does today.
- **The workspace is the unit.** `indexus` finds the folder that holds
  `core/`, `sdk/`, `dashboard/` and `experiences/` (as the audit script
  does) and keeps all state in one directory, `_run/.data` by default
  (`--data DIR`).
- **Idempotent and resumable**, like `explore.sh` and `prep-all.sh`: a step
  whose inputs did not change is skipped; a second `up` keeps what runs.
- **No new behaviour at first.** Each command starts as the script it
  replaces, then the script is deleted.

## Commands

| Command | Does | Replaces |
| --- | --- | --- |
| `indexus up [--no-explore] [--no-prep]` | Build the binaries, start the issuer and one node, build and install the batches, start the gateway and Explore; print the URLs | `core/scripts/local/explore.sh` |
| `indexus down` | Stop every service of the data directory; data stays | `explore.sh stop` |
| `indexus status [--deep]` | Services, pids, ports, node `/health` and `/status`, disk per collection, installed batch ids; `--deep` checks ownership, delegation chains and aggregates of a mesh | `_run/.data/pids` by hand; `core/ops/local/scan_consistency.py` |
| `indexus prep [collection...]` | Build the IXP1 batches Explore reads (all, or the named ones) into `_run/.data/batches`; skip unchanged inputs | `core/scripts/local/prep-all.sh` and the builders it calls: `tools/prep/lidar/{returns,vegetation,ground}.py` (and `crowns.py` on request), `tools/prep/scenes/scenes.py`, `tools/prep/weather/{weather,check}.py`, `tools/prep/metal` |
| `indexus prep ndjson\|verify\|compare\|cat\|payloads ...` | The batch tool itself | `go run ./tools/prep ...` |
| `indexus prep scene <id> [--fetch]` | Download and index a scene's sources, build its orthophoto and buildings NDJSON | `experiences/scripts/world/prepare.py`, `prepare-ortho-tiles.py`, `ortho.py`, `buildings/*.py` |
| `indexus ingest <batch dir>... [--replace]` | Install batches on the running node (or `--write URLs`) in one cut each | `localdata -bulk` (and the `import_batch` step of `explore.sh`) |
| `indexus ingest --records file.ndjson` | Write records one by one through the normal path | `localdata -write -records`; `dashboard/scripts/load_dvf.js`, `load_density.js` once their sources become NDJSON |
| `indexus export [--collections ...] > file.ndjson` | Export the records of a stopped node | `localdata -export` |
| `indexus check [--go] [--js] [--py] [--gpu]` | The test gates: `go build`, `go vet`, `gofmt -l`, `go test`; `npm test` of sdk, dashboard and experiences; the Python unit tests of the builders; with `--gpu`, build the GPU check pages and print their URLs | the commands of every README's Tests section; `sdk/scripts/gpu-check/build-*.mjs`; `experiences/scripts/gpu-smoke/run.mjs` |
| `indexus perf [run\|compare] [--suite go\|node]` | Run a benchmark suite in the `indexus-perf/1` format, or compare two results | `core/scripts/perf/run.sh`, `sdk/scripts/perf/run.mjs`, `sdk/scripts/perf/compare.mjs` |
| `indexus mesh up\|down` | A local autoscaling mesh (issuer with local spawns, bootstrap node) | `core/ops/local/mesh_up.sh`, `mesh_down.sh` |
| `indexus build [--linux]` | Production binaries | `core/ops/deploy/scripts/build.sh` |
| `indexus deploy aws` | AMI, Terraform, binaries of the AWS lab | `core/ops/deploy/scripts/{deploy,bake-ami}.sh` |

Kept as scripts, called by other programs rather than by people:
`core/ops/local/{spawn,terminate,detach,env,ensure_tls,configure_node,count,list_spawned}.sh/.py`
(run by the issuer's local launch template and by the dashboard), and the
dashboard's `ops/demo/deploy.sh` (it may become `indexus deploy demo`).

Not part of the CLI: the `scripts/research/` folders of core and
experiences (benchmarks behind the storage docs, proofs of the collection
model, the AROME indexer). They reproduce a result once; they do not operate
the stack.

## Configuration

`indexus` reads `indexus.json` at the workspace root when present, with the
values the scripts take from variables today: ports (issuer 22030, node
21031/19031, gateway 3847, Explore 5174), node limits (`GOMAXPROCS`,
`GOMEMLIMIT`, `data_bytes`), the data directory, the inputs of `prep`
(`collections/`, `launch/`, the scene sources). Every value keeps its
current default, so `indexus up` with no file does what `explore.sh` does.

## Order of work

1. `up`, `down`, `status`, `ingest`, `export`: the Go parts of `explore.sh`
   and `localdata` moved into `cmd/indexus`; `explore.sh` becomes a call to
   it, then goes.
2. `prep`: the batch tool and the orchestration of `prep-all.sh`; the Python
   and Metal builders stay as they are, run by `prep`.
3. `check` and `perf`: thin wrappers over the existing test and benchmark
   commands, so CI and people run the same thing.
4. `mesh`, `build`, `deploy`: last; they change rarely.
