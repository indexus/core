package mesh

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/testkit"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func lostRegionalReceipt(t *testing.T, net *network, source, target *Service, input causal.Right, region tree.Region) causal.Cession {
	t.Helper()
	lost := errors.New("lost regional install ACK")
	var once atomic.Bool
	var c causal.Cession
	net.setAfter(func(r Request) error {
		if r.Kind == "install" && r.Cession != nil && r.Cession.Target.Owner == target.Self.Owner && once.CompareAndSwap(false, true) {
			c = *r.Cession
			return lost
		}
		return nil
	})
	err := source.handoff(context.Background(), Request{Input: input.ID, Target: target.Self, TargetVNode: target.VNode, Region: region})
	net.setAfter(nil)
	if !errors.Is(err, lost) || !once.Load() {
		t.Fatalf("regional ACK loss not exercised: %v", err)
	}
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Transfers[c.ID].Phase != causal.TransferIssued {
		t.Fatal("source did not retain issued cession")
	}
	state, err = target.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if _, installed := state.Permissions.Find(c.Right().ID); !installed {
		t.Fatal("receiver did not install before reply was lost")
	}
	return c
}
func physicalDrainTarget(t *testing.T, net *network, source *Service, host string) *Service {
	t.Helper()
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	disk, err := storage.OpenWithOptions(filepath.Join(t.TempDir(), host), source.Self.VNode, "test", 1<<26, testStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { disk.Close() })
	endpoint, err := disk.Endpoint(host)
	if err != nil {
		t.Fatal(err)
	}
	target := &Service{Store: disk, Self: endpoint, VNode: source.VNode, Peers: net}
	t.Cleanup(target.Close)
	if err := disk.InitCausalReceiver(state.Root, state.Genesis); err != nil {
		t.Fatal(err)
	}
	net.mu.Lock()
	net.nodes[host] = target
	net.mu.Unlock()
	return target
}
func requireCessionClosed(t *testing.T, source *Service, id causal.TransferID) {
	t.Helper()
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if op, exists := state.Transfers[id]; exists && op.Phase != causal.TransferInstalled {
		t.Fatalf("source cession still open: %+v", op)
	}
}

func TestLostInstallReplyClosesAfterPhysicalReceiverDeparture(t *testing.T) {
	net, nodes := meshFixture(t, 2)
	source, receiver := nodes[0], nodes[1]
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(15*time.Second))
	defer cancel()
	row := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "seed"}
	if _, err := source.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	input := installedReadRight(t, source, tree.ID{})
	c := lostRegionalReceipt(t, net, source, receiver, input, tree.Region{})
	row.ID = "ACK-after-install-before-departure"
	if _, err := receiver.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	collectReadTransferReceipts(t, receiver)
	moved := physicalDrainTarget(t, net, receiver, "receiver-next-disk")
	var earlyPoint atomic.Bool
	net.setAfter(func(request Request) error {
		if request.Kind != "install-zone" || len(request.ZoneTransfers) == 0 || request.ZoneTransfers[0].To.Owner != moved.Self.Owner || !earlyPoint.CompareAndSwap(false, true) {
			return nil
		}
		_, _, membership, err := moved.Store.Membership()
		if err != nil {
			return err
		}
		for _, member := range membership.Members {
			if member.VNode.Name == receiver.VNode.Name && member.Endpoint.Owner == moved.Self.Owner {
				return fmt.Errorf("physical receiver published logical activation before MOVE: %+v", member)
			}
		}
		early := row
		early.ID = "ACK-before-MOVE-publication"
		if _, err := moved.Store.Write(early.ID, early); err != nil {
			return err
		}
		rows, err := source.QueryPrefixMetrics(ctx, row.Collection, "@")
		if err != nil || len(rowSet(t, rows, row.Collection, "@")) != 3 {
			return fmt.Errorf("non-root physical receiver could not serve early ACK without publication: %+v %v", rows, err)
		}
		return nil
	})
	if err := moved.ReceiveMove(ctx, receiver.Self.Host); err != nil {
		t.Fatal(err)
	}
	net.setAfter(nil)
	if !earlyPoint.Load() {
		t.Fatal("physical first point was not exercised")
	}
	net.setBlocked(receiver.Self.Host, true)
	if err := source.Sync(ctx, []string{moved.Self.Host}); err != nil {
		t.Fatal(err)
	}
	route, err := source.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if !route.ReceiverDrained(c.Target) {
		t.Fatal("completed physical departure did not retain terminal receipt")
	}
	if err := source.Resume(ctx); err != nil {
		t.Fatalf("old receiver departure stranded source: %v", err)
	}
	requireCessionClosed(t, source, c.ID)
	if err := source.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := source.QueryPrefixMetrics(ctx, row.Collection, "@")
	if err != nil || len(rowSet(t, rows, row.Collection, "@")) != 3 {
		t.Fatalf("terminal cleanup lost newer ACK: %+v %v", rows, err)
	}
}

