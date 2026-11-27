package mesh

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/testkit"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Read repairs may follow durable local referrals, but cannot call membership
// anti-entropy or carry its body inline. This is stronger than a warm-cache test.
func readOnlyTransport(net *network, observe func(string)) Transport {
	return transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if !inventoryRPC(r.Kind) || r.Membership != nil {
			return Response{}, fmt.Errorf("read attempted membership control: %s", r.Kind)
		}
		if observe != nil {
			observe(host)
		}
		out, err := net.Exchange(ctx, host, r)
		if out.Membership != nil {
			return Response{}, fmt.Errorf("read returned membership body")
		}
		return out, err
	})
}
func collectReadTransferReceipts(t testing.TB, nodes ...*Service) {
	t.Helper()
	for _, node := range nodes {
		state, err := node.Store.CausalState()
		if err != nil {
			t.Fatal(err)
		}
		for id, tr := range state.Transfers {
			if tr.Phase != causal.TransferInstalled {
				t.Fatalf("unfinished transfer %s phase=%d", id, tr.Phase)
			}
			if err := node.Store.CollectTransferred(id); err != nil {
				t.Fatal(err)
			}
		}
		state, err = node.Store.CausalState()
		if err != nil || len(state.Transfers) != 0 {
			t.Fatalf("receipts not collected: %+v %v", state.Transfers, err)
		}
	}
}
func installedReadRight(t testing.TB, node *Service, point tree.ID) causal.Right {
	t.Helper()
	state, err := node.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	right, ok := state.Permissions.At(point)
	if !ok {
		t.Fatal("fixture missing installed native right")
	}
	return right
}

// The observer never learns either handoff. Both donors collect their receipts
// before the read. Following one hop, or only open operations, loses an ACK.
func TestQueryFollowsCompactedSuccessorsWithoutGlobalKnowledge(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unavailable=%t", unavailable), func(t *testing.T) {
			net, nodes := meshFixture(t, 4)
			a, b, c, reader := nodes[0], nodes[1], nodes[2], nodes[3]
			ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(10*time.Second))
			defer cancel()
			reader.Peers = readOnlyTransport(net, nil)
			row := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "seed"}
			if _, err := a.Store.Write(row.ID, row); err != nil {
				t.Fatal(err)
			}
			var checkedFrozen bool
			for i, pair := range [][2]*Service{{a, b}, {b, c}} {
				if i == 1 {
					row.ID = "ACK-only-on-B"
					if _, err := b.Store.Write(row.ID, row); err != nil {
						t.Fatal(err)
					}
					net.setBefore(func(r Request) error {
						if r.Kind != "install-zone" || checkedFrozen {
							return nil
						}
						checkedFrozen = true
						rows, err := reader.QueryPrefixMetrics(ctx, row.Collection, "@")
						if err != nil || len(rowSet(t, rows, row.Collection, "@")) != 2 {
							return fmt.Errorf("frozen intermediary lost earlier ACK: %+v %v", rows, err)
						}
						return nil
					})
				}
				source, target := pair[0], pair[1]
				input := installedReadRight(t, source, tree.ID{})
				if err := source.handoff(ctx, Request{Input: input.ID, Target: target.Self, TargetVNode: target.VNode, Region: tree.Region{}}); err != nil {
					t.Fatal(err)
				}
				collectReadTransferReceipts(t, pair[:]...)
			}
			if !checkedFrozen {
				t.Fatal("frozen intermediary cut was not exercised")
			}
			row.ID = "ACK-only-on-C"
			if _, err := c.Store.Write(row.ID, row); err != nil {
				t.Fatal(err)
			}
			before, err := reader.Store.CausalRouting()
			if err != nil {
				t.Fatal(err)
			}
			calls := map[string]int{}
			var mu sync.Mutex
			reader.Peers = readOnlyTransport(net, func(host string) { mu.Lock(); calls[host]++; mu.Unlock() })
			net.setBlocked(c.Self.Host, unavailable)
			rows, err := reader.QueryPrefixMetrics(ctx, row.Collection, "@")
			if unavailable {
				if err == nil || rows != nil || err.Error() != "partition" {
					t.Fatalf("missing successor returned partial success: %+v %v", rows, err)
				}
			} else if got := rowSet(t, rows, row.Collection, "@"); err != nil || len(got) != 3 {
				t.Fatalf("lost earlier ACK: %v %v", got, err)
			}
			if calls[a.Self.Host] != 1 || calls[b.Self.Host] != 1 || calls[c.Self.Host] != 1 {
				t.Fatalf("did not follow A→B→C exactly once: %v", calls)
			}
			after, _ := reader.Store.CausalRouting()
			if after.Digest() != before.Digest() {
				t.Fatal("read needed membership repair")
			}
		})
	}
}

