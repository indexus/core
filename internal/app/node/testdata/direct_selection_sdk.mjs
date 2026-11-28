import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, collection, aa, zz] = process.argv.slice(2);
const { API, selectDirectRecords } = await import(pathToFileURL(resolve(sdk, "src/index.js")));
const api = new API();
const fetchHTTP = globalThis.fetch;
const calls = [];
globalThis.fetch = async (url, options) => {
  const { pathname, searchParams, port } = new URL(url);
  calls.push([options.method, pathname, port, searchParams.get("location")]);
  return fetchHTTP(url, options);
};
const read = (port, location) => api.getDirectRecords("http",
  { ip: () => "127.0.0.1", port: () => port }, collection, location, 4);

const rows = (await Promise.all([read(aa,"aa"),read(zz,"zz")])).flat();
assert.equal(rows.length,6);
const ids = ["duplicate", "outside-in", "missing-column", "zero", "absent"];
const columns = ["id", 1, "location"];
const selected = selectDirectRecords(rows, { ids, where:{metric:0,in:[0,2,5]}, columns });
assert.deepEqual(selected, [["duplicate",123,"aa"],["zero",0,"aa"],["duplicate",null,"zz"]]);
assert.deepEqual(selectDirectRecords(rows, {ids, where:{metric:0,in:[2]}, columns:["id"]}), [["duplicate"]]);
assert.deepEqual(selectDirectRecords(rows, {ids:["duplicate"], columns:["location",0]}), [["aa",2],["zz",5]]);
assert.deepEqual(selectDirectRecords(rows, {ids:["absent"], columns:["id"]}), []);
assert.deepEqual(calls, [["GET","/records/direct",aa,"aa"],["GET","/records/direct",zz,"zz"]]);
console.log(JSON.stringify({rpc:calls,read:rows.length,selected,localSelections:4}));
