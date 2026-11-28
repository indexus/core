// The expected mask is encoded in the row key; the SDK reads the actual Go bytes.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
const [sdk, blocksDir] = process.argv.slice(2);
const { decodeBlocks, suffixAt } = await import(pathToFileURL(resolve(sdk, "src/wire/block.js")));
const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_";
for (const width of ["f32", "f64"]) {
  for (const projection of ["all", "selected"]) {
    const name = width + "-" + projection;
    const file = readFileSync(resolve(blocksDir, name + ".bin"));
    const { blocks, columns } = decodeBlocks(Uint8Array.from(file));
    const selected = projection === "all" ? [0,1,2,3,4,5,6,7] : [7,0,3];
    assert.deepEqual(columns, projection === "all" ? null : selected);
    const seen = new Set();
    for (const block of blocks) {
      for (let i = 0; i < block.n; i++) {
        const key = suffixAt(block, i);
        const mask = alphabet.indexOf(key[0]) * 64 + alphabet.indexOf(key[1]);
        assert.ok(mask >= 0 && mask < 256 && !seen.has(mask));
        seen.add(mask);
        assert.equal(block.count[i], 2);
        selected.forEach((bit, column) => {
          assert.equal(block.metrics[column * block.n + i], ((mask >> bit) & 1) * 2);
        });
      }
    }
    assert.equal(seen.size,256);
    console.log(name,seen.size,"rows verified;",file.length,"bytes");
  }
}

const { presenceMasks } = await import(pathToFileURL(resolve(sdk, "src/index.js")));
const schema = Array.from({length:300}, (_, i) => i);
const requested = [...schema, 511];
const witness = new Set([0,1,63,64,65,257,299]);
const selected = [299,2,63,257,65,1,260,64];

function verify(parts, requested, visible, allowNegative, label) {
  const { present, absent } = presenceMasks(parts, requested);
  assert.equal(present & absent, 0n);
  const totals = { present:0, absent:0, unknown:0 };
  requested.forEach((source, i) => {
    const bit = 1n << BigInt(i);
    const expected = visible.has(source) && witness.has(source) ? "present" :
      visible.has(source) && allowNegative ? "absent" : "unknown";
    const actual = (present & bit) !== 0n ? "present" : (absent & bit) !== 0n ? "absent" : "unknown";
    assert.equal(actual, expected, label + " source " + source + " at result bit " + i);
    totals[actual]++;
  });
  assert.equal((present | absent) >> BigInt(requested.length), 0n);
  return totals;
}

for (const width of ["f32","f64"]) {
  const results = {};
  for (const variant of ["wide", "wide-selected", "wide-redirect", "wide-padded"]) {
    const body = readFileSync(resolve(blocksDir, width + "-" + variant + ".bin"));
    const response = decodeBlocks(Uint8Array.from(body));
    const columns = variant === "wide" ? null : variant === "wide-padded" ? [511,2] : selected;
    assert.deepEqual(response.columns, columns);
    assert.equal(response.blocks.reduce((n,b) => n + b.n, 0), 2);
    assert.ok(response.blocks.every(b => b.metricBytes === (width === "f32" ? 4 : 8)));
    assert.ok(response.blocks.every(b => b.metricCount === (columns?.length ?? 300)));
    const covered = {response, complete:true};
    if (variant === "wide-padded") {
      // Go zero-fills out-of-schema projected metrics. No field-presence
      // indicator on the wire distinguishes that zero from known absence.
      for (const b of response.blocks) for (let i=0;i<b.n;i++)
        assert.equal(b.metrics[i],0);
      assert.deepEqual(presenceMasks([covered], [511,2]), {present:0n,absent:3n});
      const totals = verify([{response,complete:false}],[511,2],new Set(),false,variant);
      results[variant] = {bytes:body.length, ...totals};
      continue;
    }
    assert.equal(response.redirects.length, variant === "wide-redirect" ? 1 : 0);
    const visible = new Set(columns ?? schema);
    const allowNegative = response.redirects.length === 0;
    const totals = verify([covered],requested,visible,allowNegative,variant);
    // Output bit 63 now represents a different source. Reconstruct the
    // expected class for every output bit from the source column and oracle.
    verify([covered],requested.toReversed(),visible,allowNegative,variant+" reversed");
    verify([covered,null],requested,visible,false,variant+" missing shard");
    results[variant] = {bytes:body.length,...totals};
  }
  console.log(width, JSON.stringify(results));
}