func TestInventoryAlwaysCompactAndRefusesKnowledgeCarry(t *testing.T) {
	_, nodes := meshFixture(t, 1)
	node := nodes[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r := Request{Kind: "inventory", Page: &storage.InventoryPageRequest{}}
	out, err := node.Handle(ctx, r)
	if err != nil || out.Membership != nil || out.Page == nil || !tree.CoversRegions(tree.Region{}, out.Page.Coverage) {
		t.Fatalf("inventory lacked compact complete coverage: %+v %v", out, err)
	}
	before, _ := node.Store.Revision()
	r.Membership = &MembershipPacket{}
	if _, err := node.Handle(ctx, r); !errors.Is(err, causal.ErrConflict) {
		t.Fatalf("inventory accepted membership carry: %v", err)
	}
	after, _ := node.Store.Revision()
	if before != after {
		t.Fatal("refused carry mutated durable state")
	}
}

func TestQueryFollowsAllSourcesOfUninstalledRegions(t *testing.T) {
	net, nodes := meshFixture(t, 4)
	a, b, receiver, reader := nodes[0], nodes[1], nodes[2], nodes[3]
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(10*time.Second))
	defer cancel()
	const collection = "Demo000000000000"
	var zones [2]tree.ZoneKey
	for _, location := range tree.ChildCandidates(tree.BASE64, "@") {
		z, _ := tree.NewZoneKey(collection, location)
		zones[int(z.ID[0])/128] = z
	}
	for i, z := range zones {
		if z.Collection == "" {
			t.Fatal("missing half-space fixture")
		}
		if _, err := a.Store.Write(fmt.Sprint(i), storage.Record{Collection: collection, Location: z.Location, ID: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	root := installedReadRight(t, a, tree.ID{})
	if err := a.handoff(ctx, Request{Input: root.ID, Target: b.Self, TargetVNode: b.VNode, Region: tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}}); err != nil {
		t.Fatal(err)
	}
	for i, source := range []*Service{a, b} {
		right := installedReadRight(t, source, zones[i].ID)
		offer := confirmedTransfer(t, source, receiver, right.ID, right.Region)
		snapshots, err := source.Store.FreezeZones(offer.Cession.ID, []tree.ZoneKey{zones[i]})
		if err != nil {
			t.Fatal(err)
		}
		if err := receiver.Store.StageZones(offer.Cession.ID, source.Self.VNode, snapshots); err != nil {
			t.Fatal(err)
		}
		if _, err := source.Store.EmitZoneTransfers(offer.Cession.ID, []tree.ZoneKey{zones[i]}); err != nil {
			t.Fatal(err)
		}
	}
	// R has two confirmed incoming pieces, neither point installed. Its inventory
	// must name BOTH sources; membership alone cannot describe this data state.
	reader.Peers = readOnlyTransport(net, nil)
	page, err := reader.call(ctx, receiver.Self.Host, Request{Kind: "inventory", Page: &storage.InventoryPageRequest{}, Target: receiver.Self})
	if err != nil {
		t.Fatal(err)
	}
	needed := map[causal.Owner]bool{}
	for _, ref := range page.Page.Referrals {
		needed[ref.Endpoint.Owner] = true
	}
	if !needed[a.Self.Owner] || !needed[b.Self.Owner] {
		t.Fatalf("receiver omitted an incoming source: %v", needed)
	}
	for _, z := range zones {
		rows, err := reader.QueryPrefixMetrics(ctx, collection, z.Location)
		if err != nil || len(rowSet(t, rows, collection, z.Location)) != 1 {
			t.Fatalf("scoped incoming read incomplete: %+v %v", rows, err)
		}
	}
	rootZone, err := tree.NewZoneKey(collection, "@")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := reader.queryHolders(ctx, []causal.Endpoint{receiver.Self}, Request{Kind: KindInventoryPrefix, Zone: rootZone})
	if err != nil || len(rowSet(t, rows, collection, "@")) != 2 {
		t.Fatalf("receiver entry lost an incoming source: %+v %v", rows, err)
	}
	rows, err = reader.QueryPrefixMetrics(ctx, collection, "@")
	if err != nil || len(rowSet(t, rows, collection, "@")) != 2 {
		t.Fatalf("incomplete successful read across two sources: %+v %v", rows, err)
	}
}

func TestJoinReplansCompactedParentWithoutInventoryRepair(t *testing.T) {
	net, nodes := meshFixture(t, 3)
	a, b, joiner := nodes[0], nodes[1], nodes[2]
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(10*time.Second))
	defer cancel()
	stale := installedReadRight(t, a, tree.ID{})
	for _, pair := range [][2]*Service{{a, b}, {b, a}} {
		source, target := pair[0], pair[1]
		right := installedReadRight(t, source, tree.ID{})
		if err := source.handoff(ctx, Request{Input: right.ID, Target: target.Self, TargetVNode: target.VNode, Region: tree.Region{}}); err != nil {
			t.Fatal(err)
		}
		collectReadTransferReceipts(t, pair[:]...)
	}
	var delayedReply, staleRequested atomic.Bool
	joiner.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		out, err := net.Exchange(ctx, host, r)
		if r.Kind == KindRights && host == a.Self.Host && !delayedReply.Swap(true) && err == nil {
			out.Rights = []causal.Right{stale} // delayed genuine receipt from before A→B→A
		}
		if r.Kind == "handoff" && r.Input == stale.ID {
			staleRequested.Store(true)
		}
		if inventoryRPC(r.Kind) {
			return Response{}, fmt.Errorf("join attempted inventory-based authority repair")
		}
		return out, err
	})
	if err := joiner.Join(ctx); err != nil {
		t.Fatal(err)
	}
	if !staleRequested.Load() {
		t.Fatal("stale installed-right receipt was never requested")
	}
	state, _ := a.Store.CausalState()
	if _, live := state.Permissions.Find(stale.ID); live {
		t.Fatal("old input was resurrected")
	}
}

