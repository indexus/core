package causal

import (
	"encoding/json"
	"github.com/indexus/go-indexus-core/internal/tree"
	"math"
	"testing"
)

func permissionFixture() (LocalPermissions, Endpoint, Endpoint) {
	a := Endpoint{Owner: Owner{VNode: "a", Storage: "disk-a"}, Host: "a", Epoch: 1}
	b := Endpoint{Owner: Owner{VNode: "b", Storage: "disk-b"}, Host: "b", Epoch: 7}
	return LocalPermissions{Native: []Right{{ID: Hash("genesis"), Region: tree.Region{}, Owner: a.Owner}}}, a, b
}
func TestLocalPermissionsTicketLifecycle(t *testing.T) {
	p, a, b := permissionFixture()
	half := tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}
	p, source, err := p.Begin("cluster", p.Native[0].ID, half, a, b)
	if err != nil {
		t.Fatal(err)
	}
	receiver, slot, state, err := (LocalPermissions{}).Reserve(source.Cession, nil, b.Owner)
	if err != nil || state != TicketOpen {
		t.Fatal(err, state)
	}
	source, err = BindTicket(source, slot.Cession.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = StartMoving(source); err == nil {
		t.Fatal("fence before confirmed preload")
	}
	slot, err = Confirm(slot, source.Cession)
	if err != nil {
		t.Fatal(err)
	}
	source, err = Confirm(source, slot.Cession)
	if err != nil {
		t.Fatal(err)
	}
	source, err = StartMoving(source)
	if err != nil || CanCancel(source) {
		t.Fatal("point fence must prohibit cancellation", err)
	}
	final := source.Cession
	final.DataDigest = Hash("bytes")
	final.Barrier = 4
	source, err = Seal(source, final)
	if err != nil {
		t.Fatal(err)
	}
	slot, err = Seal(slot, final)
	if err != nil {
		t.Fatal(err)
	}
	p, source, err = p.Emit(source, a.Owner)
	if err != nil {
		t.Fatal(err)
	}
	receiver, slot, state, err = receiver.Install(final, &slot, b.Owner)
	if err != nil || state != TicketClosed {
		t.Fatal(err, state)
	}
	if len(p.Native) != 1 || len(receiver.Native) != 1 || receiver.EndpointEpoch != 7 {
		t.Fatal("wrong installed partition")
	}
	if err = p.Validate(a.Owner); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(receiver)
	after, _, state, err := receiver.Install(final, nil, b.Owner)
	if err != nil || state != TicketClosed {
		t.Fatal("lost reply replay", err)
	}
	raw, _ := json.Marshal(after)
	if string(raw) != string(before) {
		t.Fatal("closed replay mutated state")
	}
	// A replayed zero-ticket admission creates a new tentative ticket, never a
	// replacement for the source's already bound ticket.
	oldOffer := final
	oldOffer.Ticket = 0
	receiver, newSlot, _, err := receiver.Reserve(oldOffer, nil, b.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if newSlot.Cession.Ticket <= final.Ticket {
		t.Fatal("ticket reused")
	}
	if _, err = Confirm(newSlot, final); err == nil {
		t.Fatal("old preload confirmed new ticket")
	}
	if _, err = BindTicket(source, newSlot.Cession.Ticket); err == nil {
		t.Fatal("issued source rebound")
	}
	if _, err = BindTicket(source, final.Ticket); err != nil {
		t.Fatal("same ticket retry", err)
	}
	_, _, state, err = receiver.Install(final, &newSlot, b.Owner)
	if err != nil || state != TicketClosed {
		t.Fatal("old install not obsolete", err)
	}
}
func TestLocalPermissionsCoalesceAndOverflow(t *testing.T) {
	p, a, b := permissionFixture()
	p.Native = []Right{{ID: Hash("left"), Region: tree.Region{Mask: tree.ID{128}}, Owner: a.Owner}, {ID: Hash("right"), Region: tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}, Owner: a.Owner}}
	out, right, err := p.Coalesce("c", p.Native[0].ID, p.Native[1].ID, a.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if right.Region != (tree.Region{}) || out.Sequence != 1 || len(out.Native) != 1 || len(p.Native) != 2 {
		t.Fatal("coalesce did not replace both immutably")
	}
	if _, _, err = out.Coalesce("c", right.ID, right.ID, a.Owner); err == nil {
		t.Fatal("one-input coalesce")
	}
	p.Sequence = math.MaxUint64
	if _, _, err = p.Coalesce("c", p.Native[0].ID, p.Native[1].ID, a.Owner); err == nil {
		t.Fatal("sequence wrapped")
	}
	if _, _, err = p.Begin("c", p.Native[0].ID, p.Native[0].Region, a, b); err == nil {
		t.Fatal("source sequence wrapped")
	}
	base, _, _ := permissionFixture()
	_, op, err := base.Begin("c", base.Native[0].ID, tree.Region{}, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = (LocalPermissions{Sequence: math.MaxUint64}).Reserve(op.Cession, nil, b.Owner); err == nil {
		t.Fatal("ticket wrapped")
	}
}

func TestLocalPartitionKeepsAllAddressesWritableWithoutOldIdentity(t *testing.T) {
	p, self, _ := permissionFixture()
	input := p.Native[0]
	region := tree.Region{Mask: tree.ID{192}, Value: tree.ID{64}}
	next, selected, err := p.Partition("c", input.ID, region, self.Owner)
	if err != nil || next.Validate(self.Owner) != nil {
		t.Fatal("invalid local partition", err)
	}
	if len(p.Native) != 1 || len(next.Native) != 3 || next.Sequence != p.Sequence+1 || selected.Region != region {
		t.Fatal("partition mutated input or lost its exact complement")
	}
	if _, found := next.Find(input.ID); found {
		t.Fatal("consumed parent identity survived")
	}
	for i := range 256 {
		if right, found := next.At(tree.ID{byte(i)}); !found || right.Owner != self.Owner {
			t.Fatal("local partition created a writer gap", i)
		}
	}
	unchanged, same, err := next.Partition("c", selected.ID, selected.Region, self.Owner)
	if err != nil || same != selected || unchanged.Sequence != next.Sequence {
		t.Fatal("exact-piece retry allocated another identity")
	}
	next.Sequence = math.MaxUint64
	if _, _, err := next.Partition("c", selected.ID, tree.Point(region.Value), self.Owner); err == nil {
		t.Fatal("partition wrapped its durable counter")
	}
}
func TestLocalPermissionsTentativeCancelAndValidation(t *testing.T) {
	p, a, b := permissionFixture()
	_, op, err := p.Begin("c", p.Native[0].ID, tree.Region{}, a, b)
	if err != nil {
		t.Fatal(err)
	}
	r, slot, _, err := (LocalPermissions{}).Reserve(op.Cession, nil, b.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if !CanCancel(slot) {
		t.Fatal("tentative must be cancellable")
	}
	// Dropping a tentative slot is safe: confirmation returns closed, no effects.
	state, err := r.Inspect(slot.Cession, nil, b.Owner)
	if err != nil || state != TicketClosed {
		t.Fatal(err)
	}
	if _, err = Seal(slot, slot.Cession); err == nil {
		t.Fatal("unconfirmed seal")
	}
	corrupt := p.Clone()
	corrupt.Native = append(corrupt.Native, corrupt.Native[0])
	if corrupt.Validate(a.Owner) == nil {
		t.Fatal("overlap")
	}
	corrupt = p.Clone()
	corrupt.Native[0].Owner = b.Owner
	if corrupt.Validate(a.Owner) == nil {
		t.Fatal("foreign right")
	}
}

func TestLocalPermissionsCoalescePreservesUnrelatedScopes(t *testing.T) {
	p, a, _ := permissionFixture()
	p.Native = []Right{
		{ID: Hash("quarter-a"), Region: tree.Region{Mask: tree.ID{192}}, Owner: a.Owner},
		{ID: Hash("quarter-b"), Region: tree.Region{Mask: tree.ID{192}, Value: tree.ID{64}}, Owner: a.Owner},
		{ID: Hash("unrelated-half"), Region: tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}, Owner: a.Owner},
	}
	next, merged, err := p.Coalesce("c", p.Native[0].ID, p.Native[1].ID, a.Owner)
	if err != nil || next.Validate(a.Owner) != nil {
		t.Fatalf("coalescing a valid frontier changed its invariants: %v", err)
	}
	if merged.Region != (tree.Region{Mask: tree.ID{128}}) || len(next.Native) != 2 || len(p.Native) != 3 {
		t.Fatal("coalescing changed the union or mutated the input")
	}
	if right, found := next.Find(p.Native[2].ID); !found || right != p.Native[2] {
		t.Fatal("unrelated installed right changed")
	}
}

func TestLocalPermissionsCancelledSlotOnlyAcknowledgesCleanup(t *testing.T) {
	p, source, target := permissionFixture()
	_, op, err := p.Begin("c", p.Native[0].ID, tree.Region{}, source, target)
	if err != nil {
		t.Fatal(err)
	}
	receiver, slot, _, err := (LocalPermissions{}).Reserve(op.Cession, nil, target.Owner)
	if err != nil {
		t.Fatal(err)
	}
	slot.Phase = TransferCancelled
	state, err := receiver.Inspect(slot.Cession, &slot, target.Owner)
	if err != nil || state != TicketClosed {
		t.Fatalf("cancelled cleanup slot reopened: %v %v", state, err)
	}
	_, _, state, err = receiver.Reserve(op.Cession, &slot, target.Owner)
	if err != nil || state != TicketClosed {
		t.Fatal("replayed admission replaced cancelled cleanup")
	}
	if _, err := Confirm(slot, slot.Cession); err == nil {
		t.Fatal("cancelled preload confirmed")
	}
	next, _, state, err := receiver.Install(slot.Cession, &slot, target.Owner)
	if err != nil || state != TicketClosed || len(next.Native) != 0 || next.Sequence != receiver.Sequence {
		t.Fatal("cancelled install changed permissions")
	}
}
func FuzzTransferID(f *testing.F) {
	f.Add("disk", uint64(1))
	f.Add("disk:with:colons", uint64(math.MaxUint64))
	f.Add("", uint64(0))
	f.Fuzz(func(t *testing.T, actor string, seq uint64) {
		id := TransferID{Actor: actor, Sequence: seq}
		raw, err := id.MarshalText()
		if !id.Valid() {
			if err == nil {
				t.Fatal("invalid accepted")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		var decoded TransferID
		if err = decoded.UnmarshalText(raw); err != nil || decoded != id {
			t.Fatalf("round trip %q %v", raw, err)
		}
	})
}

func FuzzLocalPermissionsReplay(f *testing.F) {
	f.Add(uint64(0), uint64(0), byte(0))
	f.Add(uint64(123), uint64(77), byte(128))
	f.Fuzz(func(t *testing.T, sourceSequence, receiverSequence uint64, bit byte) {
		if sourceSequence == math.MaxUint64 || receiverSequence >= math.MaxUint64-1 {
			return
		}
		p, a, b := permissionFixture()
		p.Sequence = sourceSequence
		mask := byte(1) << (bit % 8)
		region := tree.Region{Mask: tree.ID{mask}, Value: tree.ID{bit & mask}}
		p, source, err := p.Begin("c", p.Native[0].ID, region, a, b)
		if err != nil {
			t.Fatal(err)
		}
		r, slot, _, err := (LocalPermissions{Sequence: receiverSequence}).Reserve(source.Cession, nil, b.Owner)
		if err != nil {
			t.Fatal(err)
		}
		source, err = BindTicket(source, slot.Cession.Ticket)
		if err != nil {
			t.Fatal(err)
		}
		source, err = Confirm(source, slot.Cession)
		if err != nil {
			t.Fatal(err)
		}
		slot, err = Confirm(slot, source.Cession)
		if err != nil {
			t.Fatal(err)
		}
		c := source.Cession
		c.DataDigest = Hash("payload")
		source, err = Seal(source, c)
		if err != nil {
			t.Fatal(err)
		}
		slot, err = Seal(slot, c)
		if err != nil {
			t.Fatal(err)
		}
		p, _, err = p.Emit(source, a.Owner)
		if err != nil {
			t.Fatal(err)
		}
		r, _, _, err = r.Install(c, &slot, b.Owner)
		if err != nil {
			t.Fatal(err)
		}
		if err = p.Validate(a.Owner); err != nil {
			t.Fatal(err)
		}
		if err = r.Validate(b.Owner); err != nil {
			t.Fatal(err)
		}
		for _, x := range p.Native {
			for _, y := range r.Native {
				if _, overlap := x.Region.Intersect(y.Region); overlap {
					t.Fatal("two writers")
				}
			}
		}
		before, _ := json.Marshal(r)
		replay, _, state, err := r.Install(c, nil, b.Owner)
		if err != nil || state != TicketClosed {
			t.Fatal(err)
		}
		after, _ := json.Marshal(replay)
		if string(before) != string(after) {
			t.Fatal("closed replay mounted data")
		}
	})
}

func TestLocalPermissionsReturnAndCompactedReplay(t *testing.T) {
	p, a, b := permissionFixture()
	transfer := func(source LocalPermissions, target LocalPermissions, from, to Endpoint) (LocalPermissions, LocalPermissions, Cession) {
		var err error
		source, op, err := source.Begin("c", source.Native[0].ID, source.Native[0].Region, from, to)
		if err != nil {
			t.Fatal(err)
		}
		target, slot, _, err := target.Reserve(op.Cession, nil, to.Owner)
		if err != nil {
			t.Fatal(err)
		}
		op, err = BindTicket(op, slot.Cession.Ticket)
		if err != nil {
			t.Fatal(err)
		}
		op, err = Confirm(op, slot.Cession)
		if err != nil {
			t.Fatal(err)
		}
		slot, err = Confirm(slot, op.Cession)
		if err != nil {
			t.Fatal(err)
		}
		c := op.Cession
		c.DataDigest = Hash("data")
		op, err = Seal(op, c)
		if err != nil {
			t.Fatal(err)
		}
		slot, err = Seal(slot, c)
		if err != nil {
			t.Fatal(err)
		}
		source, _, err = source.Emit(op, from.Owner)
		if err != nil {
			t.Fatal(err)
		}
		target, _, _, err = target.Install(c, &slot, to.Owner)
		if err != nil {
			t.Fatal(err)
		}
		return source, target, c
	}
	p, r, old := transfer(p, LocalPermissions{}, a, b)
	r, p, _ = transfer(r, p, b, a)
	if len(p.Native) != 1 || len(r.Native) != 0 {
		t.Fatal("physical return failed")
	}
	// Both operation receipts have now been collected. Only current rights and
	// per-disk sequences remain. An old install cannot reactivate B.
	replay, _, state, err := r.Install(old, nil, b.Owner)
	if err != nil || state != TicketClosed || len(replay.Native) != 0 {
		t.Fatal("spent authority resurrected", err)
	}
}