func TestLostInstallReplyClosesAfterLogicalReceiverRetirement(t *testing.T) {
	net, nodes := meshFixture(t, 5)
	source, receiver, survivor := nodes[0], nodes[1], nodes[2]
	delegate, reader := nodes[3], nodes[4]
	delegate.VNode.ID = tree.ID{32, 1}
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(20*time.Second))
	defer cancel()
	receiver.VNode.ID = tree.ID{192, 1} // R:11; survivor T:10; source S:0.
	ten := tree.Region{Mask: tree.ID{192}, Value: tree.ID{128}}
	eleven := tree.Region{Mask: tree.ID{192}, Value: tree.ID{192}}
	input := installedReadRight(t, source, tree.ID{})
	if err := source.handoff(ctx, Request{Input: input.ID, Target: survivor.Self, TargetVNode: survivor.VNode, Region: ten}); err != nil {
		t.Fatal(err)
	}
	collectReadTransferReceipts(t, source, survivor)
	if err := source.Sync(ctx, []string{survivor.Self.Host}); err != nil {
		t.Fatal(err)
	}
	if err := receiver.Sync(ctx, []string{source.Self.Host}); err != nil {
		t.Fatal(err)
	}
	var zone tree.ZoneKey
	const collection = "Demo000000000000"
	for _, location := range tree.ChildCandidates(tree.BASE64, "@") {
		candidate, _ := tree.NewZoneKey(collection, location)
		if eleven.Contains(candidate.ID) {
			zone = candidate
			break
		}
	}
	if zone.Collection == "" {
		t.Fatal("missing R region fixture")
	}
	row := storage.Record{Collection: collection, Location: zone.Location, ID: "seed"}
	if _, err := source.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	input = installedReadRight(t, source, zone.ID)
	c := lostRegionalReceipt(t, net, source, receiver, input, eleven)
	row.ID = "ACK-before-retirement"
	if _, err := receiver.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	collectReadTransferReceipts(t, receiver)
	// C joins from a stale root-only view (ID bit 32), so its desired slice
	// legitimately intersects 111 even though the converged XOR placement is T.
	// No row exists there: retirement must carry this actual exception itself.
	emptyException := tree.Region{Mask: tree.ID{224}, Value: tree.ID{224}}
	childInput := installedReadRight(t, receiver, tree.ID{224})
	if err := receiver.handoff(ctx, Request{Input: childInput.ID, Target: delegate.Self, TargetVNode: delegate.VNode, Region: emptyException}); err != nil {
		t.Fatal(err)
	}
	collectReadTransferReceipts(t, receiver, delegate)
	if err := receiver.Sync(ctx, []string{delegate.Self.Host}); err != nil {
		t.Fatal(err)
	}
	if err := receiver.Sync(ctx, []string{survivor.Self.Host}); err != nil {
		t.Fatal(err)
	}
	if err := receiver.Compact(ctx, ColdConfig{}); err != nil {
		t.Fatalf("honest retirement could not drain its actual rights: %v", err)
	}
	net.setBlocked(receiver.Self.Host, true)
	if err := source.Sync(ctx, []string{survivor.Self.Host}); err != nil {
		t.Fatal(err)
	}
	route, err := source.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if !route.ReceiverDrained(c.Target) {
		t.Fatal("retirement did not provide terminal receipt")
	}
	if err := source.Resume(ctx); err != nil {
		t.Fatalf("retired receiver stranded issued source: %v", err)
	}
	requireCessionClosed(t, source, c.ID)
	rows, err := source.QueryPrefixMetrics(ctx, collection, "@")
	if err != nil || len(rowSet(t, rows, collection, "@")) != 2 {
		t.Fatalf("retirement terminal receipt lost data: %+v %v", rows, err)
	}
	_, _, knownMembers, err := survivor.Store.Membership()
	if err != nil {
		t.Fatal(err)
	}
	var withoutDelegate causal.MembershipDelta
	for _, member := range knownMembers.Members {
		if member.VNode.Name != delegate.VNode.Name {
			withoutDelegate.Members = append(withoutDelegate.Members, member)
		}
	}
	if err := reader.Store.MergeMembership(withoutDelegate); err != nil {
		t.Fatal(err)
	}
	known, err := reader.Membership()
	if err != nil {
		t.Fatal(err)
	}
	if known.Current().HasName(delegate.VNode.Name) {
		t.Fatal("observer unexpectedly learned empty exception holder")
	}
	reader.Peers = readOnlyTransport(net, nil)
	var emptyZone tree.ZoneKey
	for _, location := range tree.ChildCandidates(tree.BASE64, "@") {
		candidate, _ := tree.NewZoneKey(collection, location)
		if emptyException.Contains(candidate.ID) {
			emptyZone = candidate
			break
		}
	}
	if emptyZone.Collection == "" {
		t.Fatal("missing delegated empty exception fixture")
	}
	rows, err = reader.QueryPrefixMetrics(ctx, collection, emptyZone.Location)
	if err != nil || len(rowSet(t, rows, collection, emptyZone.Location)) != 0 {
		t.Fatalf("retirement lost empty exception coverage: %+v %v", rows, err)
	}
	first := storage.Record{Collection: collection, Location: emptyZone.Location, ID: "first-ACK-on-empty-retired-exception"}
	if _, err := delegate.Store.Write(first.ID, first); err != nil {
		t.Fatal(err)
	}
	rows, err = reader.QueryPrefixMetrics(ctx, collection, emptyZone.Location)
	if err != nil || len(rowSet(t, rows, collection, emptyZone.Location)) != 1 {
		t.Fatalf("retired directory lost future delegated ACK: %+v %v", rows, err)
	}

}

