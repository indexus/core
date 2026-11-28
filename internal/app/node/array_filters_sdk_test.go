package node

import (
	"encoding/json"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestArrayFiltersSDKTwoPeers(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	for _, row := range []struct {
		peer               *runningDataNode
		location, id, body string
	}{
		{first, "aa", "a", `{"tags":["red","red"],"values":[-3,2]}`},
		{first, "aa", "b", `{"tags":[],"values":[0]}`},
		{first, "aa", "c", `{}`},
		{second, "zz", "d", `{"tags":["blue"],"values":[1,2]}`},
		{second, "zz", "e", `{"tags":null,"values":null}`},
		{second, "zz", "f", `{"tags":["green"],"values":[]}`},
	} {
		item := &indexus.Item{Collection: directSDKCollection, Location: row.location, Id: row.id, Payload: json.RawMessage(row.body), RequestID: "array-" + row.id}
		if err := row.peer.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	runDirectSDK(t, "array_filters_sdk.mjs", sdk, first, second)
}
