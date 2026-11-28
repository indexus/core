package mesh

import (
	"context"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
)

// B's last reply already gave A their shared version. When B initiates the
// next round, B has no outbound receipt, but both peers know the same digest.
func TestMembershipReverseRoundUsesSharedVersion(t *testing.T) {
	a, sa := membershipWireFixture(t, "")
	b, sb := membershipWireFixture(t, "")
	exchange := func(sender, receiver *membershipWire, self, peer string, referenceOnly bool) {
		t.Helper()
		calls := 0
		err := sender.exchange(context.Background(), peer, func(ctx context.Context, _ string, p MembershipPacket) (MembershipReply, error) {
			calls++
			out, err := receiver.receive(ctx, self, p)
			if referenceOnly && (p.Body != nil || out.Packet.Body != nil || out.NeedBody) {
				t.Errorf("equal views exchanged bodies: request=%t reply=%t need=%t", p.Body != nil, out.Packet.Body != nil, out.NeedBody)
			}
			return out, err
		})
		if err != nil || calls != 1 {
			t.Fatalf("exchange %s to %s: calls=%d err=%v", self, peer, calls, err)
		}
	}
	exchange(a, b, "a", "b", false)
	exchange(b, a, "b", "a", true)
	if err := sb.view.Merge(causal.MembershipDelta{Members: []causal.Member{incrementalTestMember(7)}}); err != nil {
		t.Fatal(err)
	}
	exchange(a, b, "a", "b", false)
	// A learned the join from B’s reply. B still has no receipt for A’s new version.
	exchange(a, b, "a", "b", true)
	if sa.view.Digest() != sb.view.Digest() {
		t.Fatal("membership diverged")
	}
}