func TestAddressChangeCannotCloseUninstalledReceiverTicket(t *testing.T) {
	net, nodes := meshFixture(t, 2)
	source, receiver := nodes[0], nodes[1]
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(15*time.Second))
	defer cancel()
	receiver.VNode.ID = tree.ID{128, 1}
	ten := tree.Region{Mask: tree.ID{192}, Value: tree.ID{128}}
	eleven := tree.Region{Mask: tree.ID{192}, Value: tree.ID{192}}
	input := installedReadRight(t, source, tree.ID{})
	if err := source.handoff(ctx, Request{Input: input.ID, Target: receiver.Self, TargetVNode: receiver.VNode, Region: ten}); err != nil {
		t.Fatal(err)
	}
	collectReadTransferReceipts(t, source, receiver)
	if err := source.Sync(ctx, []string{receiver.Self.Host}); err != nil {
		t.Fatal(err)
	}
	input = installedReadRight(t, source, tree.ID{192})
	blocked := errors.New("native install not yet delivered")
	var c causal.Cession
	net.setBefore(func(r Request) error {
		if r.Kind == "install" && r.Cession != nil && r.Cession.Target.Owner == receiver.Self.Owner {
			c = *r.Cession
			return blocked
		}
		return nil
	})
	err := source.handoff(ctx, Request{Input: input.ID, Target: receiver.Self, TargetVNode: receiver.VNode, Region: eleven})
	net.setBefore(nil)
	if !errors.Is(err, blocked) || !c.ID.Valid() {
		t.Fatalf("uninstalled ticket fixture failed: %v", err)
	}
	state, _ := receiver.Store.CausalState()
	if _, installed := state.Permissions.Find(c.Right().ID); installed {
		t.Fatal("fixture already installed tested ticket")
	}
	next := receiver.Self
	next.Host = "same-disk-new-address"
	next.Epoch++
	if _, err := receiver.Store.PublishActivation(receiver.VNode, next); err != nil {
		t.Fatalf("honest same-disk address update refused: %v", err)
	}
	receiver.Close()
	restarted := &Service{Store: receiver.Store, Self: next, VNode: receiver.VNode, Peers: net}
	t.Cleanup(restarted.Close)
	net.mu.Lock()
	net.nodes[next.Host] = restarted
	net.mu.Unlock()
	net.setBlocked(receiver.Self.Host, true)
	var prevent atomic.Bool
	prevent.Store(true)
	var newHostCalls atomic.Int32
	source.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if r.Kind == "install" && r.Cession != nil && r.Cession.ID == c.ID {
			if host != next.Host {
				return Response{}, fmt.Errorf("retried old address %s", host)
			}
			newHostCalls.Add(1)
			if prevent.Load() {
				return Response{}, blocked
			}
		}
		return net.Exchange(ctx, host, r)
	})
	if err := source.Sync(ctx, []string{next.Host}); err != nil {
		t.Fatal(err)
	}
	route, err := source.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if route.ReceiverDrained(c.Target) {
		t.Fatal("same-owner epoch advance falsely certified a drained ticket")
	}
	if err := source.Resume(ctx); !errors.Is(err, blocked) {
		t.Fatalf("source did not retry live same-disk ticket: %v", err)
	}
	state, _ = source.Store.CausalState()
	if state.Transfers[c.ID].Phase != causal.TransferIssued {
		t.Fatal("address-only hint prematurely completed source")
	}
	state, _ = receiver.Store.CausalState()
	if _, installed := state.Permissions.Find(c.Right().ID); installed {
		t.Fatal("blocked install was nevertheless mounted")
	}
	if newHostCalls.Load() == 0 {
		t.Fatal("same-owner retry never reached new address")
	}
	prevent.Store(false)
	if err := source.Resume(ctx); err != nil {
		t.Fatalf("same-disk ticket did not resume on new address: %v", err)
	}
	requireCessionClosed(t, source, c.ID)
	state, _ = receiver.Store.CausalState()
	if _, installed := state.Permissions.Find(c.Right().ID); !installed {
		t.Fatal("successful same-disk retry did not install")
	}
}

