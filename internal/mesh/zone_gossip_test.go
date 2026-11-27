package mesh

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// ZONE-GOSSIP-1 (docs/protocol/zone-transfer-scope.md). A migration is stopped
// right after its first install-zone. A third node that never took part in it:
//   - learns no per-zone transfer from the source;
//   - is refused a complete read while the transfer is open, never served
//     part of it;
//   - accepts the receiver's activation, whose one cited transfer travels with
//     it, and still holds no other transfer;
//   - reads every row once after the migration, without syncing first.
func TestZoneTransfersStayBetweenSourceAndReceiver(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 3)
	source, receiver, reader := nodes[0], nodes[1], nodes[2]
	ctx := context.Background()
	view := tree.NewView(0)
	_ = view.Add(source.VNode.Name, source.VNode.ID)
	_ = view.Add(receiver.VNode.Name, receiver.VNode.ID)
	var seeded []storage.Record
	moving := 0
	for _, loc := range []string{"W", "X", "m", "n", "aa", "ab", "B", "C"} {
		record := storage.Record{Collection: "Probe00000000000", Location: loc, ID: "row", Metrics: []float64{1}}
		id, err := tree.ZoneID(record.Collection, record.Location)
		if err != nil {
			t.Fatal(err)
		}
		if view.Owner(id) == receiver.VNode.ID {
			moving++
		}
		if _, err := source.Write(ctx, "seed-"+loc, record); err != nil {
			t.Fatal(err)
		}
		seeded = append(seeded, record)
	}
	if moving < 2 || moving == len(seeded) {
		t.Fatalf("need zones on both sides of the split, %d of %d move", moving, len(seeded))
	}

	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var flipped tree.ZoneKey
	net.setAfter(func(request Request) error {
		if request.Kind == "install-zone" {
			once.Do(func() { flipped = request.ZoneTransfers[0].Zone; close(reached) })
			<-release
		}
		return nil
	})
	joined := make(chan error, 1)
	go func() { joined <- receiver.Join(ctx) }()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		if err := <-joined; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-reached:
	case err := <-joined:
		t.Fatalf("join returned before the first install-zone: %v", err)
	}
	// XFER-KEYS-1: the source holds the transfer as a local row, outside its
	// knowledge, and forwards the zone to its receiver.
	local, err := source.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if len(local.Transfers) != 1 {
		t.Fatal("source lost current pairwise transfer")
	}
	if rows, err := source.Store.ZoneTransfers(flipped); err != nil || len(rows) != 1 {
		t.Fatalf("the source holds %d rows for the flipped zone: %v", len(rows), err)
	}
	if holder, err := source.Store.ZoneHolder(flipped); err != nil || holder != receiver.Self {
		t.Fatalf("source forwards the flipped zone to %v (%v), want the receiver", holder, err)
	}

	// 1. The reader knows only what the source gossips: no per-zone transfer.
	// A complete read refuses during the transfer rather than return part of
	// it: the source keeps the frozen zones' fence until the certificate.
	if err := reader.Sync(ctx, []string{source.Self.Host}); err != nil {
		t.Fatal(err)
	}
	learned, err := reader.Store.CausalState()
	if err != nil {
		t.Fatal(err)
	}
	if len(learned.Transfers) != 0 || len(learned.Permissions.Native) != 0 {
		t.Fatal("membership gossip installed transfer rights")
	}
	// Reads are served during the transfer, from the source's frozen zones.
	if _, err := reader.Query(ctx); err != nil {
		t.Fatalf("third-node read refused during the transfer: %v", err)
	}

	// 2. The receiver's activation reaches the reader with its one cited
	// transfer, and with nothing else.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := reader.Sync(ctx, []string{receiver.Self.Host}); err != nil {
			t.Fatal(err)
		}
		state, err := reader.Store.CausalState()
		if err != nil {
			t.Fatal(err)
		}
		replica, err := state.MembershipReplica()
		if err != nil {
			t.Fatal(err)
		}
		if replica.Current().HasName(receiver.VNode.Name) {
			if len(state.Transfers) != 0 || len(state.Permissions.Native) != 0 {
				t.Fatal("activation gossip installed authority on observer")
			}
			if proofs, e := reader.Store.ZoneTransfers(flipped); e != nil || len(proofs) != 0 {
				t.Fatalf("per-zone proof escaped pairwise participants: %v %v", proofs, e)
			}

			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reader never accepted the receiver's activation")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 3. Once the migration is over, the same reader, without syncing, reads
	// every row once: the source's reply carries the certificate and the read
	// retries on the new holders.
	close(release)
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	joined <- nil
	rows, err := reader.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, snap := range rows {
		for _, record := range snap.Records {
			if !record.Deleted {
				seen[record.Location]++
			}
		}
	}
	for _, record := range seeded {
		if seen[record.Location] != 1 {
			t.Fatalf("third-node read after the migration: %v, want every seeded location once", seen)
		}
	}
}
