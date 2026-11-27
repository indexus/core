package storage

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// A write converts its record to the zonelog form once: to size it against
// capacity. The commit writes that same form. A row removed or replaced
// later in the update does not keep the form of the row before it.
func TestWriteEncodesItsRecordForTheLogOnce(t *testing.T) {
	store := openTestData(t, filepath.Join(t.TempDir(), "encode.db"), "D")
	defer store.Close()
	initTestCausal(t, store)
	r := Record{Collection: "encodeonce000000", Location: "Z", ID: "r", Metrics: []float64{1, 2}, Payload: json.RawMessage(`{"a": [1, 2]}`)}
	before := logEncodes.Load()
	if _, err := store.Write("w1", r); err != nil {
		t.Fatal(err)
	}
	if n := logEncodes.Load() - before; n != 1 {
		t.Fatalf("one write converted its record to the log form %d times", n)
	}
	z, _ := tree.NewZoneKey(r.Collection, r.Location)
	snap, err := store.Snapshot(z)
	if err != nil || len(snap.Records) != 1 || string(snap.Records[0].Payload) != `{"a":[1,2]}` || len(snap.Records[0].Metrics) != 2 {
		t.Fatalf("stored record %+v, %v", snap.Records, err)
	}
	// Two puts of one row, then its removal: the commit writes the last state.
	if err := store.update(func(tx *Tx) error {
		first, second := r, r
		first.Payload, second.Payload = json.RawMessage(`"first"`), json.RawMessage(`"second"`)
		other := r
		other.ID = "gone"
		for _, put := range []Record{first, second, other} {
			if _, _, err := tx.putRecord(z, put); err != nil {
				return err
			}
		}
		tx.setRecord(z, other.ID, nil)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snap, err = store.Snapshot(z)
	if err != nil || len(snap.Records) != 1 || string(snap.Records[0].Payload) != `"second"` {
		t.Fatalf("after two puts and a removal: %+v, %v", snap.Records, err)
	}
}
