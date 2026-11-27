package storage

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestLocalPartitionReleasesComplementBeforeFirstCopyFinishes(t *testing.T) {
	a, b, genesis := ticketStores(t)
	record := Record{Collection: "Demo000000000000", Location: "aa", ID: "before"}
	if _, err := a.Write("before", record); err != nil {
		t.Fatal(err)
	}
	zone, _ := recordZone(record)
	region := tree.Region{Mask: tree.ID{128}, Value: tree.ID{zone.ID[0] & 128}}
	selected, err := a.PartitionRight(genesis.Right().ID, region)
	if err != nil {
		t.Fatal(err)
	}
	state, err := a.CausalState()
	if err != nil || state.Permissions.Validate(genesis.Holder.Owner) != nil || len(state.Permissions.Native) != 2 {
		t.Fatal("local partition lost installed ownership", err)
	}
	record.ID = "after-local-split"
	if _, err := a.Write("after-local-split", record); err != nil {
		t.Fatal("local partition froze an address", err)
	}
	first := ticketAdmission(t, a, b, selected.ID, selected.Region)
	if _, err := a.PartitionRight(selected.ID, tree.Point(zone.ID)); !errors.Is(err, ErrNotReady) {
		t.Fatal("partition changed an input already pinned by a transfer", err)
	}
	var complement causal.Right
	for _, right := range state.Permissions.Native {
		if right.ID != selected.ID {
			complement = right
		}
	}
	second := ticketAdmission(t, a, b, complement.ID, complement.Region)
	// Finish a disjoint native piece while the first receiver reservation is
	// still merely Ready. No copy, fence or ACK of the first piece is required.
	ticketFinish(t, a, b, second.Cession.ID)
	state, err = a.CausalState()
	if err != nil || state.Transfers[first.Cession.ID].Phase != causal.TransferReady {
		t.Fatal("the first copy was not actually independent", err)
	}
	remote := Record{Collection: record.Collection, ID: "new-empty-address"}
	found := false
	for i := range 256 {
		remote.Location = tree.EncodeName(tree.ID{byte(i), 1})
		z, err := recordZone(remote)
		if err != nil {
			t.Fatal(err)
		}
		if complement.Region.Contains(z.ID) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no test address in the complement")
	}
	if _, err := b.Write("empty-address-receiver", remote); err != nil {
		t.Fatal("independent receiver piece cannot ACK a future address", err)
	}
	if _, err := a.Write("not-source-owned", remote); !errors.Is(err, ErrNotOwner) {
		t.Fatal("completed complement still has two writers", err)
	}
	record.ID = "while-first-copy-ready"
	if _, err := a.Write(record.ID, record); err != nil {
		t.Fatal("unfenced first piece stopped writing", err)
	}
	ticketFinish(t, a, b, first.Cession.ID)
	snapshot, err := b.Snapshot(zone)
	if err != nil || len(snapshot.Records) != 3 {
		t.Fatalf("ACKs lost while another native piece completed: count=%d err=%v", len(snapshot.Records), err)
	}
}

func TestLocalPartitionCrashKeepsExactlyOneNativeFrontier(t *testing.T) {
	cuts := 0
	for cut := 1; cut <= 16; cut++ {
		t.Run(strconv.Itoa(cut), func(t *testing.T) {
			s := newHandoffSchedule(t, int64(6100+cut), 2)
			source := &s.nodes[0]
			before, err := source.store.CausalState()
			s.must(err)
			input, ok := before.Permissions.At(s.zones[0].ID)
			if !ok {
				t.Fatal("missing installed input")
			}
			source.fs.FailAt = source.fs.Ops + cut
			_, err = source.store.PartitionRight(input.ID, tree.Point(s.zones[0].ID))
			if err != nil {
				cuts++
			}
			s.crash(0)
			s.open(0)
			after, err := s.nodes[0].store.CausalState()
			s.must(err)
			s.must(after.Permissions.Validate(s.nodes[0].holder.Owner))
			if after.Permissions.Sequence != before.Permissions.Sequence && after.Permissions.Sequence != before.Permissions.Sequence+1 {
				t.Fatal("partition recovered a partial counter transition")
			}
			_, oldExists := after.Permissions.Find(input.ID)
			if oldExists != (after.Permissions.Sequence == before.Permissions.Sequence) {
				t.Fatal("old identity and partition counter committed separately")
			}
			// The existing ACK and the entire original address space stay on the
			// same disk, whether the local metadata transaction survived or not.
			for _, piece := range input.Region.Subtract(tree.Point(s.zones[0].ID)) {
				if _, ok := after.Permissions.At(piece.Value); !ok {
					t.Fatal("partition lost an empty complement")
				}
			}
			s.check(true)
		})
	}
	if cuts == 0 {
		t.Fatal("no partition I/O cuts exercised")
	}
}

// Standalone partitioning is retained only to explore old interleavings.
// PartitionRight is a local same-owner change, before any receiver admission.
// It moves no bytes, freezes no zone and creates no remote authorization. The
// transfer will pin the selected output only, rather than a potentially huge
// parent containing unrelated future joins.
func (s *Store) PartitionRight(input causal.Digest, region tree.Region) (causal.Right, error) {
	var selected causal.Right
	err := s.updateCausal(func(tx *Tx, state CausalState) error {
		if coldBusy(tx) {
			return ErrNotReady // a cold plan names its exact native inputs
		}
		self := causal.Owner{VNode: s.node, Storage: storageID(tx)}
		for _, op := range state.Transfers {
			if op.Cession.Source.Owner == self && op.Phase < causal.TransferInstalled && op.Cession.Input.ID == input {
				return ErrNotReady
			}
		}
		before := state.Permissions.Sequence
		var err error
		state.Permissions, selected, err = state.Permissions.Partition(s.cluster, input, region, self)
		if err != nil {
			return err
		}
		if state.Permissions.Sequence == before {
			return nil
		}
		return writeCausal(tx, state)
	})
	return selected, err
}

func TestAtomicPieceReservationSurvivesLocalCoalescence(t *testing.T) {
	for _, atomic := range []bool{false, true} {
		name := "old-gap"
		if atomic {
			name = "atomic-reservation"
		}
		t.Run(name, func(t *testing.T) {
			source, target, g := ticketStores(t)
			region := tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}
			to, e := target.Endpoint(target.node)
			if e != nil {
				t.Fatal(e)
			}
			var selected causal.Right
			if atomic {
				op, e := source.BeginCausalPiece(context.Background(), g.Right().ID, to, region)
				if e != nil {
					t.Fatal(e)
				}
				selected = op.Cession.Input
			} else {
				var e error
				selected, e = source.PartitionRight(g.Right().ID, region)
				if e != nil {
					t.Fatal(e)
				}
			}
			if e := source.CoalesceLocalRights(); e != nil {
				t.Fatal(e)
			}
			state, e := source.CausalState()
			if e != nil {
				t.Fatal(e)
			}
			_, exists := state.Permissions.Find(selected.ID)
			if exists != atomic {
				t.Fatalf("selected piece exists=%t, atomic=%t", exists, atomic)
			}
			if atomic && len(state.Transfers) != 1 {
				t.Fatal("selected piece was not durably pinned")
			}
		})
	}
}
