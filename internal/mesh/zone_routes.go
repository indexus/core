package mesh

import (
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"strings"
)

// zoneRoutes are exact-zone write hints learned from ACKs: the disk that
// applied a zone's last routed write. Bounded like peer cursors (ring
// eviction). A hint is never authority, nor a membership fact: the disk it
// names verifies its own and relays a write it does not hold, so a wrong hint
// costs one relay. The owner of a zoneRoutes holds the lock guarding it.
type zoneRoutes struct {
	m     map[tree.Key]causal.Endpoint
	order []tree.Key
	next  int
}

func (r *zoneRoutes) get(key tree.Key) (causal.Endpoint, bool) {
	h, ok := r.m[key]
	return h, ok && h.Valid()
}

func (r *zoneRoutes) put(key tree.Key, holder causal.Endpoint) {
	if !holder.Valid() {
		return
	}
	if r.m == nil {
		r.m = map[tree.Key]causal.Endpoint{}
	}
	if _, ok := r.m[key]; !ok {
		if len(r.order) < membershipPeerLimit {
			r.order = append(r.order, key)
		} else {
			delete(r.m, r.order[r.next])
			r.order[r.next] = key
			r.next = (r.next + 1) % membershipPeerLimit
		}
	}
	r.m[key] = holder
}

// drop forgets the hint for key if it still names holder. The entry keeps its
// ring slot, so every key in m appears exactly once in order.
func (r *zoneRoutes) drop(key tree.Key, holder causal.Endpoint) {
	if h, ok := r.m[key]; ok && h == holder {
		r.m[key] = causal.Endpoint{}
	}
}

// regionRoutes are regional write hints learned from ACKs (ROUTE-REGION-1):
// the disk whose installed right covered an applied write, with that right's
// region. The most specific region containing an address wins, so a later
// cession of a part overrides its parent. Like zoneRoutes: bounded, never
// authority, a wrong hint costs one relay; the owner holds the lock.
type regionRoute struct {
	region tree.Region
	holder causal.Endpoint
}

type regionRoutes struct {
	entries   []regionRoute
	next      int
	textBytes int
}

// A fixed backing array is about 80 KiB on 64-bit hosts; copied endpoint
// strings retain at most another 128 KiB. Oversized hints are simply uncached.
const (
	regionRouteLimit     = 1024
	regionRouteTextBytes = 128 << 10
)

func endpointTextBytes(e causal.Endpoint) int {
	return len(e.VNode) + len(e.Storage) + len(e.Host)
}

func (r *regionRoutes) clear(i int) {
	r.textBytes -= endpointTextBytes(r.entries[i].holder)
	r.entries[i].holder = causal.Endpoint{}
}

func regionBits(r tree.Region) (n int) {
	for _, b := range r.Mask {
		for ; b != 0; b &= b - 1 {
			n++
		}
	}
	return n
}

func (r *regionRoutes) get(id tree.ID) (tree.Region, causal.Endpoint, bool) {
	best, bits := -1, -1
	for i, e := range r.entries {
		if e.holder.Valid() && e.region.Contains(id) {
			if n := regionBits(e.region); n > bits {
				best, bits = i, n
			}
		}
	}
	if best < 0 {
		return tree.Region{}, causal.Endpoint{}, false
	}
	return r.entries[best].region, r.entries[best].holder, true
}

func (r *regionRoutes) put(region tree.Region, holder causal.Endpoint) {
	if !holder.Valid() || !region.Valid() {
		return
	}
	cost := endpointTextBytes(holder)
	if cost > regionRouteTextBytes {
		return
	}
	index := -1
	for i, e := range r.entries {
		if e.region == region {
			if e.holder == holder {
				return
			}
			index = i
			r.clear(i)
			break
		}
	}
	if index < 0 {
		if r.entries == nil {
			r.entries = make([]regionRoute, 0, regionRouteLimit)
		}
		if len(r.entries) < regionRouteLimit {
			index = len(r.entries)
			r.entries = append(r.entries, regionRoute{})
		} else {
			index = r.next
			r.clear(index)
			r.next = (r.next + 1) % regionRouteLimit
		}
	}
	for r.textBytes+cost > regionRouteTextBytes {
		r.clear(r.next)
		r.next = (r.next + 1) % len(r.entries)
	}
	// Detach short substrings from possibly large transport buffers. Repeated
	// ACKs for an unchanged endpoint return above without allocating.
	holder.VNode = strings.Clone(holder.VNode)
	holder.Storage = strings.Clone(holder.Storage)
	holder.Host = strings.Clone(holder.Host)
	r.entries[index] = regionRoute{region, holder}
	r.textBytes += cost
}

// drop forgets the hint for region if it still names holder.
func (r *regionRoutes) drop(region tree.Region, holder causal.Endpoint) {
	for i, e := range r.entries {
		if e.region == region && e.holder == holder {
			r.clear(i)
		}
	}
}
