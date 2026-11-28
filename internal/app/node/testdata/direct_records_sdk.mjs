import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, collection, firstPort, secondPort] = process.argv.slice(2);
const { API } = await import(pathToFileURL(resolve(sdk, "src/index.js")));
const api = new API();
const nativeFetch = globalThis.fetch;
const calls = [];
globalThis.fetch = async (url, init) => {
  const target = new URL(url);
  const response = await nativeFetch(url, init);
  calls.push([target.pathname, target.port, target.searchParams.get("location"), response.status]);
  return response;
};
async function cold(port, location) {
  const url = new URL("http://127.0.0.1:" + port + "/set/cached");
  url.search = new URLSearchParams({ collection, location, max_age:"1m" });
  const response = await nativeFetch(url);
  assert.equal(response.status, 204, await response.text());
}
const read = (port, location, limit) =>
  api.getDirectRecords("http", {ip: () => "127.0.0.1", port: () => port}, collection, location, limit);

await cold(firstPort, "aa");
await cold(secondPort, "zz");
const a = await read(firstPort, "aa", 2);
const z = await read(secondPort, "zz", 1);
assert.deepEqual(a.map((r) => [r.id, r.metrics]).sort(), [["a",[1]],["b",[2]]]);
assert.deepEqual(z.map((r) => [r.id, r.metrics]), [["z",[5]]]);
await assert.rejects(read(firstPort, "aa", 1), (e) => e.response?.status === 413);
await assert.rejects(read(secondPort, "aa", 2), (e) => e.response?.status === 409);
await cold(firstPort, "aa");
await cold(secondPort, "zz");
assert.deepEqual(calls, [
  ["/records/direct",firstPort,"aa",200],
  ["/records/direct",secondPort,"zz",200],
  ["/records/direct",firstPort,"aa",413],
  ["/records/direct",secondPort,"aa",409],
]);
console.log(JSON.stringify({calls,documents:a.length+z.length,caches:"cold before and after"}));
