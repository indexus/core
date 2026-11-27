package mesh

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/testkit"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestHandoffGrainMovesFreshAddressesBeforeWholeJoin(t *testing.T) {
	net, source, target := streamFixture(t, 1024)
	ctx := context.Background()
	state, _ := source.Store.CausalState()
	request := Request{Kind: "handoff", Subdivide: true, Input: state.Genesis.Right().ID, Target: target.Self, Region: tree.Region{}}
	var transferred causal.Cession
	net.setBefore(func(r Request) error {
		if r.Transfer != nil {
			transferred = r.Transfer.Cession
		}
		return nil
	})
	source.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if r.Kind == KindMembership {
			return Response{}, fmt.Errorf("control deliberately blocked")
		}
		return net.Exchange(ctx, host, r)
	})
	ctx, cancel := context.WithTimeout(ctx, testkit.RaceScale(5*time.Second))
	defer cancel()
	if err := source.handoff(ctx, request); err != nil {
		t.Fatal(err)
	}
	after, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	tr := after.Transfers[transferred.ID]
	grain := tr.Cession.Region
	if grain == request.Region || tr.Phase != causal.TransferInstalled {
		t.Fatal("large region did not close in a smaller grain")
	}
	// A previously absent address in this geometric grain must already be writable
	// at R while at least one original zone remains authoritative at S.
	// Build a canonical name inside the selected prefix instead of assuming
	// the prefix spans unrelated collection names. The root location leaves
	// the encoded address intact and is absent from this leaf-only fixture.
	fresh := storage.Record{Collection: tree.EncodeName(grain.Value), Location: tree.BASE64.Root(), ID: "fresh"}
	freshZone, err := tree.NewZoneKey(fresh.Collection, fresh.Location)
	if err != nil || !grain.Contains(freshZone.ID) {
		t.Fatalf("fresh address outside grain: %v %v", freshZone, err)
	}
	if fresh.Collection == "collection-00000" {
		t.Fatal("fixture selected an existing collection")
	}
	if _, err := target.Store.Write("fresh", fresh); err != nil {
		t.Fatalf("fresh authority was not delegated: %v", err)
	}
	if _, err := source.Store.Write("duplicate", fresh); err == nil {
		t.Fatal("source can still write delegated empty space")
	}
	if holder, err := source.Store.ZoneHolder(freshZone); err != nil || holder != target.Self {
		t.Fatalf("fresh route: %v %v", holder, err)
	}
	// The persisted unique operation retries its exact grain, then may forget
	// its receipt without recreating a right or remounting old bytes.
	request.Operation = transferred.ID
	beforeID := tr.Cession.Right().ID
	if err := source.handoff(ctx, request); err != nil {
		t.Fatal(err)
	}
	rights, err := target.Store.Rights(grain)
	if err != nil {
		t.Fatal(err)
	}
	if len(rights) != 1 || rights[0].ID != beforeID {
		t.Fatal("retry changed installed grain")
	}
	cancel()
	finishCtx, finishCancel := context.WithTimeout(context.Background(), testkit.RaceScale(30*time.Second))
	defer finishCancel()
	if err := target.Join(finishCtx); err != nil {
		t.Fatalf("join did not acquire residual grants: %v", err)
	}
	rows, err := target.Store.Inventory(false)
	if err != nil || len(rows) != 1025 {
		t.Fatalf("completed join inventory: %d %v", len(rows), err)
	}

}

func TestHandoffWithoutSubdivisionKeepsWholeRequestedRegion(t *testing.T) {
	_, source, target := streamFixture(t, 1024)
	state, _ := source.Store.CausalState()
	region := tree.Region{}
	if err := source.handoff(context.Background(), Request{Kind: "handoff", Input: state.Genesis.Right().ID, Target: target.Self, Region: region}); err != nil {
		t.Fatal(err)
	}
	rights, err := target.Store.Rights(region)
	if err != nil {
		t.Fatal(err)
	}
	if len(rights) != 1 || rights[0].Region != region {
		t.Fatal("changed full-piece handoff contract")
	}
}

func TestPreparedEmptyPreloadDoesNotGrantWritesOrBuildKnowledge(t *testing.T) {
	_, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	state, _ := source.Store.CausalState()
	offer := confirmedTransfer(t, source, target, state.Genesis.Right().ID, tree.Region{})
	offer = wireTransfer(offer)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := target.Handle(ctx, Request{Kind: "preload", Transfer: &offer})
	if err != nil || out.Membership != nil {
		t.Fatalf("prepared preload: %+v %v", out, err)
	}
	record := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "not-installed"}
	if _, err = target.Store.Write(record.ID, record); err == nil {
		t.Fatal("preparation granted write authority")
	}
	offer.Cession.Input.ID = causal.Hash("unknown")
	if _, err = target.Handle(ctx, Request{Kind: "preload", Transfer: &offer}); err == nil {
		t.Fatal("changed admission binding accepted")
	}
}
