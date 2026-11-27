package storage

import (
	"context"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestHandoffGrainBoundsKeysWithoutReadingPayloads(t *testing.T) {
	s, _ := routingStore(t)
	var rows []Record
	for i := range 2048 {
		rows = append(rows, Record{Collection: fmt.Sprintf("collection-%05d", i), Location: "aa", ID: "seed"})
	}
	if err := s.SeedLocalRecords(rows); err != nil {
		t.Fatal(err)
	}
	whole := tree.Region{}
	grain, err := s.HandoffGrain(context.Background(), whole, 64, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range rows {
		z, err := tree.NewZoneKey(row.Collection, row.Location)
		if err != nil {
			t.Fatal(err)
		}
		if grain.Contains(z.ID) {
			count++
		}
	}
	if count == 0 || count > 64 || grain == whole {
		t.Fatalf("grain contains %d zones: %+v", count, grain)
	}
	coverage := append(whole.Subtract(grain), grain)
	if !tree.CoversRegions(whole, coverage) {
		t.Fatal("lost empty authority space during subdivision")
	}
	for i, a := range coverage {
		for j, b := range coverage {
			if i < j {
				if _, overlap := a.Intersect(b); overlap {
					t.Fatal("overlapping residual authority")
				}
			}
		}
	}
	// Advice must not fence or create a transfer, and an empty region stays whole.
	for _, row := range rows[:2] {
		if _, err := s.Write("after-plan-"+row.Collection, row); err != nil {
			t.Fatal(err)
		}
	}
	empty := tree.Point(tree.ID{255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 255})
	if got, err := s.HandoffGrain(context.Background(), empty, 64, 1<<20); err != nil || got != empty {
		t.Fatalf("empty region changed: %v %v", got, err)
	}
}

func TestHandoffGrainBoundsBytesBeforeZoneLimit(t *testing.T) {
	store, _ := routingStore(t)
	var records []Record
	for i := 0; i < 8; i++ {
		records = append(records, Record{Collection: fmt.Sprintf("collection-%05d", i), Location: "aa", ID: "large", Payload: bigPayload(512 << 10)})
	}
	if e := store.SeedLocalRecords(records); e != nil {
		t.Fatal(e)
	}
	grain, e := store.HandoffGrain(context.Background(), tree.Region{}, 512, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	var count int
	var bytes int64
	e = store.scanZonePages(zonePages{size: 512, rows: rowsMeta, within: grain.Contains}, func(tx *Tx, z tree.ZoneKey, rows []*row) error {
		_, size, e := zoneBytesOf(tx, liveZone(z), rows)
		count++
		bytes += size
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	if count == 0 || count >= len(records) || bytes > 1<<20 {
		t.Fatalf("grain did not respect byte target: count=%d bytes=%d", count, bytes)
	}
	state, e := store.CausalState()
	if e != nil || len(state.Transfers) != 0 || len(state.Permissions.Native) != 1 {
		t.Fatal("advisory grain changed actual authority", e)
	}
}
func TestHandoffGrainIsolatesIndivisibleOversizePoint(t *testing.T) {
	store, _ := routingStore(t)
	record := Record{Collection: "Demo000000000000", Location: "aa", ID: "oversize", Payload: bigPayload(2 << 20)}
	if e := store.SeedLocalRecords([]Record{record}); e != nil {
		t.Fatal(e)
	}
	grain, e := store.HandoffGrain(context.Background(), tree.Region{}, 512, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	z, _ := recordZone(record)
	if grain != tree.Point(z.ID) {
		t.Fatal("oversize first zone was not geometrically isolated", grain)
	}
	// No geometric subdivision can split aliases of one 96-bit point. This is
	// an explicit advisory limit, not a claim of a strict per-record byte cap.
	if _, e = store.HandoffGrain(context.Background(), grain, 512, 1<<20); e != nil {
		t.Fatal(e)
	}
}
func TestHandoffGrainRejectsInvalidBudgetsAndCancelledContext(t *testing.T) {
	store, _ := routingStore(t)
	for _, budget := range []struct {
		zones int
		bytes int64
	}{{0, 1}, {1, 0}, {-1, 1}, {1, -1}} {
		if _, e := store.HandoffGrain(context.Background(), tree.Region{}, budget.zones, budget.bytes); e == nil {
			t.Fatal("invalid budget accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := store.HandoffGrain(ctx, tree.Region{}, 512, 1<<20); e == nil {
		t.Fatal("cancelled context accepted")
	}
}
