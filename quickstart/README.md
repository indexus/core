# Get started with Indexus and DPIP

Run the Anglet guide against a local peer of the Distributed Prefix Index Protocol (DPIP).
Open the local root URL; no scene selector or URL parameter is needed. It begins with archived
weather across France. The first zoom reveals the complete place.
The next click returns to terrain and photo, then adds buildings and vegetation. Weather returns over the 3D scene;
historical sales replace it, before the final step reveals real storage cells
and their node addresses. Each source remains a separate collection.

The bundled data archive is **114,424,940 bytes** (114.42 MB compressed). It has
14 collections: six for the 1 km wide 3D scene, seven for weather, and
19 geocoded DVF 2020 house sales cropped to its 500 m radius.

This is a local preview. The proposed public image and sample release are not
published yet; a new user cannot pull them from GHCR/GitHub at this stage.

## Build the preview

Building needs Docker Desktop/Compose/BuildKit and matching `core`, `sdk-js`,
`dashboard` and `experiences` preview checkouts. The SDK must include the nearby
root/numeric fixes and GatewaySession's terminal-record option. Running an
already built image needs Docker and a recent Chrome or Edge with WebGPU.

The maintainers' sample tools reuse prepared source exports without downloading
raw LiDAR: first [`build-small-3d.py`](../scripts/local/build-small-3d.py), then
[`build-weather-scene.py`](../scripts/local/build-weather-scene.py). The second
adds temperature across metropolitan France at reduced resolution, preserving
native temperature detail in cells intersecting the 30 km Anglet window.
Outside that window, additive temperature metrics are grouped into real GPS
zones (12 bits per axis, roughly 5–10 km), without duplicating their descendants.
The existing coarse weather grids and 520 town series are retained.
It verifies all batches with `prep verify` before archiving.

Put `indexus-sample-3d-cross-collection-v2.tar.xz` in `quickstart/sample/`. Preserve the
matching SHA256. Override `INDEXUS_SAMPLE_SHA256` when rebuilding an archive;
Compose's default identifies the sample checked on 17 November 2026.

From `core/quickstart`:

```sh
docker compose up --build
```

