package storage

import (
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// stageRequests stages receipts the ledger does not hold in one session: a
// transfer that is reserved, or cancelled before its install.
func stageRequests(t *testing.T, store *Store, id causal.TransferID, zone tree.ZoneKey, requests []string) {
	t.Helper()
	if err := store.updateCausal(func(tx *Tx, state CausalState) error {
		if state.Transfers == nil {
			state.Transfers = map[causal.TransferID]CausalTransfer{}
		}
		transfer := CausalTransfer{}
		transfer.Cession.ID, transfer.Phase = id, causal.TransferInstalled
		state.Transfers[id] = transfer
		snapshot := Snapshot{Zone: zone, Seq: uint64(len(requests))}
		for n, request := range requests {
			r := Record{Collection: zone.Collection, Location: zone.Location, ID: request}
			snapshot.Records = append(snapshot.Records, r)
			snapshot.Requests = append(snapshot.Requests, RequestReceipt{RequestID: request, Digest: digest(r), Seq: uint64(n + 1), Collection: zone.Collection, Location: zone.Location})
		}
		if err := stageSnapshot(tx, id.String(), "A", snapshot); err != nil {
			return err
		}
		return writeCausal(tx, state)
	}); err != nil {
		t.Fatal(err)
	}
}

// Staging and collecting receipts the ledger does not hold costs their
// number, not the sessions present: no other session can hold a copy of a
// request whose every copy the update changes. Codex's measurement of
// 2026-11-26 (45eb506): 136, 584, 2120 reads for 64 receipts with 1, 8, 32
// sessions.
func TestStagingCostIsIndependentOfTheSessionsPresent(t *testing.T) {
	saved := verifyUsage
	verifyUsage = false // the differential scan of every check would be the cost measured
	defer func() { verifyUsage = saved }()
	zone, err := tree.NewZoneKey("Collect000000000", "a")
	if err != nil {
		t.Fatal(err)
	}
	var stage, collect []int64
	for _, sessions := range []int{1, 8, 32} {
		_, store, _ := historyStores(t, 0, 0, false)
		var first causal.TransferID
		var staged receiverWork
		for i := range sessions {
			id := causal.TransferID{Actor: "copies", Sequence: uint64(i + 1)}
			requests := make([]string, 64)
			for n := range requests {
				requests[n] = fmt.Sprintf("%d-%d", i, n)
			}
			staged = measureReceiver(t, func() error { stageRequests(t, store, id, zone, requests); return nil })
			if i == 0 {
				first = id
			}
		}
		collected := measureReceiver(t, func() error { return store.compactStaging(first.String()) })
		assertStoredUsage(t, store)
		t.Logf("%2d sessions: the last one staged with %d reads, the first collected with %d", sessions, staged.reads, collected.reads)
		stage, collect = append(stage, staged.reads), append(collect, collected.reads)
	}
	for i := 1; i < len(stage); i++ {
		if stage[i] > stage[0]+4 || collect[i] > collect[0]+4 {
			t.Errorf("reads grow with the sessions present: staging %v, collecting %v", stage, collect)
		}
	}
}

// Two sessions that hold a copy of the same request are still both read:
// the reservation is its first copy, and dropping one leaves the other.
// The stored usage is compared with the full scan at every step.
func TestSharedCopiesStillReserveOnce(t *testing.T) {
	zone, err := tree.NewZoneKey("Collect000000000", "a")
	if err != nil {
		t.Fatal(err)
	}
	_, store, _ := historyStores(t, 0, 0, false)
	one, two := causal.TransferID{Actor: "copies", Sequence: 1}, causal.TransferID{Actor: "copies", Sequence: 2}
	stageRequests(t, store, one, zone, []string{"shared", "only-one"})
	stageRequests(t, store, two, zone, []string{"shared", "only-two"})
	assertStoredUsage(t, store)
	if got := store.copies["shared"]; got != 2 {
		t.Fatalf("%d copies of the shared request in memory", got)
	}
	if err := store.compactStaging(two.String()); err != nil {
		t.Fatal(err)
	}
	assertStoredUsage(t, store)
	if err := store.compactStaging(one.String()); err != nil {
		t.Fatal(err)
	}
	assertStoredUsage(t, store)
	if len(store.copies) != 0 {
		t.Fatalf("%d requests still have copies in memory", len(store.copies))
	}
}

// The copies are counted again from the staged rows when the store opens.
func TestCopiesAreCountedAgainAtOpen(t *testing.T) {
	zone, err := tree.NewZoneKey("Collect000000000", "a")
	if err != nil {
		t.Fatal(err)
	}
	_, store, _ := historyStores(t, 0, 0, false)
	stageRequests(t, store, causal.TransferID{Actor: "copies", Sequence: 1}, zone, []string{"b", "a"})
	stageRequests(t, store, causal.TransferID{Actor: "copies", Sequence: 2}, zone, []string{"b", "c"})
	want := map[string]int32{"a": 1, "b": 2, "c": 1}
	if fmt.Sprint(store.copies) != fmt.Sprint(want) {
		t.Fatalf("copies %v, want %v", store.copies, want)
	}
	// Open refuses the transfers of this fixture, which are no valid offers.
	if err := store.updateCausal(func(tx *Tx, state CausalState) error {
		state.Transfers = nil
		return writeCausal(tx, state)
	}); err != nil {
		t.Fatal(err)
	}
	path := store.path
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWithOptions(path, "B", "test", 1<<40, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if fmt.Sprint(reopened.copies) != fmt.Sprint(want) {
		t.Fatalf("copies %v after reopening, want %v", reopened.copies, want)
	}
}
