package node

import (
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestCausalAggregatesKeepCoincidentAncestorsDistinctAfterJoin(t *testing.T) {
	source := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	collection := "0000000000000001"
	locations := []string{"0", "00", "000"}
	for i, location := range locations {
		item := &indexus.Item{Collection: collection, Location: location, Id: "same-row", RequestID: "put:" + location, Metrics: []float64{float64(i + 1)}}
		if err := source.node.New(item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	zone, err := indexus.NewZoneKey(collection, locations[0])
	if err != nil {
		t.Fatal(err)
	}
	target := startDataNode(t, indexus.BASE64.Encode(zone.ID.Bytes()), source)
	nodes := []*runningDataNode{source, target}
	for range 2 {
		for _, n := range nodes {
			if err := n.node.Update(); err != nil {
				t.Fatal(err)
			}
		}
	}
	paths := []string{"@", "0", "00", "000"}
	counts := map[string]int{"@": 3, "0": 3, "00": 2, "000": 1}
	sums := map[string]float64{"@": 6, "0": 6, "00": 5, "000": 3}
	seen := map[string]int{}
	for _, n := range nodes {
		got, err := n.node.GetAggregates(collection, paths)
		if err != nil {
			t.Fatal(err)
		}
		for path, value := range got {
			seen[path]++
			if value.Count() != counts[path] || value.Metric(0) != sums[path] {
				t.Fatalf("%s: count=%d sum=%v", path, value.Count(), value.Metric(0))
			}
		}
	}
	for _, path := range paths {
		if seen[path] != 1 {
			t.Fatalf("%s answered by %d owners", path, seen[path])
		}
	}
}
