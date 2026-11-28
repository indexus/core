// Local, preloaded JS microbenchmark. Run through search-pilot.py, or directly:
// node scripts/perf/search-bitmaps.mjs /path/to/sdk-js 100000
import assert from "node:assert/strict";
import { performance } from "node:perf_hooks";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, size = "100000"] = process.argv.slice(2);
const rows = Number(size);
if (!sdk || !Number.isSafeInteger(rows) || rows < 1 || rows > 1_000_000)
  throw new RangeError("expected SDK path and 1 to 1000000 rows");
const { DocumentUniverse } = await import(pathToFileURL(resolve(sdk, "src/index.js")));
const ids = Array.from({ length: rows }, (_, i) => String(i));
const deleted = [], cohort = [], bucket = [];
let seed = 20261128;
for (let i = 0; i < rows; i++) {
  seed = (Math.imul(1664525, seed) + 1013904223) >>> 0;
  if (seed >>> 31) cohort.push(i);
  if ((seed >>> 16) % 10 === 0) bucket.push(i);
  if (i % 101 === 0) deleted.push(i);
}
const deletedIDs = new Set(deleted), bucketIDs = new Set(bucket);
const expected = cohort.filter(id => !deletedIDs.has(id) && bucketIDs.has(id)).length;
const before = performance.now();
const u = new DocumentUniverse("pilot", "fixed", ids, deleted);
const a = u.posting(cohort), b = u.posting(bucket);
const buildMs = performance.now() - before;
assert.equal(u.intersectionCount(a, b), expected);
const iterations = 100;
const samples = [];
for (let pass = 0; pass < 5; pass++) {
  const start = performance.now();
  let total = 0;
  for (let i = 0; i < iterations; i++) total += u.intersectionCount(a, b);
  samples.push((performance.now() - start) * 1000 / iterations);
  assert.equal(total, expected * iterations);
}
console.log(JSON.stringify({ rows, seed: 20261128, deleted: deleted.length,
  cohort: cohort.length, bucket: bucket.length, count: expected, buildMs,
  query: "intersectionCount", iterationsPerPass: iterations, microsecondsPerCall: samples }));
