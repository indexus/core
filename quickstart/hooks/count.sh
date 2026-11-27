#!/bin/sh
exec curl -fsS --unix-socket "${INDEXUS_SLOT_SOCKET:-/tmp/indexus-vnodes.sock}" http://localhost/count
