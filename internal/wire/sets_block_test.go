package wire

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func setsFrom(rows map[string]*Abelian) *Set {
	set := NewSet()
	for key, value := range rows {
		set.Put(key, value)
	}
	return set
}

// basicSetsBlockInput is BASIC from testdata/sets_block/reference.py.
func basicSetsBlockInput() (map[string]*Set, []string, []SetsRedirect) {
	sets := map[string]*Set{
		"@": setsFrom(map[string]*Abelian{
			"7":  NewAbelian(3, []float64{10, 2.5}),
			"a":  NewAbelian(1, []float64{4}),
			"7a": NewAbelian(2, []float64{1, 0.25, 7}),
			"_-": NewAbelian(5, nil),
		}),
		"7": setsFrom(map[string]*Abelian{
			"7B":      NewAbelian(4, []float64{1.5}),
			"7B:ref1": NewAbelian(1, []float64{0.5}),
			"7C":      NewAbelian(2, []float64{123456.789}),
			"7Bxyz":   NewAbelian(1, []float64{3}),
		}),
	}
	redirects := []SetsRedirect{{Location: "a", Name: "peerA", IP: "10.0.0.2", Port: 21000}}
	return sets, []string{"@", "7"}, redirects
}

// edgesSetsBlockInput is EDGES from testdata/sets_block/reference.py.
func edgesSetsBlockInput() (map[string]*Set, []string, []SetsRedirect) {
	sets := map[string]*Set{
		"7B": setsFrom(map[string]*Abelian{
			"7B":       NewAbelian(2, []float64{1}),
			"7B:r2":    NewAbelian(1, []float64{2, 3}),
			"7Babcdef": NewAbelian(1, []float64{1, 2, 3, 4, 5}),
			"7Bzzzzz_": NewAbelian(3, []float64{0.5}),
			"7C":       NewAbelian(1, []float64{9}),
			"c9":       NewAbelian(4, []float64{8}),
			"c9:x":     NewAbelian(1, nil),
		}),
		"@": setsFrom(map[string]*Abelian{
			"0123456789ABCDEFGHIJKLMNOPQRSTUV":  NewAbelian(1, []float64{1}),
			"0123456789ABCDEFGHIJKLMNOPQRSTUVW": NewAbelian(2, []float64{2, 1}),
			"_":                                 NewAbelian(1, []float64{-1.5}),
		}),
		"b": setsFrom(map[string]*Abelian{
			"bQ":  NewAbelian(1, []float64{1e12, 0.1}),
			"b-_": NewAbelian(2, []float64{3e5, 1.5e7, 16777217}),
		}),
	}
	return sets, []string{"7B", "@", "b"}, nil
}

// dvfSetsBlockInput is DVF from testdata/sets_block/reference.py.
func dvfSetsBlockInput() (map[string]*Set, []string, []SetsRedirect) {
	sets := map[string]*Set{
		"u0": setsFrom(map[string]*Abelian{
			"u0A": NewAbelian(1, []float64{189056.0, 34.0, 5560.470588235294, 135.74761759038387, 183.12291497781908}),
			"u0B": NewAbelian(1, []float64{133365.0, 86.0, 1550.7558139534883, 136.6010415690312, 179.2485833346568}),
			"u0C": NewAbelian(3, []float64{2286068.0, 616.0, 11598.584995319996, 407.7495272404708, 549.9689999152474}),
			"u0D": NewAbelian(250, []float64{98538282.0, 40373.0, 916104.7833720458, 34045.66122428856, 45711.71846422679}),
			"u0E": NewAbelian(40000, []float64{15206604138.0, 6608310.0, 130492857.77623127, 5440081.885141663, 7320566.159935768}),
			"u0F": NewAbelian(200000, []float64{76030951788.0, 33002614.0, 652558071.3831596, 27200705.161940936, 36599995.199505344}),
			"u0G": NewAbelian(290000, []float64{1012345678901.0, 43210987.0, 6672345678.25, 39672000.123456, 52867000.654321}),
		}),
	}
	return sets, []string{"u0"}, nil
}

// alignSetsBlockInput is ALIGN from testdata/sets_block/reference.py.
func alignSetsBlockInput() (map[string]*Set, []string, []SetsRedirect) {
	sets := map[string]*Set{
		"@": setsFrom(map[string]*Abelian{
			"a":   NewAbelian(1, []float64{0.1}),
			"bC":  NewAbelian(2, []float64{1e300, -2.5e-300}),
			"dEf": NewAbelian(5, nil),
		}),
		"X": setsFrom(map[string]*Abelian{
			"X1":  NewAbelian(3, nil),
			"XYZ": NewAbelian(1, []float64{16777217, 1, 2}),
		}),
		"ab": setsFrom(map[string]*Abelian{
			"abc":      NewAbelian(1, []float64{3}),
			"abcdefgh": NewAbelian(2, []float64{123456.789, 0.5}),
		}),
		"q_-9Z": setsFrom(map[string]*Abelian{
			"q_-9Z0": NewAbelian(4, []float64{1.0 / 3.0}),
		}),
	}
	redirects := []SetsRedirect{{Location: "X", Name: "peerX", IP: "10.0.0.3", Port: 21001}}
	return sets, []string{"@", "X", "ab", "q_-9Z"}, redirects
}

