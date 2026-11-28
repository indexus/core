import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
const [sdk,collection,first,second] = process.argv.slice(2);
const { API, documentFilter: f } = await import(pathToFileURL(resolve(sdk,"src/index.js")));
const api=new API();
const requests=[];
const fetchPeer=globalThis.fetch;
globalThis.fetch=async (input,init)=>{
  const url=new URL(input);
  assert.equal(url.pathname,"/records/direct");
  assert.equal(url.searchParams.get("payloads"),"true");
  requests.push([url.port,url.searchParams.get("location")]);
  return fetchPeer(input,init);
};
const rows=[];
for (const [port,zone] of [[first,"aa"],[second,"zz"]]) {
  const peer={ip:()=>"127.0.0.1",port:()=>Number(port)};
  rows.push(...await api.getDirectRecords("http",peer,collection,zone,3,{includePayloads:true,timeoutMs:2000}));
}
const tags=r=>r.payload.tags, values=r=>r.payload.values;
const contains=f.contains(tags,"red"), any=f.containsAny(tags,["red","blue"]);
for (const [predicate,expected] of [
  [contains,"a"],[f.not(contains),"bcdef"],[any,"ad"],[f.not(any),"bcef"],
  [f.anyLt(values,0),"a"],[f.anyLte(values,0),"ab"],[f.anyGt(values,1),"ad"],
  [f.anyGte(values,0),"abd"],[f.and(any,f.anyLt(values,0)),"a"],
]) assert.equal(rows.filter(predicate).map(r=>r.id).sort().join(""),expected);
assert.deepEqual(requests,[[first,"aa"],[second,"zz"]]);
console.log(JSON.stringify({rows:rows.length,oracleCases:9,requests}));
