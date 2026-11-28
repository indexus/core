package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
)

// The two children are written on their respective peers. The SDK checks
// per-peer observations, including an empty leaf, without refreshing a zone.
func TestLocalChildrenSDKTwoColdPeers(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	for _, item := range []struct {
		location string
		owner    *runningDataNode
	}{{"aa", first}, {"zz", second}} {
		if err := item.owner.node.New(&indexus.Item{Collection: directSDKCollection, Location: item.location, Id: item.location, Metrics: []float64{1}}, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	cold := func() {
		t.Helper()
		for _, item := range []struct {
			location string
			owner    *runningDataNode
		}{{"@", first}, {"@", second}, {"a", first}, {"z", second}, {"aa", first}} {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, err := item.owner.node.ReadContext(ctx, directSDKCollection, item.location, indexus.ReadOptions{Mode: indexus.ReadCacheOnly, MaxAge: time.Minute})
			cancel()
			if !errors.Is(err, indexus.ErrReadCacheMiss) {
				t.Fatalf("expected cold %s, got %v", item.location, err)
			}
		}
	}
	cold()
	runDirectSDK(t, "local_children_sdk.mjs", sdk, first, second)
	cold()
}
