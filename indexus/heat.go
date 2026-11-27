package indexus

import (
	"math"
	"time"
)

// Heat is an advisory, bounded LRU of decaying activity, never authority.
// Historical keys used to remain here forever and compete with a fresh hotspot.
// Prefix counters also recognize a stream of distinct leaves in one hot area.
const heatEntries = 8 * planSampleZones
const heatHorizon = 10 * time.Second

type heatEntry struct {
	key        Key
	weight     float64
	at         time.Time
	prev, next int
}
type recentHeat struct {
	at         map[Key]int
	rows       []heatEntry
	head, tail int
}

func (h *recentHeat) unlink(i int) {
	e := h.rows[i]
	if e.prev >= 0 {
		h.rows[e.prev].next = e.next
	} else {
		h.head = e.next
	}
	if e.next >= 0 {
		h.rows[e.next].prev = e.prev
	} else {
		h.tail = e.prev
	}
}
func heatWeight(e heatEntry, now time.Time) float64 {
	return e.weight * math.Exp(-max(0, now.Sub(e.at).Seconds())/heatHorizon.Seconds())
}
func (h *recentHeat) record(key Key, now time.Time) {
	if h.at == nil {
		h.at = map[Key]int{}
		h.head = -1
		h.tail = -1
	}
	i, found := h.at[key]
	weight := 1.0
	if found {
		weight += heatWeight(h.rows[i], now)
		h.unlink(i)
	} else {
		if len(h.rows) < heatEntries {
			i = len(h.rows)
			h.rows = append(h.rows, heatEntry{})
		} else {
			i = h.tail
			h.unlink(i)
			delete(h.at, h.rows[i].key)
		}
		h.at[key] = i
	}
	h.rows[i] = heatEntry{key: key, weight: weight, at: now, prev: -1, next: h.head}
	if h.head >= 0 {
		h.rows[h.head].prev = i
	} else {
		h.tail = i
	}
	h.head = i
}
func (h *recentHeat) score(key Key, now time.Time) int64 {
	if i, ok := h.at[key]; ok {
		return int64(math.Round(heatWeight(h.rows[i], now)))
	}
	return 0
}
func (h *recentHeat) observe(key Key, now time.Time) {
	h.record(key, now)
	for _, depth := range []int{1, 3} {
		if len(key.Location) > depth {
			h.record(Key{Collection: key.Collection, Location: key.Location[:depth]}, now)
		}
	}
}
func (h *recentHeat) relief(key Key, now time.Time) int64 {
	score := h.score(key, now)
	for _, depth := range []int{1, 3} {
		if len(key.Location) > depth {
			score = max(score, h.score(Key{Collection: key.Collection, Location: key.Location[:depth]}, now))
		}
	}
	return score
}
