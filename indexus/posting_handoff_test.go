package indexus

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/mesh"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// Model an auxiliary index written by a second mesh write. Even a fresh
// inventory can expose the interval between primary and index ACKs. A version
// on a returned posting detects a stale hit; it cannot detect a missing hit.
func TestAuxiliaryPostingVisibilityAcrossHandoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const collection = "0000000000000001"
	root := VNodeDescriptor{ID: ID{255}}
	root.Name = BASE64.Encode(root.ID.Bytes())
	z, err := NewZoneKey(collection, "0")
	if err != nil {
		t.Fatal(err)
	}
	target := VNodeDescriptor{ID: z.ID}
	target.Name = BASE64.Encode(target.ID.Bytes())
	fixture := newReadPhaseFixture(t, []VNodeDescriptor{root, target}, false, false)
	primary := storage.Record{Collection: collection, Location: "0", ID: "doc", Metrics: []float64{1, 1}}
	posting := storage.Record{Collection: collection, Location: "1", ID: "doc", Metrics: []float64{1, 1}}
	write := func(reqID string, row storage.Record) (mesh.Response, error) {
		return fixture.nodes[0].MeshData(ctx, mesh.Request{Kind: "write", RequestID: reqID, Record: &row})
	}
	for _, r := range []storage.Record{primary, posting} {
		if _, err := write("initial-"+r.Location, r); err != nil {
			t.Fatal(err)
		}
	}
	// A single index row contains (category, document version). Only the newest
	// row in each zone should survive transfer, and both entry points must agree.
	check := func(primaryVersion, indexVersion float64) error {
		for i, svc := range fixture.services {
			inv, err := svc.QueryPrefixMetrics(ctx, collection, "@")
			if err != nil {
				return err
			}
			var seen [2]int
			for _, snap := range inv {
				for _, row := range snap.Records {
					if row.ID != "doc" || row.Deleted {
						continue
					}
					slot := 0
					want := primaryVersion
					if row.Location == "1" {
						slot = 1
						want = indexVersion
					} else if row.Location != "0" {
						return fmt.Errorf("unknown location %q", row.Location)
					}
					if !slices.Equal(row.Metrics, []float64{want, want}) {
						return fmt.Errorf("node %d zone %s: got %v want category/version %v", i, row.Location, row.Metrics, want)
					}
					seen[slot]++
				}
			}
			if seen != [2]int{1, 1} {
				return fmt.Errorf("node %d saw %v primary/index rows", i, seen)
			}
		}
		return nil
	}
	if err := check(1, 1); err != nil {
		t.Fatal(err)
	}
	var probed bool
	fixture.net.before = func(req mesh.Request) error {
		if req.Kind != "install" || probed {
			return nil
		}
		probed = true
		primary.Metrics = []float64{2, 2}
		ack, err := write("move-primary", primary)
		if err != nil {
			return err
		}
		if ack.AppliedBy != fixture.services[1].Self {
			return fmt.Errorf("target did not acknowledge flipped primary")
		}
		if err := check(2, 1); err != nil {
			return err
		}
		posting.Metrics = []float64{2, 2}
		if _, err := write("publish-posting", posting); err != nil {
			return err
		}
		return check(2, 2)
	}
	if err := fixture.services[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	if !probed {
		t.Fatal("regional install cut not reached")
	}
	if err := check(2, 2); err != nil {
		t.Fatal(err)
	}
}
