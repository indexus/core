package node

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/indexus"
)

// The fixture is immutable once populated. Each ID occurs at one address.
func TestHybridPayloadsSDKTwoColdPeers(t *testing.T) {
	sdk, first, second := directSDKPeers(t)
	for _, row := range []struct {
		owner               *runningDataNode
		location, id, title string
		distance            float64
		sparse              [][2]int
	}{
		{first, "aa", "a", "CAFÉ-crème café", 3, [][2]int{{8, 2}, {1024, 3}}},
		{first, "aa", "b", "crème café", 0, [][2]int{{8, -1}}},
		{second, "zz", "c", "café crème", 1, [][2]int{{5, 7}}},
		{second, "zz", "d", "port", 2, [][2]int{{1024, 1}}},
	} {
		body, err := json.Marshal(map[string]any{"title": row.title, "sparse": row.sparse, "caption": "😀 ÉCOLE, e\u0301cole!"})
		if err != nil {
			t.Fatal(err)
		}
		item := &indexus.Item{Collection: directSDKCollection, Location: row.location, Id: row.id, Metrics: []float64{row.distance, 0}, Payload: body, RequestID: "hybrid-" + row.id}
		if err := row.owner.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	cold := func() {
		t.Helper()
		for _, p := range []struct {
			owner    *runningDataNode
			location string
		}{{first, "aa"}, {second, "zz"}} {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, err := p.owner.node.ReadContext(ctx, directSDKCollection, p.location, indexus.ReadOptions{Mode: indexus.ReadCacheOnly, MaxAge: time.Minute})
			cancel()
			if !errors.Is(err, indexus.ErrReadCacheMiss) {
				t.Fatalf("expected cold %s, got %v", p.location, err)
			}
		}
	}
	cold()
	for _, mode := range []string{"direct", "roots"} {
		runDirectSDK(t, "hybrid_payloads_sdk.mjs", sdk, first, second, mode)
		cold()
	}
}
