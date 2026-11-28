import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, collection, first, second] = process.argv.slice(2);
const { API, exactKnn } = await import(pathToFileURL(resolve(sdk, "src/index.js")));
const api = new API();
const owners = [[first, "aa"], [second, "zz"]];
const httpFetch = globalThis.fetch;
async function assertCold() {
  for (const [port, location] of owners) {
    const url = new URL("http://127.0.0.1:" + port + "/set/cached");
    url.search = new URLSearchParams({ collection, location, max_age: "1m" });
    const response = await httpFetch(url);
    assert.equal(response.status, 204, await response.text());
  }
}
await assertCold();
const calls = [];
globalThis.fetch = (url, init) => {
  const req = new URL(url);
  calls.push([req.pathname, req.port, req.searchParams.get("location")]);
  return httpFetch(url, init);
};
const rows = (await Promise.all(owners.map(([port, location]) =>
  api.getDirectRecords("http", { ip: () => "127.0.0.1", port: () => port }, collection, location, 3),
))).flat();
assert.deepEqual(calls.slice().sort(), owners.map(([port, zone]) => ["/records/direct", port, zone]).sort());
globalThis.fetch = () => { throw new Error("unexpected network access during local recomposition"); };

// Manually ranked complete results for the fixed Go corpus, independent of
// the HTTP responses. Ties are resolved by document ID, then location.
const cases = [
  { group: 1, query: [0, 0], want: [["before","zz",1], ["same","aa",1], ["same","zz",1], ["far","aa",9]] },
  { group: 1, query: [-1, 0], want: [["same","zz",0], ["before","zz",2], ["same","aa",4], ["far","aa",10]] },
  { group: 2, query: [0, 0], want: [["excluded","aa",0], ["another","zz",25]] },
  { group: 3, query: [0, 0], want: [] },
];
assert.equal(rows.length, 6);
for (const { group, query, want } of cases) {
  const candidates = rows.filter(row => row.metrics[0] === group).map(row => ({
    id: row.id, location: row.location, vector: row.metrics.slice(1),
  }));
  for (const limit of [1, 2, 10]) {
    const hits = exactKnn(candidates, query, { limit });
    assert.deepEqual(hits.map(hit => [hit.id, hit.location, hit.distance_squared]), want.slice(0, limit));
  }
}
await assertCold();
console.log(JSON.stringify({ liveRows: rows.length, directRPC: calls.length, offlineRankings: cases.length * 3, coldCacheChecks: 4 }));
