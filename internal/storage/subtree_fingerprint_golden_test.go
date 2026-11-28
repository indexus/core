package storage

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

func TestSubtreeFingerprintProtocolGolden(t *testing.T) {
	var oracle struct {
		Rows []struct {
			Collection, Location, ID, StoredPayloadHex, SHA256 string
			Weight                                             float64
			Metrics                                            []float64
			Binary                                             bool
		}
		Monoid struct{ Algorithm, Sum, Count string }
	}
	b, err := os.ReadFile("../../spec/subtree-fingerprint/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &oracle); err != nil {
		t.Fatal(err)
	}
	var total SubtreeFingerprint
	for _, r := range oracle.Rows {
		payload, err := hex.DecodeString(r.StoredPayloadHex)
		if err != nil {
			t.Fatal(err)
		}
		row := Record{Collection: r.Collection, Location: r.Location, ID: r.ID, Weight: r.Weight, Metrics: r.Metrics, Binary: r.Binary, Payload: payload}
		var single SubtreeFingerprint
		single.addRow(row)
		if got := hex.EncodeToString(single.Sum[:]); got != r.SHA256 {
			t.Fatalf("%q: digest %s, want %s", r.ID, got, r.SHA256)
		}
		total.addRow(row)
	}
	if oracle.Monoid.Algorithm != "sha256-add256-v1" || hex.EncodeToString(total.Sum[:]) != oracle.Monoid.Sum ||
		strconv.FormatUint(total.Count, 10) != oracle.Monoid.Count {
		t.Fatalf("composed golden mismatch: %+v", total)
	}
}
