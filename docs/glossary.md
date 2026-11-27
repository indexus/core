# Glossary

One word, one meaning, in the four repositories (`core`, `sdk`,
`dashboard`, `experiences`). Go and JavaScript use the same names.

## The tree

| Word | Meaning |
| --- | --- |
| **tree** | The one binary tree every address lives in: nodes, collections, zones and records. |
| **ID** | A 96-bit address in the tree (`internal/tree`, `sdk/src/tree`). A node's ID is its place; it never changes, even when the node moves host. |
| **collection** | A named set of records sharing one Space, 16 characters (type + source + vintage + version, e.g. `TempeAROME0128v2`). A new version is a new collection. |
| **Space** | The dimensions of a collection (`gps` for longitude/latitude, `time`, ...) and how a point becomes a location. |
| **location** | The path of a record or a zone below its collection: one character of the routing alphabet (`0-9A-Za-z-_`, 6 bits) per level. |
| **offset** | Per-collection shift of the characters of a location (the name read in the alphabet, repeated). It spreads collections over the tree; it is why two collections' keys differ for the same place. |
| **zone** | A node of the tree under a collection: a location prefix. A zone holds the **sum** of every record below it. |
| **zone key** | `collection/location` as a map key (`zoneKey`), or its bytes in the routing space (`zoneKeyID`). |
| **Cell** | The geometric key of a cell: one integer per coordinate and a resolution, with the collection offset removed. Two collections of the same Space have the same Cell for the same place, which is what lets the GPU join them. |
| **depth** | The number of characters of a location (levels below the collection). |

## Data

| Word | Meaning |
| --- | --- |
| **record** | One datum: id, location, weight, metrics, optional payload. Up to 128 metrics. (`Item` in older code.) |
| **metric** | One additive number of a record (a value × area, a count, a moment...). Means are computed by the client as ratios of sums. |
| **Sum** | Count plus the sums of the metrics of every record below a zone. Abelian: a removed record is subtracted (`abelian*` in the SDK). Sums are compensated (Neumaier) in the store. |
| **Block** | The sums of the children of one zone, as columns: count, then one column per metric (IXB2). The same object on the wire, in the client store and on the GPU. |
| **payload** | Bytes attached to a record and never summed: a mesh, an image tile, a label. JSON by default, raw bytes for binary batches. |
| **column selection** | `/sets?columns=...`: only the listed metrics are sent (the sums are still of whole records). Used to read one hour of a weather collection. |

## Network and authority

| Word | Meaning |
| --- | --- |
| **node** | One Go process serving the zones closest to its ID: `owner(zone) = argmin XOR(zone, V_active)`. |
| **vnode** | A logical node (an ID and its data) inside a process; the unit that moves. |
| **owner** | The node whose ID is closest (XOR) to a zone. Geometry only: the right to write is separate. |
| **authority** | The right to acknowledge a write, supplied by an installed local native or point permission. A cession requires the exact confirmed receiver ticket; a stale route never grants it. |
| **issuer** | The service that signs node certificates and client write tokens. |
| **mesh** | The nodes of one network and their exchanges (`/cluster/`). |
| **gateway** | The dashboard's `server.js`: `/api/health`, `/api/mesh`, `/api/token` and a proxy to the nodes for browsers. (`gateway/`, a separate Go module, is an optional HTTP/3 edge.) |

## Storage and ingest

| Word | Meaning |
| --- | --- |
| **zonelog** | The node's storage engine (`internal/zonelog`): a log of frames, sorted segments, payload files. Records and control state commit together. |
| **segment** | An immutable sorted file of rows in zonelog; zone sums are read from its index. |
| **IXR1** | The binary value of one record in zonelog. |
| **IXP1** | The batch format of offline ingest: a directory with `manifest.json`, `part-NNNNN.ixp` files sorted in store order and `zones.ixp` (the sums of the zones above). |
| **batch** | One IXP1 directory, one collection. Built by `tools/prep`, installed by `localdata -bulk`. |
| **bulk ingest** | Installing a batch as one segment in one pass, with one receipt; `-replace` swaps a collection. |
| **prepared returns** | The LiDAR source decoded once into IXRT files (one per 32 m tile), the input of every LiDAR builder. |

## Wire

| Word | Meaning |
| --- | --- |
| **IXB2** | The `/sets` answer: aligned little-endian blocks a client hands to the GPU without per-row objects. The only `/sets` encoding. |
| **IXRB** | The binary `/records` answer (version 2), payloads as raw bytes. JSON stays the default. |

## Client (SDK layers, top to bottom)

| Word | Meaning |
| --- | --- |
| **tree** | Address algebra (`src/tree`). |
| **wire** | Peers, transport (`fetch`), IXB2 and IXRB decoders, `Network` (`src/wire`, `src/network`, `src/api`). |
| **store** | The client cache of blocks, by zone, with one byte budget (`src/store`). |
| **walk** | One descent of the tree for a view: which zones to read and show (`src/walk`). |
| **gpu** | One device per realm, the arena, kernels (`src/gpu`). |
| **arena** | The GPU buffer that holds a collection's blocks as they arrived. |
| **pack** | The kernel that turns blocks in the arena into instances to draw. |
| **field** | What one collection says of the cells of the view: per cell, `(num, den)` of its heat rule. |
| **compose / combine** | Joining up to 4 fields cell by cell on the GPU with a small expression, before drawing. |
| **raster** | A field written into a texture (`gpu/raster.js`), read by later passes (weather effects). |
| **heat pipeline** | worker → store → arena → pack → (combine) → draw: one per collection on a map (`src/map/heat`). |
| **frame** | One GPU submission per device per turn: packs, then combines, then draws. |

## Apps

| Word | Meaning |
| --- | --- |
| **Explore** | The main app (`experiences/explore.html`): weather, real estate and the 3D scene in one WebGPU renderer; MapLibre draws only the basemap and labels. |
| **scene** | A place Explore can show in 3D (Anglet), one record of `SceneExplo2026v1` naming its collections. |
| **Drawing**, **Chronicle** | The two other apps of `experiences`. |
| **dashboard** | The admin console of a network, and the gateway. |

## Current control vocabulary (25 November 2026)

**Membership** is compact current logical identities, endpoints and retirement
tombstones; it does not grant a write. **Ticket** is a receiver-local admission
bound to one source operation and scope. Historical **Knowledge**, `TokenID`
and `AuthorityReplica` name the replaced global proof graph. See
[local permissions](protocol/local-permissions.md) and the
[qualification map](protocol/local-permissions-verification.md).
