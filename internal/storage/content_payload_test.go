package storage

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestPayloadPersistsAndChangesSnapshotSeal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.db")
	s := openTestData(t, path, "D")
	defer func() { s.Close() }()
	initTestCausal(t, s)
	r := Record{Collection: "Demo000000000000", Location: "aa", ID: "pixels", Metrics: []float64{1}, Payload: json.RawMessage(`{"tree":"AA=="}`)}
	if _, err := s.Write("put-content", r); err != nil {
		t.Fatal(err)
	}
	z, err := recordZone(r)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Snapshot(z)
	if err != nil {
		t.Fatal(err)
	}
	seal, err := before.Seal()
	if err != nil {
		t.Fatal(err)
	}
	changed := before
	changed.Records = append([]Record(nil), before.Records...)
	changed.Records[0].Payload = json.RawMessage(`{"tree":"AQ=="}`)
	other, err := changed.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if seal == other {
		t.Fatal("payload omitted from seal")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestData(t, path, "D")
	after, err := s.Snapshot(z)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Records) != 1 || !bytes.Equal(after.Records[0].Payload, r.Payload) {
		t.Fatalf("payload lost: %+v", after)
	}
	empty, err := json.Marshal(Record{Collection: "Demo000000000000", Location: "aa", ID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(empty, []byte("Payload")) {
		t.Fatal("empty payload should be omitted")
	}
}
