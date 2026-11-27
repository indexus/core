package protocol

import (
	"bytes"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// planZones builds n one-record zones of a donor, as a write load of unique
// locations produces them: real zone keys in a few collections, with some
// ancestors among them.
func planZones(tb testing.TB, n int) (HotInput, []ZoneStats) {
	tb.Helper()
	rng := rand.New(rand.NewSource(7))
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	zones := make([]ZoneStats, 0, n)
	for i := 0; len(zones) < n; i++ {
		location := make([]byte, 3+rng.Intn(9))
		for j := range location {
			location[j] = letters[rng.Intn(len(letters))]
		}
		key, err := tree.NewZoneKey(tree.EncodeName(tree.ID{byte(i % 16), 1}), string(location))
		if err != nil {
			tb.Fatal(err)
		}
		zones = append(zones, ZoneStats{Zone: key, Items: 1, Bytes: 256, WriteFast: rng.Float64(), ETA: time.Millisecond})
		if i%9 == 0 && len(location) > 4 { // an ancestor that also holds a record
			parent, _ := tree.NewZoneKey(key.Collection, string(location[:len(location)-2]))
			zones = append(zones, ZoneStats{Zone: parent, Items: 1, Bytes: 256, ETA: time.Millisecond})
		}
	}
	zones = zones[:n]
	view := tree.NewView(1)
	donor := tree.ID{0x80}
	for i, id := range []tree.ID{donor, {0x01}, {0x40}, {0xC0}} {
		if err := view.Add(fmt.Sprintf("v%d", i), id); err != nil {
			tb.Fatal(err)
		}
	}
	return HotInput{Donor: donor, Current: view, Zones: zones, ItemPressure: float64(n), TargetItems: float64(n) / 2, DrainRate: 100}, zones
}

func BenchmarkHottestReliefLeaf(b *testing.B) {
	for _, n := range []int{2500, 10000, 40000} {
		_, zones := planZones(b, n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for b.Loop() {
				hottestReliefLeaf(zones, 100, false)
			}
		})
	}
}

func BenchmarkPlanRegionalAdd(b *testing.B) {
	for _, n := range []int{2500, 10000, 40000} {
		in, _ := planZones(b, n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for b.Loop() {
				_, _ = PlanRegionalAdd(in)
			}
		})
	}
}

func BenchmarkPlanHotSplit(b *testing.B) {
	for _, n := range []int{2500, 10000, 40000} {
		in, _ := planZones(b, n)
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for b.Loop() {
				_, _ = PlanHotSplit(in)
			}
		})
	}
}

// innerZones must agree with the definition it replaces: a zone is a leaf
// when it covers no other zone of the list (tree.Covers), roots, empty
// locations, duplicates and several collections included.
func TestInnerZonesAgreeWithCovers(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	locations := []string{"", tree.BASE64.Root(), "a", "ab", "abc", "abcd", "b", "ba", "c", "ca", "cab"}
	for round := range 2000 {
		zones := make([]ZoneStats, rng.Intn(9))
		for i := range zones {
			zones[i].Zone = tree.ZoneKey{Collection: fmt.Sprint("c", rng.Intn(2)), Location: locations[rng.Intn(len(locations))]}
		}
		in := innerZones(zones)
		for _, z := range zones {
			covers := false
			for _, other := range zones {
				covers = covers || z.Zone.IsAncestorOf(other.Zone)
			}
			if got := in.has(z.Zone); got != covers {
				t.Fatalf("round %d: %q/%q inner=%v, covers another zone=%v, among %v", round, z.Zone.Collection, z.Zone.Location, got, covers, zones)
			}
		}
	}
	_, real := planZones(t, 3000)
	in := innerZones(real)
	inner := 0
	for _, z := range real {
		covers := false
		for _, other := range real {
			covers = covers || z.Zone.IsAncestorOf(other.Zone)
		}
		if in.has(z.Zone) != covers {
			t.Fatalf("%q/%q: inner=%v, covers=%v", z.Zone.Collection, z.Zone.Location, in.has(z.Zone), covers)
		}
		if covers {
			inner++
		}
	}
	if inner == 0 {
		t.Fatal("the fixture holds no ancestor: the test would prove nothing")
	}
}

