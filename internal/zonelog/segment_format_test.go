package zonelog

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A segment whose footer is not "ZLS3" (the earlier gob tail was "ZLS2") is
// not read: Open fails and names the format.
func TestSegmentOfAnotherFormatIsRefused(t *testing.T) {
	dir := t.TempDir()
	opt := Options{MemoryBytes: 128 << 10, NoSync: true}
	s := must(Open(dir, opt))
	rng := rand.New(rand.NewSource(2))
	for i := range 3000 {
		r := Record{Collection: testCollection, Location: randomLocation(rng), ID: fmt.Sprint(i % 700), Metrics: []float64{float64(i)}}
		if err := s.Apply([]Record{r}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	segs, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	if len(segs) == 0 {
		t.Fatal("no segment")
	}
	raw := must(os.ReadFile(segs[0]))
	copy(raw[len(raw)-4:], "ZLS2")
	if err := os.WriteFile(segs[0], raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, opt); err == nil || !strings.Contains(err.Error(), `"ZLS2"`) {
		t.Fatalf("open: %v", err)
	}
}

// A writer that spills its index entries to a file writes the same segment
// as one that holds them, and leaves no spill behind. An open segment keeps
// under a byte per 40 rows: its index and filter are read through the
// cache, and reads are the same with a cache too small for them.
func TestIndexSpillAndResidentMemory(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	var records []Record
	for i := range 40_000 {
		loc := []byte{'L', alphabet[rng.Intn(64)], alphabet[rng.Intn(64)], alphabet[rng.Intn(8)]}
		records = append(records, Record{Collection: testCollection, Location: string(loc), ID: fmt.Sprint(i), Metrics: []float64{float64(i % 7)}, Payload: []byte(fmt.Sprint(`"`, i, `"`))})
	}
	sort.Slice(records, func(i, j int) bool {
		a, _ := makeKey(0, records[i].Location, records[i].ID)
		b, _ := makeKey(0, records[j].Location, records[j].ID)
		return bytes.Compare(a, b) < 0
	})
	write := func(spill int) (string, *Store) {
		defer func(old int) { indexSpill = old }(indexSpill)
		indexSpill = spill
		dir := t.TempDir()
		s := must(Open(dir, Options{MemoryBytes: 64 << 10, NoSync: true}))
		if err := writeBulk(s, records, false, "r"); err != nil {
			t.Fatal(err)
		}
		return dir, s
	}
	held, a := write(1 << 30)
	spilled, b := write(64)
	defer a.Close()
	defer b.Close()
	segs := must(filepath.Glob(filepath.Join(held, "*.seg")))
	if len(segs) != 1 {
		t.Fatalf("segments: %v", segs)
	}
	if !bytes.Equal(must(os.ReadFile(segs[0])), must(os.ReadFile(filepath.Join(spilled, filepath.Base(segs[0]))))) {
		t.Fatal("the spilled writer wrote another segment")
	}
	if tmp := must(filepath.Glob(filepath.Join(spilled, "*.tmp"))); len(tmp) > 0 {
		t.Fatalf("left behind: %v", tmp)
	}
	st := b.Stats()
	if st.IndexBytes*40 > int(st.Rows) { // the whole index was 2.7 B per row
		t.Fatalf("%d bytes in memory for %d rows", st.IndexBytes, st.Rows)
	}
	for i, r := range records {
		if i%97 != 0 {
			continue
		}
		got, ok, err := b.Get(r.Collection, r.Location, r.ID)
		if err != nil || !ok || !bytes.Equal(got.Payload, r.Payload) {
			t.Fatalf("get %s/%s: %v %v", r.Location, r.ID, ok, err)
		}
		if _, ok, _ := b.Get(r.Collection, r.Location, r.ID+"x"); ok {
			t.Fatalf("get of a missing id found one")
		}
	}
	x, y := must(a.Children(testCollection, "L")), must(b.Children(testCollection, "L"))
	total := int64(0)
	for _, sum := range x {
		total += sum.Count
	}
	if fmt.Sprint(x) != fmt.Sprint(y) || total != int64(len(records)) {
		t.Fatalf("children differ or are empty: %v", x)
	}
}
