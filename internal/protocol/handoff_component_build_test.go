package protocol

import (
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

const demoCol = "colAAAAAAAAAAAAA"

func mustZone(t *testing.T, loc string) tree.ZoneKey {
	t.Helper()
	z, err := tree.NewZoneKey(demoCol, loc)
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func flipLow(id tree.ID) tree.ID {
	out := id
	out[tree.Size-1] ^= 1
	if out == id {
		out[tree.Size-2] ^= 1
	}
	return out
}

func componentHas(c *ActivationComponent, name string) bool {
	for _, d := range c.Delta.Add {
		if d.Name == name {
			return true
		}
	}
	return false
}

func assertComponentAgreesWithView(t *testing.T, old, neu *tree.View, c *ActivationComponent, zones []tree.ZoneKey) {
	t.Helper()
	for _, z := range zones {
		got := c.NewView.Owner(z.ID)
		if got == old.Owner(z.ID) {
			continue
		}
		if got != neu.Owner(z.ID) {
			t.Fatalf("component %s would give %s owner %v, full view wants %v", c.ID, z.Location, got, neu.Owner(z.ID))
		}
	}
}

func independentLeafViews(t *testing.T, l1, l3 *tree.ZoneKey, zones []tree.ZoneKey) (*tree.View, *tree.View, []tree.ZoneKey) {
	t.Helper()
	d := bitID(95)
	j1 := bitID(0)
	j3 := bitID(1)
	l1.ID, l3.ID = j1, j3
	out := append([]tree.ZoneKey(nil), zones...)
	for i := range out {
		switch out[i].Location {
		case l1.Location:
			out[i].ID = j1
		case l3.Location:
			out[i].ID = j3
		default:
			out[i].ID = d
		}
	}
	old := tree.NewView(1)
	if err := old.Add("D", d); err != nil {
		t.Fatal(err)
	}
	neu, err := tree.Extend(old,
		tree.VNodeDescriptor{ID: j1, Name: "J1"},
		tree.VNodeDescriptor{ID: j3, Name: "J3"},
	)
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := tree.Extend(old, tree.VNodeDescriptor{ID: j1, Name: "J1"})
	v3, _ := tree.Extend(old, tree.VNodeDescriptor{ID: j3, Name: "J3"})
	if v1.Owner(l1.ID) != j1 || v3.Owner(l3.ID) != j3 {
		t.Fatal("synthetic vnodes must capture their leaves")
	}
	if v1.Owner(l3.ID) != d || v3.Owner(l1.ID) != d {
		t.Fatal("synthetic J1/J3 must not steal the other leaf")
	}
	return old, neu, out
}

func TestACT2_ComponentsCoverValidatedPlan(t *testing.T) {
	l1 := mustZone(t, "aa")
	l3 := mustZone(t, "ba")
	zones := []tree.ZoneKey{l1, l3, mustZone(t, "ab"), mustZone(t, "bb")}
	old, neu, zones := independentLeafViews(t, &l1, &l3, zones)
	comps, err := BuildActivationComponents(old, neu, zones)
	if err != nil {
		t.Fatal(err)
	}
	assertComponentsCover(t, old, neu, comps, zones)
}

func assertComponentsCover(t *testing.T, old, neu *tree.View, comps []*ActivationComponent, zones []tree.ZoneKey) {
	t.Helper()
	plan := map[tree.ID]tree.ID{}
	for _, z := range zones {
		if old.Owner(z.ID) != neu.Owner(z.ID) {
			plan[z.ID] = neu.Owner(z.ID)
		}
	}
	seen := map[tree.ID]ComponentID{}
	for _, c := range comps {
		local := changedBy(old, c.NewView, zones)
		for _, z := range local {
			if prev, ok := seen[z.ID]; ok {
				t.Fatalf("zone %s in %s and %s", z.Location, prev, c.ID)
			}
			seen[z.ID] = c.ID
			if c.NewView.Owner(z.ID) != plan[z.ID] {
				t.Fatalf("component %s owner != plan", c.ID)
			}
		}
		for _, z := range zones {
			if c.NewView.Owner(z.ID) == old.Owner(z.ID) {
				continue
			}
			if c.NewView.Owner(z.ID) != neu.Owner(z.ID) {
				t.Fatalf("component %s moved %s off the validated plan", c.ID, z.Location)
			}
		}
	}
	if len(seen) != len(plan) {
		t.Fatalf("covered %d changed zones, plan has %d", len(seen), len(plan))
	}
}

func TestIndependentLeavesAreSeparateComponents(t *testing.T) {
	l1 := mustZone(t, "aa")
	l2 := mustZone(t, "ab")
	l3 := mustZone(t, "ba")
	l4 := mustZone(t, "bb")
	c1 := mustZone(t, "a")
	c2 := mustZone(t, "b")
	zones := []tree.ZoneKey{l1, l2, l3, l4, c1, c2}
	old, neu, zones := independentLeafViews(t, &l1, &l3, zones)
	comps, err := BuildActivationComponents(old, neu, zones)
	if err != nil {
		t.Fatal(err)
	}
	if len(comps) != 2 {
		t.Fatalf("independent leaves must be 2 components, got %d", len(comps))
	}
	var sawJ1, sawJ3 bool
	for _, c := range comps {
		if len(c.Delta.Add) != 1 {
			t.Fatalf("independent component %s has %d vnodes", c.ID, len(c.Delta.Add))
		}
		if componentHas(c, "J1") {
			sawJ1 = true
		}
		if componentHas(c, "J3") {
			sawJ3 = true
		}
		assertComponentAgreesWithView(t, old, neu, c, zones)
	}
	if !sawJ1 || !sawJ3 {
		t.Fatal("missing J1 or J3 component")
	}
}

func TestCoupledSpawnSharesComponent(t *testing.T) {
	l1 := mustZone(t, "aa")
	child := mustZone(t, "aaa")
	zones := []tree.ZoneKey{l1, child}

	old := tree.NewView(1)
	d, err := tree.DecodeName("0123456789ABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Add("D", d); err != nil {
		t.Fatal(err)
	}
	j2 := l1.ID
	j1 := flipLow(l1.ID)
	neu, err := tree.Extend(old,
		tree.VNodeDescriptor{ID: j1, Name: "J1"},
		tree.VNodeDescriptor{ID: j2, Name: "J2"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if neu.Owner(l1.ID) == old.Owner(l1.ID) {
		t.Fatal("expected nested spawn to recapture l1")
	}
	comps, err := BuildActivationComponents(old, neu, zones)
	if err != nil {
		t.Fatal(err)
	}
	if len(comps) != 1 {
		t.Fatalf("coupled J1/J2 must share one component, got %d", len(comps))
	}
	if !componentHas(comps[0], "J1") || !componentHas(comps[0], "J2") {
		t.Fatalf("component %s missing J1 or J2", comps[0].ID)
	}
	assertComponentAgreesWithView(t, old, neu, comps[0], zones)
}
