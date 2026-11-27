# Local mesh (no AWS)

Replace EC2 spawn/terminate with processes on `127.0.0.1`.

```bash
# start issuer + bootstrap (autoscale enabled, local spawn backend)
./ops/local/mesh_up.sh

# manual spawn via issuer
curl -X POST http://127.0.0.1:22000/v1/scale \
  -H 'Content-Type: application/json' \
  -d '{"requester_id":"lab","local_inserts":9999,"spawn_count":1}'

./ops/local/list_spawned.sh
./ops/local/count.sh

./ops/local/mesh_down.sh
```

Ops console and dataset loaders: the sibling repository [dashboard](https://github.com/indexus/dashboard) (`BOOT_IP=127.0.0.1 pnpm dev`).

| Script | Role |
|--------|------|
| `mesh_up.sh` | build + issuer (`-launchTemplate local`) + bootstrap |
| `spawn.sh` | started by issuer `/v1/scale` (node names itself via PreferNear) |
| `terminate.sh` | stop the process, preserving durable data; no vnode drain |
| `count.sh` / `list_spawned.sh` | live spawned inventory |
| `mesh_down.sh` | tear everything down (ports 19000–19020 / 21000–21020) |
| `scan_consistency.py` | check ownership, delegation chains and aggregates of a live mesh (exit 1 on an inconsistency) |

## Capacity defaults (`mesh_up` / large loads)

| Env | Default | Role |
|-----|---------|------|
| `DELEGATION` / `INDEXUS_DELEGATION` | `5000` | Soft item count before Own split (`-delegation`). Override via dashboard Config → Restart mesh (`.local/data/mesh-config.env`) |
| `SPAWN_MAX` | `15` | Issuer `-spawnMax` (spawned processes, bootstrap excluded) |
| `INDEXUS_ITEMS_LIMIT` | `100000` | Owned items that force scale-up |
| `INDEXUS_P2P_TLS` | `0` | `1` → mint `.local/data/tls` and pass `-sslStorage` (HTTPS/HTTP/2 on P2P+mon). Dashboard: `P2P_PROTOCOL=https` |

Data and logs live under `.local/data/` (gitignored).

### Sticky certs

`cmd/node` prefers an existing `.cert.json` over PreferNear. `mesh_up.sh` / `spawn.sh` wipe `local-*.cert.json` before start. Force kill from the dashboard (`POST /api/terminate`) also clears keys.

## Causal runtime

Bootstrap startup passes `-createCluster`; spawned nodes join by anti-entropy
and durable local handoff. Keep `.causal.db` and `.causal-identity.json` together.
The former in-memory/WAL runtime and S3 snapshot endpoints are removed.
`INDEXUS_IN_MEMORY=1` is rejected. Stopping a process preserves its vnode for
restart; it does not drain a physical host. The executable's opt-in
`-coldCompaction` folds quiet logical vnodes with durable retirement witnesses.
See [runtime configuration](../../docs/protocol/distributed-runtime.md).
