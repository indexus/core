# Read policies and availability during handoff

**Statut : politiques de lecture actuelles.** Les anciennes mesures ci-dessous gardent leur binaire d’origine ; pour le runtime à tickets et la complétude des résumés, lire [permissions locales](local-permissions.md) et la [qualification](local-permissions-verification.md).

`Node.ReadContext(ctx, collection, prefix, ReadOptions)` exposes four explicit
policies. HTTP `/set` and `/sets` accept `read_mode`; existing calls without that
parameter retain their existing `deep`/`refresh` behavior. The local policy may return a routing hint.

| Policy | Behavior |
| --- | --- |
| `local` | Return the selected peer’s derived summary or an owner referral. If that summary is unavailable, return HTTP 503. The request leaves inventory, cache population, and summary repair to their usual paths. |
| `fresh` | Bypass derived summaries and result caches. Retry recoverable readiness, ownership and dependency states until the caller deadline, with a maximum 30-second budget and bounded backoff. A transfer in progress is no longer such a state (READ-1, 2026-11-19): the read is served by the transfer's source and may lag a zone that already flipped. |
| `cached` | Use a previously successful result within `max_age`; otherwise obtain a fresh result. |
| `stale-if-error` | Attempt a fresh inventory; if it fails, return a previous result within `max_age`. Caller cancellation is never converted to success. |

Cache modes require a positive duration, for example:

```text
/set?collection=0000000000000001&location=@&read_mode=cached&max_age=5m
/sets?collection=0000000000000001&location=@&read_mode=fresh
```

`ReadResult.Source` is `summary`, `inventory` or `cache`; `ObservedAt` records when this node
obtained the result. `FallbackError` reports a failed attempt behind a stale
fallback. `/set` exposes these as `X-Indexus-Read-Source`,
`X-Indexus-Observed-At` and, when applicable, the boolean marker
`X-Indexus-Read-Fallback: true` (no raw error text in a header).
`/sets` advertises the requested policy as `X-Indexus-Read-Mode`; a batch can mix
cache hits and fresh answers and does not claim one common observation time.
`GetSetsReadContext` applies the policy to each prefix and fails the batch
if any prefix cannot be answered. Invalid policies are rejected before dispatch.

`max_age` measures time since observation, not freshness of all descendants.
Successful historical `GetSetContext` calls also populate the cache; such a result
may come from derived summaries whose own remote contributions were already
stale. Only `fresh` explicitly bypasses those summaries. No mode promises a
linearizable transaction snapshot across multiple hosts.

Retention is bounded to 32 entries and a conservative 16 MiB estimated object
budget per node. Both aggregate arrays and individual values count against this
budget; oversized results are not retained. Cache insertion and delivery detach
all mutable `Set`/`Abelian` data. A cache hit does not refresh its observation
time. Cached copies share the existing eight-read admission budget.

A warm cache can remain available during a stopped transfer, a retired ingress
or a network partition. A cold, expired, evicted or oversized cache cannot offer
that guarantee. `fresh` is served through a transfer by its source (READ-1),
which names the receiver once a zone has flipped to it: the reader then asks
both and keeps the newer copy of each zone, so a write acknowledged before the
read is in it. A missing host, a receiver that cannot be reached after a flip,
or the caller deadline can still fail. These are
explicit availability choices, never permission to acknowledge a write.

The raw mesh `Query`/`QueryPrefix` APIs remain strict, bounded-attempt inventory
operations. They never silently fall back to stale values. Host scans overlap
with fan-out bounded to four; one failed host discards the attempt. Page cursor
invalidation and pre/post authority checks remain in force. Paging bounds RPC
size, not the full assembled result's memory.

## Verification

- Runtime test pauses a real cold handoff before `install-zone`: cached reads
  succeed at all exercised transfer boundaries; a fresh read is served before
  the installation, from the frozen source. A separate cancellation test checks that reads never
  reopen the source's on-disk write fence.
- Policy tests cover cache mutation isolation, expiry, bounded retention, missing
  prefixes, cancellation and HTTP policy/context propagation.
- The opt-in simulation test uses real `Node` and the predecessor causal mesh/storage runtime. It
  drops cold transfers, exercises loss injection for `stage-zone`/`install-zone`,
  partitions peers, reads cached results, heals, resumes compaction and requires
  a successful fresh result. It does not simulate HTTP/WAN costs.
- Volume campaigns retain their independent raw-inventory and receipt audits;
  cached count/metric checks are additional and cannot replace those audits.

### Cache-only zone lookup

`GET /set/cached?collection=COLLECTION&location=PREFIX&max_age=5s` requires
read scope. It returns a locally retained JSON `set`, its `observed_at`
timestamp and `source: "cache"`. A missing, expired or evicted entry returns
HTTP 204, including when a different peer has previously served the zone.
An empty set returned with HTTP 200 is a cached observation. Writes and
handoffs may have changed the zone since that observation. The endpoint
never fills the cache or follows referrals. It needs no inventory read.

To replay the two-peer cache routing and resume check with a JS SDK checkout:

```sh
INDEXUS_TEST_SDK=/absolute/path/to/sdk-js go test ./internal/app/node \
  -run '^TestCachedZoneSDKTwoPeers$' -count=1 -v
```

The test runs two joined HTTP nodes and restarts the JS process between
prefixes. The application persists its remaining prefixes, observations and
peer list in JSON. Its two cold probes return 204. The fixture then primes
each peer explicitly. The resumed client requests only its pending prefix.
The returned values are zone summaries and do not enumerate terminal records.