func TestPhysicalReturnUsesOriginalClosedTicketNamespace(t *testing.T) {
	net, nodes := meshFixture(t, 2)
	source, receiver := nodes[0], nodes[1]
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(20*time.Second))
	defer cancel()
	row := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "seed"}
	if _, err := source.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	input := installedReadRight(t, source, tree.ID{})
	c := lostRegionalReceipt(t, net, source, receiver, input, tree.Region{})
	row.ID = "ACK-before-roundtrip"
	if _, err := receiver.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	collectReadTransferReceipts(t, receiver)
	moved := physicalDrainTarget(t, net, receiver, "roundtrip-middle")
	if err := moved.ReceiveMove(ctx, receiver.Self.Host); err != nil {
		t.Fatal(err)
	}
	collectReadTransferReceipts(t, receiver, moved)
	if err := receiver.ReceiveMove(ctx, moved.Self.Host); err != nil {
		t.Fatal(err)
	}
	collectReadTransferReceipts(t, receiver, moved)
	if err := source.Sync(ctx, []string{receiver.Self.Host}); err != nil {
		t.Fatal(err)
	}
	route, err := source.Store.CausalRouting()
	if err != nil {
		t.Fatal(err)
	}
	if route.ReceiverDrained(c.Target) {
		t.Fatal("return to original owner guessed a terminal receipt")
	}
	var retries atomic.Int32
	source.Peers = transportFunc(func(ctx context.Context, host string, r Request) (Response, error) {
		if r.Kind == "install" && r.Cession != nil && r.Cession.ID == c.ID {
			if host != receiver.Self.Host || r.Cession.Ticket != c.Ticket {
				return Response{}, fmt.Errorf("original ticket rebound to another disk")
			}
			retries.Add(1)
		}
		return net.Exchange(ctx, host, r)
	})
	if err := source.Resume(ctx); err != nil {
		t.Fatalf("old closed ticket failed after physical return: %v", err)
	}
	requireCessionClosed(t, source, c.ID)
	if retries.Load() == 0 {
		t.Fatal("source did not use original durable ticket namespace")
	}
	state, _ := receiver.Store.CausalState()
	if _, live := state.Permissions.Find(c.Right().ID); live {
		t.Fatal("old native right was resurrected")
	}
	rows, err := receiver.QueryPrefixMetrics(ctx, row.Collection, "@")
	if err != nil || len(rowSet(t, rows, row.Collection, "@")) != 2 {
		t.Fatalf("closed-ticket reply remounted obsolete data: %+v %v", rows, err)
	}
}

