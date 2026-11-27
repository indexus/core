# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
#
# Indexus quickstart: one image with the issuer, local vnodes, the sample
# collections, the dashboard gateway and the built Explore app.
#
# Built from four sibling checkouts, passed as named contexts (see
# docker-compose.yml): core, sdk, dashboard, experiences. The quickstart
# folder itself is the main context (entrypoint, forwarder, sample/).
#
#   docker compose build        (uses docker-compose.yml)
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     --build-context core=.. --build-context sdk=../../sdk-js \
#     --build-context dashboard=../../dashboard --build-context experiences=../../experiences \
#     -t ghcr.io/indexus/quickstart:dev .
#
# Built and started on Linux arm64 through Docker Desktop on 17 Nov 2026.
# amd64 is also exercised through Docker Desktop emulation; see README.md.

ARG GO_IMAGE=golang:1.26-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0
ARG SAMPLE_IMAGE=debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
ARG NODE_IMAGE=node:22-bookworm-slim@sha256:e9f565d32859f0cbfb52125eafe8be39390f24dac302dc3cdf566e8ba3ab21ee

# ---------------------------------------------------------------- Go binaries
# Cross-compiled on the build machine: core has no dependency and no cgo.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS go-build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=quickstart
ARG COMMIT=none
WORKDIR /src/core
# Only the Go packages the binaries need: core/ops holds GBs of local state.
COPY --from=core go.mod ./
COPY --from=core cmd ./cmd
COPY --from=core indexus ./indexus
COPY --from=core internal ./internal
COPY --from=core tools ./tools
RUN --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    for target in cmd/node cmd/issuer cmd/provisioner tools/localdata; do \
      CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
        go build -trimpath -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT" \
        -o /out/$(if [ "$target" = cmd/node ]; then echo indexus-node; else basename "$target"; fi) ./$target; \
    done

# ------------------------------------------------- SDK, gateway, Explore build
# Pure JavaScript output: built once on the build machine for every platform.
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS web-build
ENV CI=1 COREPACK_ENABLE_DOWNLOAD_PROMPT=0 npm_config_fund=false npm_config_audit=false
# Same layout as at runtime: pnpm and npm keep relative links to ../sdk.
WORKDIR /opt/indexus

# sdk (npm ci runs its prepare script: rollup builds dist/)
RUN ln -s sdk-js sdk
COPY --from=sdk package.json package-lock.json rollup.config.js sdk-js/
COPY --from=sdk src sdk-js/src
RUN cd sdk && npm ci

# experiences: the static build Explore is served from (no data inside)
COPY --from=experiences package.json package-lock.json vite.config.js explore.html experiences/
COPY --from=experiences src experiences/src
COPY --from=experiences public/france.geojson experiences/public/france.geojson
COPY --from=experiences public/basemap-notices.txt experiences/public/basemap-notices.txt
ENV VITE_EXPLORE_ONLY=1
ARG VITE_MAP_STYLE_URL=
ARG VITE_DEFAULT_DATASET=weather
ARG VITE_DEMO_MODE=1
RUN cd experiences && npm ci && VITE_MAP_STYLE_URL="$VITE_MAP_STYLE_URL" VITE_DEFAULT_DATASET="$VITE_DEFAULT_DATASET" VITE_DEMO_MODE="$VITE_DEMO_MODE" npm run build

# Inventory actual main/worker bundle modules and preserve their notice texts.
# Rebuild in memory and require byte equality with dist before recording evidence.
COPY --from=core scripts/local/inspect-browser-notices.mjs ./inspect-browser-notices.mjs
RUN INDEXUS_NOTICE_OUTPUT=/opt/indexus/browser-notices.json node ./inspect-browser-notices.mjs
# Preserve upstream prebundle notices with their source-map provenance.
# Refuse this profile if a mapped source cannot be traced or a notice is missing.
COPY --from=core scripts/local/inspect-maplibre-vendor-notices.mjs ./inspect-maplibre-vendor-notices.mjs
RUN INDEXUS_NOTICE_REQUIRE_TRACED=1 INDEXUS_NOTICE_OUTPUT=/opt/indexus/maplibre-upstream-notices.json node ./inspect-maplibre-vendor-notices.mjs