// wideSetsBlockInput is WIDE from testdata/sets_block/reference.py.
func wideSetsBlockInput() (map[string]*Set, []string, []SetsRedirect) {
	embed := make([]float64, 300)
	for j := range embed {
		embed[j] = float64(j%13)*0.5 - 3
	}
	sets := map[string]*Set{
		"@": setsFrom(map[string]*Abelian{
			"a": NewAbelian(1, []float64{1, 2}),
			"b": NewAbelian(2, nil),
		}),
		"7": setsFrom(map[string]*Abelian{
			"7B": NewAbelian(1, embed),
			"7C": NewAbelian(3, embed[:7]),
		}),
	}
	return sets, []string{"@", "7"}, nil
}

// The golden files are written by reference.py from the contract alone; the
// same files are decoded by sdk/src/wire/block.test.mjs.
func TestSetsBlockMatchesReferenceGolden(t *testing.T) {
	cases := map[string]struct {
		input   func() (map[string]*Set, []string, []SetsRedirect)
		width   SetsBlockMetrics
		columns []int
	}{
		"basic":       {basicSetsBlockInput, SetsBlockF32, nil},
		"edges":       {edgesSetsBlockInput, SetsBlockF32, nil},
		"dvf":         {dvfSetsBlockInput, SetsBlockF32, nil},
		"basic-f64":   {basicSetsBlockInput, SetsBlockF64, nil},
		"align-f64":   {alignSetsBlockInput, SetsBlockF64, nil},
		"columns":     {basicSetsBlockInput, SetsBlockF32, []int{2, 0}},
		"columns-f64": {alignSetsBlockInput, SetsBlockF64, []int{1}},
		"wide":        {wideSetsBlockInput, SetsBlockF32, nil},
		"wide-f64":    {wideSetsBlockInput, SetsBlockF64, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("testdata", "sets_block")
			golden, err := os.ReadFile(filepath.Join(dir, name+".bin"))
			if err != nil {
				t.Fatal(err)
			}
			sets, locations, redirects := c.input()
			for range 8 {
				body, err := EncodeSetsBlockColumns(sets, locations, SetsPrecision, redirects, c.width, c.columns)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(body, golden) {
					t.Fatalf("encoded block differs from reference.py:\n got %x\nwant %x", body, golden)
				}
				if cap(body) != len(body) {
					t.Fatalf("output buffer sized %d for %d bytes", cap(body), len(body))
				}
			}

			raw, err := os.ReadFile(filepath.Join(dir, name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var want SetsBlock
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if len(want.Redirects) == 0 {
				want.Redirects = nil
			}
			got, err := DecodeSetsBlock(golden)
			if err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("decoded %s.bin differs from %s.json:\n got %s\nwant %s", name, name, gotJSON, wantJSON)
			}
		})
	}
}

func TestSetsBlockKeyPacking(t *testing.T) {
	var words [setsBlockMaxKeyWords]uint32
	if !packSetsBlockKey(&words, "7a") || words[0] != 0x1E400000 {
		t.Fatalf("suffix 7a packed as %#x", words[0])
	}
	full := strings.Repeat("_", SetsPrecision)
	words = [setsBlockMaxKeyWords]uint32{}
	if !packSetsBlockKey(&words, full) {
		t.Fatal("full-precision key rejected")
	}
	k := setsBlockKeyWords(SetsPrecision)
	if k != setsBlockMaxKeyWords || unpackSetsBlockKey(words[:k], SetsPrecision) != full {
		t.Fatalf("full-precision key does not round trip: %x", words)
	}
	words = [setsBlockMaxKeyWords]uint32{}
	packSetsBlockKey(&words, full[:SetsPrecision-1])
	if words[k-1] != 0xFFFFFFC0 {
		t.Fatalf("unused low bits must stay zero: %#x", words[k-1])
	}
	if packSetsBlockKey(&words, "a@") {
		t.Fatal("character outside the routing alphabet accepted")
	}
}

