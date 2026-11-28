package node

import (
	"encoding/json"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestSDKDocumentFilterTwoPayloadOwners(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	for _, row := range []struct{ location, id, payload string }{
		{"aa", "same", `{"enabled":true,"tags":["public",false]}`},
		{"aa", "empty", `{"enabled":true,"tags":[]}`},
		{"aa", "missing", `{"enabled":true}`},
		{"aa", "disabled", `{"enabled":false,"tags":["public"]}`},
		{"zz", "same", `{"enabled":true,"tags":["public","hidden"]}`},
		{"zz", "null", `{"enabled":true,"tags":null}`},
		{"zz", "typed", `{"enabled":"true","tags":[false,"public"]}`},
		{"zz", "none", ""},
		{"zza", "descendant", `{"enabled":true,"tags":["public"]}`},
	} {
		item := &indexus.Item{Collection: directSDKCollection, Location: row.location, Id: row.id,
			Payload: json.RawMessage(row.payload), RequestID: "filter-" + row.location + "-" + row.id}
		if err := first.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	runDirectSDK(t, "document_filter_payloads_sdk.mjs", sdk, first, second)
}
