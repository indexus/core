package algebra

import (
	"encoding/json"
	"math"
	"math/big"
	"math/rand"
	"os"
	"sort"
	"testing"
)

type sdkCase struct {
	Name       string    `json:"name"`
	Root       []float64 `json:"root"`
	Dimensions []struct {
		Type string    `json:"type"`
		Args []float64 `json:"args"`
	} `json:"dimensions"`
	Vectors []struct {
		Coords   []float64 `json:"coords"`
		Depth    int       `json:"depth"`
		Location string    `json:"location"`
		Bounds   []float64 `json:"bounds"`
	} `json:"vectors"`
}

// FromSDK reads an SDK collection definition as a standard Space: a linear
// dimension is one axis; a spherical one is two axes (latitude, longitude).
// What "spherical" means for drawing stays with the client.
func FromSDK(t *testing.T, c sdkCase, bitsPerChar, width int) Space {
	var s Space
	for _, d := range c.Dimensions {
		switch d.Type {
		case "linear":
			s.Axes = append(s.Axes, Axis{Min: d.Args[0], Max: d.Args[1]})
		case "spherical": // SDK args: [south, north, west, east]
			s.Axes = append(s.Axes, Axis{Min: d.Args[0], Max: d.Args[1]}, Axis{Min: d.Args[2], Max: d.Args[3]})
		default:
			t.Fatalf("unknown dimension type %q", d.Type)
		}
	}
	s.Schedule = RoundRobin(len(s.Axes), bitsPerChar*width)
	return s
}

func loadSDK(t *testing.T) []sdkCase {
	data, err := os.ReadFile("testdata/space_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []sdkCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

// The standard reproduces the SDK bit for bit: every location it encodes and
// every box it decodes, on the six collection shapes in use.
func TestSpaceMatchesTheSDK(t *testing.T) {
	total := 0
	for _, c := range loadSDK(t) {
		s := FromSDK(t, c, 6, 16)
		if err := s.ValidateSpace(6, 16, 0); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		for _, v := range c.Vectors {
			got, err := s.EncodeLocation(6, c.Name, v.Coords, v.Depth)
			if err != nil || got != v.Location {
				t.Fatalf("%s: encode %v at %d = %q, SDK %q (%v)", c.Name, v.Coords, v.Depth, got, v.Location, err)
			}
			box, err := s.DecodeLocation(6, c.Name, v.Location)
			if err != nil {
				t.Fatal(err)
			}
			// The integer path gives the same box whenever the cell fits in 64 bits.
			if j, n, err := s.CellOf(6, c.Name, v.Location); err == nil {
				ib := s.Box(j, n)
				for i := range ib {
					if ib[i] != box[i] {
						t.Fatalf("%s: integer box differs at %d", c.Name, i)
					}
				}
			}
			for i := range box {
				if box[i] != v.Bounds[i] {
					t.Fatalf("%s: decode %q bound %d = %v, SDK %v", c.Name, v.Location, i, box[i], v.Bounds[i])
				}
			}
			total++
		}
		var levels []int
		for _, a := range s.Axes {
			levels = append(levels, a.DyadicLevel())
		}
		step, ok := s.Step(6)
		t.Logf("%-16s %d axes, step %d (%v), exact cuts per axis %v", c.Name, len(s.Axes), step, ok, levels)
	}
	t.Logf("%d SDK vectors reproduced exactly", total)
}

// exactCell is the mathematical definition, in rationals:
// j = clamp(ceil(2^n (x - min) / (max - min)) - 1, 0, 2^n - 1).
func exactCell(x float64, a Axis, n int) uint64 {
	if n == 0 {
		return 0
	}
	q := new(big.Rat).Sub(new(big.Rat).SetFloat64(x), new(big.Rat).SetFloat64(a.Min))
	q.Mul(q, new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), uint(n))))
	q.Quo(q, new(big.Rat).Sub(new(big.Rat).SetFloat64(a.Max), new(big.Rat).SetFloat64(a.Min)))
	f := new(big.Int).Quo(q.Num(), q.Denom())
	if new(big.Rat).SetInt(f).Cmp(q) == 0 {
		f.Sub(f, big.NewInt(1))
	}
	if f.Sign() < 0 {
		return 0
	}
	if lim := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(n)), big.NewInt(1)); f.Cmp(lim) > 0 {
		f = lim
	}
	return f.Uint64()
}

