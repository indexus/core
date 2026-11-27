package prep

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func randomLocation(rng *rand.Rand, depth int) string {
	b := make([]byte, depth)
	for i := range b {
		b[i] = tree.BASE64.CharAt(rng.Intn(8))[0] // few characters: zones share prefixes
	}
	return string(b)
}

func randomRows(rng *rand.Rand, n, m int) []Row {
	seen := map[string]bool{}
	var rows []Row
	for len(rows) < n {
		loc := randomLocation(rng, 3+rng.Intn(4))
		id := fmt.Sprintf("r%d", rng.Intn(3))
		if seen[loc+"/"+id] {
			continue
		}
		seen[loc+"/"+id] = true
		h, err := MakeHead(loc)
		if err != nil {
			panic(err)
		}
		r := Row{Head: h, ID: []byte(id), Weight: 1, Metrics: make([]float64, m)}
		for j := range r.Metrics {
			r.Metrics[j] = rng.NormFloat64() * 1000
		}
		if rng.Intn(3) > 0 {
			r.Payload = make([]byte, rng.Intn(40))
			rng.Read(r.Payload)
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return compareKey(rows[i].Head, rows[i].ID, rows[j].Head, rows[j].ID) < 0 })
	return rows
}

func TestHeadMatchesTheTree(t *testing.T) {
	for _, loc := range []string{"0", "_", "K3gUW", "K3gUW-_09azAZ", "0000000000000000", "________________"} {
		h, err := MakeHead(loc)
		if err != nil {
			t.Fatal(err)
		}
		if h.Location() != loc || h.Depth() != len(loc) {
			t.Fatalf("%s: back as %s", loc, h.Location())
		}
		want, _ := tree.BASE64.Decode(loc)
		if !bytes.Equal(h[:len(want)], want) {
			t.Fatalf("%s: bits %x, want %x", loc, h[:len(want)], want)
		}
		for d := 0; d <= len(loc); d++ {
			p, _ := MakeHead(loc[:d])
			if h.Prefix(d) != p {
				t.Fatalf("%s: prefix %d is %s", loc, d, h.Prefix(d).Location())
			}
		}
	}
	// The head's bits are the high bits of the zone's routing id (sdk
	// zoneKeyID, Go tree.ZoneKeyID): sorted heads come in owner runs.
	for _, loc := range []string{"K3gUW", "2ngVe", "q3wpJp", "a3yC3w-_"} {
		h, _ := MakeHead(loc)
		id, err := tree.ZoneKeyID("AngVegetation001", loc)
		if err != nil {
			t.Fatal(err)
		}
		for bit := 0; bit < 6*len(loc); bit++ {
			if (h[bit/8]^id[bit/8])&(0x80>>(bit%8)) != 0 {
				t.Fatalf("%s: bit %d of the head differs from ZoneKeyID", loc, bit)
			}
		}
	}
	if _, err := MakeHead("0000000000000000A"); err == nil {
		t.Fatal("17 characters accepted")
	}
}

