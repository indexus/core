import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
const [sdk, collection, ...ports] = process.argv.slice(2);
const { API, exactMaxSim } = await import(pathToFileURL(resolve(sdk,"src/index.js")));
const peers = ports.map(port => ({ip:()=>"127.0.0.1",port:()=>Number(port)}));
const { records, requests } = await new API().readObservedCollection("http", peers, collection, {
  includePayloads:true, maxRequests:60, limitPerLocation:4,
});
assert.equal(records.length,4);
globalThis.fetch = () => { throw new Error("unexpected RPC scoring token vectors"); };
const candidates = records.map(({id,location,payload})=>({id,location,vectors:payload.v}));
const query = [[1,0],[0,1],[1,0]];
// Fixed independent oracle computed from the seeded documents. Each of the
// two 'same' documents scores three via a different assignment of tokens.
const want = [["higher","zz1",7],["same","aa",3],["same","zz",3],["negative","aa0",-3]];
for (const limit of [1,2,4,8]) {
  assert.deepEqual(exactMaxSim(candidates,query,{limit}).map(h=>[h.id,h.location,h.score]),want.slice(0,limit));
}
console.log(JSON.stringify({owners:ports.length,docs:records.length,requests,rankings:4}));