func TestQueryNeverReturnsEmptyDuringPhysicalReturnBeforePlacementPublication(t *testing.T) {
	net, nodes := meshFixture(t, 2)
	original, reader := nodes[0], nodes[1]
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(10*time.Second))
	defer cancel()
	row := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "seed"}
	if _, err := original.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	state, _ := original.Store.CausalState()
	disk, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "other"), original.Self.VNode, "test", 1<<26, testStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { disk.Close() })
	holder, _ := disk.Endpoint("other-host")
	other := &Service{Store: disk, Self: holder, VNode: original.VNode, Peers: net}
	t.Cleanup(other.Close)
	if err := disk.InitCausalReceiver(state.Root, state.Genesis); err != nil {
		t.Fatal(err)
	}
	net.mu.Lock()
	net.nodes[holder.Host] = other
	net.mu.Unlock()
	if err := other.ReceiveMove(ctx, original.Self.Host); err != nil {
		t.Fatal(err)
	}
	row.ID = "ACK-on-other-disk"
	if _, err := other.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	_, _, delta, err := other.Store.Membership()
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Store.MergeMembership(delta); err != nil {
		t.Fatal(err)
	}
	reader.Peers = readOnlyTransport(net, nil)
	var probed atomic.Bool
	net.setBefore(func(r Request) error {
		// Directory migration is after installation and before MOVE publication.
		if r.Kind != "directory" || probed.Load() {
			return nil
		}
		state, err := other.Store.CausalState()
		if err != nil {
			return err
		}
		if state.Moving == nil {
			return nil
		}
		for _, tr := range state.Transfers {
			if tr.Phase != causal.TransferInstalled {
				return fmt.Errorf("directory sent before completed transfer")
			}
		}
		m, err := state.MembershipReplica()
		if err != nil {
			return err
		}
		current, err := m.Endpoint(original.VNode.Name)
		if err != nil || current.Owner != other.Self.Owner || current.Epoch != 1 {
			return fmt.Errorf("return publication gap not retained: %+v %v", current, err)
		}
		probed.Store(true)
		// Native installation alone must make the returned data readable. No
		// ErrNotReady escape is accepted while routing still names the donor.
		rows, err := reader.QueryPrefixMetrics(ctx, row.Collection, "@")
		if err != nil || len(rowSet(t, rows, row.Collection, "@")) != 2 {
			return fmt.Errorf("physical return lost ACK before publication: %+v %v", rows, err)
		}
		return nil
	})
	if err := original.ReceiveMove(ctx, other.Self.Host); err != nil {
		t.Fatal(err)
	}
	if !probed.Load() {
		t.Fatal("physical return publication gap was not exercised")
	}
	rows, err := reader.QueryPrefixMetrics(ctx, row.Collection, "@")
	if err != nil || len(rowSet(t, rows, row.Collection, "@")) != 2 {
		t.Fatalf("return did not retain complete read: %+v %v", rows, err)
	}
}

