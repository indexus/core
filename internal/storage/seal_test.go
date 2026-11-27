package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/indexus/go-indexus-core/internal/tree"
	"reflect"
	"testing"
)

func TestSnapshotSealPreservesCanonicalBytesAndInput(t *testing.T) {
	zone, err := tree.NewZoneKey("Demo000000000000", "aa")
	if err != nil {
		t.Fatal(err)
	}
	a := Record{Collection: "Demo000000000000", Location: "aa", ID: "a", Metrics: []float64{1, 2}, Payload: json.RawMessage(`{"kind":"roof"}`)}
	b := Record{Collection: "Demo000000000000", Location: "aa", ID: "b", Deleted: true}
	ra := requestReceipt(Mutation{RequestID: "request-a", Seq: 1, Record: a}, digest(a))
	rb := requestReceipt(Mutation{RequestID: "request-b", Seq: 2, Record: b}, digest(b))
	snap := Snapshot{Zone: zone, Seq: 2, Records: []Record{b, a}, Requests: []RequestReceipt{rb, ra}}
	input, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	// Independent, explicitly ordered encoding is the persisted seal contract.
	raw, err := json.Marshal(struct {
		Records  []Record
		Requests []RequestReceipt
	}{[]Record{a, b}, []RequestReceipt{ra, rb}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := snap.Seal()
	if err != nil {
		t.Fatal(err)
	}
	want := ZoneSeal{Zone: zone, Seq: 2, Digest: sha256.Sum256(raw), Items: 1, Bytes: int64(len(raw))}
	if got != want {
		t.Fatalf("changed canonical seal: got=%+v want=%+v", got, want)
	}
	after, err := json.Marshal(snap)
	if err != nil || !bytes.Equal(input, after) {
		t.Fatalf("Seal reordered caller data: %v", err)
	}
	snap.Records = []Record{a, b, a}
	if _, err = snap.Seal(); !errors.Is(err, ErrConflict) {
		t.Fatalf("nonadjacent duplicate escaped sorting: %v", err)
	}
	snap.Records = nil
	snap.Requests = nil
	got, err = snap.Seal()
	empty := []byte(`{"Records":null,"Requests":null}`)
	if err != nil || got.Digest != sha256.Sum256(empty) || got.Bytes != int64(len(empty)) {
		t.Fatalf("changed empty seal: %+v %v", got, err)
	}
}

func TestDecodeZoneKeyPreservesPhysicalKeyContract(t *testing.T) {
	for _, raw := range [][]byte{[]byte("Demo000000000000\x00aa"), []byte("Demo000000000000\x00@"), []byte("Demo000000000000\x00")} {
		before := bytes.Clone(raw)
		collection, location, _ := bytes.Cut(raw, []byte{0})
		want, err := tree.NewZoneKey(string(collection), string(location))
		if err != nil {
			t.Fatal(err)
		}
		got, err := zoneFromBytes(raw)
		if err != nil || !reflect.DeepEqual(got, want) || !bytes.Equal(raw, before) {
			t.Fatalf("zone decode %q: %+v %v", raw, got, err)
		}
	}
	for _, raw := range [][]byte{nil, []byte("missing-separator"), []byte("Demo000000000000\x00a\x00b"), []byte("Demo000000000000\x00%")} {
		if _, err := zoneFromBytes(raw); err == nil {
			t.Fatalf("accepted malformed zone key %q", raw)
		}
	}
}
