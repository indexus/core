package storage

import (
	"fmt"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
)

// PlanningTotals reads a page of zones per transaction: the counts stay exact,
// and a commit lands between two pages instead of waiting for the whole store
// (REM-83: a whole-store count held rowsMu for up to a second under load).
func TestPlanningTotalsPagesLetCommitsIn(t *testing.T) {
	page, paged := planningPage, planningPaged
	defer func() { planningPage, planningPaged = page, paged }()
	planningPage = 3

	s, holder := newPlanningStore(t, filepath.Join(t.TempDir(), "paged.db"), "D")
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
	assertPlanningParity(t, s)     // pages of 3 zones, no write in between

	// A write between two pages completes while the count runs.
	landed := 0
	planningPaged = func() {
		if landed > 0 {
			return
		}
		landed++
		done := make(chan struct{})
		go func() { write("0000000000000002", "h"); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a commit waited for the whole count")
		}
	}
	write("0000000000000001", "z") // the cached count is for the version before
	if _, err := s.PlanningTotals(); err != nil {
		t.Fatal(err)
	}
	if landed == 0 {
		t.Fatal("the count read one page only")
	}
	planningPaged = nil
	assertPlanningParity(t, s) // the count above is not kept: it straddles a commit
}
