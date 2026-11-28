package node

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestLocalChildrenSDKResumesAndRevisitsTwoHTTPPeers(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	put := func(peer *runningDataNode, location, id string) {
		t.Helper()
		if err := peer.node.New(&indexus.Item{Collection: directSDKCollection, Location: location, Id: id, Metrics: []float64{1}}, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	put(first, "aa", "first")
	put(second, "zz", "second")
	checkpoint := filepath.Join(t.TempDir(), "frontier.json")
	run := func(phase string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "node", "testdata/frontier_sdk.mjs", sdk, directSDKCollection, strconv.Itoa(first.port), strconv.Itoa(second.port), checkpoint, phase)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("phase %s: %v\n%s", phase, err, out)
		}
		t.Logf("%s", out)
	}
	run("pause")
	run("resume")
	put(first, "aa0", "added-after-pass")
	run("revisit")
}
