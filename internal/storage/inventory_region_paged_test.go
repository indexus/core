package storage

import (
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
)

// REM-121: during a split, the handoff's inventory of its region held rowsMu
// for minutes in one read; the next commit waited for rowsMu while holding
// writeMu, every reader queued behind it and the node stopped. InventoryRegion
// now reads a page of zones per transaction: the listing is the same, and a
// commit lands between two pages.
func TestInventoryRegionPagesLetCommitsIn(t *testing.T) {
	page, paged := inventoryPage, inventoryPaged
	defer func() { inventoryPage, inventoryPaged = page, paged }()

	s, holder := newPlanningStore(t, filepath.Join(t.TempDir(), "region.db"), "D")
	defer s.Close()
	if err := s.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "planning", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	n := 0
	write := func(collection, location string) {
		n++
		r := Record{Collection: collection, Location: location, ID: fmt.Sprint("item-", n), Metrics: []float64{1}}
		if _, _, applied, err := s.ApplyRecord(fmt.Sprint("request-", n), r); err != nil || !applied {
			t.Fatalf("apply %d: applied=%v err=%v", n, applied, err)
		}
	}
	for _, collection := range []string{"0000000000000001", "0000000000000002"} {
		for _, location := range []string{"a", "b", "c", "d", "e", "f", "g"} {
			write(collection, location)
		}
	}
	write("0000000000000001", "a") // a second item in a zone

	zones := func(snaps []Snapshot) []string {
		var out []string
		for _, s := range snaps {
			out = append(out, fmt.Sprintf("%s/%s@%d", s.Zone.Collection, s.Zone.Location, s.Seq))
		}
		return out
	}
	inventoryPage = 1 << 20
	whole, err := s.InventoryRegion(tree.Region{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) != 14 {
		t.Fatalf("listed %d zones, want 14", len(whole))
	}
	for _, size := range []int{1, 3, 13, 14, 15} {
		inventoryPage = size
		paged, err := s.InventoryRegion(tree.Region{}, true)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(zones(paged), zones(whole)) {
			t.Fatalf("page size %d lists %v, want %v", size, zones(paged), zones(whole))
		}
	}

	// A write between two pages completes while the listing runs.
	inventoryPage = 3
	landed := 0
	inventoryPaged = func() {
		if landed > 0 {
			return
		}
		landed++
		done := make(chan struct{})
		go func() { write("0000000000000002", "h"); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a commit waited for the whole region listing")
		}
	}
	if _, err := s.InventoryRegion(tree.Region{}, true); err != nil {
		t.Fatal(err)
	}
	if landed == 0 {
		t.Fatal("the listing read one page only")
	}
}
