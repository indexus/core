package storage

import (
	"github.com/indexus/go-indexus-core/internal/tree"
	"testing"
)

// A record without metrics seals the same whether it was read from the
// memtable (nil) or from a segment (an empty list).
func TestSealIgnoresHowAnEmptyMetricListWasRead(t *testing.T) {
	zone, err := tree.NewZoneKey("collection-00001", "aa")
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Collection: zone.Collection, Location: zone.Location, ID: "a"}
	fromMemtable, err := Snapshot{Zone: zone, Seq: 1, Records: []Record{record}}.Seal()
	if err != nil {
		t.Fatal(err)
	}
	record.Metrics = []float64{}
	fromSegment, err := Snapshot{Zone: zone, Seq: 1, Records: []Record{record}}.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if fromMemtable != fromSegment {
		t.Fatal("the same zone has two seals: its region certificate would be refused")
	}
}
