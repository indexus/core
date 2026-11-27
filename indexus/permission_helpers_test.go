package indexus

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// freezeWithConfirmedReceiver uses a real receiver reservation and durable
// preload before the source fence. Read tests must not bypass admission by
// fabricating a source confirmation or freezing an unreserved transfer.
func freezeWithConfirmedReceiver(t *testing.T, source *storage.Store, target causal.Endpoint) causal.TransferID {
	t.Helper()
	state, err := source.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Permissions.Native) == 0 {
		t.Fatal("test source has no installed native right")
	}
	// This fixture needs an ordinary receiver, not a physical relocation of
	// the source vnode (which requires a new endpoint epoch and Move admission).
	if target.VNode == state.Genesis.Holder.VNode {
		target.VNode = "receiver"
	}
	receiver, err := storage.Open(filepath.Join(t.TempDir(), "receiver"), target.VNode, state.Genesis.Cluster, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	target, err = receiver.Endpoint(target.Host)
	if err != nil {
		t.Fatal(err)
	}
	if err = receiver.InitCausalReceiver(state.Root, state.Genesis); err != nil {
		t.Fatal(err)
	}
	op, err := source.BeginCausal(context.Background(), state.Permissions.Native[0].ID, target, tree.Region{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := receiver.ReserveCausal(op.Cession)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.BindCausal(c.ID, c.Ticket); err != nil {
		t.Fatal(err)
	}
	op.Cession = c
	if _, err = receiver.PreloadCausal(op); err != nil {
		t.Fatal(err)
	}
	if err = source.ConfirmCausal(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = source.ForceFreezeRegion(c.ID); err != nil {
		t.Fatal(err)
	}
	return c.ID
}