// Asking for the hottest leaf costs a time proportional to the zones: four
// times the zones must not cost sixteen times the work. The bound is loose
// (10x) to stay deterministic on a busy machine; the comparison of every zone
// with every other measured 16x.
func TestHottestReliefLeafIsLinearInTheZones(t *testing.T) {
	cost := func(n int) time.Duration {
		_, zones := planZones(t, n)
		best := time.Duration(1 << 62)
		for range 5 {
			start := time.Now()
			hottestReliefLeaf(zones, 100, false)
			best = min(best, time.Since(start))
		}
		return best
	}
	small, large := cost(5000), cost(20000)
	if large > 10*small {
		t.Fatalf("5 000 zones: %v, 20 000 zones: %v (more than 10x)", small, large)
	}
}

// An entry of a sample stands for several zones (PLAN-SAMPLE-1): it is
// atomic only if one of them alone exceeds the machine, not their sum.
func TestAtomicZoneJudgesOneZoneOfASample(t *testing.T) {
	in := HotInput{AtomicDisk: 1000, AtomicWrite: 100}
	sampled := ZoneStats{Bytes: 80 * 500, WriteFast: 80 * 50, Stands: 80}
	if atomicZone(in, sampled) {
		t.Fatal("80 zones of 500 bytes were taken for one zone over 1 000 bytes")
	}
	if !atomicZone(in, ZoneStats{Bytes: 80 * 2000, Stands: 80}) || !atomicZone(in, ZoneStats{Bytes: 2000}) {
		t.Fatal("a zone over the machine's disk is not atomic")
	}
	if !atomicZone(in, ZoneStats{Bytes: 1, WriteFast: 80 * 200, Stands: 80}) {
		t.Fatal("a zone over the machine's write rate is not atomic")
	}
}

// PLAN-SAMPLE-1: a decision taken from 512 zones at even ranks of the id
// order, each standing for its share, splits the vnode where the decision
// taken from all its zones does.
func TestDecisionFromASampleSplitsWhereTheFullOneDoes(t *testing.T) {
	for _, n := range []int{5000, 20000} {
		in, zones := planZones(t, n)
		slices.SortFunc(zones, func(a, b ZoneStats) int { return bytes.Compare(a.Zone.ID[:], b.Zone.ID[:]) })
		const picked = 512
		stands := int64(n / picked)
		sample := make([]ZoneStats, 0, picked)
		for i := range picked {
			z := zones[(2*i+1)*n/(2*picked)]
			z.Items, z.Bytes, z.WriteFast, z.Stands = z.Items*stands, z.Bytes*stands, z.WriteFast*float64(stands), stands
			sample = append(sample, z)
		}
		decide := func(zones []ZoneStats) Decision {
			return EvaluateMachine(MachineState{MachineID: "m", View: in.Current, Donor: in.Donor, Zones: zones, ItemLimit: int64(n / 8)})
		}
		full, sampled := decide(zones), decide(sample)
		// The new vnode's id is derived from one zone's id, so the two ids
		// differ; what must agree is the share of the donor's zones it takes.
		share := func(d Decision) float64 {
			if d.Kind != KindHotSplit && d.Kind != KindRegionalAdd || len(d.Plan.Add) != 1 {
				t.Fatalf("%d zones: decision %v (%v), want one added vnode", n, d.Kind, d.Err)
			}
			next, err := tree.Extend(in.Current, d.Plan.Add[0])
			if err != nil {
				t.Fatal(err)
			}
			owned, moved := 0, 0
			for _, z := range zones {
				if in.Current.Owner(z.ID()) != in.Donor {
					continue
				}
				owned++
				if next.Owner(z.ID()) != in.Donor {
					moved++
				}
			}
			return float64(moved) / float64(owned)
		}
		exact, estimated := share(full), share(sampled)
		t.Logf("%d zones: the full decision moves %.1f %% of the donor's zones, the sampled one %.1f %%", n, 100*exact, 100*estimated)
		if estimated < exact-0.10 || estimated > exact+0.10 || estimated <= 0 || estimated >= 1 {
			t.Fatalf("%d zones: shares %.3f and %.3f differ by more than 10 points", n, exact, estimated)
		}
	}
}
