package node

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestSDKQueryFromPeerRoots(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	for _, row := range []struct {
		location, id string
		value        float64
	}{
		{"@", "root", 10}, {"aa", "same", 2}, {"aa0", "deep", 3},
		{"zz", "same", 4}, {"zz", "deleted", 100}, {"zz1", "leaf", 5},
	} {
		payload, err := json.Marshal(map[string]any{
			"tags": []float64{row.value, 0}, "time": fmt.Sprintf("2026-01-01T00:00:%02dZ", int(row.value)%60),
		})
		if err != nil {
			t.Fatal(err)
		}
		item := &indexus.Item{Collection: directSDKCollection, Location: row.location, Id: row.id, Payload: payload, Metrics: []float64{row.value}, RequestID: "put-" + row.location + row.id}
		if err := first.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
		if row.id == "deleted" {
			item.RequestID = "delete-" + row.id
			if err := first.node.Delete(item, "@", nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	runDirectSDK(t, "observed_collection_sdk.mjs", sdk, first, second)
}

// Each write is acknowledged before the next pass begins. The two passes
// therefore check recovered coverage after mutations, independent of the
// weaker consistency of a read overlapping those mutations.
func TestSDKRootScanAfterMutations(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	const size = 192
	write := func(i, rev int, remove bool) {
		t.Helper()
		parent, owner := "aa", first
		if i%2 == 1 {
			parent, owner = "zz", second
		}
		item := &indexus.Item{Collection: directSDKCollection, Location: fmt.Sprintf("%s%x", parent, (i/2)%8),
			Id: fmt.Sprintf("doc-%03d", i), Metrics: []float64{float64(rev)}, RequestID: fmt.Sprintf("mutation-%d-%d", i, rev)}
		var err error
		if remove {
			err = owner.node.Delete(item, "@", nil)
		} else {
			err = owner.node.New(item, "@", nil)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range size {
		write(i, 1, false)
	}
	runDirectSDK(t, "root_mutations_sdk.mjs", sdk, first, second, "before")
	for i := range size {
		switch {
		case i%3 == 0:
			write(i, 2, true)
			if i%6 == 0 {
				write(i, 3, false)
			}
		case i%5 == 0:
			write(i, 2, false)
		}
	}
	runDirectSDK(t, "root_mutations_sdk.mjs", sdk, first, second, "after")
}
