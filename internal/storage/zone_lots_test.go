package storage

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// rowsBelow counts the rows below prefix.
func rowsBelow(t *testing.T, s *Store, prefix string) (n int) {
	t.Helper()
	if err := s.view(func(tx *Tx) error {
		return tx.each(prefix, "", func(string, []byte) bool { n++; return true })
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// zoneTransferRows counts the ZoneTransfer rows zones have of their own.
func zoneTransferRows(t *testing.T, s *Store, zones []tree.ZoneKey) (n int) {
	t.Helper()
	for _, z := range zones {
		n += rowsBelow(t, s, zoneTransfersRows(z))
	}
	return n
}

// reopen closes a store as a restart does and opens it again.
func reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again := openTestData(t, s.path, s.node)
	t.Cleanup(func() { again.Close() })
	return again
}

// finish seals, installs and collects the region of a lot whose zones are
// all installed and acknowledged, and checks that neither disk keeps a row
// of the transfer.
func (lot carriedLot) finish(t *testing.T) {
	t.Helper()
	if _, err := lot.source.FreezeRegion(lot.id); err != nil {
		t.Fatalf("the region is not ready to freeze: %v", err)
	}
	offer, err := lot.source.SealCausalRegion(lot.id)
	if err != nil {
		t.Fatal(err)
	}
	if err = lot.target.StageCausal(offer); err != nil {
		t.Fatalf("the receiver's lots do not make the region the source sealed: %v", err)
	}
	cession, err := lot.source.EmitCausal(lot.id)
	if err != nil {
		t.Fatal(err)
	}
	if err = lot.target.InstallCausal(cession); err != nil {
		t.Fatal(err)
	}
	if err = lot.source.CompleteCausal(lot.id); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Store{lot.target, lot.source} {
		if err = s.CollectTransferred(lot.id); err != nil {
			t.Fatal(err)
		}
		if n := rowsBelow(t, s, zoneStatesRows(lot.id)) + rowsBelow(t, s, zoneLotsRows(lot.id)) + zoneTransferRows(t, s, lot.zones); n != 0 {
			t.Fatalf("%s keeps %d zone rows of a collected transfer", s.node, n)
		}
		if err = s.view(func(tx *Tx) error {
			if tx.value(zoneStateVerPath(lot.id)) != nil {
				t.Fatalf("%s keeps the states version of a collected transfer", s.node)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A replay after the transfer is forgotten is refused and writes nothing.
	late := Record{Collection: lot.zones[0].Collection, Location: lot.zones[0].Location, ID: "after the transfer"}
	if _, err = lot.target.Write("late", late); err != nil {
		t.Fatal(err)
	}
	before, err := lot.target.Snapshot(lot.zones[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = lot.target.MountZones(lot.id, lot.source.node, nil, nil, lot.snaps, lot.proofs); !errors.Is(err, ErrNotReady) {
		t.Fatalf("replayed mount of a forgotten transfer: %v", err)
	}
	if err = lot.target.MountZones(lot.id, lot.source.node, nil, lot.seals, nil, lot.proofs); !errors.Is(err, ErrNotReady) {
		t.Fatalf("replayed mount by seals of a forgotten transfer: %v", err)
	}
	if err = lot.target.InstallZones(lot.id, lot.proofs); err == nil {
		t.Fatal("replayed install of a forgotten transfer accepted")
	}
	if n := rowsBelow(t, lot.target, zoneStatesRows(lot.id)) + rowsBelow(t, lot.target, zoneLotsRows(lot.id)) + rowsBelow(t, lot.target, stagingRows(lot.id.String())); n != 0 {
		t.Fatalf("a replay after the transfer is forgotten wrote %d rows", n)
	}
	after, err := lot.target.Snapshot(lot.zones[0])
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("the replayed install changed the zone: %+v, was %+v (%v)", after, before, err)
	}
	if _, err = lot.source.Write("back", late); err == nil {
		t.Fatal("the source writes a zone it ceded")
	}
}

// A fence and a mount write one row for their lot: no zone gets a
// ZoneTransfer row, and each proof read back is the one the fence returned,
// on both disks, before and after a restart.
func TestLotRowHoldsTheProofsOfItsZones(t *testing.T) {
	lot := fencedWarmLot(t)
	if err := lot.target.MountZones(lot.id, lot.source.node, nil, lot.seals, nil, lot.proofs); err != nil {
		t.Fatal(err)
	}
	lot.source, lot.target = reopen(t, lot.source), reopen(t, lot.target)
	for _, s := range []*Store{lot.source, lot.target} {
		if n := rowsBelow(t, s, zoneLotsRows(lot.id)); n != 1 {
			t.Fatalf("%s holds %d lot rows for one lot", s.node, n)
		}
		if n := zoneTransferRows(t, s, lot.zones); n != 0 {
			t.Fatalf("%s holds %d ZoneTransfer rows", s.node, n)
		}
		for i, z := range lot.zones {
			proofs, err := s.ZoneTransfers(z)
			if err != nil || len(proofs) != 1 || proofs[0] != lot.proofs[i] {
				t.Fatalf("%s, zone %s: proofs %+v, want the fence's %+v (%v)", s.node, z.Location, proofs, lot.proofs[i], err)
			}
		}
	}
	// A repeated fence returns the same decision from the lot row.
	_, seals, proofs, err := lot.source.FenceZones(lot.id, lot.zones)
	if err != nil {
		t.Fatal(err)
	}
	for i := range lot.zones {
		if seals[i] != lot.seals[i] || proofs[i] != lot.proofs[i] {
			t.Fatalf("zone %d: a repeated fence changed its decision", i)
		}
	}
	if n := rowsBelow(t, lot.source, zoneLotsRows(lot.id)); n != 1 {
		t.Fatalf("a repeated fence left %d lot rows", n)
	}
	if err = lot.source.AckZoneInstalls(lot.id, lot.zones); err != nil {
		t.Fatal(err)
	}
	lot.finish(t)
}

// The acknowledgement of a whole lot is one row whatever its size, and
// every zone of the lot then reads as acknowledged.
func TestAckOfAWholeLotWritesItsRowOnly(t *testing.T) {
	lot := fencedWarmLot(t)
	if err := lot.target.MountZones(lot.id, lot.source.node, nil, lot.seals, nil, lot.proofs); err != nil {
		t.Fatal(err)
	}
	puts := rowPuts.Load()
	if err := lot.source.AckZoneInstalls(lot.id, lot.zones); err != nil {
		t.Fatal(err)
	}
	if n := rowPuts.Load() - puts; n != 2 {
		t.Fatalf("the acknowledgement of a lot of %d zones wrote %d rows, want 2: the lot and the states version", len(lot.zones), n)
	}
	for key, st := range zoneStates(t, lot.source, lot.id) {
		if !st.Acked {
			t.Fatalf("zone %q of an acknowledged lot reads as not acknowledged", key)
		}
	}
	if _, err := lot.source.ReadFencedLot(lot.id, lot.zones); !errors.Is(err, ErrNotReady) {
		t.Fatalf("an acknowledged lot is still offered to a resume: %v", err)
	}
	puts = rowPuts.Load()
	if err := lot.source.AckZoneInstalls(lot.id, lot.zones); err != nil {
		t.Fatal(err)
	}
	if n := rowPuts.Load() - puts; n != 0 {
		t.Fatalf("a repeated acknowledgement wrote %d rows", n)
	}
	lot.finish(t)
}

// A transfer cut inside a lot resumes from the zones not yet transferred:
// the receiver installed one zone of the lot and the source knows it; after
// a restart of both the resume offers the other zone only, the receiver
// installs it without touching the first, which took a write meanwhile, and
// the region closes.
func TestLotResumesFromTheZonesNotInstalled(t *testing.T) {
	lot := fencedWarmLot(t)
	first, rest := lot.zones[:1], lot.zones[1:]
	if err := lot.target.MountZones(lot.id, lot.source.node, nil, lot.seals[:1], nil, lot.proofs[:1]); err != nil {
		t.Fatal(err)
	}
	if err := lot.source.AckZoneInstalls(lot.id, first); err != nil {
		t.Fatal(err)
	}
	early := Record{Collection: first[0].Collection, Location: first[0].Location, ID: "written at the receiver"}
	if _, err := lot.target.Write("early", early); err != nil {
		t.Fatal(err)
	}
	if _, err := lot.source.FreezeRegion(lot.id); !errors.Is(err, ErrNotReady) {
		t.Fatalf("the region froze with a zone of a lot not installed: %v", err)
	}
	lot.source, lot.target = reopen(t, lot.source), reopen(t, lot.target)
	states := zoneStates(t, lot.source, lot.id)
	if !states[sealKey(first[0])].Acked || states[sealKey(rest[0])].Acked {
		t.Fatalf("after a restart the source reads the lot's acknowledgements as %+v", states)
	}
	if _, err := lot.source.ReadFencedLot(lot.id, lot.zones); !errors.Is(err, ErrNotReady) {
		t.Fatalf("the resume offers a zone already acknowledged: %v", err)
	}
	replay, err := lot.source.ReadFencedLot(lot.id, rest)
	if err != nil {
		t.Fatal(err)
	}
	proofs, err := lot.source.ZoneTransfers(rest[0])
	if err != nil || len(proofs) != 1 || proofs[0] != lot.proofs[1] {
		t.Fatalf("the resume's proof %+v is not the fence's (%v)", proofs, err)
	}
	// The resume cuts its message as it likes: here the whole lot again.
	if err = lot.target.MountZones(lot.id, lot.source.node, nil, nil, lot.snaps, lot.proofs); err != nil {
		t.Fatal(err)
	}
	if n := rowsBelow(t, lot.target, zoneLotsRows(lot.id)); n != 2 {
		t.Fatalf("the receiver holds %d lot rows for two installs", n)
	}
	live, err := lot.target.Snapshot(first[0])
	if err != nil || len(live.Records) != len(lot.snaps[0].Records)+1 {
		t.Fatalf("the resumed install replaced a zone already installed: %d records (%v)", len(live.Records), err)
	}
	if seal, err := replay.Snapshots[0].Seal(); err != nil || seal != lot.seals[1] || replay.Seals[0] != seal || replay.Proofs[0] != lot.proofs[1] {
		t.Fatalf("the fenced content of the resumed zone changed (%v)", err)
	}
	if err = lot.source.AckZoneInstalls(lot.id, rest); err != nil {
		t.Fatal(err)
	}
	lot.finish(t)
}

// asWrittenBefore rewrites the transfer rows of s as a build before lot rows
// wrote them: a ZoneTransfer row per zone, the acknowledgement on each state,
// no lot row.
func asWrittenBefore(t *testing.T, s *Store, id causal.TransferID) {
	t.Helper()
	if err := s.updateCausal(func(tx *Tx, state CausalState) error {
		var states []ZoneState
		if err := eachZoneState(tx, state, id, func(st ZoneState) bool { states = append(states, st); return true }); err != nil {
			return err
		}
		for _, st := range states {
			if st.Lot == 0 {
				continue
			}
			proof, ok, err := zoneTransferOf(tx, state, id, st.Zone)
			if err != nil || !ok {
				return errors.Join(err, ErrConflict)
			}
			raw, err := json.Marshal(proof)
			if err != nil {
				return err
			}
			if err = tx.put(zoneTransferPath(st.Zone, id), raw); err != nil {
				return err
			}
			if st.Acked, err = zoneAcked(tx, id, st); err != nil {
				return err
			}
			st.Lot = 0
			if err = putZoneState(tx, id, st); err != nil {
				return err
			}
		}
		return tx.drop(zoneLotsRows(id))
	}); err != nil {
		t.Fatal(err)
	}
}

// A store whose transfer was fenced by a build before lot rows opens and
// finishes it: the rows per zone are read as they were, a zone fenced now
// goes to a lot row beside them, and the collection drops both.
func TestTransferWithRowsPerZoneFinishes(t *testing.T) {
	lot := fencedWarmLot(t)
	first, rest := lot.zones[:1], lot.zones[1:]
	// The first zone was fenced, installed and left unacknowledged by the
	// old build; the second was only fenced.
	if err := lot.target.MountZones(lot.id, lot.source.node, nil, lot.seals[:1], nil, lot.proofs[:1]); err != nil {
		t.Fatal(err)
	}
	asWrittenBefore(t, lot.source, lot.id)
	asWrittenBefore(t, lot.target, lot.id)
	lot.source, lot.target = reopen(t, lot.source), reopen(t, lot.target)
	for _, s := range []*Store{lot.source, lot.target} {
		if n := rowsBelow(t, s, zoneLotsRows(lot.id)); n != 0 {
			t.Fatalf("%s: %d lot rows on a store written before them", s.node, n)
		}
	}
	if n := zoneTransferRows(t, lot.source, lot.zones); n != 2 {
		t.Fatalf("the source holds %d ZoneTransfer rows, want 2", n)
	}
	if _, err := lot.source.Write("fenced", Record{Collection: rest[0].Collection, Location: rest[0].Location, ID: "x"}); err == nil {
		t.Fatal("the source writes a zone an old row fences")
	}
	if _, err := lot.target.Write("held", Record{Collection: first[0].Collection, Location: first[0].Location, ID: "written at the receiver"}); err != nil {
		t.Fatalf("the receiver does not write a zone an old row installed: %v", err)
	}
	// The resume replays the old proofs; the receiver installs the zone it
	// lacks, in a lot row, and keeps the other.
	replay, err := lot.source.ReadFencedLot(lot.id, lot.zones)
	if err != nil {
		t.Fatal(err)
	}
	var proofs []causal.ZoneTransfer
	for i, z := range lot.zones {
		rows, err := lot.source.ZoneTransfers(z)
		if err != nil || len(rows) != 1 || rows[0] != lot.proofs[i] {
			t.Fatalf("zone %s: old proof read as %+v (%v)", z.Location, rows, err)
		}
		proofs = append(proofs, rows[0])
	}
	if err = lot.target.MountZones(lot.id, lot.source.node, nil, nil, replay.Snapshots, proofs); err != nil {
		t.Fatal(err)
	}
	if n := rowsBelow(t, lot.target, zoneLotsRows(lot.id)); n != 1 {
		t.Fatalf("the receiver holds %d lot rows after installing one zone", n)
	}
	live, err := lot.target.Snapshot(first[0])
	if err != nil || len(live.Records) != len(lot.snaps[0].Records)+1 {
		t.Fatalf("the resumed install replaced the zone the old build installed: %d records (%v)", len(live.Records), err)
	}
	if err = lot.source.AckZoneInstalls(lot.id, lot.zones); err != nil {
		t.Fatal(err)
	}
	if n := rowsBelow(t, lot.source, zoneLotsRows(lot.id)); n != 0 {
		t.Fatalf("acknowledging zones with rows of their own wrote %d lot rows", n)
	}
	lot.finish(t)
}

// A lot acknowledged in two parts, as a resume that cut it differently does,
// is acknowledged zone by zone and closes like any other.
func TestLotAcknowledgedInPartsCloses(t *testing.T) {
	lot := fencedWarmLot(t)
	if err := lot.target.MountZones(lot.id, lot.source.node, nil, lot.seals, nil, lot.proofs); err != nil {
		t.Fatal(err)
	}
	for i := range lot.zones {
		if err := lot.source.AckZoneInstalls(lot.id, lot.zones[i:i+1]); err != nil {
			t.Fatal(err)
		}
		if _, err := lot.source.FreezeRegion(lot.id); (err == nil) != (i == len(lot.zones)-1) {
			t.Fatalf("after %d of %d acknowledgements the region freeze answered %v", i+1, len(lot.zones), err)
		}
	}
	lot.finish(t)
}
