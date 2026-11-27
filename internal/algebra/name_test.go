package algebra

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// testDescriptor: base 4, 6 characters, two axes on [0, 1], records at
// depths 4 to 6, columns [weight, price, maxPrice, open, close].
func testDescriptor() *Descriptor {
	return &Descriptor{
		BitsPerChar:  2,
		Width:        6,
		Space:        Space{Axes: []Axis{{Min: 0, Max: 1}, {Min: 0, Max: 1}}, Schedule: RoundRobin(2, 12)},
		RecordDepths: [2]int{4, 6},
		Schema:       Schema{Sum, Sum, Max, First, Last},
	}
}

// gpsTimeDescriptor: latitude, longitude, then time in ms on [0, 2^42].
func gpsTimeDescriptor() *Descriptor {
	sched := RoundRobin(2, 60)
	for i := 0; i < 36; i++ {
		sched = append(sched, 2)
	}
	return &Descriptor{
		BitsPerChar:  6,
		Width:        16,
		Nonce:        [16]byte{1},
		Space:        Space{Axes: []Axis{{Min: -90, Max: 90}, {Min: -180, Max: 180}, {Min: 0, Max: 1 << 42}}, Schedule: sched},
		RecordDepths: [2]int{10, 10},
		Schema:       Schema{Sum, Min, Max, AffineA, AffineB},
	}
}

func TestNameIsDeterministicAndRoundTrips(t *testing.T) {
	for _, d := range []*Descriptor{testDescriptor(), gpsTimeDescriptor()} {
		c, err := d.Canonical()
		if err != nil {
			t.Fatal(err)
		}
		name, _ := d.Name()
		if len(name) != 16 || strings.Trim(name, nameAlphabet) != "" {
			t.Fatalf("name %q is not 16 routing characters", name)
		}
		if again, _ := d.Name(); again != name {
			t.Fatalf("name not deterministic")
		}
		back, digest, err := Verify(name, c)
		if err != nil {
			t.Fatal(err)
		}
		c2, _ := back.Canonical()
		if !bytes.Equal(c, c2) || NameOf(digest) != name {
			t.Fatalf("round trip changed the description")
		}
		t.Logf("%s  %d bytes", name, len(c))
	}
}

func TestEveryMathematicalChangeChangesTheName(t *testing.T) {
	base, _ := testDescriptor().Name()
	mutations := map[string]func(*Descriptor){
		"nonce":         func(d *Descriptor) { d.Nonce[15] ^= 1 },
		"geometry":      func(d *Descriptor) { d.Width = 7; d.Space.Schedule = RoundRobin(2, 14) },
		"axis bound":    func(d *Descriptor) { d.Space.Axes[1].Max = 2 },
		"schedule":      func(d *Descriptor) { d.Space.Schedule[0], d.Space.Schedule[1] = 1, 0 },
		"record depths": func(d *Descriptor) { d.RecordDepths[1] = 5 },
		"column kind":   func(d *Descriptor) { d.Schema[1] = Min },
		"column order":  func(d *Descriptor) { d.Schema[3], d.Schema[4] = Last, First },
		"extra column":  func(d *Descriptor) { d.Schema = append(d.Schema, Sum) },
	}
	seen := map[string]string{base: "base"}
	for what, mutate := range mutations {
		d := testDescriptor()
		mutate(d)
		name, err := d.Name()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if prev, dup := seen[name]; dup {
			t.Fatalf("%s gives the same name as %s", what, prev)
		}
		seen[name] = what
	}
}

func TestVerifyRejectsTamperingAndWrongNames(t *testing.T) {
	d := gpsTimeDescriptor()
	c, _ := d.Canonical()
	name, _ := d.Name()
	for i := range c {
		bad := append([]byte(nil), c...)
		bad[i] ^= 0x01
		if _, _, err := Verify(name, bad); err == nil {
			t.Fatalf("a flipped bit at byte %d was accepted", i)
		}
	}
	if _, _, err := Verify(name, append(append([]byte(nil), c...), 0)); !errors.Is(err, ErrEncoding) {
		t.Fatalf("trailing bytes accepted: %v", err)
	}
	other, _ := testDescriptor().Name()
	if _, _, err := Verify(other, c); !errors.Is(err, ErrName) {
		t.Fatalf("a description was accepted under another name: %v", err)
	}
}

