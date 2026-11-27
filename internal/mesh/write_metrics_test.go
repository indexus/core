package mesh

import (
	"context"
	"testing"

	"github.com/indexus/go-indexus-core/internal/metrics"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// The fleet must see work at the holder exactly once, even when the client
// enters through another vnode and retries a durable request.
func TestAppliedMetricExcludesRelaysAndIdempotentRetries(t *testing.T) {
	// Not parallel: metrics are process-wide.
	_, source, target := streamFixture(t, 64)
	ctx := context.Background()
	if err := target.Join(ctx); err != nil {
		t.Fatal(err)
	}
	before := metrics.Writes.Applied.Load()
	r := storage.Record{Collection: "collection-00000", Location: "aa", ID: "holder-metric"}
	for range 2 {
		out, err := source.Handle(ctx, Request{Kind: "write", RequestID: "holder-metric", Record: &r})
		if err != nil || out.AppliedBy == source.Self {
			t.Fatalf("write must reach the holder: appliedBy=%v err=%v", out.AppliedBy, err)
		}
	}
	if got := metrics.Writes.Applied.Load() - before; got != 1 {
		t.Fatalf("new holder applications=%d, want one despite relay and retry", got)
	}
}