// An unresolved old receipt may retain backup data, but its fence cannot follow
// the geometry forever and suppress a distinct permission acquired later.
func TestOldIssuedFenceCannotBlockReacquiredPointOrNativePermission(t *testing.T) {
	net, nodes := meshFixture(t, 3)
	source, first, next := nodes[0], nodes[1], nodes[2]
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(20*time.Second))
	defer cancel()
	row := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "seed"}
	if _, err := source.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	original := lostRegionalReceipt(t, net, source, first, installedReadRight(t, source, tree.ID{}), tree.Region{})
	row.ID = "ACK-on-first-receiver"
	if _, err := first.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	collectReadTransferReceipts(t, first)
	input := installedReadRight(t, first, tree.ID{})
	if err := first.handoff(ctx, Request{Input: input.ID, Target: next.Self, TargetVNode: next.VNode, Region: tree.Region{}}); err != nil {
		t.Fatal(err)
	}
	collectReadTransferReceipts(t, first, next)
	// R remains an active logical vnode. No MOVE/retirement fact can stand in for
	// its old closed ticket, and it is now unreachable to the original source.
	net.setBlocked(first.Self.Host, true)
	var pointChecked atomic.Bool
	net.setAfter(func(request Request) error {
		if request.Kind != "install-zone" || len(request.ZoneTransfers) == 0 || request.ZoneTransfers[0].To.Owner != source.Self.Owner || !pointChecked.CompareAndSwap(false, true) {
			return nil
		}
		newer := row
		newer.ID = "ACK-on-reacquired-point"
		if _, err := source.Store.Write(newer.ID, newer); err != nil {
			return fmt.Errorf("old issued fence blocked new point permission: %w", err)
		}
		state, err := source.Store.CausalState()
		if err != nil {
			return err
		}
		if state.Transfers[original.ID].Phase != causal.TransferIssued {
			return fmt.Errorf("fixture silently closed old receipt")
		}
		rows, err := source.QueryPrefixMetrics(ctx, row.Collection, row.Location)
		if err != nil || len(rowSet(t, rows, row.Collection, row.Location)) != 3 {
			return fmt.Errorf("old issued referral blocked new point ACK read: %+v %v", rows, err)
		}
		return nil
	})
	input = installedReadRight(t, next, tree.ID{})
	if err := next.handoff(ctx, Request{Input: input.ID, Target: source.Self, TargetVNode: source.VNode, Region: tree.Region{}}); err != nil {
		t.Fatal(err)
	}
	net.setAfter(nil)
	if !pointChecked.Load() {
		t.Fatal("reacquired provisional point was not exercised")
	}
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Transfers[original.ID].Phase != causal.TransferIssued {
		t.Fatal("old source receipt unexpectedly disappeared")
	}
	if _, resurrected := state.Permissions.Find(original.Input.ID); resurrected {
		t.Fatal("old consumed input was resurrected")
	}
	row.ID = "ACK-on-reacquired-native"
	if _, err := source.Store.Write(row.ID, row); err != nil {
		t.Fatalf("old issued fence blocked later native right: %v", err)
	}
	rows, err := source.QueryPrefixMetrics(ctx, row.Collection, row.Location)
	if err != nil || len(rowSet(t, rows, row.Collection, row.Location)) != 4 {
		t.Fatalf("old receiver partition hid new native ACK: %+v %v", rows, err)
	}
	for _, node := range []*Service{first, next} {
		if _, _, err := node.Store.Apply("old-owner-probe", row); !errors.Is(err, storage.ErrNotOwner) {
			t.Fatalf("a former owner still writes: %v", err)
		}
	}
}

