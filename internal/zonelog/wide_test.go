package zonelog

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Rows wider than 255 metrics (embeddings) are written IXR2 and in wide
// pages; narrow rows keep IXR1 and narrow pages. Both read back exact, in the
// memtable, in segments, across merges and after a reopen, and their sums
// are the sums of what was written.
func TestWideRowsRoundTripAndSum(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(4))
	live := map[string]Record{}
	widths := []int{3, 300, 1536, 4096}
	write := func(r Record) {
		t.Helper()
		if err := s.Apply([]Record{r}); err != nil {
			t.Fatal(err)
		}
		key := r.Location + "/" + r.ID
		if r.Deleted {
			delete(live, key)
		} else {
			live[key] = r
		}
	}
	record := func(i int) Record {
		m := make([]float64, widths[rng.Intn(len(widths))])
		for j := range m {
			m[j] = float64(rng.Intn(1000) - 500)
		}
		loc := string(alphabet[rng.Intn(4)]) + string(alphabet[rng.Intn(64)])
		return Record{Collection: testCollection, Location: loc, ID: fmt.Sprint(i % 700), Metrics: m}
	}
	check := func(when string) {
		t.Helper()
		for _, want := range live {
			got, ok, err := s.Get(testCollection, want.Location, want.ID)
			if err != nil || !ok || len(got.Metrics) != len(want.Metrics) {
				t.Fatalf("%s: %s/%s: ok=%v err=%v %d metrics, want %d", when, want.Location, want.ID, ok, err, len(got.Metrics), len(want.Metrics))
			}
			for j := range want.Metrics {
				if got.Metrics[j] != want.Metrics[j] {
					t.Fatalf("%s: %s/%s metric %d: %v, want %v", when, want.Location, want.ID, j, got.Metrics[j], want.Metrics[j])
				}
			}
		}
		sums, err := s.Children(testCollection, "@")
		if err != nil {
			t.Fatal(err)
		}
		for c := 0; c < 4; c++ {
			want := make([]float64, 4096)
			var count int64
			for _, r := range live {
				if r.Location[0] == alphabet[c] {
					count++
					for j, v := range r.Metrics {
						want[j] += v
					}
				}
			}
			got := sums[1+c]
			if got.Count != count {
				t.Fatalf("%s: child %c counts %d, want %d", when, alphabet[c], got.Count, count)
			}
			for j, v := range want {
				var g float64
				if j < len(got.Metrics) {
					g = got.Metrics[j]
				}
				if g != v {
					t.Fatalf("%s: child %c metric %d sums %v, want %v", when, alphabet[c], j, g, v)
				}
			}
		}
	}
	for round := 0; round < 4; round++ {
		for i := 0; i < 400; i++ {
			r := record(round*400 + i)
			if rng.Intn(8) == 0 {
				for _, l := range live {
					r = Record{Collection: testCollection, Location: l.Location, ID: l.ID, Deleted: true}
					break
				}
			}
			write(r)
		}
		check(fmt.Sprintf("round %d memtable", round))
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		check(fmt.Sprintf("round %d segments", round))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(dir, Options{NoSync: true}); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check("reopened")
	if !hasMagic(t, dir, "ZLS4") {
		t.Fatalf("no segment holds the wide rows as ZLS4")
	}
	if err := s.Apply([]Record{{Collection: testCollection, Location: "ab", ID: "too-wide", Metrics: make([]float64, MaxMetrics+1)}}); err == nil {
		t.Fatalf("a row wider than MaxMetrics was accepted")
	}
}

// A store of narrow rows writes the format it always wrote: fewer than 64
// columns, or up to 255 with a value that float32 would round.
func TestNarrowRowsKeepTheirFormat(t *testing.T) {
	for _, c := range []struct {
		width int
		value float64
	}{{63, 1}, {255, 0.1}} {
		dir := t.TempDir()
		writeRows(t, dir, 300, func(int) []float64 {
			m := make([]float64, c.width)
			for j := range m {
				m[j] = c.value
			}
			return m
		})
		if hasMagic(t, dir, "ZLS4") || !hasMagic(t, dir, "ZLS3") {
			t.Fatalf("%d columns of %v changed the segment format", c.width, c.value)
		}
	}
}

// Vectors whose every value is a float32 (embeddings) are stored on 4 bytes
// a value: segments about half the size, the same values and the same sums.
// One value that float32 would round keeps its page on 8 bytes.
func TestF32PagesHalveVectorsLosslessly(t *testing.T) {
	const rows, width = 2000, 128
	vector := func(exact bool) func(int) []float64 {
		rng := rand.New(rand.NewSource(7))
		return func(int) []float64 {
			m := make([]float64, width)
			for j := range m {
				m[j] = float64(float32(rng.NormFloat64()))
			}
			if !exact {
				m[rng.Intn(width)] += 1e-12
			}
			return m
		}
	}
	sizes := map[bool]int64{}
	for _, exact := range []bool{true, false} {
		dir := t.TempDir()
		want := map[string][]float64{}
		gen := vector(exact)
		writeRows(t, dir, rows, func(i int) []float64 {
			m := gen(i)
			want[fmt.Sprint(i)] = m
			return m
		})
		s, err := Open(dir, Options{NoSync: true})
		if err != nil {
			t.Fatal(err)
		}
		total := make([]float64, width)
		for id, m := range want {
			got, ok, err := s.Get(testCollection, "ab", id)
			if err != nil || !ok {
				t.Fatalf("exact=%v %s: ok=%v err=%v", exact, id, ok, err)
			}
			for j := range m {
				if math.Float64bits(got.Metrics[j]) != math.Float64bits(m[j]) {
					t.Fatalf("exact=%v %s metric %d: %v, want %v", exact, id, j, got.Metrics[j], m[j])
				}
			}
		}
		sums, err := s.Children(testCollection, "@")
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range want {
			for j, v := range m {
				total[j] += v
			}
		}
		got := sums[1+strings.IndexByte(alphabet, 'a')]
		for j := range total {
			if d := math.Abs(got.Metrics[j] - total[j]); d > 1e-9 {
				t.Fatalf("exact=%v metric %d sums %v, want %v", exact, j, got.Metrics[j], total[j])
			}
		}
		s.Close()
		if hasMagic(t, dir, "ZLS4") != exact {
			t.Fatalf("exact=%v: ZLS4 is %v", exact, !exact)
		}
		sizes[exact] = segBytes(t, dir)
	}
	ratio := float64(sizes[true]) / float64(sizes[false])
	t.Logf("%d vectors of %d: %d bytes in f32 pages, %d in f64 (%.2f)", rows, width, sizes[true], sizes[false], ratio)
	if ratio > 0.55 {
		t.Fatalf("f32 segments are %.2f of f64 ones (%d against %d bytes)", ratio, sizes[true], sizes[false])
	}
}

func writeRows(t *testing.T, dir string, n int, metrics func(int) []float64) {
	t.Helper()
	s, err := Open(dir, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := s.Apply([]Record{{Collection: testCollection, Location: "ab", ID: fmt.Sprint(i), Metrics: metrics(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func segBytes(t *testing.T, dir string) (n int64) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		n += info.Size()
	}
	return n
}

func hasMagic(t *testing.T, dir, magic string) bool {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(string(b), magic) {
			return true
		}
	}
	return false
}
