import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
const [sdk, collection, ...ports] = process.argv.slice(2);
const { API, selectDirectRecords, documentFilter, numericStats, topByAttributes, scalarFacet, groupCounts } =
  await import(pathToFileURL(resolve(sdk, "src/index.js")));
const peers = ports.map(port => ({ ip: () => "127.0.0.1", port: () => Number(port) }));
const calls = [];
const original = globalThis.fetch;
globalThis.fetch = async (input, init) => {
  const url = new URL(input);
  assert.ok(ports.includes(url.port));
  assert.ok(["/zones/children/local", "/records/direct"].includes(url.pathname));
  assert.equal(init.redirect, "error");
  const response = await original(input, init);
  calls.push([url.pathname, url.searchParams.get("location"), response.status]);
  return response;
};

const { records, requests, complete } = await new API().readObservedCollection(
  "http", peers, collection, { includePayloads: true, maxRequests: 60, limitPerLocation: 4, timeoutMs: 2000 });
assert.equal(complete, false);
assert.equal(requests, calls.length);
const ids = selectDirectRecords(records, { columns: ["location", "id"] }).sort();
assert.deepEqual(ids, [["@", "root"], ["aa", "same"], ["aa0", "deep"], ["zz", "same"], ["zz1", "leaf"]]);
assert.deepEqual(selectDirectRecords(records, { ids: ["same", "deleted"], columns: ["location", 0] }).sort(),
  [["aa", 2], ["zz", 4]]);
const metric = row => row.metrics[0];
const keep = documentFilter.and(documentFilter.oneOf(metric, [2, 3, 4, 5]), documentFilter.not(documentFilter.oneOf(metric, [3])));
const filtered = records.filter(keep);
assert.deepEqual(numericStats(filtered, metric), { matched: 3, present: 3, missing: 0, sum: 11, avg: 11 / 3, min: 2, max: 5 });
assert.deepEqual(scalarFacet(filtered, row => row.id).buckets, [{value:"leaf",count:1},{value:"same",count:2}]);
assert.deepEqual(topByAttributes(filtered, [{select: row => row.id}, {select: metric, order:"desc"}], {limit:2})
  .map(row => row.location), ["zz1", "zz"]);
const tags = documentFilter.containsAny(row => row.payload.tags, [2, 4]);
const day = documentFilter.timeRange(row => row.payload.time, {gte:"2026-01-01T01:00:03+01:00",lt:"2026-01-01T00:00:06Z"});
assert.deepEqual(records.filter(tags).map(row => row.location).sort(), ["aa", "zz"]);
assert.deepEqual(records.filter(day).map(row => row.id).sort(), ["deep", "leaf", "same"]);
const selectors = [row => row.id, row => row.payload.tags.length];
const page = offset => groupCounts(records, selectors, {offset, limit:2});
assert.deepEqual(page(2), [{values:["root",2], count:1},{values:["same",2],count:2}]);
assert.deepEqual(page(4), []);
assert.equal(requests, calls.length);
console.log(JSON.stringify({ documents: ids, requests, calls, complete }));
