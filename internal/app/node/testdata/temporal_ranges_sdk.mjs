import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, collection, p, q] = process.argv.slice(2);
const { API, documentFilter:f, selectDirectRecords } = await import(pathToFileURL(resolve(sdk,"src/index.js")));
const api = new API(), calls = [];
const fetchHTTP = globalThis.fetch;
globalThis.fetch = async (input, init) => {
  const url = new URL(input);
  assert.equal(url.pathname,"/records/direct");
  assert.equal(url.searchParams.get("payloads"),"true");
  assert.equal(url.searchParams.get("collection"),collection);
  assert.equal(init.redirect,"error");
  const response=await fetchHTTP(input,init);
  calls.push([url.port,url.searchParams.get("location"),response.status]);
  return response;
};
const docs = [];
for (const [port, location] of [[p,"aa"],[q,"zz"]]) {
  const peer = {ip:()=>"127.0.0.1",port:()=>Number(port)};
  const rows = await api.getDirectRecords("http",peer,collection,location,3,{includePayloads:true});
  docs.push(...selectDirectRecords(rows,{columns:["id",{payload:"created"},0]}));
}
const time = f.timeRange(row=>row[1],{gte:"2024-02-29T00:00:00Z",lt:"2024-03-01T00:00:00Z"});
const height = f.numberRange(row=>row[2],{gt:4,lte:6});
assert.deepEqual(docs.filter(time).map(row=>row[0]).sort(),["inside","start"]);
assert.deepEqual(docs.filter(f.and(time,height)).map(row=>row[0]),["start"]);
assert.equal(docs.length,5);
assert.deepEqual(calls,[[p,"aa",200],[q,"zz",200]]);
console.log(JSON.stringify({calls, documents:docs.length, utcDay:["inside","start"], afterHeight:["start"]}));
