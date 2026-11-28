package node

import (
	"encoding/json"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestSDKTokenMaxSimFromTwoPeerRoots(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	for _, row := range []struct{ loc, id, body string }{
		{"aa", "same", `{"v":[[1,0],[0,1]]}`}, {"zz", "same", `{"v":[[1,1]]}`},
		{"aa0", "negative", `{"v":[[-1,-1],[-2,-2]]}`}, {"zz1", "higher", `{"v":[[2,0],[0,3]]}`},
	} {
		item := &indexus.Item{Collection: directSDKCollection, Location: row.loc, Id: row.id,
			Payload: json.RawMessage(row.body), RequestID: "tokens-" + row.loc + "-" + row.id}
		if err := first.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	runDirectSDK(t, "token_maxsim_sdk.mjs", sdk, first, second)
}