func TestCatalogIsIdempotentAndRefusesASecondDescriptionForOneName(t *testing.T) {
	cat := Catalog{}
	d := testDescriptor()
	c, _ := d.Canonical()
	name, _ := d.Name()
	for k := 0; k < 3; k++ {
		if _, err := cat.Admit(name, c); err != nil {
			t.Fatal(err)
		}
	}
	held := cat[name]
	held.Digest[0] ^= 1 // simulate a colliding description already held
	cat[name] = held
	if _, err := cat.Admit(name, c); !errors.Is(err, ErrCollision) {
		t.Fatalf("collision not detected: %v", err)
	}
}

func TestSpaceDigestIsSharedByCollectionsWithTheSameSpace(t *testing.T) {
	a, b := testDescriptor(), testDescriptor()
	b.Nonce[0] = 7
	b.Schema[1] = Min
	na, _ := a.Name()
	nb, _ := b.Name()
	if na == nb || a.Space.SpaceDigest() != b.Space.SpaceDigest() {
		t.Fatalf("different collections with one space must share the space digest only")
	}
	c := testDescriptor()
	c.Space.Axes[0].Min = -1
	if c.Space.SpaceDigest() == a.Space.SpaceDigest() {
		t.Fatalf("a different space must have a different digest")
	}
}

func TestDescriptorValidation(t *testing.T) {
	cases := map[string]func(*Descriptor){
		"zero width":       func(d *Descriptor) { d.Width = 0 },
		"too many bits":    func(d *Descriptor) { d.BitsPerChar = 9 },
		"records too deep": func(d *Descriptor) { d.RecordDepths[1] = 7 },
		"records inverted": func(d *Descriptor) { d.RecordDepths = [2]int{5, 4} },
		"unknown kind":     func(d *Descriptor) { d.Schema[0] = Kind(99) },
		"split affine":     func(d *Descriptor) { d.Schema = Schema{AffineA, Sum} },
	}
	if err := testDescriptor().Validate(); err != nil {
		t.Fatal(err)
	}
	for what, mutate := range cases {
		d := testDescriptor()
		mutate(d)
		if err := d.Validate(); err == nil {
			t.Fatalf("%s accepted", what)
		}
	}
}

func TestRecordsDeeperThanFloatPrecisionAreRefused(t *testing.T) {
	d := &Descriptor{BitsPerChar: 6, Width: 16, RecordDepths: [2]int{4, 16}, Schema: Schema{Sum}}
	// Chronicle's time axis, in milliseconds from 1990 to 2100: exact for 21 cuts only.
	d.Space = Space{Axes: []Axis{{Min: 631152000000, Max: 4102444800000}}, Schedule: RoundRobin(1, 96)}
	if err := d.Validate(); err == nil {
		t.Fatalf("a time axis cut 96 times was accepted")
	}
	d.RecordDepths = [2]int{3, 3} // 18 cuts: exact
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	// Dyadic bounds (0 and 2^42 ms, about 139 years) stay exact for 53 cuts.
	d.Space.Axes[0] = Axis{Min: 0, Max: 1 << 42}
	d.RecordDepths = [2]int{4, 8} // 48 cuts
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
}

func FuzzDecode(f *testing.F) {
	for _, d := range []*Descriptor{testDescriptor(), gpsTimeDescriptor()} {
		c, _ := d.Canonical()
		f.Add(c)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := Decode(data)
		if err != nil {
			return
		}
		again, err := d.Canonical()
		if err != nil || !bytes.Equal(again, data) {
			t.Fatalf("decoded bytes are not canonical")
		}
	})
}

