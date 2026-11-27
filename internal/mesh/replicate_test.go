package mesh

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// W3 at the mesh: Replicate brings every zone's shadow copies on the other
// members of its R(z) to the owner's sequence, keeps them there as writes
// continue, and a member cut off catches up once reachable again.
func TestREPL3_ReplicateSyncsShadowCopies(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 3)
	ctx := context.Background()
	for _, n := range nodes[1:] {
		if err := n.Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var hosts []string
	for _, n := range nodes {
		hosts = append(hosts, n.Self.Host)
	}
	for _, n := range nodes {
		if err := n.Sync(ctx, hosts); err != nil {
			t.Fatal(err)
		}
	}
	write := func(round int) {
		for i := range 60 {
			r := storage.Record{Collection: fmt.Sprintf("Collection%06d", i), Location: "aa", ID: fmt.Sprint("row-", round), Metrics: []float64{1}}
			if _, err := nodes[0].Write(ctx, fmt.Sprintf("r%d-%d", round, i), r); err != nil {
				t.Fatal(err)
			}
		}
	}
	replicateAll := func() error {
		var first error
		for _, n := range nodes {
			if err := n.Replicate(ctx); err != nil && first == nil {
				first = err
			}
		}
		return first
	}
	// check: every member of R(z) other than the owner holds the owner's seq.
	check := func(skip string) int {
		t.Helper()
		copies := 0
		for _, owner := range nodes {
			owned, err := owner.Store.OwnedSeqs()
			if err != nil {
				t.Fatal(err)
			}
			routing, _ := owner.Store.CausalRouting()
			for _, zs := range owned {
				for _, m := range replicas(routing, zs.Zone, machineOf) {
					if m == owner.Self || m.Host == skip {
						continue
					}
					member := net.nodes[m.Host]
					seq, ok, err := member.Store.ShadowSeq(owner.Self, zs.Zone)
					if err != nil || !ok || seq != zs.Seq {
						t.Fatalf("zone %s on %s: shadow seq %d %t %v, owner %d", zs.Zone.Collection, m.VNode, seq, ok, err, zs.Seq)
					}
					copies++
				}
			}
		}
		return copies
	}
	write(0)
	if err := replicateAll(); err != nil {
		t.Fatal(err)
	}
	if n := check(""); n < 60*2 {
		t.Fatalf("%d shadow copies, want two per zone", n)
	}
	write(1)
	// Cut off a member that is not the owner of the written zones.
	routing, _ := nodes[0].Store.CausalRouting()
	first, _ := tree.NewZoneKey("Collection000000", "aa")
	owner, _ := routing.Route(first.ID)
	cut := nodes[0]
	for _, n := range nodes {
		if n.Self != owner {
			cut = n
		}
	}
	net.setBlocked(cut.Self.Host, true)
	if err := replicateAll(); err == nil {
		t.Fatal("a member cut off reported no failure")
	}
	check(cut.Self.Host)
	net.setBlocked(cut.Self.Host, false)
	if err := replicateAll(); err != nil {
		t.Fatal(err)
	}
	check("")
	// Shadow copies stay out of every inventory (SHADOW-1).
	rows, err := nodes[0].Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, s := range rows {
		count += len(s.Records)
	}
	if count != 120 {
		t.Fatalf("a complete read returns %d rows, want 120", count)
	}
}

// W1 and W3 for acknowledgements: a write acknowledged "replicated" is held
// by another member of its R(z) at that moment; with every other member cut
// off it is acknowledged "local" after ackTimeout, and Replicate catches the
// members up later.
func TestREPL3_WriteWaitsForOneMember(t *testing.T) {
	net, nodes := meshFixture(t, 3)
	ctx := context.Background()
	for _, n := range nodes[1:] {
		if err := n.Join(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var hosts []string
	for _, n := range nodes {
		hosts = append(hosts, n.Self.Host)
	}
	for _, n := range nodes {
		if err := n.Sync(ctx, hosts); err != nil {
			t.Fatal(err)
		}
		n.ReplicateWrites = true
	}
	record := storage.Record{Collection: "Collection000000", Location: "aa", ID: "row", Metrics: []float64{1}}
	zone, _ := tree.NewZoneKey(record.Collection, record.Location)
	routing, _ := nodes[0].Store.CausalRouting()
	members := replicas(routing, zone, machineOf)
	owner := net.nodes[members[0].Host]
	for i := range 5 {
		record.ID = fmt.Sprint("row-", i)
		response, err := nodes[0].Write(ctx, fmt.Sprint("w", i), record)
		if err != nil {
			t.Fatal(err)
		}
		if response.Durability != DurabilityReplicated {
			t.Fatalf("write %d: durability %q, want replicated", i, response.Durability)
		}
		held := 0
		for _, m := range members[1:] {
			if seq, ok, _ := net.nodes[m.Host].Store.ShadowSeq(owner.Self, zone); ok && seq >= response.Mutation.Seq {
				held++
			}
		}
		if held == 0 {
			t.Fatalf("write %d acknowledged replicated, no member holds seq %d", i, response.Mutation.Seq)
		}
	}
	saved := ackTimeout
	ackTimeout = 200 * time.Millisecond
	defer func() { ackTimeout = saved }()
	for _, m := range members[1:] {
		net.setBlocked(m.Host, true)
	}
	record.ID = "cut"
	response, err := owner.Write(ctx, "cut", record)
	if err != nil {
		t.Fatal(err)
	}
	if response.Durability != DurabilityLocal {
		t.Fatalf("with every member cut off: durability %q, want local", response.Durability)
	}
	for _, m := range members[1:] {
		net.setBlocked(m.Host, false)
	}
	if err := owner.Replicate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, m := range members[1:] {
		if seq, ok, _ := net.nodes[m.Host].Store.ShadowSeq(owner.Self, zone); !ok || seq < response.Mutation.Seq {
			t.Fatalf("%s did not catch up: seq %d %t, want %d", m.VNode, seq, ok, response.Mutation.Seq)
		}
	}
}
