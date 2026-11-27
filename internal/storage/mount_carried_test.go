package storage

import (
	"errors"
	"reflect"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// carriedLot is a transfer whose receiver holds a warm copy of two zones,
// one with two records, fenced at the source: what a mount by seals needs.
type carriedLot struct {
	source, target *Store
	id             causal.TransferID
	zones          []tree.ZoneKey
	snaps          []Snapshot
	seals          []ZoneSeal
	proofs         []causal.ZoneTransfer
}

func fencedWarmLot(t *testing.T) carriedLot {
	t.Helper()
	source, target, genesis := ticketStores(t)
	lot := carriedLot{source: source, target: target}
	for i, r := range []Record{
		{Collection: "Demo000000000000", Location: "aa", ID: "one", Payload: []byte(`"first"`)},
		{Collection: "Demo000000000000", Location: "aa", ID: "two", Metrics: []float64{2}},
		{Collection: "Demo000000000000", Location: "ab", ID: "one", Payload: []byte(`"other zone"`)},
	} {
		if _, err := source.Write("w"+string(rune('0'+i)), r); err != nil {
			t.Fatal(err)
		}
		if z, _ := recordZone(r); len(lot.zones) == 0 || lot.zones[len(lot.zones)-1] != z {
			lot.zones = append(lot.zones, z)
		}
	}
	op := ticketAdmission(t, source, target, genesis.Right().ID, tree.Region{})
	lot.id = op.Cession.ID
	offer := op
	for _, z := range lot.zones {
		snap, err := source.Snapshot(z)
		if err != nil {
			t.Fatal(err)
		}
		offer.Snapshots = append(offer.Snapshots, snap)
	}
	if _, err := target.PreloadCausal(offer); err != nil {
		t.Fatal(err)
	}
	var err error
	if lot.snaps, lot.seals, lot.proofs, err = source.FenceZones(lot.id, lot.zones); err != nil {
		t.Fatal(err)
	}
	return lot
}

// installed checks that the receiver holds exactly the fenced content of
// each zone, receipts included, and writes it.
func (lot carriedLot) installed(t *testing.T) {
	t.Helper()
	for i, z := range lot.zones {
		live, err := lot.target.Snapshot(z)
		if err != nil {
			t.Fatal(err)
		}
		if seal, err := live.Seal(); err != nil || seal != lot.seals[i] {
			t.Fatalf("zone %s: the installed content has seal %+v, the fence %+v (%v)", z.Location, seal, lot.seals[i], err)
		}
		if !reflect.DeepEqual(live.Records, lot.snaps[i].Records) {
			t.Fatalf("zone %s: installed %+v, fenced %+v", z.Location, live.Records, lot.snaps[i].Records)
		}
		if _, err = lot.target.Write("new-"+z.Location, Record{Collection: z.Collection, Location: z.Location, ID: "new"}); err != nil {
			t.Fatalf("zone %s is not writable after its install: %v", z.Location, err)
		}
	}
	replay, err := lot.target.Write("w0", Record{Collection: "Demo000000000000", Location: "aa", ID: "one", Payload: []byte(`"first"`)})
	if err != nil || replay.Seq != 1 {
		t.Fatalf("the receipt of an acknowledged write did not follow its zone: %+v %v", replay, err)
	}
}

// A mount whose preparation certified the staged copies installs what the
// certification decoded: under the writer it reads no staged record or
// receipt row again.
func TestMountInstallsThePreparedCopyWithoutReadingStaging(t *testing.T) {
	lot := fencedWarmLot(t)
	reads, decodes := stagedReadBacks.Load(), recordDecodes.Load()
	if err := lot.target.MountZones(lot.id, lot.source.node, nil, lot.seals, nil, lot.proofs); err != nil {
		t.Fatal(err)
	}
	if n := stagedReadBacks.Load() - reads; n != 0 {
		t.Fatalf("the install read %d staged rows under the writer, want 0", n)
	}
	if n := recordDecodes.Load() - decodes; n != 3 {
		t.Fatalf("the mount decoded %d staged records, want 3: each once, for its certification", n)
	}
	lot.installed(t)
}

// The staged copy stays until the collection, as before: a restart between
// the mount and the collection finds it, and the collection drops it.
func TestMountKeepsTheStagedCopyUntilTheCollection(t *testing.T) {
	lot := fencedWarmLot(t)
	if err := lot.target.MountZones(lot.id, lot.source.node, nil, lot.seals, nil, lot.proofs); err != nil {
		t.Fatal(err)
	}
	if err := lot.target.view(func(tx *Tx) error {
		for i, z := range lot.zones {
			snap, _, err := stagedSnapshot(tx, stagedTable(lot.id.String(), z), z)
			if err != nil {
				return err
			}
			if seal, err := snap.Seal(); err != nil || seal != lot.seals[i] {
				t.Fatalf("zone %s: staged copy after the mount has seal %+v (%v)", z.Location, seal, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertStagingIndex(t, lot.target)
}

// A preparation made before staging changed is refused whole, as before,
// and installs nothing: the next one reads staging again and installs the
// fenced content.
func TestMountRefusesAPreparedCopyOfChangedStaging(t *testing.T) {
	lot := fencedWarmLot(t)
	stale, err := lot.target.prepareMount(lot.id, lot.source.node, nil, lot.seals, nil, lot.proofs)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale.certified) != len(lot.zones) {
		t.Fatalf("the preparation carries %d copies, want %d", len(stale.certified), len(lot.zones))
	}
	// Between the preparation and its commit, the first zone's copy is
	// replaced by other bytes at the same sequence.
	other := lot.snaps[0]
	other.Records = append([]Record(nil), other.Records...)
	other.Records[0].Payload = []byte(`"not the fenced bytes"`)
	if err = lot.target.update(func(tx *Tx) error {
		return restageSnapshot(tx, lot.id.String(), lot.source.node, other)
	}); err != nil {
		t.Fatal(err)
	}
	err = lot.target.updateCausal(func(tx *Tx, state CausalState) error {
		return lot.target.mountPreparedTx(tx, state, lot.id, stale, lot.proofs)
	})
	if !errors.Is(err, errMountChanged) {
		t.Fatalf("a preparation of changed staging was accepted: %v", err)
	}
	for _, z := range lot.zones {
		if live, err := lot.target.Snapshot(z); err != nil || len(live.Records) != 0 {
			t.Fatalf("zone %s: the refused mount installed %d records (%v)", z.Location, len(live.Records), err)
		}
	}
	// The staged bytes no longer match the seal; the fenced snapshots repair
	// the copy, and the mount installs them.
	if err = lot.target.MountZones(lot.id, lot.source.node, nil, lot.seals, nil, lot.proofs); !errors.Is(err, ErrSequence) {
		t.Fatalf("replaced bytes mounted under the fence's seal: %v", err)
	}
	if err = lot.target.MountZones(lot.id, lot.source.node, nil, nil, lot.snaps, lot.proofs); err != nil {
		t.Fatal(err)
	}
	lot.installed(t)
}

// A zone certified by an earlier request has no prepared copy in the mount
// that installs it: the install reads its staged rows, as it did.
func TestMountWithoutAPreparedCopyReadsStaging(t *testing.T) {
	lot := fencedWarmLot(t)
	if err := lot.target.CertifyZones(lot.id, lot.source.node, lot.seals); err != nil {
		t.Fatal(err)
	}
	reads := stagedReadBacks.Load()
	if err := lot.target.MountZones(lot.id, lot.source.node, nil, nil, nil, lot.proofs); err != nil {
		t.Fatal(err)
	}
	if n := stagedReadBacks.Load() - reads; n != 6 {
		t.Fatalf("the install read %d staged rows, want 6: 3 records and 3 receipts", n)
	}
	lot.installed(t)
}

// A prepared copy is installed only for the seal it was certified against:
// with another one the install reads the staged rows.
func TestInstallIgnoresACopyCertifiedForAnotherSeal(t *testing.T) {
	lot := fencedWarmLot(t)
	prepared, err := lot.target.prepareMount(lot.id, lot.source.node, nil, lot.seals, nil, lot.proofs)
	if err != nil {
		t.Fatal(err)
	}
	z := lot.zones[0]
	wrong := prepared.certified[z]
	wrong.seal.Digest[0] ^= 1
	wrong.records = []Record{{Collection: z.Collection, Location: z.Location, ID: "never fenced"}}
	wrong.receipts = nil
	prepared.certified[z] = wrong
	reads := stagedReadBacks.Load()
	if err = lot.target.updateCausal(func(tx *Tx, state CausalState) error {
		return lot.target.mountPreparedTx(tx, state, lot.id, prepared, lot.proofs)
	}); err != nil {
		t.Fatal(err)
	}
	if n := stagedReadBacks.Load() - reads; n != 4 {
		t.Fatalf("the install read %d staged rows, want 4: the 2 records and 2 receipts of the zone without a valid copy", n)
	}
	lot.installed(t)
}