// Within the dyadic level, the integer cell of a location is the exact
// mathematical quantisation of the point: no float behaviour leaks in.
func TestCellIsTheExactQuantisation(t *testing.T) {
	checked, beyond := 0, 0
	for _, c := range loadSDK(t) {
		s := FromSDK(t, c, 6, 16)
		for _, v := range c.Vectors {
			j, n, err := s.CellOf(6, c.Name, v.Location)
			if err != nil { // an axis cut more than 64 times has no integer cell
				beyond++
				continue
			}
			for a, ax := range s.Axes {
				if n[a] > ax.DyadicLevel() || n[a] > 63 {
					beyond++
					continue
				}
				if want := exactCell(v.Coords[a], ax, n[a]); j[a] != want {
					t.Fatalf("%s %q axis %d: cell %d, exact %d", c.Name, v.Location, a, j[a], want)
				}
				checked++
			}
		}
	}
	t.Logf("%d (axis, location) cells equal the exact quantisation; %d beyond the dyadic level not compared", checked, beyond)
}

func TestDyadicLevelOfTheAxesInUse(t *testing.T) {
	want := map[[2]float64]int{
		{-90, 90}: 48, {-180, 180}: 48, {0, 4096}: 53, {331000, 333048}: 45, {6276000, 6278048}: 41,
		{1700000000, 1900000000}: 31, {631152000000, 4102444800000}: 21, {0, 1 << 42}: 53, {0.1, 0.7}: 0,
	}
	for b, n := range want {
		if got := (Axis{Min: b[0], Max: b[1]}).DyadicLevel(); got != n {
			t.Errorf("[%v, %v]: %d exact cuts, want %d", b[0], b[1], got, n)
		}
	}
}

func TestCustomScheduleSpaceThenTime(t *testing.T) {
	// Latitude, longitude, time. The first 10 characters cut only space
	// (lat and lon alternately), the last 6 only time: inside a zone of
	// depth 10, address order is time order.
	s := Space{
		Axes: []Axis{{Min: -90, Max: 90}, {Min: -180, Max: 180}, {Min: 0, Max: 1 << 40}},
	}
	for k := 0; k < 96; k++ {
		if k < 60 {
			s.Schedule = append(s.Schedule, uint8(k%2))
		} else {
			s.Schedule = append(s.Schedule, 2)
		}
	}
	if err := s.ValidateSpace(6, 16, 16); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Step(6); ok {
		t.Fatalf("this schedule has no uniform step")
	}
	if a, ok := s.OrderedAxis(6, 10); !ok || a != 2 {
		t.Fatalf("below depth 10 the order should be the time axis, got %d %v", a, ok)
	}
	if _, ok := s.OrderedAxis(6, 9); ok {
		t.Fatalf("below depth 9 the order is interleaved")
	}
	cuts := s.BitsAt(6, 16)
	t.Logf("cuts per axis at depth 16: lat %d, lon %d, time %d", cuts[0], cuts[1], cuts[2])
	// Round trip of points through the custom schedule.
	pts := [][]float64{{48.85, 2.35, 123456789}, {-33.9, 151.2, 1e12}, {90, 180, 1 << 40}, {-90, -180, 0}}
	for _, p := range pts {
		loc, err := s.EncodeLocation(6, "GpsThenTime00001", p, 16)
		if err != nil {
			t.Fatal(err)
		}
		box, _ := s.DecodeLocation(6, "GpsThenTime00001", loc)
		for a := range p {
			if p[a] > box[2*a+1] || p[a] < box[2*a] {
				t.Fatalf("point %v not in its box on axis %d", p, a)
			}
		}
	}
}

