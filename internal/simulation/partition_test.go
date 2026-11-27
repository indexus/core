package mockup

import (
	"context"
	"fmt"
	"testing"

	indexus "github.com/indexus/go-indexus-core/indexus"
)

// A split brain is the general case of duplicate ownership: both halves elect
// an owner for the same zone and both accept writes, so on remerge two nodes
// claim the zone with diverged item sets. The protocol has no reconciliation
// subsystem and needs none — the duplicate is a placement question, and R3
// answers it. See protocol.md §4/Q and the crash matrix row "Partition heal".
//
// What this pins, which no unit test can: the two halves keep serving during
// the cut, and the remerge loses nothing and duplicates nothing.
func TestConvergence_PartitionRemergeKeepsTheUnion(t *testing.T) {
	ResetNetwork()
	const seeded, perSide = 120, 40
	collection, err := indexus.BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}
	n1 := newConvergenceNode(t, 25)
	n2 := newConvergenceNode(t, 25, n1)
	n3 := newConvergenceNode(t, 25, n1)
	n4 := newConvergenceNode(t, 25, n1)
	nodes := []*indexus.Node{n1, n2, n3, n4}
	for i := 0; i < seeded; i++ {
		mustInsert(t, n1, &indexus.Item{Collection: collection, Location: twoCharLocation(i), Id: fmt.Sprintf("seed-%d", i), Metrics: []float64{1, 2, 3, 4, 5}})
	}
	converge(t, nodes, 5, 2, 60)
	network.Partition([]*indexus.Node{n1, n2}, []*indexus.Node{n3, n4})
	type attempt struct {
		target   *indexus.Node
		item     *indexus.Item
		accepted bool
	}
	var attempts []attempt
	accepted := seeded
	refused := 0
	for i := 0; i < perSide; i++ {
		for side, target := range []*indexus.Node{n1, n3} {
			id := fmt.Sprintf("partition-%d-%d", side, i)
			item := &indexus.Item{Collection: collection, Location: twoCharLocation(i), Id: id, RequestID: id, Metrics: []float64{1, 2, 3, 4, 5}}
			err := target.New(item, "@", nil)
			attempts = append(attempts, attempt{target, item, err == nil})
			if err == nil {
				accepted++
			} else {
				refused++
			}
		}
	}
	if refused == 0 {
		t.Fatal("partition admitted writes without access to authority/owner")
	}
	if total := totalCount(t, nodes); total != accepted {
		t.Fatalf("partition changed accepted union: got %d want %d", total, accepted)
	}
	network.Heal()
	converge(t, nodes, 6, 3, 80)
	if total := totalCount(t, nodes); total != accepted {
		t.Fatalf("heal lost accepted rows or replayed refusals: got %d want %d", total, accepted)
	}
	// The client retries the same identity after connectivity recovers. Both
	// earlier ACKs and earlier refusals must yield exactly one logical record.
	for _, attempt := range attempts {
		if attempt.accepted && !readableFrom(t, n2, collection, attempt.item.Location, attempt.item.Id) {
			t.Fatalf("accepted item %s lost", attempt.item.Id)
		}
		if err := attempt.target.New(attempt.item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	if total := totalCount(t, nodes); total != seeded+2*perSide {
		t.Fatalf("retry union: %d", total)
	}
	assertOwnershipDisjointAndComplete(t, nodes)
	for _, attempt := range attempts {
		if !readableFrom(t, n2, collection, attempt.item.Location, attempt.item.Id) {
			t.Fatalf("item %s unreadable after recovery", attempt.item.Id)
		}
	}
}

// readableFrom reads one record back through the mesh from one node. /set
// answers aggregates, so the record read is the one that names items.
func readableFrom(t *testing.T, from *indexus.Node, collection, location, id string) bool {
	t.Helper()
	items, err := from.GetRecordsContext(context.Background(), collection, location, 1000)
	if err != nil {
		return false
	}
	for _, item := range items {
		if item.Location == location && item.Id == id {
			return true
		}
	}
	return false
}

// A cut link is not an empty answer. An unreachable owner must read as a miss,
// so the reader keeps looking and the zone is not remembered as empty — the
// mistake that would otherwise propagate into a parent stub and stick.
func TestPartitionedReadIsAMissNotAnEmptyZone(t *testing.T) {
	ResetNetwork()

	const (
		delegation  = 30
		collections = 20
		perColl     = 12
	)
	names := make([]string, collections)
	for i := range names {
		name, err := indexus.BASE64.RandomName()
		if err != nil {
			t.Fatalf("RandomName: %v", err)
		}
		names[i] = name
	}
	zone, err := indexus.ZoneID(names[0], indexus.BASE64.CharAt(0))
	if err != nil {
		t.Fatal(err)
	}
	n1 := newConvergenceNode(t, delegation)
	n2 := createConvergenceNode(t, indexus.BASE64.Encode(zone.Bytes()), delegation, true, n1)
	nodes := []*indexus.Node{n1, n2}
	for _, name := range names {
		for j := 0; j < perColl; j++ {
			loc := indexus.BASE64.CharAt(j)
			if err := n1.New(&indexus.Item{
				Collection: name,
				Location:   loc,
				Id:         fmt.Sprintf("id-%d", j),
				Metrics:    []float64{1},
			}, "@", nil); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
	converge(t, nodes, 5, 2, 60)

	// Any zone n2 owns and n1 can read is a link worth cutting.
	collection, location, before := remoteReadableZone(t, n1, n2)
	if collection == "" {
		t.Fatal("exact seeded-zone owner has no readable zone")
	}

	network.Partition([]*indexus.Node{n1}, []*indexus.Node{n2})
	_, after, err := n1.Get(collection, location, true, nil, false)
	if err == nil && after != nil && after.Count() == 0 {
		t.Fatal("an unreachable owner was reported as an empty zone")
	}

	network.Heal()
	_, healed, err := n1.Get(collection, location, true, nil, false)
	if err != nil || healed == nil || healed.Count() != before {
		t.Fatalf("the read did not recover after the heal: set=%v err=%v want count=%d", healed, err, before)
	}
}

// remoteReadableZone finds a zone owned by owner that reader resolves to a
// non-empty set, and returns it with the count read before any cut.
func remoteReadableZone(t *testing.T, reader, owner *indexus.Node) (collection, location string, count int) {
	t.Helper()
	own, err := owner.Ownership()
	if err != nil {
		t.Fatalf("Ownership: %v", err)
	}
	for coll, locs := range own {
		for loc := range locs {
			_, set, err := reader.Get(coll, loc, true, nil, false)
			if err != nil || set == nil || set.Count() == 0 {
				continue
			}
			return coll, loc, set.Count()
		}
	}
	return "", "", 0
}

// twoCharLocation spreads writes over the alphabet so zones actually split.
func twoCharLocation(i int) string {
	base := indexus.BASE64
	return base.CharAt(i%base.Length()) + base.CharAt((i/base.Length())%base.Length())
}
