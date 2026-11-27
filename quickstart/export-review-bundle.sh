#!/usr/bin/env bash
# Prepare a local platform-specific bundle; never upload or send it.
set -euo pipefail
[ "$#" -eq 2 ] || { echo "Usage: $0 IMAGE NEW_OUTPUT_DIRECTORY" >&2; exit 2; }
image=$1
out=$2
image_id="$(docker image inspect --format '{{.Id}}' "$image")"
arch="$(docker image inspect --format '{{.Architecture}}' "$image")"
[ "$(docker image inspect --format '{{.Os}}' "$image")" = linux ]
case "$arch" in arm64|amd64) ;; *) echo "Unsupported architecture: $arch" >&2; exit 1;; esac
[ ! -e "$out" ] || { echo "Choose a new directory; existing files are preserved" >&2; exit 1; }
mkdir -p "$out"
short="${image_id#sha256:}"
short="${short:0:12}"
ref="indexus/quickstart:review-$arch-$short"
docker tag "$image_id" "$ref"
printf '%s\n' "$image_id" > "$out/image-id.txt"
printf '%s\n' "$ref" > "$out/image-ref.txt"
printf '%s\n' "$arch" > "$out/architecture.txt"
docker save "$ref" | gzip -1 > "$out/image.tar.gz"
cat > "$out/compose.yml" <<YAML
name: indexus-review-$arch-$short
services:
  indexus:
    image: $ref
    platform: linux/$arch
    pull_policy: never
    ports:
      - "127.0.0.1:\${INDEXUS_PORT:-5174}:5174"
    environment:
      INDEXUS_PORT: "\${INDEXUS_PORT:-5174}"
      DEMO_MODE: "1"
      INDEXUS_MAX_VNODES: "\${INDEXUS_MAX_VNODES:-1}"
      INDEXUS_WRITES_PER_VNODE: "\${INDEXUS_WRITES_PER_VNODE:-50}"
      INDEXUS_ITEMS_PER_VNODE: "\${INDEXUS_ITEMS_PER_VNODE:-1000000}"
    volumes:
      - data:/data
    stop_grace_period: 20s
volumes:
  data:
YAML
cat > "$out/start.sh" <<'START'
#!/usr/bin/env bash
set -euo pipefail
cd "$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
if command -v sha256sum >/dev/null; then sha256sum -c SHA256SUMS;
elif command -v shasum >/dev/null; then shasum -a 256 -c SHA256SUMS;
else echo "Install a SHA256 checker (sha256sum or shasum)" >&2; exit 1; fi
docker info >/dev/null
docker compose version >/dev/null
arch="$(cat architecture.txt)"
host_arch="$(docker info --format '{{.Architecture}}')"
case "$host_arch" in aarch64) host_arch=arm64;; x86_64) host_arch=amd64;; esac
if [ "$host_arch" != "$arch" ] && [ "${INDEXUS_ALLOW_EMULATION:-0}" != 1 ]; then
  echo "This bundle targets $arch; your Docker engine is $host_arch. Use the matching bundle." >&2
  exit 1
fi
ref="$(cat image-ref.txt)"
expected="$(cat image-id.txt)"
actual="$(docker image inspect --format '{{.Id}}' "$ref" 2>/dev/null || true)"
if [ "$actual" != "$expected" ]; then docker load --input image.tar.gz; fi
[ "$(docker image inspect --format '{{.Id}}' "$ref")" = "$expected" ] || { echo "Image identity mismatch" >&2; exit 1; }
docker compose -f compose.yml up -d
printf '\nOpen http://127.0.0.1:%s/ after initialization.\n' "${INDEXUS_PORT:-5174}"
printf 'Stop with: docker compose -f compose.yml down\n'
START
cat > "$out/README.txt" <<README
Indexus local review bundle — linux/$arch
Prerequisites: running Docker with Compose v2, Bash, a SHA256 checker, and
recent Chrome or Edge with WebGPU. No checkout, Go or Node is needed.

Run: ./start.sh
Then open http://127.0.0.1:5174/ after initialization.
Use INDEXUS_PORT=5188 ./start.sh if that port is occupied.
Stop and preserve data: docker compose -f compose.yml down
Running ./start.sh again resumes the same volume.

Image and sample data are included. The OpenFreeMap basemap requires Internet.
The 1 km Anglet scene has 19 historical DVF 2020 sales and archived weather
from 28–30 January 2025. Roof matches are approximate, not current valuations.
A fresh volume starts with one peer. To let it delegate zones to other
peers on this Docker host as you add data, run:
  INDEXUS_MAX_VNODES=4 ./start.sh
To reuse settings on later starts, place them in a .env beside compose.yml.
The default split signals are 50 writes/sec and 1,000,000 items per peer.
INDEXUS_WRITES_PER_VNODE and INDEXUS_ITEMS_PER_VNODE override them.
Watch the current peers at http://127.0.0.1:5174/api/demo/mesh.
There is no machine-failure tolerance or automatic recovery from disk loss.

This opens the same guided experience as indexus.io, without its landing page.
Weather across France is shown first. Follow the
suggested clicks: the first zoom shows the complete place, then the photo
base and progressive building/vegetation layers. Weather returns over the 3D scene; historical
sales replace it, then real loaded cells reveal the storage nodes.
The menu above the compass starts collapsed. It offers Weather, 3D place,
Sales 2020 and Storage shortcuts. Close the guide to explore freely.

The floating Layers panel contains the individual controls:
- Checkboxes show or hide each collection's data.
- An eye shows that collection's loaded cells, not an invented uniform grid.
- Grid density limits the number of cells drawn (16–320). Depth limit is an
  optional control for inspecting coarser or finer retained cells.
- Cell colours identify the storage nodes listed with their Base64 addresses.
  Only a few cell addresses are labelled on the map to keep it readable.
- Search starts nearby results; Next continues farther from the same centre.
  Results colour the roofs and distance rings show the search progression.
- Weather controls the archived weather display and its timeline. Sales shows
  the local historical DVF sample on approximately associated roofs.

Record time to the first usable map, browser/OS, errors and any confusing
controls. Docker/API checks do not replace an independent browser test.

Image: $image_id
This local package has not been uploaded or sent. Data/software provenance
and public-distribution review remain separate.
README
chmod +x "$out/start.sh"
(
 cd "$out"
 files=(image.tar.gz compose.yml start.sh README.txt image-id.txt image-ref.txt architecture.txt)
 if command -v sha256sum >/dev/null; then sha256sum "${files[@]}" > SHA256SUMS;
 else shasum -a 256 "${files[@]}" > SHA256SUMS; fi
)
printf 'Prepared %s (%s)\n' "$out" "$image_id"
