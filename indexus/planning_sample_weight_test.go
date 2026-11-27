package indexus

import (
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
)

// With 1,023 one-item zones, 512 equal integer weights would count only
// 512 zones. Unequal rank slices must account for the remainder as well.
func TestPlanningSampleWeightsCoverNonDivisibleInventory(t *testing.T) {
	n := newNodeOn(t, 64)
	const zones = 1023
	records := make([]storage.Record, zones)
	for i := range records {
		records[i] = storage.Record{Collection: "Demo000000000000", Location: fmt.Sprintf("z%05d", i), ID: "one", Payload: []byte(`{"v":1}`)}
	}
	if err := n.durableStore().SeedLocalRecords(records); err != nil {
		t.Fatal(err)
	}
	stats := mustZoneStats(t, n)
	if len(stats) != planSampleZones {
		t.Fatalf("sample has %d rows, want %d", len(stats), planSampleZones)
	}
	var represented, items int64
	for _, z := range stats {
		represented += z.Stands
		items += z.Items
	}
	if represented != zones || items != zones {
		t.Fatalf("sample represents %d zones and %d uniform items, want %d", represented, items, zones)
	}
}
