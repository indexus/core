package mesh

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestACKRoutesOnlyInstalledSpaceBeforeAndAfterNativeInstall(t *testing.T) {
	net, source, target := streamFixture(t, 64)
	ctx := context.Background()
	var checked atomic.Bool
	var point tree.ZoneKey
	net.setAfter(func(r Request) error {
		if r.Kind != "install-zone" || len(r.ZoneTransfers) == 0 || !checked.CompareAndSwap(false, true) {
			return nil
		}
		point = r.ZoneTransfers[0].Zone
		record := storage.Record{Collection: point.Collection, Location: point.Location, ID: "early-ACK"}
		response, err := target.Handle(ctx, Request{Kind: "write", RequestID: "early-ACK", Record: &record})
		if err != nil {
			return err
		}
		if response.AppliedRegion == nil || *response.AppliedRegion != tree.Point(point.ID) {
			t.Errorf("point ACK advertised uninstalled neighbors: %+v", response.AppliedRegion)
		}
		return nil
	})
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if err := source.handoff(ctx, Request{Kind: "handoff", Input: state.Genesis.Right().ID, Target: target.Self, Region: tree.Region{}}); err != nil {
		t.Fatal(err)
	}
	if !checked.Load() {
		t.Fatal("no point installation exercised")
	}
	record := storage.Record{Collection: point.Collection, Location: point.Location, ID: "native-ACK"}
	response, err := target.Handle(ctx, Request{Kind: "write", RequestID: "native-ACK", Record: &record})
	if err != nil {
		t.Fatal(err)
	}
	if response.AppliedBy != target.Self || response.AppliedRegion == nil || *response.AppliedRegion != (tree.Region{}) {
		t.Fatalf("native ACK did not announce its empty space: %+v", response)
	}
	if _, err := source.Store.Write("stale", record); err == nil {
		t.Fatal("route change reopened source authority")
	}
	snap, err := target.Store.Snapshot(point)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Records) != 3 {
		t.Fatalf("early ACK lost during consolidation: %d records", len(snap.Records))
	}
}
