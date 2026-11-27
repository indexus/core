package storage

import (
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// Unlike ReceiverCostIsIndependentOfHistory, this measures sessions which
// still contain staging and the collection itself, not just preload/mount.
// Times are diagnostics; correctness uses exact accounting and row contents.
func TestCollectionWorkAcrossLiveSessionsAndReceipts(t *testing.T) {
	saved := verifyUsage
	verifyUsage = false // exclude the differential oracle from the measured work
	defer func() { verifyUsage = saved }()
	for _, sessions := range []int{1, 8, 32} {
		for _, receipts := range []int{1, 16, 64} {
			t.Run(fmt.Sprintf("sessions=%d/receipts=%d", sessions, receipts), func(t *testing.T) {
				_, store, _ := historyStores(t, 0, 0, false)
				zone, err := tree.NewZoneKey("Collect000000000", "a")
				if err != nil {
					t.Fatal(err)
				}
				var operations []causal.TransferID
				for i := range sessions {
					id := causal.TransferID{Actor: "collection-work", Sequence: uint64(i + 1)}
					operations = append(operations, id)
					if err := store.updateCausal(func(tx *Tx, state CausalState) error {
						if state.Transfers == nil {
							state.Transfers = map[causal.TransferID]CausalTransfer{}
						}
						transfer := CausalTransfer{}
						transfer.Cession.ID, transfer.Phase = id, causal.TransferInstalled
						state.Transfers[id] = transfer
						snapshot := Snapshot{Zone: zone, Seq: uint64(receipts)}
						for n := range receipts {
							// Disjoint receipts: other sessions should not need to be
							// searched merely to prove they contain no such request.
							r := Record{Collection: zone.Collection, Location: zone.Location, ID: fmt.Sprintf("%d-%d", i, n)}
							snapshot.Records = append(snapshot.Records, r)
							snapshot.Requests = append(snapshot.Requests, RequestReceipt{RequestID: r.ID, Digest: digest(r), Seq: uint64(n + 1), Collection: zone.Collection, Location: zone.Location})
						}
						if err := stageSnapshot(tx, id.String(), "A", snapshot); err != nil {
							return err
						}
						return writeCausal(tx, state)
					}); err != nil {
						t.Fatal(err)
					}
				}
				assertStoredUsage(t, store)
				work := measureReceiver(t, func() error { return store.compactStaging(operations[0].String()) })
				assertStoredUsage(t, store)
				if err := store.view(func(tx *Tx) error {
					indexed, err := stagingSessions(tx)
					if err == nil && len(indexed) != sessions-1 {
						t.Errorf("collection changed unrelated sessions: %d remain, want %d", len(indexed), sessions-1)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				t.Logf("sessions=%d receipts=%d reads=%d scanned=%d commits=%d elapsed=%s", sessions, receipts, work.reads, work.keys, work.commits, work.elapsed)
			})
		}
	}
}
