# Local subtree content fingerprint

**Status** Implemented. The optional read-scoped HTTP operation is
`GET /zones/fingerprint/local?collection=C&location=L`. The client addresses
one peer and one canonical prefix. `@` observes the collection root. Direct
records at the prefix and all its descendants participate, including records
at intermediate addresses. A peer reports physical local live copies.
A ceded copy may remain on disk. A probe is never proof of global coverage.

## Response and client reuse

HTTP 200 echoes `collection` and `location`, sets `complete:false`, and
returns `fingerprint`, `guard`, and `monoid`. Successful observations have
two opaque, nonempty strings and a monoid object. A known empty scope has
count `"0"`, a sum of 64 zeroes, and valid tokens. An inconclusive probe
sets **all three fields to JSON null**, including for a budget refusal or a
migration. Clients discard cached reuse on an unknown result, changed scope
or peer roster, unrecognized algorithm, or unavailable endpoint. An older
peer may return 404. Ordinary traversal remains available. The response uses
`Cache-Control: no-store`. Invalid parameters return 400 and an unavailable
reader may return 503.

To establish a reusable baseline, probe every relevant peer before and after
reading the rows. Require matching non-null fingerprints **and guards** per
peer. The guard detects a write followed by its reversal during the reads.
For reuse of an established baseline, compare the fingerprints on the same
incarnations, scopes and peer roster. Unrelated writes can change the guard
while preserving the scope's content. A reopened store invalidates the opaque
tokens. The tokens have no portable encoding or combination rule. This
procedure preserves the consistency limits of the original multi-peer read.

## Portable monoid

The v1 wire object has three JSON strings. `algorithm` is
`"sha256-add256-v1"`. `sum` is a zero-padded lower-case hexadecimal encoding
of a 256-bit big-endian unsigned integer. `count` is the unsigned decimal row
count in canonical notation, with no leading zeroes except `"0"`. Parse both
values with integer arithmetic that preserves their full width.

Each visible row contributes count one, irrespective of its stored weight.
Hash the following concatenation with SHA-256. There are no delimiters or
padding between fields. `u64` means an unsigned 64-bit big-endian integer.
A framed value consists of its byte length as a u64 followed by its bytes.

| Order | Value | Bytes |
|---|---|---|
| 1 | Domain | ASCII `indexus/subtree-row/v1` followed by a single zero byte |
| 2–4 | Collection name, location, document ID | Frame each stored string separately using its exact bytes (UTF-8 for JSON strings), without normalization |
| 5 | Weight | Big-endian IEEE-754 binary64. A stored zero weight has effective value 1 |
| 6 | Metric count | u64 |
| 7 | Metrics in stored order | One big-endian IEEE-754 binary64 per metric |
| 8 | Binary flag | One byte, 0 for JSON or omitted body, 1 for binary |
| 9 | Payload | Framed stored payload bytes |

For ordinary JSON the payload is the compact JSON byte sequence retained
by the store. Object key order and escape spellings can matter. In Go's
ordinary write path `encoding/json` compacts the supplied JSON and applies
HTML escaping. A missing payload contributes zero bytes. JSON null contributes
the four bytes `6e756c6c`. Binary content contributes its JSON-quoted standard
base64 spelling, including any padding. Empty binary content contributes the
two quote bytes `2222`. Parsing an HTTP JSON response and serializing its
payload again can lose the exact source bytes.

Interpret each 32-byte digest as a big-endian integer. Sum all leaf digests
modulo 2²⁵⁶ and add their counts. Empty input yields `(0,0)`. Local parent
content decomposes into records directly at the parent plus its disjoint
child subtrees. Combining overlapping prefixes or copies on different peers
can count the same record twice. Comparable raw aggregate values require the
same algorithm and disjoint contributions from the intended view. Digest
equality is a probabilistic change detector.

The [golden fixture](../../spec/subtree-fingerprint/v1.json) gives stored
payload bytes, each leaf digest and a wrapped aggregate. Run
`node spec/subtree-fingerprint/check.mjs` and
`go test ./internal/storage -run '^TestSubtreeFingerprintProtocolGolden$'`
from the repository root to verify the independent Node encoder and the Go
storage implementation against it.
