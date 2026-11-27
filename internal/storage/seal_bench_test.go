package storage

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// lotSnapshot is one zone of about 1 MiB: 4,000 records of 200 payload bytes.
func lotSnapshot(tb testing.TB) Snapshot {
	z, err := tree.NewZoneKey("sealbench0000000", "Z")
	if err != nil {
		tb.Fatal(err)
	}
	snap := Snapshot{Zone: z, Seq: 4000}
	payload, _ := json.Marshal(fmt.Sprintf("%0200d", 7))
	for i := range 4000 {
		snap.Records = append(snap.Records, Record{Collection: z.Collection, Location: z.Location, ID: fmt.Sprintf("r%06d", i), Metrics: []float64{float64(i), 1.5}, Payload: payload})
	}
	return snap
}

// BenchmarkLotSeal is one Seal of a 1 MiB lot: what each recomputation of a
// seal already known costs (cutoverLot inside the no-writer window,
// stageZonesTx twice per staged snapshot).
func BenchmarkLotSeal(b *testing.B) {
	snap := lotSnapshot(b)
	b.ReportAllocs()
	for range b.N {
		if _, err := snap.Seal(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLotEncode is one JSON encoding of the same lot: what copyLot pays
// to measure a snapshot the transport then encodes again.
func BenchmarkLotEncode(b *testing.B) {
	snap := lotSnapshot(b)
	b.ReportAllocs()
	for range b.N {
		if _, err := json.Marshal(snap); err != nil {
			b.Fatal(err)
		}
	}
}
