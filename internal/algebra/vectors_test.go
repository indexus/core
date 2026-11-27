package algebra

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// Golden vectors shared with the JS client (sdk-js/src/tree/testdata): IXC1
// bytes and their names, decoded fields, cell order keys, and folds of
// records in cell order. INDEXUS_WRITE_VECTORS=1 rewrites the file; otherwise
// the file must be what this code produces.

type vectorRecord struct {
	Location string    `json:"location"`
	ID       string    `json:"id"`
	Metrics  []float64 `json:"metrics"`
}

type vectorFold struct {
	Records []vectorRecord `json:"records"`
	Count   int64          `json:"count"`
	Cols    []float64      `json:"cols"`
}

type vectorDescription struct {
	Bytes        string           `json:"bytes"`
	Name         string           `json:"name"`
	BitsPerChar  int              `json:"bitsPerChar"`
	Width        int              `json:"width"`
	Nonce        string           `json:"nonce"`
	RecordDepths [2]int           `json:"recordDepths"`
	Axes         [][2]float64     `json:"axes"`
	Schedule     []int            `json:"schedule"`
	Schema       []string         `json:"schema"`
	CellOrder    map[string][]int `json:"cellOrder"`
	Fold         vectorFold       `json:"fold"`
}

func vectorDescriptors() []*Descriptor {
	opaque := testDescriptor()
	opaque.Space = Space{}
	opaque.RecordDepths = [2]int{1, 4}
	opaque.Schema = Schema{Sum, Sum, Sum, Max, AffineA, AffineB, Last}
	wide := testDescriptor()
	wide.Space = Space{}
	wide.RecordDepths = [2]int{1, 3}
	wide.Nonce = [16]byte{9}
	for i := 0; i < 300; i++ {
		wide.Schema = append(wide.Schema, Sum)
	}
	wide.Schema = append(Schema{Min}, wide.Schema...)
	return []*Descriptor{testDescriptor(), gpsTimeDescriptor(), opaque, wide}
}

func buildVectors(t *testing.T) []vectorDescription {
	rng := rand.New(rand.NewSource(42))
	var out []vectorDescription
	for _, d := range vectorDescriptors() {
		c, err := d.Canonical()
		if err != nil {
			t.Fatal(err)
		}
		name, _ := d.Name()
		v := vectorDescription{Bytes: hex.EncodeToString(c), Name: name, BitsPerChar: d.BitsPerChar, Width: d.Width,
			Nonce: hex.EncodeToString(d.Nonce[:]), RecordDepths: d.RecordDepths, Schedule: []int{}, CellOrder: map[string][]int{}}
		for _, a := range d.Space.Schedule {
			v.Schedule = append(v.Schedule, int(a))
		}
		v.Axes = [][2]float64{}
		for _, a := range d.Space.Axes {
			v.Axes = append(v.Axes, [2]float64{a.Min, a.Max})
		}
		for _, k := range d.Schema {
			v.Schema = append(v.Schema, k.String())
		}
		size := 1 << d.BitsPerChar
		lo := max(1, d.RecordDepths[0])
		hi := min(d.RecordDepths[1], max(lo, 4))
		var recs []vectorRecord
		for i := 0; i < 64; i++ {
			depth := lo + rng.Intn(hi-lo+1)
			loc := make([]byte, depth)
			for k := range loc {
				loc[k] = nameAlphabet[rng.Intn(min(size, 6))]
			}
			m := make([]float64, len(d.Schema))
			for j, k := range d.Schema {
				switch k {
				case AffineA:
					m[j] = 0.9 + float64(rng.Intn(100))/1000
				default:
					m[j] = float64(rng.Intn(2000)-1000) / 8 // exact in binary: sums are exact in any order
				}
			}
			recs = append(recs, vectorRecord{Location: string(loc), ID: string(rune('a' + rng.Intn(3))), Metrics: m})
			key := []int{}
			for _, b := range CellOrder(d.BitsPerChar, name, string(loc)) {
				key = append(key, int(b))
			}
			v.CellOrder[string(loc)] = key
		}
		v.CellOrder["@"] = []int{}
		// Fold in cell order, then record id.
		sorted := append([]vectorRecord(nil), recs...)
		sortVector(d, name, sorted)
		acc := d.Schema.Zero()
		for _, r := range sorted {
			acc = d.Schema.Combine(acc, d.Schema.Lift(r.Metrics))
		}
		v.Fold = vectorFold{Records: recs, Count: acc.Count, Cols: acc.Cols}
		out = append(out, v)
	}
	return out
}

func sortVector(d *Descriptor, name string, rs []vectorRecord) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0; j-- {
			a, b := rs[j-1], rs[j]
			c := bytes.Compare(CellOrder(d.BitsPerChar, name, a.Location), CellOrder(d.BitsPerChar, name, b.Location))
			if c < 0 || c == 0 && a.ID <= b.ID {
				break
			}
			rs[j-1], rs[j] = b, a
		}
	}
}

func TestDescriptionVectorsForTheClient(t *testing.T) {
	got, err := json.MarshalIndent(buildVectors(t), "", " ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "description_vectors.json")
	if os.Getenv("INDEXUS_WRITE_VECTORS") == "1" {
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
		t.Fatalf("%s is not what this code produces: rerun with INDEXUS_WRITE_VECTORS=1", path)
	}
}
