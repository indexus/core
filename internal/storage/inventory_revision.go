package storage

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// Read cursors track visible data, not receipts, counters or staging cleanup.
// One version per collection is updated in the data commit. Whole-disk queries
// keep the conservative transaction version. Authority/forwarding are checked
// separately in inventoryScope.
var inventoryRevisionKey = []byte("inventory-revision")
var inventoryRevisionPrefix = key(metaBucket, inventoryRevisionKey)

func inventoryRevisionPath(collection string) string {
	return inventoryRevisionPrefix + element([]byte(collection))
}

func (tx *Tx) inventoryRevision(query *tree.ZoneKey) uint64 {
	if query == nil {
		return uint64(tx.ID())
	}
	return tx.number8(inventoryRevisionPath(query.Collection)) + 1 // zero is not a cursor
}

func (tx *Tx) advanceInventory(collections map[string]bool) error {
	if len(collections) == 0 {
		return nil
	}
	// The durable row is the sequence of this data cut, not an incrementing
	// counter that must be fetched before every write. All commits serialize
	// under the existing disk writer; the published sequence after this cut
	// is at least version+1 (also for Bulk's single reserved sequence).
	if tx.version >= math.MaxUint64-1 {
		return ErrSequence
	}
	for collection := range collections {
		if err := tx.put(inventoryRevisionPath(collection), number(tx.version+1)); err != nil {
			return err
		}
	}
	return nil
}

func (tx *Tx) indexInventory() error {
	collections := map[string]bool{}
	var control *causalSnapshot
	loaded := false
	mark := func(z tree.ZoneKey) {
		if collections[z.Collection] {
			return
		}
		if !loaded && tx.owner != nil {
			control, _ = tx.owner.causalSnapshot(tx)
			loaded = true
		}
		// Physically collecting already ceded rows does not change the answer.
		if control == nil || !errors.Is(tx.owner.readableIn(tx, control, z), ErrNotOwner) {
			collections[z.Collection] = true
		}
	}
	for _, pending := range tx.records {
		// A newly buffered live record must invalidate this collection.
		// Pure removal may only collect already ceded rows, so keep the
		// permission check for that case, not on every client write.
		for _, record := range pending.rows {
			if record != nil {
				collections[pending.zone.Collection] = true
				break
			}
		}
		mark(pending.zone)
	}
	prefix := key(liveBucket)
	sequence := element(sequenceKey)
	for path, payload := range tx.pending {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		if len(payload) > 0 && !strings.HasSuffix(path, sequence) {
			continue // receipts do not change a client's inventory
		}
		parts := split(path[len(prefix):])
		if len(parts) != 1 && (len(parts) != 2 || string(parts[1]) != string(sequenceKey)) {
			continue
		}
		if pending := tx.records[string(parts[0])]; pending != nil && collections[pending.zone.Collection] {
			continue // this zone's data already invalidates the collection
		}
		if z, err := zoneFromBytes(parts[0]); err == nil {
			mark(z)
		}
	}
	return tx.advanceInventory(collections)
}

// Canonicalize just the queried coverage and its current successors. No
// membership history, global snapshot, or new network agreement is retained.
func inventoryScope(page InventoryPage) [32]byte {
	parts := make([]string, 0, len(page.Coverage)+len(page.Referrals))
	for _, region := range page.Coverage {
		raw, _ := json.Marshal(region)
		parts = append(parts, "coverage:"+string(raw))
	}
	for _, ref := range page.Referrals {
		raw, _ := json.Marshal(ref)
		parts = append(parts, "referral:"+string(raw))
	}
	slices.Sort(parts)
	raw, _ := json.Marshal(slices.Compact(parts))
	return sha256.Sum256(raw)
}
