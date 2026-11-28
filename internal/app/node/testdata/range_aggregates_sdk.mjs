import assert from "node:assert/strict";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

const [sdk, collection, first, second] = process.argv.slice(2);
const { API, numericFacet, numericStats, topByAttribute, topByAttributes } = await import(
  pathToFileURL(resolve(sdk, "src/index.js"))
);
const api = new API();
const owners = [
  [first, "aa"],
  [second, "zz"],
];
const httpFetch = globalThis.fetch;
const calls = [];
async function assertCold() {
  for (const [port, location] of owners) {
    const url = new URL("http://127.0.0.1:" + port + "/set/cached");
    url.search = new URLSearchParams({ collection, location, max_age: "1m" });
    const response = await httpFetch(url);
    assert.equal(response.status, 204, await response.text());
  }
}
globalThis.fetch = async (url, init) => {
  const req = new URL(url);
  calls.push([req.pathname, req.port, req.searchParams.get("location")]);
  return httpFetch(url, init);
};
await assertCold();
const records = (
  await Promise.all(
    owners.map(([port, location]) =>
      api.getDirectRecords(
        "http",
        { ip: () => "127.0.0.1", port: () => port },
        collection,
        location,
        5,
      ),
    ),
  )
).flat();
assert.equal(records.length, 9);
const measure = (r) => r.metrics[1];
function query(lo, hi, inclusiveLo, inclusiveHi) {
  const selected = records.filter((r) => {
    const key = r.metrics[0];
    return (
      Number.isFinite(key) &&
      (inclusiveLo ? key >= lo : key > lo) &&
      (inclusiveHi ? key <= hi : key < hi)
    );
  });
  const stats = numericStats(selected, measure);
  const { matched, missing, buckets } = numericFacet(selected, measure);
  assert.equal(stats.matched, matched);
  assert.equal(stats.missing, missing);
  const top = topByAttribute(selected, measure, { limit: 3, order: "desc" }).map(
    (r) => r.id + "@" + r.location,
  );
  return [
    matched,
    stats.present,
    stats.sum,
    stats.avg,
    stats.min,
    stats.max,
    missing,
    buckets.map((b) => [b.value, b.count]),
    top,
  ];
}
// Independent expected results from the Go fixture, including unmatched,
// all-missing, tied numeric sort keys and tied IDs from different locations.
const a = "interior@aa",
  z = "interior@zz",
  lower = "lower@aa",
  upper = "upper@zz";
const cases = [
  [
    [5, 10, true, true],
    [
      6,
      4,
      9,
      2.25,
      -1,
      4,
      2,
      [
        [-1, 1],
        [2, 1],
        [4, 2],
      ],
      [a, z, lower],
    ],
  ],
  [
    [5, 10, false, true],
    [
      5,
      3,
      7,
      7 / 3,
      -1,
      4,
      2,
      [
        [-1, 1],
        [4, 2],
      ],
      [a, z, upper],
    ],
  ],
  [
    [5, 10, true, false],
    [
      4,
      3,
      10,
      10 / 3,
      2,
      4,
      1,
      [
        [2, 1],
        [4, 2],
      ],
      [a, z, lower],
    ],
  ],
  [
    [5, 10, false, false],
    [3, 2, 8, 4, 4, 4, 1, [[4, 2]], [a, z, "null-measure@aa"]],
  ],
  [
    [11, 11, true, true],
    [1, 0, 0, null, null, null, 1, [], ["all-null@zz"]],
  ],
  [
    [11, 11, false, true],
    [0, 0, 0, null, null, null, 0, [], []],
  ],
];
for (const [args, want] of cases) assert.deepEqual(query(...args), want);
// The upper rows share the first key: the present second key precedes
// "aa-upper-null" even though a comparison on document IDs would reverse them.
// Both interior rows tie on both keys and ID, leaving location to break the tie.
const multi = topByAttributes(
  records.filter((r) => r.metrics[0] >= 5 && r.metrics[0] <= 10),
  [
    { select: (r) => r.metrics[0], order: "desc" },
    { select: measure, order: "asc" },
  ],
  { limit: 5 },
).map((r) => r.id + "@" + r.location);
assert.deepEqual(multi, [upper, "aa-upper-null@zz", "null-measure@aa", a, z]);
assert.deepEqual(
  calls.slice().sort(),
  owners.map(([port, zone]) => ["/records/direct", port, zone]).sort(),
);
await assertCold();
console.log(
  JSON.stringify({
    cases: cases.length,
    multi,
    docs: records.length,
    directRPC: calls.length,
    coldCacheChecks: 4,
  }),
);
