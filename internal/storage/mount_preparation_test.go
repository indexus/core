package storage

import (
	"errors"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestMountPreparationRejectsConcurrentMirror(t *testing.T) {
	source, target, genesis := ticketStores(t)
	r := Record{Collection: "Demo000000000000", Location: "aa", ID: "base"}
	if _, err := source.Write("base", r); err != nil {
		t.Fatal(err)
	}
	op := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	zone, _ := recordZone(r)
	base, err := source.Snapshot(zone)
	if err != nil {
		t.Fatal(err)
	}
	offer := op
	offer.Snapshots = []Snapshot{base}
	if _, err = target.PreloadCausal(offer); err != nil {
		t.Fatal(err)
	}
	r.ID = "late-ack"
	mutation, err := source.Write("late-ack", r)
	if err != nil {
		t.Fatal(err)
	}
	snaps, _, proofs, err := source.FenceZones(op.Cession.ID, []tree.ZoneKey{zone})
	if err != nil {
		t.Fatal(err)
	}
	seal, err := snaps[0].Seal()
	if err != nil {
		t.Fatal(err)
	}
	delta := []ZoneMutations{{Zone: zone, Mutations: []Mutation{mutation}}}
	seals := []ZoneSeal{seal}
	prepared, err := target.prepareMount(op.Cession.ID, source.node, delta, seals, nil, proofs)
	if err != nil {
		t.Fatal(err)
	}
	// A mirror request commits in the deterministic prepare/commit gap.
	if err = target.MirrorBatch(op.Cession.ID.String(), delta); err != nil {
		t.Fatal(err)
	}
	err = target.updateCausal(func(tx *Tx, state CausalState) error {
		return target.mountPreparedTx(tx, state, op.Cession.ID, prepared, proofs)
	})
	if !errors.Is(err, errMountChanged) {
		t.Fatalf("changed staging accepted: %v", err)
	}
	if _, err = target.Write("not-installed", r); err == nil {
		t.Fatal("failed preparation granted write authority")
	}
	if err = target.MountZones(op.Cession.ID, source.node, delta, seals, nil, proofs); err != nil {
		t.Fatal(err)
	}
	replay, err := target.Write("late-ack", r)
	if err != nil || replay.Seq != mutation.Seq {
		t.Fatalf("ACK receipt lost: %+v %v", replay, err)
	}
}

func TestMountPreparationIndependentIncomingLots(t *testing.T) {
	source, target, genesis := ticketStores(t)
	zones := make([]tree.ZoneKey, 2)
	for i, loc := range []string{"aa", "bb"} {
		r := Record{Collection: "Demo000000000000", Location: loc, ID: loc}
		if _, err := source.Write(loc, r); err != nil {
			t.Fatal(err)
		}
		zones[i], _ = recordZone(r)
	}
	var region tree.Region
	for i := range zones[0].ID {
		difference := zones[0].ID[i] ^ zones[1].ID[i]
		if difference != 0 {
			bit := difference & -difference
			region.Mask[i], region.Value[i] = bit, zones[0].ID[i]&bit
			break
		}
	}
	selected, err := source.PartitionRight(genesis.Right().ID, region)
	if err != nil {
		t.Fatal(err)
	}
	rights, err := source.Rights(tree.Point(zones[1].ID))
	if err != nil || len(rights) != 1 {
		t.Fatalf("complement: %+v %v", rights, err)
	}
	ops := []CausalTransfer{
		ticketAdmission(t, source, target, selected.ID, selected.Region),
		ticketAdmission(t, source, target, rights[0].ID, rights[0].Region),
	}
	prepared := make([]preparedMount, 2)
	proofs := make([][]causal.ZoneTransfer, 2)
	for i, zone := range zones {
		snaps, _, zts, err := source.FenceZones(ops[i].Cession.ID, []tree.ZoneKey{zone})
		if err != nil {
			t.Fatal(err)
		}
		proofs[i] = zts
		prepared[i], err = target.prepareMount(ops[i].Cession.ID, source.node, nil, nil, snaps, zts)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Both operations are prepared before either commits. Installing one
	// must not overwrite the other zone's state or invalidate its preparation.
	for i := range prepared {
		if err := target.updateCausal(func(tx *Tx, state CausalState) error {
			return target.mountPreparedTx(tx, state, ops[i].Cession.ID, prepared[i], proofs[i])
		}); err != nil {
			t.Fatalf("lot %d: %v", i, err)
		}
	}
	for _, zone := range zones {
		r := Record{Collection: zone.Collection, Location: zone.Location, ID: "receiver"}
		if _, err := target.Write("receiver-"+zone.Location, r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMountPreparationRejectsSameSequenceRestage(t *testing.T) {
	source, target, genesis := ticketStores(t)
	r := Record{Collection: "Demo000000000000", Location: "aa", ID: "base", Payload: []byte(`"original"`)}
	if _, err := source.Write("base", r); err != nil {
		t.Fatal(err)
	}
	op := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	zone, _ := recordZone(r)
	base, err := source.Snapshot(zone)
	if err != nil {
		t.Fatal(err)
	}
	offer := op
	offer.Snapshots = []Snapshot{base}
	if _, err = target.PreloadCausal(offer); err != nil {
		t.Fatal(err)
	}
	snaps, _, proofs, err := source.FenceZones(op.Cession.ID, []tree.ZoneKey{zone})
	if err != nil {
		t.Fatal(err)
	}
	seal, err := snaps[0].Seal()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := target.prepareMount(op.Cession.ID, source.node, nil, []ZoneSeal{seal}, nil, proofs)
	if err != nil {
		t.Fatal(err)
	}
	replacement := snaps[0]
	replacement.Records = append([]Record(nil), replacement.Records...)
	replacement.Records[0].Payload = []byte(`"different bytes, same sequence"`)
	if err = target.update(func(tx *Tx) error {
		return restageSnapshot(tx, op.Cession.ID.String(), source.node, replacement)
	}); err != nil {
		t.Fatal(err)
	}
	err = target.updateCausal(func(tx *Tx, state CausalState) error {
		return target.mountPreparedTx(tx, state, op.Cession.ID, prepared, proofs)
	})
	if !errors.Is(err, errMountChanged) {
		t.Fatalf("same-seq replacement escaped header guard: %v", err)
	}
	// A retry prepares from the actual replacement and refuses the original
	// seal; the abandoned preparation never certifies different bytes.
	if err = target.MountZones(op.Cession.ID, source.node, nil, []ZoneSeal{seal}, nil, proofs); !errors.Is(err, ErrSequence) {
		t.Fatalf("replacement accepted under stale seal: %v", err)
	}
	// The canonical final snapshot can still repair the uncertified copy.
	if err = target.MountZones(op.Cession.ID, source.node, nil, nil, snaps, proofs); err != nil {
		t.Fatal(err)
	}
}
