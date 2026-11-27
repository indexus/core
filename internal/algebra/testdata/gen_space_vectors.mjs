// Generates encode/decode vectors with the real SDK (sdk-js/src/tree), so that
// the Go reading of the collection description standard can be checked
// against the client's behaviour bit for bit.
// Run from core/: node internal/algebra/testdata/gen_space_vectors.mjs > internal/algebra/testdata/space_vectors.json
import { Collection, Space } from "../../../../sdk-js/src/tree/index.js";

let seed = 12345;
const rand = () => {
  seed = (seed * 1103515245 + 12345) % 2147483648;
  return seed / 2147483648;
};

const cases = [
  { name: "DvFMV2020idx0001", dimensions: [{ name: "gps", type: "spherical", args: [-90, 90, -180, 180] }] },
  { name: "TempeAROME0128v2", dimensions: [{ name: "gps", type: "spherical", args: [-90, 90, -180, 180] }] },
  { name: "DrawingCanvas001", dimensions: ["x", "y"].map((n) => ({ name: n, type: "linear", args: [0, 4096] })) },
  {
    name: "VegetationZone01",
    dimensions: ["x", "y", "z"].map((n, i) => ({ name: n, type: "linear", args: [[331000, 6276000, -50][i], [331000, 6276000, -50][i] + 2048] })),
  },
  { name: "ChronicleEvents1", dimensions: [{ name: "time", type: "linear", args: [0, 4294967296] }] },
  {
    name: "GpsAndTime000001",
    dimensions: [
      { name: "gps", type: "spherical", args: [-90, 90, -180, 180] },
      { name: "time", type: "linear", args: [1700000000, 1900000000] },
    ],
  },
];

const out = [];
for (const c of cases) {
  const col = new Collection(c.name, c.dimensions);
  const space = new Space(col.dimensions(), col.mask(), col.offset());
  const root = space._root();
  const axes = root.length / 2;
  const points = [];
  for (let k = 0; k < 40; k++) {
    const p = [];
    for (let a = 0; a < axes; a++) p.push(root[2 * a] + rand() * (root[2 * a + 1] - root[2 * a]));
    points.push(p);
  }
  // Edges: every axis at its min, its max, its midpoint.
  points.push(Array.from({ length: axes }, (_, a) => root[2 * a]));
  points.push(Array.from({ length: axes }, (_, a) => root[2 * a + 1]));
  points.push(Array.from({ length: axes }, (_, a) => (root[2 * a] + root[2 * a + 1]) / 2));
  const vectors = [];
  for (const p of points) {
    for (const depth of [1, 3, 8, 16]) {
      const location = space._encode(p, depth);
      vectors.push({ coords: p, depth, location, bounds: space._decode(location) });
    }
  }
  out.push({ name: c.name, dimensions: c.dimensions, root, mask: col.mask().value, vectors });
}
process.stdout.write(JSON.stringify(out));
