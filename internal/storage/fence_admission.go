package storage

import (
	"github.com/indexus/go-indexus-core/internal/tree"
	"sync"
)

// zoneAdmission is a transient local scheduling gate, not an authority or a
// durable fence. Its tiny map lock protects counters only; no disk operation,
// snapshot, hash, writer wait or network call runs while it is held.
// Refcounts keep overlapping lot preparations from releasing each other.
type zoneAdmission struct {
	mu    sync.RWMutex
	zones map[tree.ZoneKey]int
}

func (g *zoneAdmission) contains(zone tree.ZoneKey) bool {
	g.mu.RLock()
	blocked := g.zones[zone] != 0
	g.mu.RUnlock()
	return blocked
}

func (s *Store) beginFencePreparation(zones []tree.ZoneKey) func() {
	g := &s.fenceAdmission
	g.mu.Lock()
	if g.zones == nil {
		g.zones = make(map[tree.ZoneKey]int)
	}
	for _, zone := range zones {
		g.zones[zone]++
	}
	g.mu.Unlock()
	// Drain the callback that could already have passed its under-writer
	// gate check. Every subsequent callback sees the published admission gate.
	// This acquires no disk snapshot and issues no I/O or authority transition.
	s.writeMu.Lock()
	s.writeMu.Unlock()
	return func() {
		g.mu.Lock()
		for _, zone := range zones {
			if g.zones[zone]--; g.zones[zone] == 0 {
				delete(g.zones, zone)
			}
		}
		g.mu.Unlock()
	}
}