func TestCancelledWarmReturnKeepsCurrentSourceWithoutOldReceiver(t *testing.T) {
	net, nodes := meshFixture(t, 3)
	source, first, current := nodes[0], nodes[1], nodes[2]
	ctx, cancel := context.WithTimeout(context.Background(), testkit.RaceScale(20*time.Second))
	defer cancel()
	row := storage.Record{Collection: "Demo000000000000", Location: "aa", ID: "seed"}
	if _, err := source.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	original := lostRegionalReceipt(t, net, source, first, installedReadRight(t, source, tree.ID{}), tree.Region{})
	collectReadTransferReceipts(t, first)
	input := installedReadRight(t, first, tree.ID{})
	if err := first.handoff(ctx, Request{Input: input.ID, Target: current.Self, TargetVNode: current.VNode, Region: tree.Region{}}); err != nil {
		t.Fatal(err)
	}
	collectReadTransferReceipts(t, first, current)
	row.ID = "ACK-on-current-source"
	if _, err := current.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	net.setBlocked(first.Self.Host, true)
	// The fresh source confirms a warm return but cancels it BEFORE any fence.
	// It retains native authority. Forgetting the slot must not restore S's old
	// mandatory referral to partitioned R or forget the newly learned source C.
	input = installedReadRight(t, current, tree.ID{})
	warm := confirmedTransfer(t, current, source, input.ID, tree.Region{})
	if err := current.Store.AbortCausal(warm.Cession.ID); err != nil {
		t.Fatal(err)
	}
	if err := source.Store.CancelCausal(warm.Cession); err != nil {
		t.Fatal(err)
	}
	if err := current.Store.FinishAbortCausal(warm.Cession.ID); err != nil {
		t.Fatal(err)
	}
	state, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Transfers[original.ID].Phase != causal.TransferIssued {
		t.Fatal("fixture silently closed old receipt")
	}
	if _, exists := state.Transfers[warm.Cession.ID]; exists {
		t.Fatal("cancelled receiving slot survived")
	}
	row.ID = "ACK-after-warm-return-cancelled"
	if _, err := current.Store.Write(row.ID, row); err != nil {
		t.Fatal(err)
	}
	source.Peers = readOnlyTransport(net, nil)
	rows, err := source.QueryPrefixMetrics(ctx, row.Collection, row.Location)
	if err != nil || len(rowSet(t, rows, row.Collection, row.Location)) != 3 {
		t.Fatalf("warm cancellation resurrected obsolete mandatory receiver: %+v %v", rows, err)
	}
	if _, _, err := source.Store.Apply("no-returned-permission", row); err == nil {
		t.Fatal("cancelled warm return granted write permission")
	}
}
