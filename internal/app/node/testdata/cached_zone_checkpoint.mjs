import assert from "node:assert/strict";
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, checkpoint, phase, collection, firstPort, secondPort] = process.argv.slice(2);
const { Network, API } = await import(pathToFileURL(resolve(sdk, "src/index.js")));
const ports = { aa: firstPort, zz: secondPort };
const requests = [];
const transport = globalThis.fetch;
globalThis.fetch = async (...args) => {
  const url = new URL(args[0]);
  const response = await transport(...args);
  requests.push({ path: url.pathname, location: url.searchParams.get("location"), port: url.port, status: response.status });
  return response;
};

const progress = phase === "cold" ? { pending: ["aa", "zz"], results: [] }
  : JSON.parse(readFileSync(checkpoint, "utf8"));
const hosts = phase === "cold" ? Object.values(ports).map((port) => `127.0.0.1|${port}`) : [];
const network = new Network("http", new API(), hosts, 2, 16, {
  meshDiscovery: false, seedPeers: progress.peers,
});
await network.whenReady();
assert.equal(network.listPeers().length, 2);

const read = (prefix) => network.getCachedZone(collection, prefix, { maxAgeMs: 60_000 });
if (phase === "cold") {
  for (const prefix of progress.pending) assert.equal(await read(prefix), null);
  progress.peers = network.listPeers().map(({ hash: name, ip, port }) => ({ name, ip, port }));
} else {
  const prefix = progress.pending.shift();
  assert.equal(prefix, phase === "first" ? "aa" : "zz");
  const observation = await read(prefix);
  assert.ok(observation && Date.parse(observation.observedAt));
  assert.equal(observation.set[prefix]?.count, 1);
  progress.results.push({ prefix, metric: observation.set[prefix].metrics[0] });
  if (phase === "resume") {
    assert.deepEqual(progress.pending, []);
    assert.deepEqual(progress.results, [{ prefix: "aa", metric: 1 }, { prefix: "zz", metric: 2 }]);
  }
}
const probes = requests.filter(({ path }) => path !== "/ping");
const expected = phase === "cold" ? ["aa","zz"] : [phase === "first" ? "aa" : "zz"];
assert.deepEqual(probes, expected.map((location) => ({
  path: "/set/cached", location, port: ports[location], status: phase === "cold" ? 204 : 200,
})));
writeFileSync(checkpoint, JSON.stringify(progress));
console.log(JSON.stringify({ probes, pending: progress.pending, results: progress.results }));
