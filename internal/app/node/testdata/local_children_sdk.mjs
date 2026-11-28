import assert from "node:assert/strict";
import {resolve} from "node:path";
import {pathToFileURL} from "node:url";

const [sdk, collection, first, second] = process.argv.slice(2);
const {API} = await import(pathToFileURL(resolve(sdk,"src/index.js")));
const api = new API();
const requests = [];
const fetchPeer = globalThis.fetch;
globalThis.fetch = async (input, init) => {
  const url = new URL(input);
  assert.equal(url.pathname, "/zones/children/local");
  assert.equal(url.searchParams.get("collection"), collection);
  assert.equal(init.redirect, "error");
  requests.push([url.port,url.searchParams.get("location")]);
  return fetchPeer(input,init);
};
const observations = [];
for (const [port,parent,expectedChild] of [
  [first,"@","a"],[second,"@","z"],[first,"a","aa"],[second,"z","zz"],[first,"aa",null],
]) {
  const peer = {ip:()=>"127.0.0.1",port:()=>Number(port)};
  const result = await api.getLocalChildren("http",peer,collection,parent,{timeoutMs:2000});
  assert.equal(result.complete,false);
  if (expectedChild === null) assert.deepEqual(result.children,[]);
  else assert.ok(result.children.includes(expectedChild), JSON.stringify({parent,expectedChild,result}));
  observations.push(result);
}
assert.deepEqual(requests,[[first,"@"],[second,"@"],[first,"a"],[second,"z"],[first,"aa"]]);
console.log(JSON.stringify({requests,observations}));
