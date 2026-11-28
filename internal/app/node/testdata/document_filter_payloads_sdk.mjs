import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, collection, first, second] = process.argv.slice(2);
const { API, documentFilter: f } = await import(pathToFileURL(resolve(sdk, "src/index.js")));
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
globalThis.fetch = () => { throw new Error("unexpected RPC evaluating loaded JSON"); };
assert.equal(rows.length, 8);
const enabled = f.oneOf(r => r.payload?.enabled, [true]);
const tags = r => r.payload?.tags;
const hidden = f.contains(tags, "hidden"), publicTag = f.contains(tags, "public");
const cases = [
  [f.and(enabled, f.not(hidden), f.or(publicTag, f.missing(tags))), ["missing@aa","null@zz","same@aa"]],
  [f.contains(tags, false), ["same@aa","typed@zz"]],
  [f.missing(tags), ["missing@aa","none@zz","null@zz"]],
  [f.and(enabled, f.not(f.missing(tags)), f.not(publicTag)), ["empty@aa"]],
  [f.and(enabled, hidden), ["same@zz"]],
];
for (const [predicate, expected] of cases) {
  assert.deepEqual(rows.filter(predicate).map(r => r.id + "@" + r.location).sort(), expected);
}
console.log(JSON.stringify({ liveRows: rows.length, payloadRPCs: calls.length, offlineFilters: cases.length }));
