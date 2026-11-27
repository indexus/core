package storage

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestFencePreparationAdmissionCoversQueuedWritesOnlyForItsZones(t *testing.T) {
	source, target, genesis := ticketStores(t)
	r := Record{Collection: "Demo000000000000", Location: "aa", ID: "base"}
	if _, err := source.Write("base", r); err != nil {
		t.Fatal(err)
	}
	op := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	zone, _ := recordZone(r)
	queued, err := newWriteRequest("queued-before-gate", r)
	if err != nil {
		t.Fatal(err)
	}
	release := source.beginFencePreparation([]tree.ZoneKey{zone})
	defer release()
	// A callback admitted before the gate appeared must recheck under writer.
	// A sibling zone in its same disk transaction must still be committed.
	sibling := r
	sibling.Location, sibling.ID = "bb", "sibling"
	other, err := newWriteRequest("sibling", sibling)
	if err != nil {
		t.Fatal(err)
	}
	var refused, accepted writeOutcome
	if err = source.update(func(tx *Tx) error {
		if err := source.applyWrite(tx, queued, &refused); err != nil {
			return err
		}
		return source.applyWrite(tx, other, &accepted)
	}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(refused.refuse, ErrFrozen) || refused.applied || !accepted.applied {
		t.Fatalf("refused=%+v sibling=%+v", refused, accepted)
	}
	if _, err = source.Write("new-during-prepare", r); !errors.Is(err, ErrFrozen) {
		t.Fatalf("pre-admission did not refuse: %v", err)
	}
	prepared, err := source.prepareFence(op.Cession.ID, []tree.ZoneKey{zone})
	if err != nil {
		t.Fatal(err)
	}
	if err = source.updateCausal(func(tx *Tx, state CausalState) error {
		return source.freezePreparedTx(tx, state, op.Cession.ID, prepared)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFencePreparationAdmissionRefcountsAndFailureCleanup(t *testing.T) {
	source, _, _ := ticketStores(t)
	r := Record{Collection: "Demo000000000000", Location: "aa", ID: "after-failed-prepare"}
	zone, _ := recordZone(r)
	first := source.beginFencePreparation([]tree.ZoneKey{zone})
	second := source.beginFencePreparation([]tree.ZoneKey{zone})
	first()
	if !source.fenceAdmission.contains(zone) {
		t.Fatal("one overlapping caller released the other's gate")
	}
	second()
	if source.fenceAdmission.contains(zone) {
		t.Fatal("gate leaked after both callers exited")
	}
	if _, _, _, err := source.FenceZones(causal.TransferID{}, []tree.ZoneKey{zone}); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if source.fenceAdmission.contains(zone) {
		t.Fatal("failed fence leaked its transient gate")
	}
	if _, err := source.Write("after-failed-prepare", r); err != nil {
		t.Fatalf("transient gate became authority: %v", err)
	}
}

func TestFencePreparationAdmissionDrainsInFlightWriter(t *testing.T) {
	source, _, _ := ticketStores(t)
	zone, _ := tree.NewZoneKey("Demo000000000000", "aa")
	source.writeMu.Lock()
	locked := true
	defer func() {
		if locked {
			source.writeMu.Unlock()
		}
	}()
	entered := make(chan func(), 1)
	go func() { entered <- source.beginFencePreparation([]tree.ZoneKey{zone}) }()
	deadline := time.Now().Add(5 * time.Second)
	for !source.fenceAdmission.contains(zone) {
		if time.Now().After(deadline) {
			t.Fatal("preparation did not publish admission gate")
		}
		runtime.Gosched()
	}
	select {
	case release := <-entered:
		release()
		t.Fatal("preparation bypassed an in-flight writer")
	default:
	}
	source.writeMu.Unlock()
	locked = false
	select {
	case release := <-entered:
		release()
	case <-time.After(5 * time.Second):
		t.Fatal("preparation did not finish draining the writer")
	}
	if source.fenceAdmission.contains(zone) {
		t.Fatal("drained preparation leaked its gate")
	}
}