Clone [indexus/sdk](https://github.com/indexus/sdk) into the sibling directory `sdk-js`. An existing checkout named `sdk` can be selected with `INDEXUS_SDK=../../sdk`.
Open [the scene](http://localhost:5174/). Begin with the weather timeline, then
follow the suggested clicks. Previous goes back; the final button starts again.
The menu button above the compass opens a compact panel, collapsed by default.
Its Weather, 3D place, Sales · 2020 and Storage shortcuts jump to an experience.
You can also close the guide and explore freely.

The panel keeps the individual layer switches, eyes, grid controls and Nearby.
Disabling Weather hides its timeline, clock, date and city temperatures;
re-enabling it restores them at the selected hour. The eye beside each layer
toggles actual loaded cells, using retained caches only. Grids update as cells
load or leave the cache; native 3D vegetation cells render as cubes.
The **Grid density** slider controls how many real loaded cells are drawn,
from 16 to 320. The default follows the viewport and adds a little more detail
than the previous full-level cutoff. Parents are added before nearby finer
cells, so density increases progressively instead of jumping a whole level.
The optional **Depth limit** control stays folded and caps the cell depth
independently. The overlay never fabricates an unread cell or parent.
The **Weather** eye inspects nonempty read zones of the temperature cube
(`TempeAROME0128v2`), referenced by the current view's drawing plan. It does
not show the regional raster used to render clouds and rain. At coarser depths
it chooses an already loaded ancestor; an unread level retains the nearest
loaded parent. Unrelated cached zones and empty read responses are excluded.
The counter shows how many zones are included in the view. Native tile layers
without retained cells at a level stay empty. Moving the slider starts no reads. Nearby starts at **0 buildings**. **Search** loads the first five records;
the button then becomes **Next** to continue the same walk. **↻** restarts it. The panel shows only the building count; results stay on the map.
Numbered markers and labelled concentric rings show their order and distance.
Earlier roofs are blue, the latest page orange, and a hovered roof yellow.
Hover a returned building to see its indexed distance above the roof. Picking
uses projected roof bounds, not exact mesh triangles. Optional **ⓘ** details
combine its LiDAR/terrain heights, footprint and regional weather at the chosen
hour. These are approximate geometry and regional forecast values; no cadastral
area or building weather sensor is implied. Distances use indexed locations
from the scene centre, not entrances or distinct houses.

The timeline uses an **AROME archive beginning 28 January 2025**, not today's
weather. Temperature covers metropolitan France, with native detail around
Anglet and reduced detail elsewhere. Zoom out to see the national field and
city temperatures. The coarse atmospheric grids and town series come from the
same prepared France series.
The scene is a static 3D sample, not a record of buildings changing over time.
The **Sales · 2020** switch adds local sale labels over associated roofs in
the 3D aerial view. Turning off buildings or aerial imagery hides these labels.
There is no national DVF view in this demo; metropolitan weather remains
available when zooming out. A sale outside the circular 3D view or without
an unambiguous roof match gets no roof label. Cadastral parcels and news are
not bundled in this profile.

`docker compose down` stops the app and keeps the data volume. Set
`INDEXUS_PORT` if port 5174 is occupied. The published host port uses loopback.
The final image retains the SDK source exports and production dependencies;
its Rollup build tools stay in the build stage. Geometry, photo and weather are bundled. The default vector basemap uses a
local adapted Dark Matter style, with light/dark colours changed in place;
no MapTiler account or API key is needed. Tiles, fonts and sprites come from
OpenFreeMap and still require Internet. Credits and the preserved style notices
are served at `/basemap-notices.txt`. `VITE_MAP_STYLE_URL` remains an optional
build-time override for another compatible style.

## Inspect the local P2P mesh

A fresh volume starts with one Go peer (a vnode), one issuer and one read-only
gateway. To let the node delegate zones as demand grows, run from this
directory:

```sh
INDEXUS_MAX_VNODES=4 docker compose -p dpip-anglet up -d --build
docker compose -p dpip-anglet logs -f indexus
```

The project name selects a dedicated persistent volume. Reuse that name on
every run. For repeatable settings, put `INDEXUS_MAX_VNODES=4` in
`quickstart/.env`, and set `INDEXUS_PORT` there to change the host port.
Stop with `docker compose -p dpip-anglet stop`; resume the same container with
`docker compose -p dpip-anglet start`. Recreating the container restores every
saved vnode from its own store, including those in an older three-node volume.
Lowering the limit controls future starts. Existing disks retain their owners.

The node observes pressure and asks the local provisioner to start a peer.
By default a vnode targets 50 writes/s and 1,000,000 owned records. Adjust
`INDEXUS_WRITES_PER_VNODE` and `INDEXUS_ITEMS_PER_VNODE` to change the load
signals. These settings guide splits; a burst can exceed them. As the
receiver installs zones the sender can forward writes to it. Every vnode
here shares the host's CPU, RAM and disk. Allow approximately 512 MiB of Go
heap per active vnode plus memory for the kernel, gateway and file cache.
Choose a cap that fits the host. Idle peers stay available across restarts.

To add points while the mesh serves reads, prepare NDJSON records with the
target collection's location encoding, metrics and payload schema. For the
Anglet layers see [data provenance](DATA-PROVENANCE.md) and the [preparation
pipeline](../docs/ingest/pipeline.md). Import through the private peer port
from inside the running container:

```sh
docker compose -p dpip-anglet exec -T indexus /opt/indexus/bin/localdata \
  -records /dev/stdin -write http://127.0.0.1:21031 \
  -issuer http://127.0.0.1:22030 < prepared-records.ndjson
curl -fsS http://127.0.0.1:5174/api/demo/mesh
```

The writer acquires a scoped token and routes each record to its owner.
Repeating an unchanged record reuses its request ID. A new collection
also needs a matching Explore layer to become visible on the map. Only
the read-only gateway is published on the host loopback.

Enable a layer's eye to colour its actual loaded cells by storage node:
**N1** blue, **N2** orange, **N3** purple. The compact node legend appears only while a collection grid is enabled and
shows full Base64 addresses. The Storage guide step keeps the same legend
inside its explanation; in free exploration it stays in the Layers panel. The Layers panel keeps the switches, grid depth and Nearby
controls. The density slider adds or removes cells; Depth limit is optional.
Enabling an eye labels at most two real loaded cells with their full Base64
routing addresses; cube labels follow the scene camera. The collection and
zone location determine the address through the SDK zone-key function. A bounded
sample and screen-space spacing keep the annotations readable.
Grey means several nodes or unknown
hosting; a parent cell can span several nodes. The labels and colours come
from the nodes' stored-zone inventories, cached for up to one minute, through
read-only `/api/demo/mesh`. They do not represent server locations in the city,
private-query guarantees or a global consistency snapshot. Unavailable nodes
are marked explicitly. Inspecting hosting creates no fabricated data cells.

Qualification on Linux/ARM64 used a separate Docker volume with 600 records
and a 200-record split threshold. The new peer received 283 records. One
write sent to the bootstrap returned `201` with the receiver's address in
`X-Indexus-Owner`. The receiver counted the application. Both peers returned
the record before and after restarting the container. The full Anglet volume
was tested separately with its normal thresholds.

## Pinned build inputs

The default Go, Node and Debian base images and the Dockerfile frontend use
multi-platform SHA256 index references; see [BASE-IMAGES.json](BASE-IMAGES.json).
They retain the exact bases used for the validated local images, with linux/arm64
and linux/amd64 descriptors checked in the registry. Updating a pin is an
explicit maintenance step followed by build/runtime and notice checks.

These defaults do not freeze apt repositories, named source checkouts, external
map resources or a future published image. Dependency lockfiles and the sample
checksum remain necessary. Build-argument overrides can select other bases.

## Validation and release gates

The current P2P profile uses 14 prepared collections with 308,551 records.
It enlarges the scene from 300 m to 1 km so that 19 local DVF sales are
available. This increases the archive from 26 MB to 114 MB; it includes the
expanded geometry and imagery, not just extra sale labels.
The current P2P profile is verified against 14 prepared collections. Source
batches pass `prep verify`; application and Go tests cover the integration.
Runtime checks and exact image/source revisions live in the launch journal.
The preceding P2P/DVF profile (app 9d44f3c) and unified layer/grid controls were
built and exercised on arm64 and on amd64 through Docker Desktop emulation on an arm64 Mac.
The amd64 check used a fresh volume and a restart: all 14 collection counts,
three peers, 19 sales, 20 nearby results matched to loaded roofs, and demo write
restrictions passed. Browser assets and packaged notices match across images.
This does not certify native amd64 hardware or browser GPU rendering on amd64.
The arm64-served scene was inspected in a browser: vegetation cubes, node
addresses and density changes from 16 to 320 cells rendered; a capture is kept
in the launch evidence. This is not a frame-rate benchmark.
The guided progression (app 80a1f6a) has 391 passing application tests and
arm64-served browser walkthrough evidence. Its matching arm64 and amd64
review packages were started on fresh volumes and checked again after restart:
14 collection counts, three peers, 19 sales, 20 nearby records matched to roofs
and demo write restrictions. The 28 served browser assets and packaged browser
notices match across architectures. The amd64 checks used Docker emulation,
not native hardware or an amd64 browser GPU.
A local boot time is not a download-to-map result.

Source/Go licence files are preserved in the image; see [NOTICES.md](NOTICES.md).
Data provenance is recorded separately in [DATA-PROVENANCE.md](DATA-PROVENANCE.md),
including the Open-Meteo distribution of the archived AROME inputs.
Before public distribution: finish data/software notices and provenance, check
a clean machine (including native amd64), publish approved artifacts, then have three people
outside the project start it unaided in under five minutes. No third-party gate
has been passed. The macOS alternative script is a separate, untested preview.

Indexus reads eventually updated summaries. There is no global snapshot or
automatic recovery from the loss of the single serving disk in this demo.


## Prepare a local review bundle

Maintainers can export an already built image without uploading it:

```sh
./export-review-bundle.sh indexus/quickstart:scene-timeline /path/to/new-bundle
```

The new directory contains a compressed Docker image, checksum manifest,
platform/identity files, Compose configuration, and a self-contained
`./start.sh`. A reviewer needs Docker/Compose, Bash and a SHA256 checker,
without source checkouts, Go or Node. The starter verifies checksums, checks
the Docker architecture, imports the exact image when needed and starts it
without pulling. It refuses an architecture mismatch unless emulation is
explicitly enabled. Existing output directories are preserved.

This prepares a versioned local snapshot; it does not upload or authorize
external distribution. The basemap still needs Internet. Independent first-run
and browser checks, source/data provenance and release approval remain open.
