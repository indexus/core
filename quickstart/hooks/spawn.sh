#!/bin/sh
exec curl -fsS --max-time 130 --unix-socket "${INDEXUS_SLOT_SOCKET:-/tmp/indexus-vnodes.sock}" \
  -X POST -G --data-urlencode "near=${INDEXUS_PREFER_NEAR:?}" http://localhost/spawn