func TestSetsBlockEdges(t *testing.T) {
	empty, err := EncodeSetsBlock(nil, []string{"missing"}, SetsPrecision, nil, SetsBlockF32)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := DecodeSetsBlock(empty); err != nil || len(empty) != setsBlockHeaderBytes || len(decoded.Sections) != 0 {
		t.Fatalf("empty block = %x, %v", empty, err)
	}
	sets := map[string]*Set{"ab": setsFrom(map[string]*Abelian{
		"ab":  NewAbelian(1, []float64{1}),
		"abc": NewAbelian(2, nil),
		"zz":  NewAbelian(3, nil),
	})}
	body, err := EncodeSetsBlock(sets, []string{"ab"}, SetsPrecision, nil, SetsBlockF32)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSetsBlock(body)
	if err != nil {
		t.Fatal(err)
	}
	var shape []string
	for _, s := range decoded.Sections {
		shape = append(shape, fmt.Sprintf("%s/%d/%v/%d", s.Parent, s.SuffixChars, s.Suffixes, s.MetricCount))
	}
	// The self-key is a zero-character suffix; a key outside its parent is
	// sent whole under parentLength 0, after the parent's own sections.
	if got := strings.Join(shape, " "); got != "ab/0/[]/1 ab/1/[c]/0 /2/[zz]/0" {
		t.Fatalf("sections = %s", got)
	}
	if _, err := EncodeSetsBlock(sets, []string{"ab"}, SetsPrecision+1, nil, SetsBlockF32); err == nil {
		t.Fatal("precision beyond SetsPrecision accepted")
	}
	bad := map[string]*Set{"@": setsFrom(map[string]*Abelian{"a@": NewAbelian(1, nil)})}
	if _, err := EncodeSetsBlock(bad, []string{"@"}, SetsPrecision, nil, SetsBlockF32); err == nil {
		t.Fatal("key outside the routing alphabet accepted")
	}
	for _, corrupt := range [][]byte{body[:15], append([]byte("IXB1"), body[4:]...), body[:len(body)-4]} {
		if _, err := DecodeSetsBlock(corrupt); err == nil {
			t.Fatalf("corrupt block accepted: %x", corrupt)
		}
	}
}

// The radix sort must give the order of the plain comparison, ties between
// keys of several words included.
func TestSetsBlockRadixSortMatchesComparison(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	for round := 0; round < 64; round++ {
		rows := make([]setsBlockRow, random.Intn(3000))
		for i := range rows {
			row := &rows[i]
			row.location = int32(random.Intn(3))
			row.fallback = random.Intn(5) == 0
			row.chars = uint8(1 + random.Intn(SetsPrecision))
			k := setsBlockKeyWords(int(row.chars))
			for w := 0; w < k; w++ {
				// Few distinct values, so that runs of equal first words occur.
				row.words[w] = uint32(random.Intn(4)) << 30
			}
			row.order = uint64(row.location)<<39 | uint64(row.chars)<<32 | uint64(row.words[0])
			if row.fallback {
				row.order |= 1 << 38
			}
			row.count = i
		}
		want := slices.Clone(rows)
		slices.SortStableFunc(want, func(a, b setsBlockRow) int {
			if c := cmp.Compare(a.order, b.order); c != 0 {
				return c
			}
			return slices.Compare(a.words[1:], b.words[1:])
		})
		sortSetsBlockRows(rows, nil)
		for i := range rows {
			if rows[i].order != want[i].order || rows[i].words != want[i].words {
				t.Fatalf("round %d row %d: got %x %x, want %x %x", round, i, rows[i].order, rows[i].words, want[i].order, want[i].words)
			}
		}
		seen := make([]bool, len(rows))
		for _, row := range rows {
			if seen[row.count] {
				t.Fatalf("round %d: row %d placed twice", round, row.count)
			}
			seen[row.count] = true
		}
	}
}

// With metrics=f64, folded rows are summed in float64 and written as they are.
func TestSetsBlockF64FoldsWithoutRounding(t *testing.T) {
	sets := map[string]*Set{"@": setsFrom(map[string]*Abelian{
		"ab:1": NewAbelian(1, []float64{0.1, 1 << 40}),
		"ab:2": NewAbelian(1, []float64{0.2, 1}),
	})}
	body, err := EncodeSetsBlock(sets, []string{"@"}, SetsPrecision, nil, SetsBlockF64)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSetsBlock(body)
	if err != nil {
		t.Fatal(err)
	}
	got := decoded.Sections[0].Metrics
	a, b := 0.1, 0.2 // float64 sum, not exact constant arithmetic
	if got[0][0] != a+b || got[1][0] != 1<<40+1 || decoded.Sections[0].Count[0] != 2 {
		t.Fatalf("folded metrics %v", got)
	}
	if binary.LittleEndian.Uint16(body[6:])&setsBlockFlagF64 == 0 {
		t.Fatal("f64 flag not set")
	}
}

// The pooled scratch carries nothing from one body to the next: a small
// body encodes to the same bytes before and after larger, wider ones.
func TestSetsBlockScratchReuse(t *testing.T) {
	small := map[string]*Set{"00": benchmarkSetsOf(300)["@"]}
	big := benchmarkSetsOf(5000)
	first, err := EncodeSetsBlock(small, []string{"00"}, SetsPrecision, nil, SetsBlockF32)
	if err != nil {
		t.Fatal(err)
	}
	for _, width := range []SetsBlockMetrics{SetsBlockF64, SetsBlockF32} {
		if _, err := EncodeSetsBlockColumns(big, []string{"@"}, SetsPrecision, nil, width, []int{4, 2}); err != nil {
			t.Fatal(err)
		}
		if _, err := EncodeSetsBlock(big, []string{"@"}, 3, nil, width); err != nil {
			t.Fatal(err)
		}
		again, err := EncodeSetsBlock(small, []string{"00"}, SetsPrecision, nil, SetsBlockF32)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatal("a reused scratch changed the body")
		}
	}
}
