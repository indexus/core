package storage

import (
	"sync/atomic"

	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// Counters of the work a transfer does, for tests, as controlValueReads and
// logEncodes are: one atomic add on each path.
var (
	reservationInventories atomic.Int64 // session inventories loaded by reservation accounting
	rowPuts                atomic.Int64 // zonelog rows committed with a value
	rowDeletes             atomic.Int64 // zonelog rows committed as tombstones
	rowCommits             atomic.Int64 // zonelog batches committed
	pointReads             atomic.Int64 // point reads of the store by a transaction
	sinceExpands           atomic.Int64 // subtrees opened by ZoneInventory.Since
	rangeScans             atomic.Int64 // scans of the store by a transaction
	recordDecodes          atomic.Int64 // records decoded from a JSON row: staged copy, fenced snapshot
	logDecodes             atomic.Int64 // records converted from their zonelog form
	sealsComputed          atomic.Int64 // zone seals: copy, sort, JSON and SHA-256 of a snapshot
)

func countCommit(batch []zonelog.Record) {
	deleted := 0
	for i := range batch {
		if batch[i].Deleted {
			deleted++
		}
	}
	rowCommits.Add(1)
	rowDeletes.Add(int64(deleted))
	rowPuts.Add(int64(len(batch) - deleted))
}
