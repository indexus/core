package convergent

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

// withFault runs f with a deliberately wrong rule and restores the default.
func withFault(t *testing.T, f Fault, run func()) {
	t.Helper()
	injected = f
	defer func() { injected = NoFault }()
	run()
}

// randomVersions draws versions over few IDs, with equal Ver and both
// deleted and live versions, so every merge rule is exercised.
func randomVersions(rng *rand.Rand, n int) []Version {
	out := make([]Version, n)
	for i := range out {
		inc := Incarnation(fmt.Sprintf("w%d", rng.IntN(3)))
		out[i] = NewVersion(fmt.Sprintf("id%d", rng.IntN(4)), uint64(1+rng.IntN(3)), rng.IntN(4) == 0, int64(rng.IntN(5)), Dot{inc, uint64(i + 1)})
	}
	return out
}

func mergedInOrder(vs []Version, order []int) *State {
	s := NewState()
	for _, i := range order {
		s.Apply(vs[i])
	}
	return s
}

// U1: merge is associative, commutative and idempotent: any order and any
// duplication of the same versions gives the same records.
func u1(seeds int) error {
	for seed := range seeds {
		rng := rand.New(rand.NewPCG(uint64(seed), 1))
		vs := randomVersions(rng, 12)
		ref := mergedInOrder(vs, rng.Perm(len(vs)))
		for range 5 {
			order := rng.Perm(len(vs))
			order = append(order, order[:rng.IntN(len(order))]...) // duplicates
			if got := mergedInOrder(vs, order); !slices.Equal(recordList(got), recordList(ref)) {
				return fmt.Errorf("seed %d: order changes the result", seed)
			}
		}
		// State-to-state merge in either direction agrees.
		a, b := mergedInOrder(vs, []int{0, 1, 2, 3, 4, 5}), mergedInOrder(vs, []int{6, 7, 8, 9, 10, 11})
		ab, ba := a.Clone(), b.Clone()
		ab.Merge(b.DeltaSince(NewVector()))
		ba.Merge(a.DeltaSince(NewVector()))
		if !ab.SameData(ba) {
			return fmt.Errorf("seed %d: state merge is not commutative", seed)
		}
	}
	return nil
}

func recordList(s *State) []Version {
	out := []Version{}
	for _, id := range []string{"id0", "id1", "id2", "id3"} {
		if v, ok := s.Records[id]; ok {
			out = append(out, v)
		}
	}
	return out
}

func TestU1MergeLaws(t *testing.T) {
	if err := u1(1000); err != nil {
		t.Fatal(err)
	}
}

func TestU1DetectsMissingDigestRule(t *testing.T) {
	withFault(t, NoDigestRule, func() {
		if u1(1000) == nil {
			t.Fatal("U1 passed without merge rule 3")
		}
	})
}

// U2: at equal Ver, a delete wins against a concurrent write, in both orders.
func u2() error {
	// Many values: without rule 2 the digest would pick a winner, and some
	// writes have a smaller digest than the delete.
	for value := range int64(32) {
		if err := u2pair(value); err != nil {
			return err
		}
	}
	return nil
}

func u2pair(value int64) error {
	del := NewVersion("k", 2, true, 0, Dot{"a", 1})
	put := NewVersion("k", 2, false, value, Dot{"b", 1})
	for _, order := range [][]Version{{del, put}, {put, del}} {
		s := NewState()
		for _, v := range order {
			s.Apply(v)
		}
		if !s.Records["k"].Deleted {
			return fmt.Errorf("write won over a concurrent delete")
		}
		if s.Live != 0 || s.Sum != 0 {
			return fmt.Errorf("aggregates count a deleted record: live %d sum %d", s.Live, s.Sum)
		}
	}
	return nil
}

func TestU2DeleteWins(t *testing.T) {
	if err := u2(); err != nil {
		t.Fatal(err)
	}
}

func TestU2DetectsMissingDeleteRule(t *testing.T) {
	withFault(t, NoDeleteWins, func() {
		if u2() == nil {
			t.Fatal("U2 passed without merge rule 2")
		}
	})
}

// U3: a dot after a gap never makes the gap covered.
func u3() error {
	v := NewVector()
	v.Add(Dot{"a", 1})
	v.Add(Dot{"a", 3})
	if v.Covers(Dot{"a", 2}) {
		return fmt.Errorf("gap at counter 2 is covered")
	}
	if !v.Covers(Dot{"a", 3}) {
		return fmt.Errorf("received counter 3 is not covered")
	}
	v.Add(Dot{"a", 2})
	if v.Base["a"] != 3 || len(v.Extra) != 0 {
		return fmt.Errorf("filled gap not compacted: base %d extra %d", v.Base["a"], len(v.Extra))
	}
	// Merge keeps gaps too.
	w := NewVector()
	w.Add(Dot{"b", 2})
	x := NewVector()
	x.Merge(w)
	if x.Covers(Dot{"b", 1}) {
		return fmt.Errorf("merge covers a gap")
	}
	return nil
}

