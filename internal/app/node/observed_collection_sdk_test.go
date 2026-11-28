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
