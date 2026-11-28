package node

import (
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

// The first column is the range key, the optional second column the measure.
// Neither store is mutated while the client combines its two direct reads.
func TestSDKRangeAggregatesTwoColdPeers(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	for _, row := range []struct {
		location, id string
		metrics      []float64
	}{
		{"aa", "lower", []float64{5, 2}}, {"aa", "interior", []float64{7, 4}}, {"aa", "null-measure", []float64{8}},
		{"aa", "no-range", nil}, {"aa", "outside", []float64{4, 100}},
		{"zz", "interior", []float64{7, 4}}, {"zz", "upper", []float64{10, -1}}, {"zz", "aa-upper-null", []float64{10}},
		{"zz", "all-null", []float64{11}},
		{"aaa", "descendant", []float64{7, 100}},
	} {
		item := &indexus.Item{Collection: directSDKCollection, Location: row.location, Id: row.id, Metrics: row.metrics, RequestID: "range-" + row.location + "-" + row.id}
		if err := first.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	runDirectSDK(t, "range_aggregates_sdk.mjs", sdk, first, second)
}