func TestOrderedColumnsFollowCellOrderNotCharacterOrder(t *testing.T) {
	// One time axis. Records at depth 2 (12 cuts). An affine recurrence over
	// time must be folded in time order. Sorting children by character (the
	// storage order of location strings) breaks it, because of the rotation;
	// sorting by ChildRank restores it.
	s := Space{Axes: []Axis{{Min: 0, Max: 4096}}, Schedule: RoundRobin(1, 96)}
	name := "TimeSeriesTest01"
	sch := Schema{AffineA, AffineB}
	type rec struct {
		loc string
		t   float64
		v   Value
	}
	var recs []rec
	for k := 0; k < 300; k++ {
		x := float64(k) * 13.37
		loc, err := s.EncodeLocation(6, name, []float64{x}, 2)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec{loc, x, sch.Lift([]float64{0.97, math.Sin(x)})})
	}
	byTime := append([]rec(nil), recs...)
	sort.SliceStable(byTime, func(i, j int) bool { return byTime[i].t < byTime[j].t })
	want := Value{}
	for _, r := range byTime {
		want = sch.Combine(want, r.v)
	}
	byChars := append([]rec(nil), recs...)
	sort.SliceStable(byChars, func(i, j int) bool { return byChars[i].loc < byChars[j].loc })
	byRank := append([]rec(nil), recs...)
	key := func(l string) [2]int { return [2]int{ChildRank(6, name, 0, l[0]), ChildRank(6, name, 1, l[1])} }
	sort.SliceStable(byRank, func(i, j int) bool {
		a, b := key(byRank[i].loc), key(byRank[j].loc)
		return a[0] < b[0] || (a[0] == b[0] && a[1] < b[1])
	})
	fold := func(rs []rec) Value {
		acc := Value{}
		for _, r := range rs {
			acc = sch.Combine(acc, r.v)
		}
		return acc
	}
	if got := fold(byRank); !close(sch, got, want) {
		t.Fatalf("cell order must be time order")
	}
	if got := fold(byChars); close(sch, got, want) {
		t.Fatalf("character order happened to match: pick another name")
	}
}

func TestEncodeRefusesPointsOutsideTheBox(t *testing.T) {
	s := Space{Axes: []Axis{{Min: -90, Max: 90}, {Min: -180, Max: 180}}, Schedule: RoundRobin(2, 96)}
	for _, p := range [][]float64{{91, 0}, {0, -180.0001}, {math.NaN(), 0}} {
		if _, err := s.EncodeLocation(6, "AAAAAAAAAAAAAAAA", p, 4); err == nil {
			t.Errorf("point %v accepted", p)
		}
	}
}

func TestSpaceValidation(t *testing.T) {
	ok := Space{Axes: []Axis{{Min: -90, Max: 90}, {Min: -180, Max: 180}}, Schedule: RoundRobin(2, 96)}
	if err := ok.ValidateSpace(6, 16, 16); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Space){
		"schedule too short": func(s *Space) { s.Schedule = s.Schedule[:95] },
		"axis never cut":     func(s *Space) { s.Schedule = RoundRobin(1, 96) },
		"unknown axis":       func(s *Space) { s.Schedule[3] = 7 },
		"empty interval":     func(s *Space) { s.Axes[0].Max = -90 },
		"infinite bound":     func(s *Space) { s.Axes[0].Max = math.Inf(1) },
		"too deep":           func(s *Space) { s.Axes[0] = Axis{Min: 631152000000, Max: 4102444800000} },
	}
	for what, mutate := range bad {
		s := Space{Axes: append([]Axis(nil), ok.Axes...), Schedule: append([]uint8(nil), ok.Schedule...)}
		mutate(&s)
		if s.ValidateSpace(6, 16, 16) == nil {
			t.Errorf("%s: accepted", what)
		}
	}
}

// Bases below 64: the rotation character's index can exceed the base, so it
// is taken modulo the base. Encoding and decoding must still be inverse and
// cell order keys must be ranks of the base.
func TestSmallBasesRotateWithinTheirOwnCharacters(t *testing.T) {
	name := "zz_-Q9aB0123456X" // indices well above 2^c for c < 6
	rng := rand.New(rand.NewSource(5))
	for c := 1; c <= 6; c++ {
		s := Space{Axes: []Axis{{Min: 0, Max: 1}, {Min: -1, Max: 1}}, Schedule: RoundRobin(2, c*12)}
		for i := 0; i < 200; i++ {
			x := []float64{rng.Float64(), 2*rng.Float64() - 1}
			loc, err := s.EncodeLocation(c, name, x, 12)
			if err != nil {
				t.Fatal(err)
			}
			box, err := s.DecodeLocation(c, name, loc)
			if err != nil {
				t.Fatal(err)
			}
			for a := range x {
				if !(x[a] >= box[2*a] && x[a] <= box[2*a+1]) {
					t.Fatalf("base 2^%d: %v not in the box of %s %v", c, x, loc, box)
				}
			}
			for l, k := range CellOrder(c, name, loc) {
				if int(k) >= 1<<c || int(k) != ChildRank(c, name, l, loc[l]) {
					t.Fatalf("base 2^%d: rank %d at level %d", c, k, l)
				}
			}
		}
	}
}
