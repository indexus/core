package mesh

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestPhysicalMoveRefusesIncomingPointsBeforeClosingIntake(t *testing.T) {
	net, nodes := meshFixture(t, 2)
	source, receiver := nodes[0], nodes[1]
	ctx := context.Background()
	const collection = "Demo000000000000"
	first, _ := tree.NewZoneKey(collection, "aa")
	second, _ := tree.NewZoneKey(collection, "ab")
	for i, zone := range []tree.ZoneKey{first, second} {
		row := storage.Record{Collection: collection, Location: zone.Location, ID: "seed"}
		if _, err := source.Store.Write(string(rune('a'+i)), row); err != nil {
			t.Fatal(err)
		}
	}
	input := installedReadRight(t, source, tree.ID{})
	offer := confirmedTransfer(t, source, receiver, input.ID, tree.Region{})
	installPoint := func(zone tree.ZoneKey) {
		t.Helper()
		snaps, err := source.Store.FreezeZones(offer.Cession.ID, []tree.ZoneKey{zone})
		if err != nil {
			t.Fatal(err)
		}
		if err := receiver.Store.StageZones(offer.Cession.ID, source.Self.VNode, snaps); err != nil {
			t.Fatal(err)
		}
		grants, err := source.Store.EmitZoneTransfers(offer.Cession.ID, []tree.ZoneKey{zone})
		if err != nil {
			t.Fatal(err)
		}
		if err := receiver.Store.InstallZones(offer.Cession.ID, grants); err != nil {
			t.Fatal(err)
		}
		if err := source.Store.AckZoneInstalls(offer.Cession.ID, []tree.ZoneKey{zone}); err != nil {
			t.Fatal(err)
		}
	}
	installPoint(first)
	if err := receiver.publishActivation(); err != nil {
		t.Fatal(err)
	}
	acked := storage.Record{Collection: collection, Location: first.Location, ID: "ACK-before-physical-move"}
	if _, err := receiver.Store.Write(acked.ID, acked); err != nil {
		t.Fatal(err)
	}
	state, _ := receiver.Store.CausalState()
	if len(state.Permissions.Native) != 0 {
		t.Fatal("fixture unexpectedly consolidated native permission")
	}
	disk, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "future"), receiver.Self.VNode, "test", 1<<26, testStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { disk.Close() })
	endpoint, _ := disk.Endpoint("future-receiver-host")
	future := &Service{Store: disk, Self: endpoint, VNode: receiver.VNode, Peers: net}
	t.Cleanup(future.Close)
	if err := disk.InitCausalReceiver(state.Root, state.Genesis); err != nil {
		t.Fatal(err)
	}
	net.mu.Lock()
	net.nodes[endpoint.Host] = future
	net.mu.Unlock()
	if err := receiver.MoveTo(ctx, endpoint); !errors.Is(err, storage.ErrNotReady) {
		t.Fatalf("physical move admitted unconsolidated incoming points: %v", err)
	}
	state, err = receiver.Store.CausalState()
	if err != nil || state.Moving != nil {
		t.Fatalf("refused move nevertheless closed incoming admission: Moving=%+v err=%v", state.Moving, err)
	}
	// In particular, a second staged point must not now fail receiverClosed.
	installPoint(second)
	if _, err := source.Store.FreezeRegion(offer.Cession.ID); err != nil {
		t.Fatal(err)
	}
	sealed, err := source.Store.SealCausalRegion(offer.Cession.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.Store.StageCausal(sealed); err != nil {
		t.Fatal(err)
	}
	cession, err := source.Store.EmitCausal(offer.Cession.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.Store.InstallCausal(cession); err != nil {
		t.Fatal(err)
	}
	if err := source.Store.CompleteCausal(cession.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := receiver.QueryPrefixMetrics(ctx, collection, "@")
	if err != nil {
		t.Fatal(err)
	}
	got := rowSet(t, rows, collection, "@")
	if len(got) != 3 {
		t.Fatalf("incoming completion lost an ACK after refused move: %v", got)
	}
	// The same physical move is admissible once all actual incoming rights are
	// consolidated. No permanent ban on moving a recently joined vnode is needed.
	if err := receiver.MoveTo(ctx, endpoint); err != nil {
		t.Fatalf("completed incoming transfer still blocks physical move: %v", err)
	}
	if _, _, err := receiver.Store.Apply("old-slot", acked); !errors.Is(err, storage.ErrNotOwner) {
		t.Fatalf("old physical slot still writes: %v", err)
	}
	if _, _, err := future.Store.Apply("new-slot", acked); err != nil {
		t.Fatalf("future slot did not acquire permission: %v", err)
	}
}
