package prep

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// ReadPart, reading each part once from a stream, sees the rows Each sees,
// across blocks, and refuses a part changed anywhere.
func TestReadPartStreamsWhatEachReads(t *testing.T) {
	dir := t.TempDir()
	w, err := NewBatch(dir, Options{Collection: "C", Metrics: 2, Payloads: true, SplitDepth: 1, PartBytes: 1 << 30, Inputs: Inputs{Tool: "t"}})
	if err != nil {
		t.Fatal(err)
	}
	var rows []Row
	for i := range 70000 {
		head, err := MakeHead(fmt.Sprintf("A%c%c", "0123456789ABCDEF"[i/4096%16], "0123456789ABCDEF"[i/256%16]))
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, Row{Head: head, ID: []byte(fmt.Sprintf("%05d", i%256*1000+i/256)), Weight: 1, Metrics: []float64{float64(i), 1}, Payload: bytes.Repeat([]byte{'x'}, i%40)})
	}
	sort.Slice(rows, func(i, j int) bool { return compareKey(rows[i].Head, rows[i].ID, rows[j].Head, rows[j].ID) < 0 })
	for _, r := range rows {
		if err := w.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	m, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Parts) != 1 {
		t.Fatalf("fixture: %d parts, want one of two blocks", len(m.Parts))
	}
	var want, got []string
	Each(dir, m, func(r Row) error {
		want = append(want, fmt.Sprint(r.Head.Location(), string(r.ID), r.Metrics, len(r.Payload)))
		return nil
	})
	for _, p := range m.Parts {
		f, _ := os.Open(filepath.Join(dir, p.File))
		err := ReadPart(f, p, m.Metrics, func(r Row) error {
			got = append(got, fmt.Sprint(r.Head.Location(), string(r.ID), r.Metrics, len(r.Payload)))
			return nil
		})
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) || len(got) != 70000 {
		t.Fatalf("streamed %d rows, Each %d", len(got), len(want))
	}
	raw, _ := os.ReadFile(filepath.Join(dir, m.Parts[0].File))
	for _, at := range []int{3, 20, len(raw) / 2, len(raw) - 40, len(raw) - 5} {
		bad := bytes.Clone(raw)
		bad[at] ^= 1
		if err := ReadPart(bytes.NewReader(bad), m.Parts[0], m.Metrics, func(Row) error { return nil }); err == nil {
			t.Errorf("a flipped byte at %d of %d passed", at, len(raw))
		}
	}
	if err := ReadPart(bytes.NewReader(raw[:len(raw)-1]), m.Parts[0], m.Metrics, func(Row) error { return nil }); err == nil {
		t.Error("a truncated part passed")
	}
}
