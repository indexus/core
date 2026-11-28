import assert from "node:assert/strict";
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, collection, p, q, checkpoint, phase] = process.argv.slice(2);
const { API, walkLocalChildren } = await import(pathToFileURL(resolve(sdk,"src/index.js")));
const api = new API();
const calls = [];
const fetchHTTP = globalThis.fetch;
globalThis.fetch = async (input, init) => {
  const url = new URL(input);
  assert.equal(init.method, "GET");
  assert.equal(init.redirect, "error");
  assert.equal(url.searchParams.get("collection"), collection);
  assert.equal(url.hostname,"127.0.0.1");
  assert.ok([p,q].includes(url.port));
  const response = await fetchHTTP(input,init);
  calls.push([url.pathname,url.port === p ? "P" : "Q",url.searchParams.get("location")]);
  assert.equal(response.status,200);
  return response;
};
const pair = (peer, location) => ({peer,location});
const roots = [pair("P","@"),pair("Q","@")];
const initial = {collection,pending:roots};
const oracle = [...roots,pair("P","a"),pair("Q","z"),pair("P","aa"),pair("Q","zz")];
const peer = id => ({ip:()=>"127.0.0.1",port:()=>Number(id === "P" ? p : q)});
function expectedCalls(pairs) {
  return pairs.map(({peer,location})=>["/zones/children/local",peer,location]);
}
async function walk(state, maxRequests) {
  const result = await walkLocalChildren(state,{
    maxRequests,
    observe:(id,c,l)=>api.getLocalChildren("http",peer(id),c,l,{timeoutMs:3000}),
  });
  assert.ifError(result.error);
  assert.equal(result.complete,false);
  assert.ok(result.observations.every(o=>o.complete===false));
  assert.equal(result.observations.length,result.attempts);
  return result;
}
const save = state => writeFileSync(checkpoint, JSON.stringify(state));
const load = () => JSON.parse(readFileSync(checkpoint,"utf8"));
if (phase === "pause") {
  const result = await walk(initial,2);
  assert.deepEqual(result.state.visited,roots);
  assert.deepEqual(result.state.pending,oracle.slice(2,4));
  assert.deepEqual(calls,expectedCalls(roots));
  save(result.state);
} else if (phase === "resume") {
  const result = await walk(load(),10);
  assert.deepEqual(result.state.visited,oracle);
  assert.deepEqual(result.state.pending,[]);
  assert.deepEqual(calls,expectedCalls(oracle.slice(2)));
  save(result.state);
} else if (phase === "revisit") {
  const old = await walk(load(),10);
  assert.deepEqual(old.state.visited,oracle);
  assert.equal(old.attempts,0);
  assert.deepEqual(calls,[]);
  const fresh = await walk(initial,10);
  const expected = [...oracle,pair("P","aa0")];
  assert.deepEqual(fresh.state.visited,expected);
  assert.deepEqual(fresh.state.pending,[]);
  assert.deepEqual(calls,expectedCalls(expected));

  const rows = await api.getDirectRecords("http",peer("P"),collection,"aa0",1);
  assert.deepEqual(rows.map(r=>r.id),["added-after-pass"]);
  assert.deepEqual(calls.at(-1),["/records/direct","P","aa0"]);
} else {
  throw new Error("Unknown fixture phase");
}
console.log(JSON.stringify({phase, calls, complete:false}));
