package indexus

import (
	"context"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestWriteACKDoesNotWaitForGlobalViewRefreshOrSummaryLock(t *testing.T) {
	root := VNodeDescriptor{ID: ID{255}}
	root.Name = BASE64.Encode(root.ID.Bytes())
	f := newReadPhaseFixture(t, []VNodeDescriptor{root}, false, true)
	n := f.nodes[0]
	current := n.mesh.summary.View()
	stale, err := tree.NewViewFrom(current.Generation+1, current.Active)
	if err != nil {
		t.Fatal(err)
	}
	n.mesh.summary.SetView(stale, true, false)
	n.mesh.summary.Mu.Lock()
	locked := true
	defer func() {
		if locked {
			n.mesh.summary.Mu.Unlock()
		}
	}()
	// The old post-commit refresh would try to repush every boundary under Mu
	// before ACKing this durable write, even though this disk owns the record.
	done := make(chan error, 1)
	go func() {
		record := storage.Record{Collection: "Demo000000000000", Location: "A", ID: "durable"}
		result, err := n.MeshData(context.Background(), mesh.Request{Kind: "write", RequestID: "write", Record: &record})
		if err == nil && !result.Applied {
			err = storage.ErrNotReady
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		n.mesh.summary.Mu.Unlock()
		locked = false
		<-done
		t.Fatal("write ACK waited for derived global maintenance")
	}
	if n.mesh.summary.View().Digest != stale.Digest {
		t.Fatal("ACK synchronously refreshed the global view")
	}
	n.mesh.summary.Mu.Unlock()
	locked = false
	if err := n.meshRefreshViewIfChanged(); err != nil {
		t.Fatal(err)
	}
	if n.mesh.summary.View().Digest != current.Digest {
		t.Fatal("ordinary maintenance did not refresh routing")
	}
}
