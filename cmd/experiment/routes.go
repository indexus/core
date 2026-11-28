package main

import (
	"math/bits"
	"sync"

	"github.com/indexus/go-indexus-core/indexus"
)

// routes is what a loader in route=direct mode learned of where keys are
// written: cells of the key space and their owner's address, from the
// X-Indexus-Owner headers of relayed writes. A key goes to the owner of the
// most specific cell holding its zone ID, or through any entry when none
// does. A cell learned replaces every cell it overlaps: after a split the
// smaller cell, after a fold the larger one, is the current one. A stale
// route only costs a relay, which answers with the current one.
type routes struct {
	mu         sync.RWMutex
	cells      []route
	cellOwners map[indexus.Region]string
	exact      map[indexus.ID]string
	order      []indexus.ID
	next       int
}

type route struct {
	cell  indexus.Region
	owner string
	bits  int
}

// owner is the address to write a key of zone id to, if one is known.
func (r *routes) owner(id indexus.ID) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if owner := r.exact[id]; owner != "" {
		return owner, true
	}
	best, found := -1, ""
	for _, c := range r.cells {
		if c.bits > best && c.cell.Contains(id) {
			best, found = c.bits, c.owner
		}
	}
	return found, best >= 0
}

// learnZone uses the actual applier of this one ACK, including before a
// region certificate. It never generalizes partial ownership to a whole cell.
func (r *routes) learnZone(id indexus.ID, owner string) {
	if owner == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exact == nil {
		r.exact = map[indexus.ID]string{}
	}
	if _, held := r.exact[id]; !held {
		if len(r.order) < 4096 {
			r.order = append(r.order, id)
		} else {
			delete(r.exact, r.order[r.next])
			r.order[r.next] = id
			r.next = (r.next + 1) % len(r.order)
		}
	}
	r.exact[id] = owner
}

// learn records the route a write's answer gave; malformed ones are ignored.
func (r *routes) learn(owner, cell string) {
	if owner == "" || cell == "" {
		return
	}
	region, err := indexus.DecodeCell(cell)
	if err != nil {
		return
	}
	n := 0
	for _, b := range region.Mask {
		n += bits.OnesCount8(b)
	}
	// The ACK path learns the exact zone separately. Subtracting a point
	// from a region would fragment the regional cache for every new key.
	if n == 8*len(region.Mask) {
		return
	}
	r.mu.RLock()
	known := r.cellOwners[region] == owner
	r.mu.RUnlock()
	if known {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// A concurrent update may already have installed this region.
	if r.cellOwners[region] == owner {
		return
	}
	for _, c := range r.cells {
		if c.owner == owner {
			if overlap, ok := c.cell.Intersect(region); ok && overlap == region {
				return
			}
		}
	}
	kept := make([]route, 0, len(r.cells)+1)
	for _, c := range r.cells {
		// Replacing one installed piece must retain the old destination for
		// its exact complement, including addresses never written before.
		for _, part := range c.cell.Subtract(region) {
			depth := 0
			for _, b := range part.Mask {
				depth += bits.OnesCount8(b)
			}
			kept = append(kept, route{cell: part, owner: c.owner, bits: depth})
		}
	}
	r.cells = append(kept, route{cell: region, owner: owner, bits: n})
	if len(r.cells) > 4096 {
		r.cells = r.cells[len(r.cells)-4096:]
	}
	r.indexCells()
}

func (r *routes) indexCells() {
	if r.cellOwners == nil {
		r.cellOwners = make(map[indexus.Region]string, len(r.cells))
	}
	clear(r.cellOwners)
	for _, c := range r.cells {
		r.cellOwners[c.cell] = c.owner
	}
}

// forget drops the routes to an owner that did not answer.
func (r *routes) forget(owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, host := range r.exact {
		if host == owner {
			r.exact[id] = ""
		}
	}
	kept := r.cells[:0]
	for _, c := range r.cells {
		if c.owner != owner {
			kept = append(kept, c)
		}
	}
	r.cells = kept
	r.indexCells()
}

func (r *routes) count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.cells)
}
