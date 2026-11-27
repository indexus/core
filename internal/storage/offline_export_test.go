package storage

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOfflineExportPreservesContentAndTombstones(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	s := openTestData(t, path, "D")
	initTestCausal(t, s)
	want := []Record{{Collection: "Demo000000000000", Location: "a", ID: "one", Metrics: []float64{1, 2}, Payload: json.RawMessage(`{"v":"content"}`)}, {Collection: "Demo000000000000", Location: "b", ID: "two", Deleted: true}}
	if err := s.SeedLocalRecords(want); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var got []Record
	err := ExportOfflineRecords(path, func(r Record) error {
		got = append(got, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	stop := errors.New("stop")
	if err := ExportOfflineRecords(path, func(Record) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("visitor error lost: %v", err)
	}
}
