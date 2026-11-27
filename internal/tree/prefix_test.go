package tree

import "testing"

func TestGuardBitAndPrefix(t *testing.T) {
	d := SetBit(ID{}, 95, 1)
	u := SetBit(ID{}, 0, 1)
	g := GuardBit(d, []ID{d, u})
	if g != 1 {
		t.Fatalf("g(D)=%d want 1", g)
	}
	p := Prefix{ID: d, Bits: 0}
	if !p.Contains(u) {
		t.Fatal("0-bit prefix must contain every id")
	}
	p = Prefix{ID: d, Bits: g}
	if p.Contains(u) {
		t.Fatal("guard prefix must not contain the defining neighbor")
	}
	if g2 := GuardBit(d, nil); g2 != 0 {
		t.Fatalf("solo g(D)=%d", g2)
	}
}

func TestFirstDiffFrom(t *testing.T) {
	a := ID{}
	b := SetBit(a, 10, 1)
	got, ok := FirstDiffFrom(a, b, 0)
	if !ok || got != 10 {
		t.Fatalf("diff=%d ok=%v", got, ok)
	}
	if _, ok := FirstDiffFrom(a, b, 11); ok {
		t.Fatal("no diff at or after 11")
	}
}

// One width for every 96-bit name: a vnode name, a collection, the deepest
// location, and so every zone key is 12 bytes.
func TestNamesCollectionsAndZoneKeysShareOneWidth(t *testing.T) {
	if Width != 16 || BASE64.IDLength() != Width || len(BASE64.NewID()) != Size {
		t.Fatalf("width %d, id length %d, new id %d bytes", Width, BASE64.IDLength(), len(BASE64.NewID()))
	}
	name, err := BASE64.RandomName()
	if err != nil || CheckName(name) != nil {
		t.Fatalf("random name %q: %v", name, err)
	}
	for _, bad := range []string{"", "demo", "RecoveringPeerAAA", "DvFMV2020idx000!"} {
		if CheckName(bad) == nil {
			t.Fatalf("name %q accepted", bad)
		}
		if _, err := DecodeName(bad); err == nil {
			t.Fatalf("DecodeName(%q) accepted", bad)
		}
	}
	for _, location := range []string{"@", "5", "5xS4R2qq77xabVnr"} {
		id, err := ZoneKeyID("DvFMV2020idx0001", location)
		if err != nil || len(id) != Size {
			t.Fatalf("zone key %q: %d bytes, %v", location, len(id), err)
		}
	}
	for _, c := range [][2]string{{"demo", "5x"}, {"DvFMV2020idx0001", "5xS4R2qq77xabVnrA"}, {"DvFMV2020idx0001", "5x!"}} {
		if _, err := ZoneKeyID(c[0], c[1]); err == nil {
			t.Fatalf("zone key %v accepted", c)
		}
	}
}
