package node

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
)

const directSDKCollection = "Demo000000000000"

func directSDKPeers(t *testing.T) (string, *runningDataNode, *runningDataNode) {
	t.Helper()
	sdk := os.Getenv("INDEXUS_TEST_SDK")
	if sdk == "" {
		t.Skip("set INDEXUS_TEST_SDK to the JS SDK checkout")
	}
	sdk, err := filepath.Abs(sdk)
	if err != nil {
		t.Fatal(err)
	}
	start := func(location string, bootstrap *runningDataNode) *runningDataNode {
		t.Helper()
		id, err := indexus.ZoneID(directSDKCollection, location)
		if err != nil {
			t.Fatal(err)
		}
		return startDataNode(t, indexus.BASE64.Encode(id.Bytes()), bootstrap)
	}
	first := start("aa", nil)
	second := start("zz", first)
	return sdk, first, second
}

func runDirectSDK(t *testing.T, script, sdk string, first, second *runningDataNode, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmdArgs := []string{"testdata/" + script, sdk, directSDKCollection, strconv.Itoa(first.port), strconv.Itoa(second.port)}
	cmd := exec.CommandContext(ctx, "node", append(cmdArgs, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// INDEXUS_TEST_SDK points to a checkout exposing API.getDirectRecords. Both
// owners serve real HTTP; the SDK process checks its own requests and replies.
func TestDirectRecordsSDKTwoColdPeers(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	for _, row := range []struct {
		location, id string
		metric       float64
	}{
		{"aa", "a", 1}, {"aa", "b", 2}, {"aaa", "descendant", 3}, {"aa", "gone", 4}, {"zz", "z", 5},
	} {
		item := &indexus.Item{Collection: directSDKCollection, Location: row.location, Id: row.id, Metrics: []float64{row.metric}, RequestID: "put-" + row.id}
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
	runDirectSDK(t, "direct_records_sdk.mjs", sdk, first, second)
}

// A fixed, nullable numeric column spans both owners. The independent
// JS oracle below is a hand-calculated answer for these immutable rows.
func TestDirectSDKFacetsAndOrderTwoPeers(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	for _, row := range []struct {
		location, id string
		metrics      []float64
	}{
		{"aa", "same", []float64{1, 4}}, {"aa", "m", []float64{1}}, {"aa", "b", []float64{1, 9}}, {"aa", "excluded", []float64{0, 99}},
		{"zz", "same", []float64{1, 4}}, {"zz", "a", []float64{1, 4}}, {"zz", "z", []float64{1}}, {"zz", "c", []float64{1, 9}},
		{"aaa", "descendant", []float64{1, 100}},
	} {
		item := &indexus.Item{Collection: directSDKCollection, Location: row.location, Id: row.id, Metrics: row.metrics, RequestID: "put-" + row.location + "-" + row.id}
		if err := first.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	cold := func() {
		t.Helper()
		for _, owner := range []struct {
			location string
			peer     *runningDataNode
		}{{"aa", first}, {"zz", second}} {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, err := owner.peer.node.ReadContext(ctx, directSDKCollection, owner.location, indexus.ReadOptions{Mode: indexus.ReadCacheOnly, MaxAge: time.Minute})
			cancel()
			if !errors.Is(err, indexus.ErrReadCacheMiss) {
				t.Fatalf("expected cold zone %s, got %v", owner.location, err)
			}
		}
	}
	cold()
	runDirectSDK(t, "direct_facets_sdk.mjs", sdk, first, second)
	cold()
}
