package tree

import (
	"math/rand/v2"
	"sync"
	"time"
)

// JoinHint returns an id that halves one of the largest XOR cells of the
// active set (JOIN-1). A vnode's cell is the subtree fixed by one bit more
// than the longest prefix it shares with any other vnode; the new id keeps
// that prefix and takes the other half, so the owner and the newcomer split
// the cell evenly. Ties are broken at random and the remaining bits are
// random, so concurrent joiners rarely pick the same point.
//
// Sequential joins keep every cell within a factor 2 of every other; random
// ids leave the largest cell about ln N times the mean.
func JoinHint(active []VNodeDescriptor, rng *rand.Rand) ID {
	var id ID
	for i := range id {
		id[i] = byte(rng.IntN(256))
	}
	if len(active) == 0 {
		return id
	}
	depth := func(v ID) int {
		d := 0
		for _, o := range active {
			if o.ID != v {
				d = max(d, int(CommonPrefixLen(v, o.ID))+1)
			}
		}
		return d
	}
	best, ties := -1, 0
	var owner ID
	for _, v := range active {
		d := depth(v.ID)
		switch {
		case best < 0 || d < best:
			best, owner, ties = d, v.ID, 1
		case d == best:
			ties++
			if rng.IntN(ties) == 0 {
				owner = v.ID
			}
		}
	}
	if best >= 8*Size {
		return id
	}
	// Keep the owner's first `best` bits, flip bit `best`, randomize the rest.
	for i := 0; i <= best; i++ {
		mask := byte(1) << (7 - i%8)
		bit := owner[i/8] & mask
		if i == best {
			bit ^= mask
		}
		id[i/8] = id[i/8]&^mask | bit
	}
	return id
}

// RecentHints remembers the hints a node gave until their joiners appear in
// its view (REM-120). A bootstrap learns of a join through gossip, rounds of
// several seconds, while joins can follow each other faster: from the view
// alone, two joiners in a row are told to halve the same cell and a larger
// cell stays whole. A hint given less than ttl ago counts as an active vnode.
type RecentHints struct {
	mu    sync.Mutex
	given []recentHint
}

type recentHint struct {
	id ID
	at time.Time
}

// Next is JoinHint over the active set and the hints given within ttl whose
// joiner is not active yet; the new hint is remembered.
func (h *RecentHints) Next(active []VNodeDescriptor, now time.Time, ttl time.Duration, rng *rand.Rand) ID {
	h.mu.Lock()
	defer h.mu.Unlock()
	known := make(map[ID]bool, len(active))
	for _, v := range active {
		known[v.ID] = true
	}
	set := append(make([]VNodeDescriptor, 0, len(active)+len(h.given)), active...)
	keep := h.given[:0]
	for _, g := range h.given {
		if now.Sub(g.at) < ttl && !known[g.id] {
			keep = append(keep, g)
			set = append(set, VNodeDescriptor{ID: g.id})
		}
	}
	h.given = keep
	id := JoinHint(set, rng)
	h.given = append(h.given, recentHint{id: id, at: now})
	return id
}
