# Client wire: `/sets` blocks (IXB2) and `/records` (IXRB)

**Statut : actuel.**

Status: contract. IXB2 is the only `/sets` encoding (the former v1 and its
`IXS1` envelope are gone), and IXRB v2 the binary form of `/records`
(section at the end). This document is the single source of truth: the Go
encoder (`indexus`) and the JS decoder (`sdk/src/wire`) are tested
against the same golden files.

## Why

The former v1 packed columns bit by bit, sent keys as text and quantised
metrics: a client decoded every value one at a time and built objects per
row. IXB2
lays out one block per (parent zone, key depth) as aligned little-endian
columns, so that a client can read every column with
`new Uint32Array(buffer, offset, n)` / `new Float32Array(...)` and hand it to
a `GPUBuffer` without creating any per-row object.

## Negotiation

- Request: `GET /sets?...` (`format=block` is still accepted, and ignored).
  `deep`, `via`, `refresh` and the read policy keep their meaning;
  `envelope` is ignored: redirects always travel in their own section.
- `&metrics=f64` asks for float64 metric columns (header flag bit 1, see
  below). Without it, or with `metrics=f32`, metrics are float32.
- `&columns=0,13,14` sends only those metrics, in that order (1 to 255
  distinct positions; anything else is a 400). The body then has header flag
  bit 2 and a columns section before the rows; every rows section has
  `metricCount` equal to the number of columns, column `j` holding metric
  `columns[j]` (0 when a row has no such metric). Counts and sums are those
  of the whole records: selection changes what is sent, not what is summed.
- Response: `Content-Type: application/vnd.indexus.block`, body as below.
- Compression belongs to HTTP (`Content-Encoding`), never to the body. A
  node gzips a `/sets` answer of 1400 bytes or more when the
  request accepts gzip, and sends `Vary: Accept-Encoding` and the compressed
  `Content-Length`. Nodes do not ask each other for gzip (see Between peers).
- Browsers read `Content-Type` cross-origin without
  `Access-Control-Expose-Headers` (it is CORS-safelisted); only the
  `X-Indexus-Ingress-*` hint needs exposing, and it is.

## Layout

All integers are little-endian. Every section starts and ends on a 4-byte
boundary; padding bytes are zero.

```
Header (16 bytes)
  0  [4]u8  magic   "IXB2"
  4  u16    headerBytes = 16
  6  u16    flags     bit 0: a redirects section is present
                      bit 1: metrics are f64 (metrics=f64)
                      bit 2: a columns section is present (columns=...)
                      bit 3: a wide rows section (kind 4) is present
  8  u32    sectionCount
  12 u32    reserved = 0

Section header (8 bytes), then the body of that kind
  0  u32    sectionBytes   total, header included, multiple of 4
  4  u16    kind           1 = rows, 2 = redirects, 3 = columns,
                           4 = wide rows
                           (unknown kinds are skipped)
  6  u16    reserved = 0

Rows body (kind 1)
  0  u32    rowCount n                 n ≥ 1
  4  u8     parentLength               characters of the parent location
  5  u8     suffixChars s              characters of every key below the parent
  6  u8     keyWords k = ceil(6·s/32)
  7  u8     metricCount m
  8  u32    revision                   0 = unknown (reserved for zone versions)
  12 [parentLength]u8  parent          ASCII, root "@" is sent as length 0
     zero padding to 4
     [n·k]u32   keys                   row-major: row i owns words [i·k, i·k+k)
     [n]u32     count
     [m][n]f32  metrics                column-major: metric j owns [j·n, j·n+n)
   or, with flag bit 1 and m > 0:
     zero padding to 8 from the start of the body
     [m][n]f64  metrics                same order

Wide rows body (kind 4): a rows body of more than 255 metrics
  0..11     as kind 1, with metricCount (byte 7) = 0
  12 u32    metricCount m              256 ≤ m ≤ 4096
  16 [parentLength]u8  parent
     then keys, count and metrics exactly as kind 1

Columns body (kind 3), first after the header when present
  0  u32    count c                    1 ≤ c ≤ 4096
  4  [c]u16 metric positions           distinct, in the order asked
     zero padding to 4

Redirects body (kind 2)
  0  u32    count
  then per redirect:
     u16 locationLength, u16 nameLength, u16 ipLength, u16 port,
     location, name, ip (ASCII), zero padding to 4
```

An encoder writes kind 4 only for a section wider than 255 metrics (an
embedding), so a body without embeddings is byte for byte what it was
before kind 4 existed. A decoder that predates kind 4 skips those sections,
as it skips any unknown kind; bit 3 says that it did.

With f64 metrics the column starts on a multiple of 8 counted from the first
byte of the body (the magic; a columns section before the rows counts), so that a decoder can view it as a
`Float64Array` on a buffer aligned on 8. A section without metrics takes no
padding. Sections stay multiples of 4, not 8: the padding is computed from
the body start, so a section that starts at 4 mod 8 still aligns its column,
and no rule depends on where a section ends.

### Keys

A key is the part of a child location below its parent. For parent `p` and
child location `c`, the suffix is `c[len(p):]` (`p` is empty for the root).
Each suffix character is replaced by its index in the routing alphabet

```
0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_
```

(`protocol.Base.IndexOf`). The 6-bit values are concatenated, first character
first, into a bit string read MSB-first across the `k` words: character `i`
occupies bits `[6i, 6i+6)`, word `w` holds bits `[32w, 32w+32)` with bit 0 of
the string in the word's most significant bit. Unused low bits are zero.

