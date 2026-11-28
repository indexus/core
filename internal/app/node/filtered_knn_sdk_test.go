package node

import (
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

// Known owners hold a fixed corpus. The record layout is [group, x, y].
func TestSDKFilteredKnnTwoColdPeers(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	for _, row := range []struct {
		location, id string
		group, x, y  float64
	}{
		{"aa", "same", 1, 1, 0}, {"aa", "far", 1, 0, 3},
		{"aa", "excluded", 2, 0, 0}, {"aa", "gone", 1, 0, 0},
		{"zz", "same", 1, -1, 0}, {"zz", "before", 1, 0, 1},
		{"zz", "another", 2, 3, 4}, {"aaa", "descendant", 1, 0, 0},
	} {
		item := &indexus.Item{Collection: directSDKCollection, Location: row.location, Id: row.id,
			Metrics: []float64{row.group, row.x, row.y}, RequestID: "knn-" + row.location + "-" + row.id}
		if err := first.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
		if row.id == "gone" {
			item.RequestID = "delete-gone"
			if err := first.node.Delete(item, "@", nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	runDirectSDK(t, "filtered_knn_sdk.mjs", sdk, first, second)
}
