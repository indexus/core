import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, collection, first, second, phase] = process.argv.slice(2);
const { API, selectDirectRecords } = await import(pathToFileURL(resolve(sdk, "src/index.js")));
const peers = [first, second].map(port => ({ ip: () => "127.0.0.1", port: () => Number(port) }));
const { records, requests, complete } = await new API().readObservedCollection("http", peers, collection,
  { maxRequests: 120, limitPerLocation: 12, timeoutMs: 3000 });
assert.equal(complete, false);
const expected = [];
for (let i = 0; i < 192; i++) {
  if (phase === "after" && i % 6 === 3) continue;
  const revision = phase === "before" ? 1 : i % 6 === 0 ? 3 : i % 5 === 0 ? 2 : 1;
  const zone = (i % 2 === 0 ? "aa" : "zz") + (Math.floor(i / 2) % 8);
  expected.push(["doc-" + String(i).padStart(3, "0"), zone, revision]);
}
const actual = selectDirectRecords(records, { columns: ["id", "location", 0] });
actual.sort((a, b) => a[0].localeCompare(b[0]));
assert.deepEqual(actual, expected);
console.log(JSON.stringify({ phase, requests, matched: actual.length, complete }));