func TestU3VectorGaps(t *testing.T) {
	if err := u3(); err != nil {
		t.Fatal(err)
	}
}

func TestU3DetectsMaxVector(t *testing.T) {
	withFault(t, VectorTakesMax, func() {
		if u3() == nil {
			t.Fatal("U3 passed with a max-counter vector")
		}
	})
}

// U4: R(z) is pure, has distinct machines, starts with owner(z), and one
// join replaces at most one member.
func u4(seeds int) error {
	for seed := range seeds {
		rng := rand.New(rand.NewPCG(uint64(seed), 4))
		var active []tree.VNodeDescriptor
		machines := map[tree.VNodeID]string{}
		randomID := func() tree.ID {
			var id tree.ID
			for i := range id {
				id[i] = byte(rng.IntN(256))
			}
			return id
		}
		for i := range 30 {
			id := randomID()
			active = append(active, tree.VNodeDescriptor{ID: id, Name: fmt.Sprint(i)})
			machines[id] = fmt.Sprintf("m%d", rng.IntN(8))
		}
		machine := func(id tree.VNodeID) string { return machines[id] }
		for range 20 {
			zone := randomID()
			r := modelReplicas(zone, active, machine, ReplicaCount)
			if !slices.Equal(r, modelReplicas(zone, active, machine, ReplicaCount)) {
				return fmt.Errorf("seed %d: not pure", seed)
			}
			owner := active[0].ID
			for _, d := range active[1:] {
				if tree.XORCompare(zone, d.ID, owner) < 0 {
					owner = d.ID
				}
			}
			if r[0] != owner {
				return fmt.Errorf("seed %d: R(z)[0] is not owner(z)", seed)
			}
			seen := map[string]bool{}
			for _, id := range r {
				if seen[machine(id)] {
					return fmt.Errorf("seed %d: two members on machine %s", seed, machine(id))
				}
				seen[machine(id)] = true
			}
			joined := randomID()
			machines[joined] = fmt.Sprintf("m%d", rng.IntN(10))
			after := modelReplicas(zone, append(slices.Clone(active), tree.VNodeDescriptor{ID: joined}), machine, ReplicaCount)
			if removed(r, after) > 1 || removed(after, r) > 1 {
				return fmt.Errorf("seed %d: one join changed more than one member", seed)
			}
		}
	}
	return nil
}

func removed(from, to []tree.VNodeID) int {
	n := 0
	for _, id := range from {
		if !slices.Contains(to, id) {
			n++
		}
	}
	return n
}

func TestU4ReplicaSet(t *testing.T) {
	if err := u4(200); err != nil {
		t.Fatal(err)
	}
}

func TestU4DetectsMissingMachineCheck(t *testing.T) {
	withFault(t, NoMachineCheck, func() {
		if u4(200) == nil {
			t.Fatal("U4 passed without the machine check")
		}
	})
}

// U5: a retry on another replica returns the first result.
func u5() error {
	members := []Member{"o", "r1"}
	cfg := DefaultConfig()
	o := Open("o", "o#1", members, cfg, nil, 0)
	r := Open("r1", "r1#1", members, cfg, nil, 0)
	if err := o.Write(1, "req", "k", false, 5); err != nil {
		return err
	}
	out, _ := o.TakeOutbox()
	for _, m := range out {
		r.Receive(2, m)
	}
	r.Tick(2 + cfg.Suspect) // owner silent: fallback
	if !r.InFallback() {
		return fmt.Errorf("replica did not enter fallback")
	}
	if err := r.Write(3+cfg.Suspect, "req", "k", false, 5); err != nil {
		return err
	}
	_, acks := r.TakeOutbox()
	first := o.State.Receipts["req"].Result
	if len(acks) != 1 || acks[0].Result != first {
		return fmt.Errorf("retry on the replica gave %+v, want the first result", acks)
	}
	if r.State.Records["k"].Ver != 1 {
		return fmt.Errorf("retry created a second version")
	}
	return nil
}

func TestU5RetryOnReplica(t *testing.T) {
	if err := u5(); err != nil {
		t.Fatal(err)
	}
}

func TestU5DetectsUnreplicatedReceipts(t *testing.T) {
	withFault(t, NoReceiptReplication, func() {
		if u5() == nil {
			t.Fatal("U5 passed without receipt replication")
		}
	})
}
