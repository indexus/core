package mesh

import (
	"context"
	"errors"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
	"testing"
	"time"
)

// Once a point is fenced it cannot be rolled back. A waiting ingress write
// follows its installed successor, refunds source admission and pays at target.
func TestCAP2_CutoverRefundsSourceAndChargesReceiver(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, nodes := meshFixture(t, 2)
	source, target := nodes[0], nodes[1]
	record := storage.Record{Collection: "0000000000000001", Location: "00", ID: "seed", Metrics: []float64{1}}
	if _, err := source.Write(ctx, "seed", record); err != nil {
		t.Fatal(err)
	}
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	right := state.Permissions.Native[0]
	offer := confirmedTransfer(t, source, target, right.ID, right.Region)
	id := offer.Cession.ID
	zones, err := source.Store.TransferZones(id)
	if err != nil || len(zones) == 0 {
		t.Fatalf("zones: %v %d", err, len(zones))
	}
	keys := []tree.ZoneKey{zones[0].Zone}
	done := source.beginCutover(keys)
	snaps, err := source.Store.FreezeZones(id, keys)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Store.AbortCausal(id); !errors.Is(err, storage.ErrNotReady) {
		t.Fatalf("fenced operation reopened: %v", err)
	}
	source.WriteRate, target.WriteRate = 1, 1
	written := make(chan error, 1)
	record.ID = "during-cutover"
	go func() {
		out, e := source.Handle(ctx, Request{Kind: "write", RequestID: record.ID, Record: &record})
		if e == nil && out.AppliedBy.Owner != target.Self.Owner {
			e = errors.New("not applied by target")
		}
		written <- e
	}()
	time.Sleep(20 * time.Millisecond)
	if err = target.Store.StageZones(id, source.Self.VNode, snaps); err != nil {
		t.Fatal(err)
	}
	proofs, err := source.Store.EmitZoneTransfers(id, keys)
	if err != nil {
		t.Fatal(err)
	}
	if err = target.Store.InstallZones(id, proofs); err != nil {
		t.Fatal(err)
	}
	if err = source.Store.AckZoneInstalls(id, keys); err != nil {
		t.Fatal(err)
	}
	source.endCutover(keys, done)
	if err = <-written; err != nil {
		t.Fatal(err)
	}
	source.writeCap.mu.Lock()
	sourceTokens := source.writeCap.tokens
	source.writeCap.mu.Unlock()
	target.writeCap.mu.Lock()
	targetTokens := target.writeCap.tokens
	target.writeCap.mu.Unlock()
	if sourceTokens < 1 || targetTokens >= 1 {
		t.Fatalf("admission source=%v target=%v", sourceTokens, targetTokens)
	}
	record.ID = "must-cap-target"
	if _, err = target.Write(ctx, record.ID, record); !errors.Is(err, storage.ErrWriteOverload) {
		t.Fatalf("receiver bypassed cap: %v", err)
	}
}
