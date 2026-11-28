import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, collection, firstPort, secondPort] = process.argv.slice(2);
const { API, buildBM25Index, exactKnn, exactSparseDot, numericHistogram, mergeNumericHistograms, histogramQuantile, fuseRankings } = await import(pathToFileURL(resolve(sdk, "src/index.js")));
const api = new API();
const fetchPeer = globalThis.fetch;
const calls = [];
globalThis.fetch = async (input, init) => {
  const url = new URL(input);
  assert.equal(url.pathname, "/records/direct");
  assert.equal(url.searchParams.get("collection"), collection);
  assert.equal(url.searchParams.get("payloads"), "true");
  assert.equal(url.searchParams.get("limit"), "2");
  assert.equal(init.redirect, "error");
  const response = await fetchPeer(input, init);
  calls.push([url.port, url.searchParams.get("location"), response.status]);
  return response;
};

const docs = [];
for (const [port, zone] of [[firstPort, "aa"], [secondPort, "zz"]]) {
  const peer = { ip: () => "127.0.0.1", port: () => Number(port) };
  const rows = await api.getDirectRecords("http", peer, collection, zone, 2, { includePayloads: true, timeoutMs: 2000 });
  for (const { id, location, metrics, payload } of rows) {
    docs.push({ id, location, vector: metrics, title: payload.title, sparse: payload.sparse });
  }
}
assert.equal(docs.length, 4);
const query = "cafe creme";
const index = buildBM25Index(docs, { title: 1 }, { analyzer: "latin-folded-words-v1" });
const phrase = [...index.filterPhrase(docs, "title", query)];
assert.deepEqual(phrase.map(row => row.id).sort(), ["a", "c"]);
const prefix = [...index.filterTokenPrefix(docs, "title", "CRÈ")];
assert.deepEqual(prefix.map(row => row.id).sort(), ["a", "b", "c"]);
const sparse = exactSparseDot(docs.map(({id,location,sparse}) => ({id,location,vector:sparse})), [[8,2],[1024,1]]);
assert.deepEqual(sparse.map(r => [r.id,r.score]), [["a",7],["d",1],["c",0],["b",-2]]);
// The sorted distances in the fixture are 0,1,2,3. Bounds 1 and 3
// place the two median rows in the same half-open interval.
const histograms = ["aa","zz"].map(zone =>
  numericHistogram(docs.filter(r => r.location === zone), r => r.vector[0], [1,3]));
const histogram = mergeNumericHistograms(histograms);
assert.deepEqual(histogram.counts, [1,2,1]);
assert.deepEqual(histogramQuantile(histogram,0.5), {
  rank:2, firstRank:2, lastRank:3, lowerInclusive:1, upperExclusive:3,
});
const lexical = index.search(query);
const vector = exactKnn(docs, [0, 0]);
assert.deepEqual(vector.map(hit => [hit.id, hit.distance_squared]), [["b",0],["c",1],["d",4],["a",9]]);
const ids = hits => hits.map(hit => hit.id);
assert.deepEqual(ids(lexical), ["a","b","c"]);
// Title lengths 3, 2, 2, 1. Each term occurs in three documents.
const idf = Math.log(1 + 1.5 / 3.5);
const standard = 2 * idf;
const repeated = idf * (4.4 / (2 + 1.65) + 2.2 / (1 + 1.65));
function verifyScores(hits, expected) {
  assert.equal(hits.length, expected.length);
  hits.forEach(({score}, i) => assert.ok(Math.abs(score - expected[i]) < 1e-12));
}
verifyScores(lexical, [repeated, standard, standard]);
// The shared view identifies the four already loaded, immutable fixture rows.
const view = { collection, snapshot: "hybrid-fixture-without-writes" };
const fused = fuseRankings([{ ...view, hits: lexical }, { ...view, hits: vector }], { constant: 1 });
assert.deepEqual(ids(fused), ["b","a","c","d"]);
verifyScores(fused, [1/3+1/2, 1/2+1/5, 1/4+1/3, 1/4]);
assert.deepEqual(calls, [[firstPort, "aa", 200], [secondPort, "zz", 200]]);
console.log(JSON.stringify({ calls, prefix: ids(prefix), sparse: ids(sparse), histogram: histogram.counts, phrase: ids(phrase), lexical: ids(lexical), vector: ids(vector), fused: ids(fused) }));
