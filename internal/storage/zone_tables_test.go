package storage

import (
	"bytes"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFlatZonesPreserveRecordsReceiptsCountersAndRestart(t *testing.T) {
	s, _ := routingStore(t)
	rows := []Record{{Collection: "Demo000000000000", Location: "abc", ID: "sequence", Metrics: []float64{2, 3}}, {Collection: "Demo000000000000", Location: "abc", ID: "\x01odd", Metrics: []float64{4}}, {Collection: "Demo000000000000", Location: "abd", ID: "records", Metrics: []float64{6}}}
	for i, r := range rows {
		if _, _, _, e := s.ApplyRecord(fmt.Sprint(i), r); e != nil {
			t.Fatal(e)
		}
	}
	if _, _, applied, e := s.ApplyRecord("0", rows[0]); e != nil || applied {
		t.Fatalf("retry %v %v", applied, e)
	}
	assertPlanningParity(t, s)
	assertStoredUsage(t, s)
	snapshots, e := s.Inventory(true)
	if e != nil {
		t.Fatal(e)
	}
	if len(snapshots) != 2 || len(snapshots[0].Records) != 2 || len(snapshots[0].Requests) != 2 {
		t.Fatal(snapshots)
	}
	if e = s.view(func(tx *Tx) error {
		z := zoneTable(key(liveBucket, []byte("Demo000000000000\x00abc")))
		if tx.any(z.row([]byte("records"))) || tx.any(z.row(operationsBucket)) {
			t.Fatal("nested bucket remains")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	path := s.path
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	reopened, e := OpenWithOptions(path, "D", "test", 1<<26, Options{NoSync: true})
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	got, e := reopened.Inventory(true)
	if e != nil || !reflect.DeepEqual(got, snapshots) {
		t.Fatalf("restart mismatch %v", e)
	}
	if _, _, _, e = reopened.ApplyRecord("after-restart", Record{Collection: "Demo000000000000", Location: "abe", ID: "new", Metrics: []float64{8}}); e != nil {
		t.Fatal(e)
	}
	assertStoredUsage(t, reopened)
	assertPlanningParity(t, reopened)
}
func TestFlatZoneOptionPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "flat.db")
	s, e := OpenWithOptions(p, "D", "test", 1<<26, Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.view(func(tx *Tx) error {
		if !bytes.Equal(tx.value(meta(storageFormatKey)), storageFormat) {
			t.Fatal("option not durable")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
