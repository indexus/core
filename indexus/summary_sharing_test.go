package indexus

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A waiting maintenance/first-read caller must be cancellable even when another
// goroutine already owns the inventory rebuild.
func TestInventoryRebuildWaitHonorsCancellation(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 5000)
	n.mesh.summary.InventoryMu.Lock()
	defer n.mesh.summary.InventoryMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := n.mesh.summary.Fold(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
