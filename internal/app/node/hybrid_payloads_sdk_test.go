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
	}{
		{first, "aa", "a", "CAFÉ-crème café", 3},
		{first, "aa", "b", "crème café", 0},
		{second, "zz", "c", "café crème", 1},
		{second, "zz", "d", "port", 2},
	} {
		body, err := json.Marshal(map[string]string{"title": row.title})
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
	runDirectSDK(t, "hybrid_payloads_sdk.mjs", sdk, first, second)
	cold()
}
