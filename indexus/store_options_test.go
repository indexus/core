package indexus

import "github.com/indexus/go-indexus-core/internal/storage"

// testStore skips fsync: a test's restarts close and reopen the store in one
// process, so the OS cache already holds what was written. On macOS each
// fsync is a full flush (10-20 ms), and it made the mesh suite three times
// slower. Crash durability is tested in storage and zonelog.
var testStore = storage.Options{NoSync: true}
