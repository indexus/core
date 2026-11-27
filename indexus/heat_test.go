package indexus

import (
	"fmt"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
)

func TestRecentHeatIsBoundedAndExpires(t *testing.T) {
	var h recentHeat
	now := time.Unix(100, 0)
	hot := Key{Collection: "c", Location: "hot"}
	for i := 0; i < 3*heatEntries; i++ {
		h.record(Key{Collection: "c", Location: fmt.Sprint(i)}, now)
		h.record(hot, now)
	}
	if len(h.rows) != heatEntries || len(h.at) != heatEntries {
		t.Fatalf("unbounded heat: %d/%d", len(h.rows), len(h.at))
	}
	if h.score(hot, now) != 3*heatEntries {
		t.Fatal("frequently used entry evicted or lost")
	}
	if h.score(hot, now.Add(20*heatHorizon)) != 0 {
		t.Fatal("old hotspot retained its weight")
	}
	// Eviction reuses every row without corrupting the double-linked list.
	seen := map[int]bool{}
	for i := h.head; i >= 0; i = h.rows[i].next {
		if seen[i] {
			t.Fatal("LRU cycle")
		}
		seen[i] = true
	}
	if len(seen) != heatEntries {
		t.Fatalf("unlinked rows: %d", len(seen))
	}
}

func TestRecentHeatRecognizesPrefixWithoutMixingCollections(t *testing.T) {
	var h recentHeat
	now := time.Unix(100, 0)
	for i := 0; i < 100; i++ {
		h.observe(Key{Collection: "c", Location: fmt.Sprintf("abc%04d", i)}, now)
	}
	if got := h.relief(Key{Collection: "c", Location: "abc9999"}, now); got != 100 {
		t.Fatalf("prefix heat %d", got)
	}
	if h.relief(Key{Collection: "other", Location: "abc9999"}, now) != 0 {
		t.Fatal("heat leaked between collections")
	}
	if h.score(Key{Collection: "c", Location: "abc9999"}, now) != 0 {
		t.Fatal("prefix estimate became exact write count")
	}
}

func TestRelayedWriteDoesNotHeatIngress(t *testing.T) {
	n := &Node{}
	if err := n.meshAccountWrite(storage.Record{Collection: "c", Location: "abc"}, nil, false); err != nil {
		t.Fatal(err)
	}
	if n.zoneWriteHeat(Key{Collection: "c", Location: "abc"}) != 0 {
		t.Fatal("forwarded ACK heats an unrelated ingress")
	}
}
