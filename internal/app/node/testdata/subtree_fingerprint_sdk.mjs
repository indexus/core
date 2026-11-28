import assert from "node:assert/strict";
import {resolve} from "node:path";
import {pathToFileURL} from "node:url";
const [sdk, collection, firstPort, secondPort, control] = process.argv.slice(2);
const {API, mergeSubtreeFingerprints} = await import(pathToFileURL(resolve(sdk, "src/index.js")));
const api = new API();
const peers = [firstPort, secondPort].map(port=>({ip:()=>"127.0.0.1", port:()=>Number(port)}));
const calls = [];
const originalFetch = globalThis.fetch;
globalThis.fetch = async (input, init) => {
  const url = new URL(input);
  const response = await originalFetch(input, init);
  calls.push({path:url.pathname, location:url.searchParams.get("location"), port:url.port, status:response.status});
  return response;
};
const probe = (peer, location) => api.getLocalFingerprint("http", peer, collection, location);
const initial = await Promise.all(peers.map(p=>probe(p,"@")));
assert.ok(initial.every(p=>typeof p.fingerprint==="string" && p.complete===false));
const locations = ["aa0","aa1"];
const verifySum = async () => {
  const [parent, ...children] = await Promise.all(["aa", ...locations].map(l => probe(peers[0], l)));
  assert.deepEqual(parent.monoid, mergeSubtreeFingerprints(children.map(c => c.monoid)));
  return parent.monoid;
};
const initialContent = await verifySum();
assert.equal(initialContent.count, "2");
const read = async location => {
  const rows = await api.getDirectRecords("http",peers[0],collection,location,4,{includePayloads:true});
  return rows.map(row=>row.payload);
};
const refresh = (baseline,load) => api.refreshObservedCut("http", peers[0], collection, locations,
  {baseline, load, concurrency:2, maxRequests:32});
const initialCut = await refresh(undefined,read);
assert.deepEqual(initialCut.loads.map(l=>[l.location,l.value,l.verified]),
  locations.map(location=>[location,["original"],true]));
const stable = await refresh(initialCut.baseline);
assert.equal(stable.requests,1);
if (process.env.INDEXUS_BENCH_PROBES === "1") {
  const durations = [];
  for (let i = 0; i < 110; i++) {
    const started = performance.now();
    const result = await refresh(initialCut.baseline);
    const ms = performance.now() - started;
    assert.equal(result.requests, 1);
    assert.equal(result.unchanged.length, 2);
    if (i >= 10) durations.push(ms);
  }
  durations.sort((a,b)=>a-b);
  console.log(JSON.stringify({stableProbes:100,p50Ms:durations[49],p95Ms:durations[94]}));
}
assert.equal(stable.unchanged.length,2);
const mutate = async action => assert.equal((await originalFetch(control+"/"+action,{method:"POST"})).status,204);
await mutate("edit");
const after = await Promise.all(peers.map(p=>probe(p,"@")));
assert.notEqual(after[0].fingerprint,initial[0].fingerprint);
assert.equal(after[1].fingerprint,initial[1].fingerprint);
const updatedContent = await verifySum();
assert.equal(updatedContent.count, "2");
assert.notEqual(updatedContent.sum, initialContent.sum);
calls.length=0;
const changed = await refresh(initialCut.baseline);
assert.deepEqual(changed.changed,["aa0"]);
assert.deepEqual(changed.unchanged,["aa1"]);
assert.equal(changed.changedFraction,0.5);
assert.ok(calls.every(c=>c.path==="/zones/fingerprint/local"));
const edited = await refresh(changed.baseline,read);
assert.deepEqual(edited.loads,[{location:"aa0",value:["modified"],verified:true}]);
assert.deepEqual(calls.filter(c=>c.path==="/records/direct").map(c=>c.location),["aa0"]);
assert.deepEqual((await refresh(edited.baseline)).unchanged.sort(),locations);
await mutate("delete");
calls.length=0;
const removed = await refresh(edited.baseline,read);
assert.deepEqual(removed.changed,["aa0"]);
assert.deepEqual(removed.loads,[{location:"aa0",value:[],verified:true}]);
assert.deepEqual(calls.filter(c=>c.path==="/records/direct").map(c=>c.location),["aa0"]);
assert.deepEqual(await read("aa0"),[]);
assert.equal((await verifySum()).count, "1");
// The fetched transient value must never become a verified cached value,
// even though the subtree is empty at both ends of the read.
const beforeRace = await probe(peers[0],"aa0");
const race = await api.refreshObservedCut("http",peers[0],collection,["aa0"],{
  maxRequests:16, async load(location) {
    await mutate("transient");
    const values = await read(location);
    await mutate("undo");
    return values;
  }
});
assert.deepEqual(race.loads,[{location:"aa0",value:["brief"],verified:false}]);
assert.deepEqual(race.unknown,["aa0"]);
const afterRace = await probe(peers[0],"aa0");
assert.equal(beforeRace.fingerprint,afterRace.fingerprint);
assert.notEqual(beforeRace.guard,afterRace.guard);
console.log(JSON.stringify({changed:changed.changed, fraction:changed.changedFraction, removed:removed.changed}));
