package node

import (
	"encoding/json"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestSDKGroupedOffsetsTwoOwners(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	for _, row := range []struct{ location, id, body string }{
		{"aa", "1", `{"usage":"shop","size":2}`}, {"aa", "2", `{"usage":"shop","size":2}`},
		{"aa", "3", `{"usage":"house","size":3}`}, {"aa", "4", `{"size":1}`},
		{"zz", "1", `{"usage":"shop","size":2}`}, {"zz", "2", `{"usage":"house","size":1}`},
		{"zz", "3", `{"usage":"shop"}`}, {"zz", "4", `{"usage":"house","size":3}`},
	} {
		item := &indexus.Item{Collection: directSDKCollection, Location: row.location, Id: row.id,
			Payload: json.RawMessage(row.body), RequestID: "groups-" + row.location + "-" + row.id}
		if err := first.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	runDirectSDK(t, "group_offset_sdk.mjs", sdk, first, second)
}