Example, suffix `"7a"` (indices 7 and 36), `k = 1`:
`000111 100100` followed by 20 zero bits = `0x1E400000`.

Geometry (client): `Space` writes its cells in the same alphabet. At level
`l` of the full location (parent length + position in the suffix) the 6-bit
cell index is `(r - offset(l)) mod 64`, where `r` is the key
digit above (its index in the routing alphabet) and `offset(l)` the
collection's offset: the routing-alphabet index of character `l mod n` of the
collection name, `n` its length. The offset repeats the name cyclically, so it
is defined at every level up to `SetsPrecision` (32). A kernel needs no
alphabet table: only the offsets and the mask.

This rule addresses the stored data. Locations written under the former
client rule (cells in `A-Za-z0-9-_`, offset undefined past the name) are not
readable under it and must be re-loaded (the alphabet migration note is
archived outside the repository).
The byte format of this document is unchanged.

### Rows

- Item keys are stripped of their `:reference` suffix and truncated to
  `SetsPrecision`, as in v1. A node sends the keys of a zone
  ([query.md](query.md#keys-of-a-zone)), so its sections have `suffixChars`
  0 to 4; the format itself allows up to 32. Rows whose keys then collide are **folded by the
  encoder** (count and metrics summed). Keys are unique within a section and
  sorted ascending by their words.
- `count` is `Abelian.Count()`. `metrics[j]` is `float32(Abelian.Metric(j))`
  for `j < m`, where `m` is the largest metric length among the rows of the
  section. Missing metrics are 0. Unlike v1 there is no integer scaling and no
  metric is omitted. Folded rows are summed in float64; with f64 metrics the
  sum is written as it is, otherwise rounded once to float32. float32 keeps 24
  bits: a sum is off by at most 2^-24 of itself, which ratios of positive sums
  tolerate, but a sum whose terms span more than 24 bits is not exact; ask for
  f64 then.
- Sections appear in the order of the requested locations, then by ascending
  `suffixChars`. A location with no rows produces no section.
- A key that does not start with its parent is sent in a section with
  `parentLength = 0` and its full location as the suffix.

### Redirects

Same meaning as the IXS1 envelope: the peer to ask next for `location`.

## Limits

- `suffixChars ≤ SetsPrecision` (32), so `k ≤ 6`.
- `n ≤ 2^32 − 1`, `m ≤ 255`, `parentLength ≤ 255`.
- A decoder rejects: wrong magic, `headerBytes ≠ 16`, a section that overruns
  the body or is not a multiple of 4, `k ≠ ceil(6·s/32)`, bytes after the
  last section, and an f64 body whose buffer is not aligned on 8.

## Between peers

A zone served by a node that does not hold it carries the same values as the
one its owner serves: count and every metric in float64, never a v1
projection.

- The mesh runtime does not relay `/sets` bodies. The entry node reads the
  holder's records over the mesh (`QueryPrefixMetrics`, metrics in float64)
  and groups them itself, then encodes IXB2 for its client. Owner
  summary, owner scan (`refresh=true`) and relay all answer the keys of a
  zone ([query.md](query.md#keys-of-a-zone)): the zone itself, a child's one
  location when all its records sit there (at most 4 characters below the
  zone), else the one-character child. Their bodies are equal byte for byte
  (`TestRelayedBlockReadEqualsTheOwners`,
  `TestZoneKeysAreTheSameFromOwnerAndRelay`), within the limits stated there.
- The peer `/sets` client (`internal/adapter/peer`, `Contact.Get`) asks
  `metrics=f64` and decodes with `indexus.DecodeSetsBlock`;
  owner redirects come from the redirects section. Any other content type
  is an error.
  The mesh runtime has no caller of this client today.
- No gzip between peers: for 10 000 rows the f64 block is 520 kB against
  171 kB in v1 (104 kB against 97 kB gzipped); compressing would cost about
  2 ms on each side per read to save bytes on a link that is usually local.

## `/records?format=binary` (IXRB)

`/records` answers JSON (`{"records": [...]}`) by default. With
`format=binary` it answers `Content-Type: application/vnd.indexus.records`,
little endian, written as it goes (`indexus.EncodeRecords`, decoded by
`indexus.DecodeRecords`):

```
"IXRB" | u8 version (2) | 3 × u8 0 | u32 count | count × record
record: u16 n, location | u16 n, id | u8 flags | f64 weight |
        u8 n, n × f64 metrics | u32 n | 0 to 3 × u8 0 | payload
flags:  bit 0  the payload is raw bytes (a binary record); else JSON text
```

Every payload starts at a body offset that is a multiple of 4 (the zero
bytes before it, checked as zero by a decoder), so a client can view a
binary payload as 32-bit words in place. Version 1 (2026-11-14 morning:
an 8-byte header, no padding) was never served to a released client; a
decoder refuses any version but 2.

The collection is the request's. `weight` is 0 for a plain record (one
item). In JSON a binary record's payload is the JSON string of the base64 of
its bytes, with `"binary": true`; in IXRB it is the bytes. Records are in
the same order as in JSON.

## Golden files

`internal/wire/testdata/sets_block/*.bin` with their `*.json` expectation
(`columns`, `columns-f64` for `columns=`) are shared
with `sdk/src/wire/testdata/`. `internal/wire/testdata/records/basic.bin` (IXRB) and
`basic.json` (the same records as JSON, and what a decoder reads back) are
shared with `sdk/src/api/testdata/records/`; `TestRecordsGoldenFile`
rewrites them with `INDEXUS_WRITE_GOLDEN=1`. Any change to this document changes those
files in the same commit, on both sides.
