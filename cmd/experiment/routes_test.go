package main

import (
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

// A split's smaller cells replace the cell they come from, a fold's larger
// cell replaces the cells it covers, and an owner that did not answer is
// forgotten: a key always goes to the owner of the cell learned last.
func TestRoutesFollowSplitsAndFolds(t *testing.T) {
	cell := func(bits int, value byte) string {
		var r indexus.Region
		for i := 0; i < bits; i++ {
			r.Mask[i/8] |= 128 >> (i % 8)
		}
		r.Value[0] = value & r.Mask[0]
		return indexus.EncodeCell(r)
	}
	id := func(first byte) indexus.ID { return indexus.ID{first, 1, 2} }
	var r routes
	if _, ok := r.owner(id(0x00)); ok {
		t.Fatal("a route before any was learned")
	}
	r.learn("http://a", cell(1, 0x00)) // a owns 0…
	r.learn("http://b", cell(1, 0x80)) // b owns 1…
	if o, _ := r.owner(id(0x40)); o != "http://a" {
		t.Fatalf("0100… goes to %q, want a", o)
	}
	r.learn("http://c", cell(2, 0x40)) // a split: c owns 01…
	if o, _ := r.owner(id(0x40)); o != "http://c" {
		t.Fatalf("after the split 0100… goes to %q, want c", o)
	}
	if o, ok := r.owner(id(0x00)); !ok || o != "http://a" {
		t.Fatalf("split lost the unchanged complement: %q", o)
	}
	r.learn("http://a", cell(2, 0x00))
	r.learn("http://d", cell(0, 0)) // a fold of everything into d
	if r.count() != 1 {
		t.Fatalf("%d routes after the fold, want 1", r.count())
	}
	if o, _ := r.owner(id(0xC0)); o != "http://d" {
		t.Fatalf("after the fold 11… goes to %q, want d", o)
	}
	r.learn("http://e", "not a cell")
	r.forget("http://d")
	if _, ok := r.owner(id(0xC0)); ok || r.count() != 0 {
		t.Fatal("a forgotten owner, or a malformed route, is still used")
	}
}

func TestExactACKRouteWorksBeforeCellAndIsBounded(t *testing.T) {
	var routes routes
	zone := indexus.ID{42, 1}
	other := indexus.ID{42, 2}
	routes.learnZone(zone, "http://receiver")
	if owner, ok := routes.owner(zone); !ok || owner != "http://receiver" {
		t.Fatal("installed zone waited for regional route")
	}
	if _, ok := routes.owner(other); ok {
		t.Fatal("one ACK generalized to a neighboring zone")
	}
	routes.forget("http://receiver")
	if _, ok := routes.owner(zone); ok {
		t.Fatal("failed exact hint retained")
	}
	for i := 0; i < 5000; i++ {
		routes.learnZone(indexus.ID{byte(i >> 8), byte(i)}, "http://owner")
	}
	if len(routes.exact) != 4096 || len(routes.order) != 4096 {
		t.Fatal("exact routes unbounded")
	}
}

func TestPointACKsDoNotFragmentRegionalRoutes(t *testing.T) {
	var r routes
	r.learn("source", indexus.EncodeCell(indexus.Region{}))
	var point indexus.Region
	for i := range point.Mask {
		point.Mask[i] = 255
	}
	for i := 0; i < 4096; i++ {
		point.Value = indexus.ID{byte(i >> 8), byte(i)}
		r.learn("receiver", indexus.EncodeCell(point))
		r.learnZone(point.Value, "receiver")
	}
	if r.count() != 1 {
		t.Fatalf("got %d regional routes after point hints", r.count())
	}
	if owner, _ := r.owner(point.Value); owner != "receiver" {
		t.Fatalf("point goes to %q", owner)
	}
	if owner, _ := r.owner(indexus.ID{255}); owner != "source" {
		t.Fatalf("neighbor goes to %q", owner)
	}
}

func TestRepeatedACKIndexTracksSplitsAndForgottenOwners(t *testing.T) {
	var cache routes
	root := indexus.EncodeCell(indexus.Region{})
	child := indexus.EncodeCell(indexus.Region{Mask: indexus.ID{128}})
	cache.learn("source", root)
	cache.learn("receiver", child)
	cache.learn("source", root)
	if got, _ := cache.owner(indexus.ID{}); got != "source" {
		t.Fatalf("relearned root left the old child owner %q", got)
	}
	cache.forget("source")
	cache.learn("source", root)
	if got, _ := cache.owner(indexus.ID{}); got != "source" {
		t.Fatalf("a forgotten advertisement prevented relearning: %q", got)
	}
}

func BenchmarkDirectRoutesRepeatedACKs(b *testing.B) {
	for _, cells := range []int{512, 4096} {
		b.Run(strconv.Itoa(cells), func(b *testing.B) {
			var cache routes
			encoded := make([]string, cells)
			for i := range encoded {
				region := indexus.Region{
					Mask:  indexus.ID{255, 240},
					Value: indexus.ID{byte(i >> 4), byte(i&15) << 4},
				}
				encoded[i] = indexus.EncodeCell(region)
				cache.learn("receiver", encoded[i])
			}
			var sequence atomic.Uint64
			b.ReportAllocs()
			b.SetParallelism(8)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					n := sequence.Add(1)
					cell := n % uint64(cells)
					id := indexus.ID{byte(cell >> 4), byte(cell&15) << 4, byte(n >> 12), byte(n >> 20)}
					cache.owner(id)
					cache.learn("receiver", encoded[cell])
					cache.learnZone(id, "receiver")
				}
			})
		})
	}
}
