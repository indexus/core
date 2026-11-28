import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, collection, aa, zz] = process.argv.slice(2);
const { API, numericFacet, topByAttribute } = await import(pathToFileURL(resolve(sdk, "src/index.js")));
const api = new API();
const fetchHTTP = globalThis.fetch;
const requests = [];
globalThis.fetch = async (url, init) => {
  const target = new URL(url);
  const [port, location] = [[aa, "aa"], [zz, "zz"]][requests.length] ?? [];
  assert.equal(target.href, `http://127.0.0.1:${port}/records/direct?collection=${collection}&location=${location}&limit=4`);
  assert.equal(init.method, "GET");
  assert.equal(init.redirect, "error");
  requests.push([port, location]);
  const response = await fetchHTTP(url, init);
  assert.equal(response.status, 200);
  return response;
};

const signal = AbortSignal.timeout(5000);
const read = (port, location) => api.getDirectRecords("http",
  { ip: () => "127.0.0.1", port: () => port }, collection, location, 4, { signal });
const start = performance.now();
const rows = (await Promise.all([read(aa, "aa"), read(zz, "zz")])).flat();
const received = performance.now();
assert.equal(rows.length, 8);
const eligible = rows.filter(row => row.metrics[0] === 1);
const height = row => row.metrics[1] ?? null;
const key = row => `${row.id}@${row.location}`;
const facets = numericFacet(eligible, height);
const highest = topByAttribute(eligible, height, {order:"desc", limit:4}).map(key);
const lowest = topByAttribute(eligible, height, {order:"asc", limit:10}).map(key);
// Fixed oracle from the Go input. No sorting/counting code shared with the SDK.
assert.deepEqual(facets, {matched:7, missing:2, buckets:[{value:4,count:3},{value:9,count:2}]});
assert.deepEqual(highest, ["b@aa","c@zz","a@zz","same@aa"]);
assert.deepEqual(lowest, ["a@zz","same@aa","same@zz","b@aa","c@zz","m@aa","z@zz"]);
assert.deepEqual(requests, [[aa,"aa"],[zz,"zz"]]);
console.log(JSON.stringify({ requests, received:rows.length, facets, highest, lowest,
  read_ms:received-start, total_ms:performance.now()-start }));
