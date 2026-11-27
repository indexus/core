package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/internal/prep"
	"github.com/indexus/go-indexus-core/internal/tree"
)

type readRow struct {
	Location, ID string
	Metrics      []float64
}

func readBatch(t *testing.T, dir string) []readRow {
	t.Helper()
	m, err := prep.ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []readRow
	if err := prep.Each(dir, m, func(r prep.Row) error {
		out = append(out, readRow{r.Head.Location(), string(r.ID), append([]float64(nil), r.Metrics...)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// Staging keeps the last version of a record, one batch per collection;
// splitting gives each node exactly the zones its name is closest to.
func TestStageAndSplitByOwner(t *testing.T) {
	var lines []string
	for i := range 400 {
		lines = append(lines, fmt.Sprintf(`{"Collection":"0000000000000001","Location":"%s","ID":"r","Metrics":[%d,1]}`, tree.BASE64.Encode([]byte{byte(i), byte(i * 7)})[:3], i))
	}
	lines = append(lines, `{"Collection":"0000000000000001","Location":"000","ID":"r","Metrics":[-1,1]}`)
	lines = append(lines, `{"Collection":"0000000000000002","Location":"A","ID":"x","Metrics":[],"Payload":{"a":1}}`)
	dir := t.TempDir()
	batches, err := stageRecords(strings.NewReader(strings.Join(lines, "\n")), dir)
	if err != nil || len(batches) != 2 {
		t.Fatalf("%v %v", batches, err)
	}
	rows := readBatch(t, batches[0])
	for _, r := range rows {
		if r.Location == "000" && r.Metrics[0] != -1 {
			t.Fatal("staging kept an older version")
		}
	}
	names := []string{tree.EncodeName(tree.ID{0x10}), tree.EncodeName(tree.ID{0x90}), tree.EncodeName(tree.ID{0xd0})}
	endpoints := []string{"a", "b", "c"}
	pieces, err := splitByOwner(batches[0], endpoints, names)
	if err != nil || len(pieces) < 2 {
		t.Fatalf("pieces %v %v", pieces, err)
	}
	total := 0
	for i, endpoint := range endpoints {
		piece, ok := pieces[endpoint]
		if !ok {
			continue
		}
		mine, _ := tree.DecodeName(names[i])
		for _, r := range readBatch(t, piece) {
			zone, _ := tree.ZoneID("0000000000000001", r.Location)
			for j, name := range names {
				other, _ := tree.DecodeName(name)
				if j != i && tree.XORCompare(zone, other, mine) < 0 {
					t.Fatalf("%s went to %s, %s is closer", r.Location, names[i], name)
				}
			}
			total++
		}
		os.RemoveAll(piece)
	}
	if total != len(rows) {
		t.Fatalf("split %d of %d rows", total, len(rows))
	}
	if _, err := os.Stat(filepath.Join(batches[1], "manifest.json")); err != nil {
		t.Fatal(err)
	}
}

// Binary scene payloads must keep their encoding when a batch is sharded.
func TestSplitByOwnerPreservesBinaryPayloadEncoding(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "binary")
	b, err := prep.NewBatch(dir, prep.Options{Collection: "BuildLidHD2025f2", Metrics: 1, Payloads: true, PayloadEncoding: "binary", SplitDepth: 2, ZoneDepths: [2]int{0, -1}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := prep.MakeHead("ot7b24dJjSjHRAy5")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{'I', 'X', 'B', '3', 0, 255}
	if err = b.Add(prep.Row{Head: h, ID: []byte("roof"), Weight: 1, Metrics: []float64{1}, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Close(); err != nil {
		t.Fatal(err)
	}
	pieces, err := splitByOwner(dir, []string{"a", "b"}, []string{"0000000000000001", "W000000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	for _, piece := range pieces {
		m, err := prep.ReadManifest(piece)
		if err != nil {
			t.Fatal(err)
		}
		if m.PayloadEncoding != "binary" {
			t.Fatalf("binary encoding lost: %q", m.PayloadEncoding)
		}
		if err = prep.Each(piece, m, func(row prep.Row) error {
			if string(row.Payload) != string(payload) {
				t.Fatal("binary payload changed")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