# dashboard: only the gateway (server.js, server/, lib/) and its runtime packages
COPY --from=dashboard package.json pnpm-lock.yaml pnpm-workspace.yaml server.js dashboard/
COPY --from=dashboard server dashboard/server
COPY --from=dashboard lib dashboard/lib
RUN cd dashboard && corepack enable && pnpm install --prod --frozen-lockfile
# The gateway serves dashboard/dist when NODE_ENV=production: here, Explore.
RUN cp -R experiences/dist dashboard/dist
# Files keep their mode from the checkouts (600 is common): make them readable
# by the runtime user.
RUN chmod -R a+rX /opt/indexus/sdk-js /opt/indexus/dashboard

# Keep the build tree intact for browser evidence; ship SDK production dependencies only.
FROM web-build AS sdk-runtime
RUN cd /opt/indexus/sdk && npm prune --omit=dev --ignore-scripts

# --------------------------------------------------------------- sample data
# The release asset (see build-sample.sh), or the same file dropped in sample/.
FROM --platform=$BUILDPLATFORM ${SAMPLE_IMAGE} AS sample
ARG SAMPLE_FILE=indexus-sample-3d-cross-collection-v2.tar.xz
ARG SAMPLE_URL=https://github.com/indexus/core/releases/download/sample-3d-cross-collection-v2/indexus-sample-3d-cross-collection-v2.tar.xz
ARG SAMPLE_SHA256=
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
    && rm -rf /var/lib/apt/lists/*
RUN --mount=type=bind,source=sample,target=/in \
    set -eu; \
    if [ -f "/in/$SAMPLE_FILE" ]; then cp "/in/$SAMPLE_FILE" /tmp/sample.tar.xz; \
    else curl -fL --retry 3 -o /tmp/sample.tar.xz "$SAMPLE_URL"; fi; \
    : "${SAMPLE_SHA256:?set SAMPLE_SHA256 to the expected sample archive hash}"; \
    echo "$SAMPLE_SHA256  /tmp/sample.tar.xz" | sha256sum -c -; \
    mkdir -p /out && tar -xJf /tmp/sample.tar.xz -C /out --strip-components=1 --no-same-owner; \
    chmod -R a+rX /out; ls /out/*/manifest.json >/dev/null; \
    rm /tmp/sample.tar.xz

# ------------------------------------------------------------------- runtime
FROM ${NODE_IMAGE} AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl tini \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /opt/indexus
COPY --from=go-build /out/ bin/
COPY --from=sdk-runtime /opt/indexus/sdk-js sdk-js
RUN ln -s sdk-js sdk
COPY --from=web-build /opt/indexus/dashboard dashboard
COPY --chmod=644 --from=web-build /opt/indexus/browser-notices.json notices/browser/inventory.json
COPY --chmod=644 --from=web-build /opt/indexus/maplibre-upstream-notices.json notices/browser/maplibre-upstream.json
COPY --from=sample /out sample
# Preserve the licences of the source checkouts actually built. These copies
# do not change their licences or replace third-party/data notice review.
COPY --chmod=644 --from=sdk LICENSE* sdk-js/
COPY --chmod=644 --from=dashboard LICENSE dashboard/LICENSE
COPY --chmod=644 --from=core LICENSE notices/core/LICENSE
COPY --chmod=644 --from=experiences LICENSE notices/experiences/LICENSE
COPY --chmod=644 --from=go-build /usr/local/go/LICENSE notices/go/LICENSE
COPY --chmod=755 entrypoint.sh ./
COPY --chmod=644 forward.mjs vnodes.mjs ./
COPY --chmod=755 hooks/ hooks/
RUN chmod -R a+rX notices && chmod 755 bin/* && mkdir -p /data && chown node:node /data
USER node
VOLUME /data
# 5174: Explore and its /api (the only port published). Inside the container,
# on loopback only: issuer 22030, node p2p 21031, monitoring 19031, gateway 3847.
EXPOSE 5174
HEALTHCHECK --interval=10s --timeout=5s --start-period=120s --retries=3 \
  CMD test -f /data/ready && curl -fsS http://127.0.0.1:3847/api/health >/dev/null && curl -fsS http://127.0.0.1:19031/health >/dev/null
ENTRYPOINT ["/usr/bin/tini", "--", "/opt/indexus/entrypoint.sh"]
