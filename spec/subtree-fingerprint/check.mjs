// Independent Node implementation; run from any directory with node check.mjs.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

const fixture = JSON.parse(readFileSync(new URL("./v1.json", import.meta.url), "utf8"));
function uint64(n) {
  const b = Buffer.alloc(8);
  b.writeBigUInt64BE(BigInt(n));
  return b;
}
function framed(bytes) {
  return Buffer.concat([uint64(bytes.length), bytes]);
}
function float64(x) {
  const b = Buffer.alloc(8);
  b.writeDoubleBE(x);
  return b;
}
let total = 0n;
for (const row of fixture.rows) {
  const parts = [Buffer.from("indexus/subtree-row/v1\0")];
  for (const value of [row.collection, row.location, row.id]) parts.push(framed(Buffer.from(value, "utf8")));
  parts.push(float64(row.weight === 0 ? 1 : row.weight), uint64(row.metrics.length));
  for (const metric of row.metrics) parts.push(float64(metric));
  parts.push(Buffer.from([Number(row.binary)]), framed(Buffer.from(row.storedPayloadHex, "hex")));
  const digest = createHash("sha256").update(Buffer.concat(parts)).digest("hex");
  assert.equal(digest, row.sha256, row.id);
  total += BigInt("0x" + digest);
}
assert.equal(BigInt.asUintN(256,total).toString(16).padStart(64,"0"), fixture.monoid.sum);
assert.equal(BigInt(fixture.monoid.count), BigInt(fixture.rows.length));
console.log(fixture.monoid);
