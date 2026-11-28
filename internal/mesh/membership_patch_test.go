package mesh

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func patchMember(name string) causal.Member {
	return causal.Member{VNode: tree.VNodeDescriptor{Name: name, ID: tree.ID{name[0]}}, Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: name, Storage: name}, Host: name}}
}
func patchEnvelope(names ...string) *MembershipEnvelope {
	root := patchMember("a")
	e := &MembershipEnvelope{Root: root.VNode, Genesis: causal.Genesis{Cluster: "test", Holder: root.Endpoint}}
	for _, name := range names {
		e.Delta.Members = append(e.Delta.Members, patchMember(name))
	}
	return e
}

func TestMembershipPatchRoundTrip(t *testing.T) {
	base := patchEnvelope("a", "c", "f", "h")
	next := patchEnvelope("a", "b", "c", "e", "f", "h", "z")
	next.Delta.Members[2].Endpoint.Epoch++
	next.Delta.Members[2].Endpoint.Host = "new"
	next.Delta.Members[4].Retired = true
	next.Delta.Members[4].Redirect = next.Genesis.Holder
	patch := membershipChanges(base, next)
	if patch == nil {
		t.Fatal("no patch")
	}
	if got := len(patch.Delta.Members); got != 5 {
		t.Fatalf("expected five changed members, got %d", got)
	}
	full, err := expandMembership(base, patch)
	if err != nil || causal.Hash(full) != causal.Hash(next) {
		t.Fatalf("wrong reconstructed digest: %v", err)
	}
	patch.Delta.Members[0].Endpoint.Host = "changed"
	full.Delta.Members[0].Endpoint.Host = "changed"
	if next.Delta.Members[1].Endpoint.Host == "changed" || base.Delta.Members[0].Endpoint.Host == "changed" || full.Delta.Members[1].Endpoint.Host == "changed" {
		t.Fatal("results alias input")
	}
}

func TestMembershipPatchRejectsNonCanonicalVersions(t *testing.T) {
	base := patchEnvelope("a", "c", "f")
	for _, names := range [][]string{{"a", "f"}, {"a", "c", "c", "f"}, {"a", "f", "c"}, {"a", "c", "f"}} {
		if p := membershipChanges(base, patchEnvelope(names...)); p != nil {
			t.Fatalf("patch for %v", names)
		}
	}
	for _, names := range [][]string{{"b", "b"}, {"e", "b"}, {}} {
		if _, err := expandMembership(base, patchEnvelope(names...)); !errors.Is(err, causal.ErrConflict) {
			t.Errorf("malformed patch %v: %v", names, err)
		}
	}
	for _, change := range []func(*MembershipEnvelope){
		func(e *MembershipEnvelope) { e.Root.ID[0]++ },
		func(e *MembershipEnvelope) { e.Genesis.Cluster += "other" },
		func(e *MembershipEnvelope) { e.Delta.Members[1].VNode.ID[1]++ },
		func(e *MembershipEnvelope) { e.Delta.Members[1].Endpoint.Host = "fork" },
	} {
		bad := patchEnvelope("a", "c", "f")
		change(bad)
		if p := membershipChanges(base, bad); p != nil {
			t.Error("accepted illegal transition")
		}
		bad.Delta.Members = slices.Clone(bad.Delta.Members[1:2])
		if _, err := expandMembership(base, bad); !errors.Is(err, causal.ErrConflict) {
			t.Errorf("accepted illegal patch: %v", err)
		}
	}
}

func TestMembershipPatchPreservesEpochAndRetirement(t *testing.T) {
	base := patchEnvelope("a", "c", "f")
	previous := &base.Delta.Members[1]
	previous.Endpoint.Epoch = 2
	previous.Retired = true
	previous.Redirect = base.Genesis.Holder
	for _, change := range []func(*causal.Member){
		func(m *causal.Member) { m.Endpoint.Epoch-- },
		func(m *causal.Member) { m.Retired = false; m.Redirect = causal.Endpoint{} },
		func(m *causal.Member) { m.Redirect = base.Delta.Members[2].Endpoint },
	} {
		next := base.clone()
		change(&next.Delta.Members[1])
		if membershipChanges(base, &next) != nil {
			t.Fatal("proposed regression of retained record")
		}
		next.Delta.Members = next.Delta.Members[1:2]
		if _, err := expandMembership(base, &next); !errors.Is(err, causal.ErrConflict) {
			t.Fatal("accepted regression of retained record")
		}
	}
}

func TestMembershipPatchRejectsCorruptTargetBeforeMerge(t *testing.T) {
	a, sender := membershipWireFixture(t, "a")
	b, receiver := membershipWireFixture(t, "a")
	send := membershipJSONSend(t, b, nil)
	ctx := context.Background()
	if err := a.exchange(ctx, "b", send); err != nil {
		t.Fatal(err)
	}
	beforeA, beforeB := a.receipt("b"), b.receipt("sender")
	beforeDigest, beforeMerges := receiver.view.Digest(), receiver.merges
	if err := sender.view.Merge(causal.MembershipDelta{Members: []causal.Member{patchMember("c")}}); err != nil {
		t.Fatal(err)
	}
	err := a.exchange(ctx, "b", func(ctx context.Context, host string, p MembershipPacket) (MembershipReply, error) {
		if p.Base == (causal.Digest{}) || p.Body == nil || len(p.Body.Delta.Members) != 1 {
			t.Fatal("expected an incremental packet on synchronized peers")
		}
		p.Version[0] ^= 1
		reply, err := send(ctx, host, p)
		if reply.Accepted != (causal.Digest{}) {
			t.Fatal("acknowledged a corrupted packet")
		}
		return reply, err
	})
	if !errors.Is(err, causal.ErrConflict) {
		t.Fatalf("corrupted target digest returned %v", err)
	}
	if receiver.view.Digest() != beforeDigest || receiver.merges != beforeMerges ||
		a.receipt("b") != beforeA || b.receipt("sender") != beforeB {
		t.Fatal("corrupted patch changed the receiver inventory, merge count or peer receipts")
	}
}
