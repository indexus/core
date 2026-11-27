package storage

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// preloadOffer is a receiver of the given capacity, admitted to a transfer,
// and the offer that preloads three zones of the source into it.
func preloadOffer(t *testing.T, capacity int64) (*Store, CausalTransfer) {
	t.Helper()
	dir := t.TempDir()
	source := openTestData(t, filepath.Join(dir, "a"), "A")
	target, err := Open(filepath.Join(dir, "b"), "B", "test", capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close(); target.Close() })
	identity, err := source.Identity()
	if err != nil {
		t.Fatal(err)
	}
	genesis := causal.Genesis{Cluster: "test", Holder: causal.Endpoint{Owner: causal.Owner{VNode: "A", Storage: identity}, Host: "A"}}
	for _, s := range []*Store{source, target} {
		if err := s.InitCausal(tree.VNodeDescriptor{Name: "A", ID: tree.ID{1}}, genesis); err != nil {
			t.Fatal(err)
		}
	}
	var zones []tree.ZoneKey
	for i, location := range []string{"aa", "ab", "ac"} {
		r := Record{Collection: "Demo000000000000", Location: location, ID: "r", Metrics: []float64{float64(i)}, Payload: []byte(`"payload"`)}
		if _, err := source.Write("w-"+location, r); err != nil {
			t.Fatal(err)
		}
		z, _ := recordZone(r)
		zones = append(zones, z)
	}
	offer := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	for _, z := range zones {
		snap, err := source.Snapshot(z)
		if err != nil {
			t.Fatal(err)
		}
		offer.Snapshots = append(offer.Snapshots, snap)
	}
	return target, offer
}

func physicalLoad(t *testing.T, s *Store) int64 {
	t.Helper()
	_, used, err := s.CapacityUsage()
	if err != nil {
		t.Fatal(err)
	}
	return used.Owned + used.Residual + used.Staging + used.ReservedIncoming
}

// A preload checks the capacity once, after its last write; a replay too.
func TestPreloadChecksCapacityOnce(t *testing.T) {
	target, offer := preloadOffer(t, 1<<24)
	for _, attempt := range []string{"preload", "replay"} {
		before := capacityChecks.Load()
		if _, err := target.PreloadCausal(offer); err != nil {
			t.Fatalf("%s: %v", attempt, err)
		}
		if checks := capacityChecks.Load() - before; checks != 1 {
			t.Fatalf("%s: %d capacity checks in one transaction, want 1", attempt, checks)
		}
	}
}

// The one check that is left refuses exactly: a receiver with the bytes the
// preload needs takes it, a receiver one byte short refuses it and writes
// nothing, the reservation of the session included.
func TestPreloadOverCapacityIsRefusedExactlyAndWritesNothing(t *testing.T) {
	probe, offer := preloadOffer(t, 1<<24)
	admitted := physicalLoad(t, probe)
	if _, err := probe.PreloadCausal(offer); err != nil {
		t.Fatal(err)
	}
	needed := physicalLoad(t, probe)
	if needed <= admitted {
		t.Fatalf("the preload takes no capacity: %d bytes before, %d after", admitted, needed)
	}

	exact, offer := preloadOffer(t, needed)
	if _, err := exact.PreloadCausal(offer); err != nil {
		t.Fatalf("a receiver of exactly %d bytes refused the preload: %v", needed, err)
	}
	if got := physicalLoad(t, exact); got != needed {
		t.Fatalf("usage %d after the preload, want %d", got, needed)
	}

	short, offer := preloadOffer(t, needed-1)
	digest, seq := diskDigest(t, short), short.db.Seq()
	var before protocol.ResourceCounters
	_, before, _ = short.CapacityUsage()
	progress, err := short.PreloadCausal(offer)
	if !errors.Is(err, ErrDataCapacity) {
		t.Fatalf("a receiver one byte short: %v (progress %v), want ErrDataCapacity", err, progress)
	}
	if short.db.Seq() != seq || diskDigest(t, short) != digest {
		t.Fatal("the refused preload wrote")
	}
	if _, after, _ := short.CapacityUsage(); after != before {
		t.Fatalf("usage %+v after the refusal, %+v before", after, before)
	}
	assertStagingIndex(t, short)
	if err := short.view(func(tx *Tx) error {
		for _, snap := range offer.Snapshots {
			if _, _, err := stagedZone(tx, offer.Cession.ID.String(), snap.Zone); !errors.Is(err, ErrNotReady) {
				t.Errorf("zone %s is staged after the refusal (%v)", snap.Zone.Location, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The refusal is not sticky: the same offer passes once there is room.
	if err := short.Close(); err != nil {
		t.Fatal(err)
	}
	roomy, err := Open(short.path, "B", "test", needed)
	if err != nil {
		t.Fatal(err)
	}
	defer roomy.Close()
	if _, err := roomy.PreloadCausal(offer); err != nil {
		t.Fatalf("the offer refused for capacity is refused with room: %v", err)
	}
	if got := physicalLoad(t, roomy); got != needed {
		t.Fatalf("usage %d after the preload, want %d", got, needed)
	}
}

// A row the preload does not change is the same before and after it: the
// reservation accounting reads it once. 2026-11-26: each request's ledger
// row was read twice, once for each side of the difference.
func TestPreloadReadsAnUnchangedRowOnce(t *testing.T) {
	saved := verifyUsage
	verifyUsage = false // its differential scan would be the reads counted
	defer func() { verifyUsage = saved }()
	target, offer := preloadOffer(t, 1<<24)
	const requests = 3 // the writes of preloadOffer
	before := pointReads.Load()
	if _, err := target.PreloadCausal(offer); err != nil {
		t.Fatal(err)
	}
	reads := pointReads.Load() - before
	t.Logf("%d requests, %d point reads", requests, reads)
	if limit := int64(32); reads > limit { // 35 with the second read of each ledger row
		t.Fatalf("%d point reads for %d requests, want at most %d", reads, requests, limit)
	}
}
