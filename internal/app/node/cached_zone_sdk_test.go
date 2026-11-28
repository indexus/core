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

// Opt-in cross-repository check. INDEXUS_TEST_SDK points to the JS SDK checkout.
// The three JS invocations run in separate processes; only the client's JSON
// file connects them. Two live, joined Go HTTP servers share the fixture.
func TestCachedZoneSDKTwoPeers(t *testing.T) {
	sdk := os.Getenv("INDEXUS_TEST_SDK")
	if sdk == "" {
		t.Skip("set INDEXUS_TEST_SDK to the JS SDK checkout")
	}
	sdk, err := filepath.Abs(sdk)
	if err != nil {
		t.Fatal(err)
	}
	const collection = "Demo000000000000"
	locations := []string{"aa", "zz"}
	var nodes []*runningDataNode
	for i, prefix := range locations {
		id, err := indexus.ZoneID(collection, prefix)
		if err != nil {
			t.Fatal(err)
		}
		var bootstrap *runningDataNode
		if i > 0 {
			bootstrap = nodes[0]
		}
		nodes = append(nodes, startDataNode(t, indexus.BASE64.Encode(id.Bytes()), bootstrap))
	}
	for i, prefix := range locations {
		if err := nodes[i].node.New(&indexus.Item{Collection: collection, Location: prefix, Id: prefix, Metrics: []float64{float64(i + 1)}}, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(t.TempDir(), "progress.json")
	run := func(phase string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "node", "testdata/cached_zone_checkpoint.mjs", sdk, state, phase, collection, strconv.Itoa(nodes[0].port), strconv.Itoa(nodes[1].port))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", phase, err, out)
		}
		t.Logf("%s: %s", phase, out)
	}
	run("cold")
	// Only the fixture primes the two caches, on their respective owners.
	for i, prefix := range locations {
		_, s, err := nodes[i].node.GetSetContext(context.Background(), collection, prefix, true, nil, true)
		if err != nil || s == nil || s.Count() != 1 {
			t.Fatalf("warm %s: %v %v", prefix, s, err)
		}
	}
	run("first")
	run("resume")
}
