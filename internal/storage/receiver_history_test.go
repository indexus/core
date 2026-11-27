package storage

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
)

// The receiver of the AWS run of 2026-11-26 held its writer 67.76 s out of
// 68 s, a third of its CPU in the enumeration of the staging sessions. This
// file reproduces it on a real Store: one identical lot is preloaded and
// mounted on receivers that differ only by what they did before (staging
// sessions already collected) and by what else they hold (foreign zones).
// The numbers are in docs/benchmarks/receiver-writer-20261126.md.

const (
	historyLotZones    = 16 // zones of the measured lot
	historyLotRecords  = 4  // records of each
	historyDeadZones   = 4  // zones a collected session staged
	historyDeadRecords = 8  // records, and as many receipts, of each
)

// receiverWork is what one step cost the receiver.
type receiverWork struct {
	keys, dead     int64 // keys zonelog scans examined, and the dead ones among them
	scans, reads   int64 // scans and point reads of the transactions
	commits        int64
	capacityChecks int64
	elapsed        time.Duration
}

func measureReceiver(t *testing.T, step func() error) receiverWork {
	t.Helper()
	keys, dead := zonelog.ScanWork()
	scans, reads, commits, checks := rangeScans.Load(), pointReads.Load(), rowCommits.Load(), capacityChecks.Load()
	started := time.Now()
	if err := step(); err != nil {
		t.Fatal(err)
	}
	w := receiverWork{elapsed: time.Since(started)}
	w.keys, w.dead = zonelog.ScanWork()
	w.keys, w.dead = w.keys-keys, w.dead-dead
	w.scans, w.reads = rangeScans.Load()-scans, pointReads.Load()-reads
	w.commits, w.capacityChecks = rowCommits.Load()-commits, capacityChecks.Load()-checks
	return w
}

