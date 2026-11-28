import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, collection, first, second] = process.argv.slice(2);
const { API, groupCounts } = await import(pathToFileURL(resolve(sdk, "src/index.js")));
const api = new API();
const owners = [[first, "aa"], [second, "zz"]];
const httpFetch = globalThis.fetch;
const calls = [];
globalThis.fetch = (url, init) => {
  const req = new URL(url);
  calls.push([req.pathname, req.port, req.searchParams.get("location"), req.searchParams.get("payloads")]);
  return httpFetch(url, init);
};
const rows = (await Promise.all(owners.map(([port, location]) =>
  api.getDirectRecords("http", { ip: () => "127.0.0.1", port: () => port }, collection, location, 4, { includePayloads: true }),
))).flat();
assert.deepEqual(calls.slice().sort(), owners.map(([port, location]) => ["/records/direct",port,location,"true"]).sort());
globalThis.fetch = () => { throw new Error("unexpected RPC during local group pagination"); };
const selectors = [r=>r.payload.usage, r=>r.payload.size];
const page = offset => groupCounts(rows, selectors, {offset,limit:2});
const pages = [page(0),page(2),page(4),page(6)];
assert.deepEqual(pages, [
  [{values:["house",1],count:1},{values:["house",3],count:2}],
  [{values:["shop",2],count:3},{values:["shop",null],count:1}],
  [{values:[null,1],count:1}], [],
]);
assert.deepEqual(pages.flat(), groupCounts(rows, selectors));
assert.equal(pages.flat().reduce((sum,group)=>sum+group.count,0),8);
console.log(JSON.stringify({directRPCs:calls.length, rows:rows.length, groups:pages.flat().length, pageSizes:pages.map(p=>p.length)}));
