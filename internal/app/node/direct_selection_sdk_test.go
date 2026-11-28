package node

import (
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestDirectSDKSelectIDsMembershipAndProjectionTwoPeers(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	rows := []struct {
		loc, id string
		metrics []float64
	}{
		{"aa", "duplicate", []float64{2, 123, 999}},
		{"aa", "outside-in", []float64{3, 99}},
		{"aa", "outside-ids", []float64{2, 22}},
		{"aa", "zero", []float64{0, 0}},
		{"zz", "duplicate", []float64{5}},
		{"zz", "missing-column", nil},
		{"aaa", "duplicate", []float64{2, 88}},
	}
	for _, r := range rows {
		item := &indexus.Item{Collection: directSDKCollection, Location: r.loc, Id: r.id, Metrics: r.metrics, RequestID: r.loc + "-" + r.id}
		if err := first.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	runDirectSDK(t, "direct_selection_sdk.mjs", sdk, first, second)
}