func TestSchemaRunsAreCompactAndCanonical(t *testing.T) {
	d := testDescriptor()
	d.Space = Space{} // opaque addresses: no axes, no schedule
	d.Schema = nil
	for i := 0; i < 768; i++ {
		d.Schema = append(d.Schema, Sum)
	}
	d.Schema = append(d.Schema, Max, AffineA, AffineB, AffineA, AffineB, Sum)
	c, err := d.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	// header 8, nonce 16, depths 2, axes 1, runs 2 + 4 runs of 3 bytes.
	if len(c) != 8+16+2+1+2+4*3 {
		t.Fatalf("%d bytes", len(c))
	}
	name, _ := d.Name()
	back, _, err := Verify(name, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Schema) != len(d.Schema) || back.Schema[768] != Max || back.Schema[770] != AffineB {
		t.Fatalf("schema not restored")
	}
	// Two neighbouring runs of one code are not canonical.
	split := append([]byte(nil), c[:27]...)
	split = append(split, 5, 0)                   // 5 runs
	split = append(split, 0, 0x80, 1, 0, 0x80, 1) // sum x 384, sum x 384
	split = append(split, c[29+3:]...)
	if back, err := Decode(split); err == nil || back != nil {
		t.Fatalf("a schema split into two runs of one code was accepted")
	}
	for _, bad := range []func([]byte){
		func(b []byte) { b[29+1], b[29+2] = 0, 0 }, // empty run
		func(b []byte) { b[29] = 6 },               // unknown code
	} {
		x := append([]byte(nil), c...)
		bad(x)
		if _, err := Decode(x); err == nil {
			t.Fatalf("bad run accepted")
		}
	}
	d.Schema = make(Schema, MaxColumns+1)
	if err := d.Validate(); err == nil {
		t.Fatalf("more than MaxColumns accepted")
	}
}

func TestOpaqueSpaceHasNoCoordinates(t *testing.T) {
	s := Space{}
	if err := s.ValidateSpace(6, 16, 16); err != nil {
		t.Fatal(err)
	}
	if err := (Space{Schedule: []uint8{0}}).ValidateSpace(6, 16, 16); err == nil {
		t.Fatalf("a schedule without axes accepted")
	}
	if _, err := s.EncodeLocation(6, "0123456789ABCDEF", nil, 3); !errors.Is(err, ErrOpaque) {
		t.Fatalf("encode on an opaque space: %v", err)
	}
	if _, err := s.DecodeLocation(6, "0123456789ABCDEF", "abc"); !errors.Is(err, ErrOpaque) {
		t.Fatalf("decode on an opaque space: %v", err)
	}
	if _, ok := s.Step(6); ok {
		t.Fatalf("step on an opaque space")
	}
}

func TestCellOrderSortsPrefixFirstWithoutRotation(t *testing.T) {
	name := "Q-zz0123456789AB"
	// Ranks at level 0 start at the rotation: the character "Q" has rank 0.
	if k := CellOrder(6, name, "Q"); len(k) != 1 || k[0] != 0 {
		t.Fatalf("rank of the rotation character: %v", k)
	}
	if CellOrder(6, name, "@") != nil {
		t.Fatalf("root key not empty")
	}
	a, b := CellOrder(6, name, "Q"), CellOrder(6, name, "Q0")
	if bytes.Compare(a, b) >= 0 {
		t.Fatalf("a prefix must sort before its extensions")
	}
	// "-" sorts before "0" as bytes but after it as an index (63 vs 0):
	// cell order uses indices.
	for l := 0; l < 3; l++ {
		x := []byte("QQQ")
		y := []byte("QQQ")
		x[l], y[l] = '0', '1'
		if bytes.Compare(CellOrder(6, name, string(x)), CellOrder(6, name, string(y))) == 0 {
			t.Fatalf("distinct characters give one rank")
		}
	}
	for c := 0; c < 64; c++ {
		if int(CellOrder(6, name, string(nameAlphabet[c]))[0]) != ChildRank(6, name, 0, nameAlphabet[c]) {
			t.Fatalf("CellOrder and ChildRank disagree")
		}
	}
}
