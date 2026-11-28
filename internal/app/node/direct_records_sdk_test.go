package node

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
)

// INDEXUS_TEST_SDK points to a checkout exposing API.getDirectRecords. Both
// owners serve real HTTP; the SDK process checks its own requests and replies.
func TestDirectRecordsSDKTwoColdPeers(t *testing.T) {
	sdk := os.Getenv("INDEXUS_TEST_SDK")
	if sdk == "" {
		t.Skip("set INDEXUS_TEST_SDK to the JS SDK checkout")
	}
	sdk, err := filepath.Abs(sdk)
	if err != nil {
		t.Fatal(err)
	}
	const collection = "Demo000000000000"
	start := func(location string, bootstrap *runningDataNode) *runningDataNode {
		t.Helper()
		id, err := indexus.ZoneID(collection, location)
		if err != nil {
			t.Fatal(err)
		}
		return startDataNode(t, indexus.BASE64.Encode(id.Bytes()), bootstrap)
	}
	first := start("aa", nil)
	second := start("zz", first)
	for _, row := range []struct {
		location, id string
		metric       float64
	}{
		{"aa", "a", 1}, {"aa", "b", 2}, {"aaa", "descendant", 3}, {"aa", "gone", 4}, {"zz", "z", 5},
	} {
		item := &indexus.Item{Collection: collection, Location: row.location, Id: row.id, Metrics: []float64{row.metric}, RequestID: "put-" + row.id}
		if err := first.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
		if row.id == "gone" {
			item.RequestID = "delete-" + row.id
			if err := first.node.Delete(item, "@", nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "testdata/direct_records_sdk.mjs", sdk, collection, strconv.Itoa(first.port), strconv.Itoa(second.port))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", out)
}