func TestBatchRoundTripAndZones(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	rows := randomRows(rng, 5000, 3)
	dir := t.TempDir()
	b, err := NewBatch(dir, Options{Collection: "TestCollection01", Metrics: 3, Payloads: true, SplitDepth: 2, PartBytes: 20000,
		ZoneDepths: [2]int{0, 4}, Inputs: Inputs{Tool: "test", Sources: map[string]string{"x": "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := b.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	m, err := b.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Parts) < 3 || m.Records != 5000 {
		t.Fatalf("%d parts, %d records", len(m.Parts), m.Records)
	}
	// No part straddles a zone of the split depth.
	for i := 1; i < len(m.Parts); i++ {
		if m.Parts[i-1].Last[:2] == m.Parts[i].First[:2] {
			t.Fatalf("parts %d and %d share zone %s", i-1, i, m.Parts[i].First[:2])
		}
	}
	got, worst, err := Verify(dir, 1e-12)
	if err != nil {
		t.Fatal(err)
	}
	if worst != 0 || got.ID != BatchID(Inputs{Tool: "test", Sources: map[string]string{"x": "1"}}) {
		t.Fatalf("worst %v, id %s", worst, got.ID)
	}
	i := 0
	if err := Each(dir, got, func(r Row) error {
		w := rows[i]
		i++
		if r.Head != w.Head || !bytes.Equal(r.ID, w.ID) || r.Weight != w.Weight || !bytes.Equal(r.Payload, w.Payload) || fmt.Sprint(r.Metrics) != fmt.Sprint(w.Metrics) {
			return fmt.Errorf("row %d: %v, want %v", i, r, w)
		}
		return nil
	}); err != nil || i != len(rows) {
		t.Fatal(err, i)
	}
	// The same rows give the same bytes.
	dir2 := t.TempDir()
	b2, _ := NewBatch(dir2, Options{Collection: "TestCollection01", Metrics: 3, Payloads: true, SplitDepth: 2, PartBytes: 20000,
		ZoneDepths: [2]int{0, 4}, Inputs: Inputs{Tool: "test", Sources: map[string]string{"x": "1"}}})
	for _, r := range rows {
		b2.Add(r)
	}
	m2, _ := b2.Close()
	for i := range m.Parts {
		if m.Parts[i].SHA256 != m2.Parts[i].SHA256 {
			t.Fatal("same rows, other bytes")
		}
	}
	if m.Zones.SHA256 != m2.Zones.SHA256 {
		t.Fatal("same rows, other zones")
	}
}

func TestBatchIDIsCanonical(t *testing.T) {
	// python3 -c 'import json,hashlib;print(hashlib.sha256(json.dumps({"tool":"t","sources":{"b":"2","a":"1"},"options":{"x":"y"}},sort_keys=True,separators=(",",":")).encode()).hexdigest())'
	got := BatchID(Inputs{Tool: "t", Sources: map[string]string{"b": "2", "a": "1"}, Options: map[string]string{"x": "y"}})
	if got != "dff0dc9632946fb921d9e641a948a31cafdc30307fe3213c51014a3430124fd5" {
		t.Fatal(got)
	}
}

func TestRefusals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.ixp")
	w, _ := Create(path, 1, false)
	h1, _ := MakeHead("AB")
	h2, _ := MakeHead("A")
	if err := w.Add(Row{Head: h1, Metrics: []float64{1}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(Row{Head: h2, Metrics: []float64{1}}); err == nil {
		t.Fatal("a key before the previous one accepted")
	}
	if err := w.Add(Row{Head: h1, Metrics: []float64{1}}); err == nil {
		t.Fatal("a duplicate key accepted")
	}
	if err := w.Add(Row{Head: h1, ID: []byte("x"), Metrics: []float64{1, 2}}); err == nil {
		t.Fatal("a row of two metrics accepted in a part of one")
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// A flipped byte in a block is found.
	raw, _ := os.ReadFile(path)
	raw[headerLen+20] ^= 1
	os.WriteFile(path, raw, 0o644)
	pr, f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pr.Each(func(Row) error { return nil }); err == nil {
		t.Fatal("a corrupt block read")
	}
}

func TestManyBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.ixp")
	w, _ := Create(path, 0, false)
	n := maxRows*2 + 17
	for i := range n {
		var h Head
		h[0], h[1], h[2] = byte(i>>16), byte(i>>8), byte(i)
		h[locLen] = 4
		if err := w.Add(Row{Head: h}); err != nil {
			t.Fatal(err)
		}
	}
	info, err := w.Close()
	if err != nil || info.Records != uint64(n) {
		t.Fatal(err, info)
	}
	pr, f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	k := 0
	if err := pr.Each(func(Row) error { k++; return nil }); err != nil || k != n || pr.Blocks() != 3 {
		t.Fatal(err, k, pr.Blocks())
	}
}