// A delegated empty span has no payload to carry its forwarding metadata. A
// physical move must migrate that directory entry even before the first write.
func TestPhysicalMoveCarriesEmptyDelegationToUnawareReader(t *testing.T) {
	net, nodes := meshFixture(t, 3)
	source, delegate, reader := nodes[0], nodes[1], nodes[2]
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(10*time.Second))
	defer cancel()
	rightHalf := tree.Region{Mask: tree.ID{128}, Value: tree.ID{128}}
	root := installedReadRight(t, source, tree.ID{})
	if err := source.handoff(ctx, Request{Input: root.ID, Target: delegate.Self, TargetVNode: delegate.VNode, Region: rightHalf}); err != nil {
		t.Fatal(err)
	}
	collectReadTransferReceipts(t, source, delegate)
	// All stores are still empty: directory migration cannot piggyback a record.
	for _, node := range nodes {
		rows, err := node.Store.Inventory(false)
		if err != nil || len(rows) != 0 {
			t.Fatalf("fixture contains rows before directory move: %+v %v", rows, err)
		}
	}
	state, _ := source.Store.CausalState()
	disk, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), "moved-empty"), source.Self.VNode, "test", 1<<26, testStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { disk.Close() })
	endpoint, _ := disk.Endpoint("moved-empty-host")
	moved := &Service{Store: disk, Self: endpoint, VNode: source.VNode, Peers: net}
	t.Cleanup(moved.Close)
	if err := disk.InitCausalReceiver(state.Root, state.Genesis); err != nil {
		t.Fatal(err)
	}
	net.mu.Lock()
	net.nodes[endpoint.Host] = moved
	net.mu.Unlock()
	if err := moved.ReceiveMove(ctx, source.Self.Host); err != nil {
		t.Fatal(err)
	}
	_, _, members, err := moved.Store.Membership()
	if err != nil {
		t.Fatal(err)
	}
	// The reader learns only the physical address of A, never B's activation.
	var onlySource causal.MembershipDelta
	for _, member := range members.Members {
		if member.VNode == source.VNode {
			onlySource.Members = append(onlySource.Members, member)
		}
	}
	if err := reader.Store.MergeMembership(onlySource); err != nil {
		t.Fatal(err)
	}
	known, err := reader.Membership()
	if err != nil {
		t.Fatal(err)
	}
	if known.Current().HasName(delegate.VNode.Name) {
		t.Fatal("reader unexpectedly learned delegate membership")
	}
	refs, err := moved.Store.ReadDirectory()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range refs {
		if _, overlap := ref.Region.Intersect(rightHalf); overlap && ref.Endpoint.Owner == delegate.Self.Owner {
			found = true
		}
	}
	if !found {
		t.Fatal("physical move lost empty delegation directory")
	}
	net.setBlocked(source.Self.Host, true)
	reader.Peers = readOnlyTransport(net, nil)
	const collection = "Demo000000000000"
	var zone tree.ZoneKey
	for _, location := range tree.ChildCandidates(tree.BASE64, "@") {
		candidate, _ := tree.NewZoneKey(collection, location)
		if rightHalf.Contains(candidate.ID) {
			zone = candidate
			break
		}
	}
	if zone.Collection == "" {
		t.Fatal("no delegated empty address found")
	}
	rows, err := reader.QueryPrefixMetrics(ctx, collection, zone.Location)
	if err != nil || len(rowSet(t, rows, collection, zone.Location)) != 0 {
		t.Fatalf("empty delegated coverage became unknown: %+v %v", rows, err)
	}
	row := storage.Record{Collection: collection, Location: zone.Location, ID: "first-ACK-after-move"}
	if _, err := delegate.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	rows, err = reader.QueryPrefixMetrics(ctx, collection, zone.Location)
	if err != nil || len(rowSet(t, rows, collection, zone.Location)) != 1 {
		t.Fatalf("migrated empty delegation lost later ACK: %+v %v", rows, err)
	}
}