// historyStores opens a source that holds the lot and a receiver that holds
// foreign zones and has staged, installed and collected dead sessions.
// With flushed, the foreign zones are in a first segment and the tombstones
// of the dead sessions in a second one, as on a receiver that ran for a
// while; without, everything is in the memtable.
func historyStores(t *testing.T, dead, foreign int, flushed bool) (source, target *Store, genesis causal.Genesis) {
	t.Helper()
	dir := t.TempDir()
	open := func(node string) *Store {
		// Seeded identities: the order of the session names is the same in every run.
		entropy := rand.New(rand.NewSource(int64(node[0])))
		s, err := OpenWithOptions(filepath.Join(dir, node), node, "test", 1<<40, Options{NoSync: true, Entropy: entropy})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	source, target = open("A"), open("B")
	identity, err := source.Identity()
	if err != nil {
		t.Fatal(err)
	}
	holder := causal.Endpoint{Owner: causal.Owner{VNode: "A", Storage: identity}, Host: "A"}
	genesis = causal.Genesis{Cluster: "test", Holder: holder}
	for _, s := range []*Store{source, target} {
		if err := s.InitCausal(tree.VNodeDescriptor{Name: "A", ID: tree.ID{1}}, genesis); err != nil {
			t.Fatal(err)
		}
	}
	foreignZones(t, target, foreign)
	flush := func() {
		if !flushed {
			return
		}
		if err := target.db.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	flush()
	for i := range dead {
		// Half of the dead sessions sort before the live one, half after.
		actor := []string{"0dead", "zdead"}[i%2]
		deadSession(t, target, causal.TransferID{Actor: actor, Sequence: uint64(i + 1)})
	}
	flush()
	return source, target, genesis
}

// foreignZones writes n zones of one record each straight into the rows: the
// receiver holds them and the lot has nothing to do with them.
func foreignZones(t *testing.T, store *Store, n int) {
	t.Helper()
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	for from := 0; from < n; from += 1000 {
		if err := store.update(func(tx *Tx) error {
			for i := from; i < min(from+1000, n); i++ {
				location := string([]byte{alphabet[i/4096%64], alphabet[i/64%64], alphabet[i%64]})
				z, err := tree.NewZoneKey("Foreign000000000", location)
				if err != nil {
					return err
				}
				if _, _, err := tx.putRecord(z, Record{Collection: z.Collection, Location: z.Location, ID: "r", Metrics: []float64{1}}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// deadSession stages the copies of an installed transfer and collects them,
// as the receiver of a finished handoff does: its staged rows are deleted,
// and stay in zonelog as tombstones until a merge drops them. The transfer
// itself is forgotten, so that only the staging history differs.
func deadSession(t *testing.T, store *Store, operation causal.TransferID) {
	t.Helper()
	waitingSession(t, store, operation)
	if err := store.compactStaging(operation.String()); err != nil {
		t.Fatal(err)
	}
	if err := store.updateCausal(func(tx *Tx, state CausalState) error {
		delete(state.Transfers, operation)
		return writeCausal(tx, state)
	}); err != nil {
		t.Fatal(err)
	}
}

// waitingSession stages the copies of an installed transfer and leaves them
// there: a session that waits for its collection.
func waitingSession(t *testing.T, store *Store, operation causal.TransferID) {
	t.Helper()
	if err := store.updateCausal(func(tx *Tx, state CausalState) error {
		if state.Transfers == nil {
			state.Transfers = map[causal.TransferID]CausalTransfer{}
		}
		transfer := CausalTransfer{}
		transfer.Cession.ID, transfer.Phase = operation, causal.TransferInstalled
		state.Transfers[operation] = transfer
		for zone := range historyDeadZones {
			z, err := tree.NewZoneKey("Dead000000000000", fmt.Sprintf("d%d", zone))
			if err != nil {
				return err
			}
			snap := Snapshot{Zone: z, Seq: historyDeadRecords}
			for i := range historyDeadRecords {
				id := fmt.Sprintf("%s-%d-%d", operation, zone, i)
				r := Record{Collection: z.Collection, Location: z.Location, ID: id}
				snap.Records = append(snap.Records, r)
				snap.Requests = append(snap.Requests, RequestReceipt{Digest: digest(r), RequestID: id, Seq: uint64(i + 1), Collection: z.Collection, Location: z.Location})
			}
			if err := stageSnapshot(tx, operation.String(), "A", snap); err != nil {
				return err
			}
		}
		return writeCausal(tx, state)
	}); err != nil {
		t.Fatal(err)
	}
}

// preloadAndMount moves the lot from source to target and returns what the
// preload and the mount cost the target.
func preloadAndMount(t *testing.T, source, target *Store, genesis causal.Genesis) (preload, mount receiverWork, id causal.TransferID) {
	t.Helper()
	zones := make([]tree.ZoneKey, historyLotZones)
	for i := range zones {
		for k := range historyLotRecords {
			r := Record{Collection: "Demo000000000000", Location: fmt.Sprintf("a%c", 'a'+i), ID: fmt.Sprint("r", k), Metrics: []float64{1}}
			if _, err := source.Write(fmt.Sprintf("w-%d-%d", i, k), r); err != nil {
				t.Fatal(err)
			}
			zones[i], _ = recordZone(r)
		}
	}
	offer := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	for _, z := range zones {
		snap, err := source.Snapshot(z)
		if err != nil {
			t.Fatal(err)
		}
		offer.Snapshots = append(offer.Snapshots, snap)
	}
	preload = measureReceiver(t, func() error {
		_, err := target.PreloadCausal(offer)
		return err
	})
	_, seals, proofs, err := source.FenceZones(offer.Cession.ID, zones)
	if err != nil {
		t.Fatal(err)
	}
	mount = measureReceiver(t, func() error {
		return target.MountZones(offer.Cession.ID, source.node, nil, seals, nil, proofs)
	})
	for _, z := range zones {
		if _, err := target.Write("after-"+z.Location, Record{Collection: z.Collection, Location: z.Location, ID: "after"}); err != nil {
			t.Fatalf("the mounted lot is not writable: %v", err)
		}
	}
	return preload, mount, offer.Cession.ID
}

// The cost of a preload and of a mount depends on the lot, not on the
// sessions the receiver collected before nor on the zones it holds besides.
func TestReceiverCostIsIndependentOfHistory(t *testing.T) {
	saved := verifyUsage
	verifyUsage = false // the differential scan of every check would be the cost measured
	defer func() { verifyUsage = saved }()
	type run struct {
		dead, foreign  int
		flushed        bool
		preload, mount receiverWork
	}
	runs := []run{
		{dead: 0, foreign: 10_000}, {dead: 20, foreign: 10_000}, {dead: 200, foreign: 10_000},
		{dead: 0, foreign: 40_000}, {dead: 20, foreign: 40_000}, {dead: 200, foreign: 40_000},
		{dead: 200, foreign: 40_000, flushed: true},
	}
	t.Log("dead foreign flushed | step    keys   dead scans reads commits checks time")
	for i := range runs {
		r := &runs[i]
		source, target, genesis := historyStores(t, r.dead, r.foreign, r.flushed)
		r.preload, r.mount, _ = preloadAndMount(t, source, target, genesis)
		source.Close()
		target.Close()
		for _, step := range []struct {
			name string
			w    receiverWork
		}{{"preload", r.preload}, {"mount", r.mount}} {
			t.Logf("%4d %7d %7t | %-7s %6d %6d %5d %5d %7d %6d %s", r.dead, r.foreign, r.flushed, step.name, step.w.keys, step.w.dead, step.w.scans, step.w.reads, step.w.commits, step.w.capacityChecks, step.w.elapsed.Round(10*time.Microsecond))
		}
	}
	base := runs[0]
	for _, r := range runs[1:] {
		if r.preload.keys != base.preload.keys || r.mount.keys != base.mount.keys {
			t.Errorf("%d dead sessions, %d foreign zones: preload examined %d keys and mount %d; with none and %d zones, %d and %d",
				r.dead, r.foreign, r.preload.keys, r.mount.keys, base.foreign, base.preload.keys, base.mount.keys)
		}
		if r.preload.commits != base.preload.commits || r.mount.commits != base.mount.commits {
			t.Errorf("%d dead sessions, %d foreign zones: %d and %d commits, not %d and %d", r.dead, r.foreign, r.preload.commits, r.mount.commits, base.preload.commits, base.mount.commits)
		}
	}
}

// Collecting a transfer drops the staged copies of requests the ledger
// holds: it reserves nothing before nor after, whatever else is staged.
// AWS 2026-11-26, receiver 10.77.8.11: the writer was held 9.85 s of 10 by
// collections that read, for each receipt, one row per staging session.
func TestCollectionCostIsIndependentOfWaitingSessions(t *testing.T) {
	saved := verifyUsage
	verifyUsage = false // the differential scan of every check would be the cost measured
	defer func() { verifyUsage = saved }()
	var base receiverWork
	for _, waiting := range []int{0, 8, 32} {
		source, target, genesis := historyStores(t, 0, 0, false)
		for i := range waiting {
			waitingSession(t, target, causal.TransferID{Actor: []string{"0wait", "zwait"}[i%2], Sequence: uint64(i + 1)})
		}
		_, _, id := preloadAndMount(t, source, target, genesis)
		if err := target.updateCausal(func(tx *Tx, state CausalState) error {
			transfer := state.Transfers[id]
			transfer.Phase = causal.TransferInstalled
			state.Transfers[id] = transfer
			return writeCausal(tx, state)
		}); err != nil {
			t.Fatal(err)
		}
		collect := measureReceiver(t, func() error { return target.compactStaging(id.String()) })
		t.Logf("%2d waiting sessions: collect read %d rows, examined %d keys, %d commits", waiting, collect.reads, collect.keys, collect.commits)
		if waiting == 0 {
			base = collect
			continue
		}
		if collect.reads != base.reads {
			t.Errorf("%d waiting sessions: the collection read %d rows, %d with none", waiting, collect.reads, base.reads)
		}
	}
}
