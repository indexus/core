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
