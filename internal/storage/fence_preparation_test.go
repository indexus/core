package storage

import (
	"errors"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// Drive the exact optimistic gap deterministically, without a timing hook or
// sleeps: an ACK lands after the MVCC read but before the writer transaction.
func TestFencePreparationRejectsConcurrentACK(t *testing.T) {
	source, target, genesis := ticketStores(t)
	r := Record{Collection: "Demo000000000000", Location: "aa", ID: "first", Metrics: []float64{1}}
	if _, err := source.Write("first", r); err != nil {
		t.Fatal(err)
	}
	op := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	zone, _ := recordZone(r)
	prepared, err := source.prepareFence(op.Cession.ID, []tree.ZoneKey{zone})
	if err != nil {
		t.Fatal(err)
	}
	r.ID = "ack-in-gap"
	if _, err = source.Write("ack-in-gap", r); err != nil {
		t.Fatal(err)
	}
	err = source.updateCausal(func(tx *Tx, state CausalState) error {
		return source.freezePreparedTx(tx, state, op.Cession.ID, prepared)
	})
	if !errors.Is(err, errFenceChanged) {
		t.Fatalf("stale preparation accepted: %v", err)
	}
	if err = source.view(func(tx *Tx) error {
		st, _, err := zoneStateOf(tx, CausalState{}, op.Cession.ID, zone)
		if st.Frozen {
			t.Fatal("failed preparation committed a fence")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snaps, _, proofs, err := source.FenceZones(op.Cession.ID, []tree.ZoneKey{zone})
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps[0].Records) != 2 || snaps[0].Seq != prepared[0].snap.Seq+1 {
		t.Fatalf("ACK omitted: %+v", snaps[0])
	}
	seal, err := snaps[0].Seal()
	if err != nil {
		t.Fatal(err)
	}
	if [32]byte(proofs[0].DataDigest) != seal.Digest {
		t.Fatal("proof does not bind current content")
	}
	if err = target.MountZones(op.Cession.ID, source.node, nil, nil, snaps, proofs); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Write("after-fence", r); err == nil {
		t.Fatal("source still writable")
	}
	r.ID = "receiver"
	if _, err = target.Write("receiver", r); err != nil {
		t.Fatal(err)
	}
}

func TestFencedSnapshotsRequireSavedFencedContent(t *testing.T) {
	source, target, genesis := ticketStores(t)
	r := Record{Collection: "Demo000000000000", Location: "aa", ID: "first"}
	if _, err := source.Write("first", r); err != nil {
		t.Fatal(err)
	}
	op := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	zone, _ := recordZone(r)
	zones := []tree.ZoneKey{zone}
	if _, err := source.ReadFencedLot(op.Cession.ID, zones); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unfenced snapshot returned: %v", err)
	}
	if _, _, _, err := source.FenceZones(op.Cession.ID, zones); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ReadFencedLot(op.Cession.ID, zones); err != nil {
		t.Fatal(err)
	}
	path := transferRows(op.Cession.ID) + element(zoneBytes(zone))
	if err := source.update(func(tx *Tx) error { return tx.put(path, []byte("corrupt")) }); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ReadFencedLot(op.Cession.ID, zones); err == nil {
		t.Fatal("corruption hidden by a live snapshot")
	}
	if err := source.update(func(tx *Tx) error { return tx.del(path) }); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ReadFencedLot(op.Cession.ID, zones); !errors.Is(err, ErrNotReady) {
		t.Fatalf("missing durable snapshot reconstructed: %v", err)
	}
}

func TestFencePreparationConflictsRollBackWholeLot(t *testing.T) {
	source, target, genesis := ticketStores(t)
	zones := make([]tree.ZoneKey, 2)
	for i, loc := range []string{"aa", "bb"} {
		r := Record{Collection: "Demo000000000000", Location: loc, ID: loc}
		if _, err := source.Write(loc, r); err != nil {
			t.Fatal(err)
		}
		zones[i], _ = recordZone(r)
	}
	op := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	prepared, err := source.prepareFence(op.Cession.ID, zones)
	if err != nil {
		t.Fatal(err)
	}
	// Only the second zone changes. The transaction visits the first one
	// before detecting the conflict, so its provisional fence must roll back.
	r := Record{Collection: zones[1].Collection, Location: zones[1].Location, ID: "late"}
	if _, err = source.Write("late", r); err != nil {
		t.Fatal(err)
	}
	err = source.updateCausal(func(tx *Tx, state CausalState) error {
		return source.freezePreparedTx(tx, state, op.Cession.ID, prepared)
	})
	if !errors.Is(err, errFenceChanged) {
		t.Fatalf("expected conflict: %v", err)
	}
	if err := source.view(func(tx *Tx) error {
		for _, zone := range zones {
			st, _, err := zoneStateOf(tx, CausalState{}, op.Cession.ID, zone)
			if err != nil {
				return err
			}
			if st.Frozen {
				t.Fatal("partial fence escaped rollback")
			}
			if _, err := fencedSnapshot(tx, op.Cession.ID, zone); !errors.Is(err, ErrNotReady) {
				t.Fatalf("partial snapshot escaped rollback: %v", err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFencePreparationIgnoresUnrelatedWrites(t *testing.T) {
	source, target, genesis := ticketStores(t)
	r := Record{Collection: "Demo000000000000", Location: "aa", ID: "base"}
	if _, err := source.Write("base", r); err != nil {
		t.Fatal(err)
	}
	op := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	zone, _ := recordZone(r)
	prepared, err := source.prepareFence(op.Cession.ID, []tree.ZoneKey{zone})
	if err != nil {
		t.Fatal(err)
	}
	r.Location, r.ID = "bb", "unrelated"
	if _, err = source.Write("unrelated", r); err != nil {
		t.Fatal(err)
	}
	if err = source.updateCausal(func(tx *Tx, state CausalState) error {
		return source.freezePreparedTx(tx, state, op.Cession.ID, prepared)
	}); err != nil {
		t.Fatalf("unrelated zone invalidated fence: %v", err)
	}
}

// A real power-loss image is restored after the source's durable fence but
// before the first install RPC. Neither route state nor volatile snapshots
// may be necessary to resend exactly the decided bytes and permission.
func TestFencedLotPowerLossBeforeFirstDelivery(t *testing.T) {
	schedule := newHandoffSchedule(t, 251125, 2)
	source, target := schedule.nodes[0].store, schedule.nodes[1].store
	zone := schedule.zones[0]
	rights, err := source.Rights(tree.Point(zone.ID))
	if err != nil || len(rights) != 1 {
		t.Fatalf("source right: %+v %v", rights, err)
	}
	r := Record{Collection: zone.Collection, Location: zone.Location, ID: "acked-before-power-loss"}
	ack, err := source.Write(r.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	op := ticketAdmission(t, source, target, rights[0].ID, rights[0].Region)
	zones := []tree.ZoneKey{zone}
	snaps, _, proofs, err := source.FenceZones(op.Cession.ID, zones)
	if err != nil {
		t.Fatal(err)
	}
	before, err := snaps[0].Seal()
	if err != nil {
		t.Fatal(err)
	}
	schedule.crash(0)
	schedule.open(0)
	source = schedule.nodes[0].store
	recovered, err := source.ReadFencedLot(op.Cession.ID, zones)
	if err != nil {
		t.Fatal(err)
	}
	after, err := recovered.Snapshots[0].Seal()
	if err != nil || after != before || recovered.Seals[0] != before || recovered.Proofs[0] != proofs[0] {
		t.Fatalf("fence changed on recovery: %+v %+v %v", before, after, err)
	}
	replayed, _, replayProofs, err := source.FenceZones(op.Cession.ID, zones)
	if err != nil {
		t.Fatal(err)
	}
	replaySeal, err := replayed[0].Seal()
	if err != nil || replaySeal != before || replayProofs[0] != proofs[0] {
		t.Fatalf("fence replay changed decision: %v", err)
	}
	if _, err = source.Write("must-stay-fenced", r); err == nil {
		t.Fatal("power loss restored source write authority")
	}
	if err = target.MountZones(op.Cession.ID, source.node, nil, nil, recovered.Snapshots, replayProofs); err != nil {
		t.Fatal(err)
	}
	got, err := target.Write(r.ID, r)
	if err != nil || got.Seq != ack.Seq {
		t.Fatalf("pre-fence ACK receipt lost: %+v %v", got, err)
	}
	r.ID = "receiver-ack-after-install"
	receiverACK, err := target.Write(r.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	if err = target.MountZones(op.Cession.ID, source.node, nil, nil, recovered.Snapshots, replayProofs); err != nil {
		t.Fatal(err)
	}
	repeated, err := target.Write(r.ID, r)
	if err != nil || repeated.Seq != receiverACK.Seq {
		t.Fatalf("replayed install lost later ACK: %+v %v", repeated, err)
	}
}

// FenceZones hands out the seal of each fenced snapshot, the one its proof
// binds: the source sends it as it is and does not hash the lot again while
// the lot has no writer. The receiver certifies its preloaded copy against
// exactly these seals, and a replay of the fence returns the same ones.
func TestFenceZonesReturnsTheSealOfEachSnapshot(t *testing.T) {
	source, target, genesis := ticketStores(t)
	var zones []tree.ZoneKey
	for i, location := range []string{"aa", "ab", "ac"} {
		r := Record{Collection: "Demo000000000000", Location: location, ID: "r", Metrics: []float64{float64(i)}}
		if _, err := source.Write("w"+location, r); err != nil {
			t.Fatal(err)
		}
		zone, _ := recordZone(r)
		zones = append(zones, zone)
	}
	op := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	offer := op
	for _, zone := range zones {
		base, err := source.Snapshot(zone)
		if err != nil {
			t.Fatal(err)
		}
		offer.Snapshots = append(offer.Snapshots, base)
	}
	if _, err := target.PreloadCausal(offer); err != nil {
		t.Fatal(err)
	}
	snaps, seals, proofs, err := source.FenceZones(op.Cession.ID, zones)
	if err != nil || len(seals) != len(zones) || len(snaps) != len(zones) {
		t.Fatalf("fence: %d snapshots, %d seals, %v", len(snaps), len(seals), err)
	}
	for i, snap := range snaps {
		want, err := snap.Seal()
		if err != nil || seals[i] != want {
			t.Fatalf("seal %d is %+v, its snapshot seals as %+v (%v)", i, seals[i], want, err)
		}
		if seals[i].Zone != zones[i] || [32]byte(proofs[i].DataDigest) != seals[i].Digest || proofs[i].Seq != seals[i].Seq {
			t.Fatalf("seal %d does not match its zone and proof", i)
		}
	}
	_, replayed, _, err := source.FenceZones(op.Cession.ID, zones)
	if err != nil || len(replayed) != len(seals) {
		t.Fatalf("fence replay: %d seals, %v", len(replayed), err)
	}
	for i := range seals {
		if replayed[i] != seals[i] {
			t.Fatalf("fence replay changed seal %d", i)
		}
	}
	if err = target.MountZones(op.Cession.ID, source.node, nil, seals, nil, proofs); err != nil {
		t.Fatalf("the receiver refused the seals of the fence: %v", err)
	}
	for _, zone := range zones {
		if _, err = target.Write("receiver"+zone.Location, Record{Collection: zone.Collection, Location: zone.Location, ID: "new"}); err != nil {
			t.Fatalf("zone %s not installed at the receiver: %v", zone.Location, err)
		}
	}
}

func TestColdFencePrepLeavesSourceWritableAndIncludesLateACK(t *testing.T) {
	source, target, genesis := ticketStores(t)
	row := Record{Collection: "Demo000000000000", Location: "aa", ID: "early"}
	if _, err := source.Write("early", row); err != nil {
		t.Fatal(err)
	}
	op := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	z, _ := recordZone(row)
	zones := []tree.ZoneKey{z}
	commit, err := source.PrepareColdFence(op.Cession.ID, zones)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's slice cannot redirect this prepared decision.
	zones[0], _ = tree.NewZoneKey(row.Collection, "bb")
	row.ID = "late"
	if _, err := source.Write("late", row); err != nil {
		t.Fatalf("source cannot ACK during warm preparation: %v", err)
	}
	snaps, _, proofs, err := commit()
	if err != nil || len(snaps) != 1 || snaps[0].Zone != z || len(snaps[0].Records) != 2 {
		t.Fatalf("stale or redirected fence: %+v, %v", snaps, err)
	}
	if err := target.MountZones(op.Cession.ID, source.node, nil, nil, snaps, proofs); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := target.Apply("late", row); err != nil || applied {
		t.Fatalf("late ACK was not deduplicated at the receiver: applied=%v err=%v", applied, err)
	}
	row.ID = "after"
	if _, err := source.Write("after", row); err == nil {
		t.Fatal("source still writable after commit")
	}
	if _, err := target.Write("after", row); err != nil {
		t.Fatal(err)
	}
}
