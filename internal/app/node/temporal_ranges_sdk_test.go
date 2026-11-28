package node

import (
	"encoding/json"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestTemporalRangeSDKAcrossTwoPeers(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	for _, r := range []struct {
		owner              *runningDataNode
		location, id, when string
		height             float64
	}{
		{first, "aa", "start", "2024-02-28T19:00:00-05:00", 5},
		{first, "aa", "before", "2024-02-29T00:59:59.999+01:00", 6},
		{second, "zz", "end", "2024-03-01T01:00:00+01:00", 6},
		{second, "zz", "inside", "2024-03-01T00:59:59.999+01:00", 4},
		{second, "zz", "missing", "", 9},
	} {
		fields := map[string]string{}
		if r.when != "" {
			fields["created"] = r.when
		}
		payload, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.owner.node.New(&indexus.Item{Collection: directSDKCollection, Location: r.location, Id: r.id, Metrics: []float64{r.height}, Payload: payload}, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	runDirectSDK(t, "temporal_ranges_sdk.mjs", sdk, first, second)
}
