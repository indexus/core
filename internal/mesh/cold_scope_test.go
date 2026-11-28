package mesh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/protocol"
	"github.com/indexus/go-indexus-core/internal/tree"
)

// An announcement changes placement, never installed data. Inject it once
// before the first piece and again in a separate run just before retirement.
func TestColdFoldOnlyDependsOnPairTopology(t *testing.T) {
	for _, phase := range []string{KindColdPin, "directory"} {
		for _, tc := range []struct {
			name string
			id   tree.ID
			keep bool
		}{
			{"other branch splits", tree.ID{129}, true},
			{"different sibling", tree.ID{0, 2}, false},
			{"same sibling but different cell", tree.ID{64}, false},
		} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				net, nodes, rows := coldPair(t, ctx, 8)
				source, target := nodes[1], nodes[0]
				announce := func(name string, id tree.ID) error {
					record := causal.Member{VNode: tree.VNodeDescriptor{Name: name, ID: id}, Endpoint: causal.Endpoint{Owner: causal.Owner{VNode: name, Storage: name + "-disk"}, Host: name}}
					return source.Store.MergeMembership(causal.MembershipDelta{Members: []causal.Member{record}})
				}
				if err := announce("far", tree.ID{128}); err != nil {
					t.Fatal(err)
				}
				changed := false
				net.setAfter(func(r Request) error {
					if changed || r.Kind != phase {
						return nil
					}
					changed = true
					return announce("new", tc.id)
				})
				err := source.Compact(ctx, ColdConfig{})
				if !changed {
					t.Fatalf("missed injection: %v", err)
				}
				if tc.keep && err != nil || !tc.keep && !errors.Is(err, protocol.ErrColdPlanPreempted) {
					t.Fatalf("keep=%v err=%v", tc.keep, err)
				}
				drain, err := source.Store.ColdStatus()
				if err != nil || (tc.keep && (drain.Drain == nil || !drain.Drain.Witnessed)) || (!tc.keep && drain.Drain != nil) {
					t.Fatalf("donor %+v: %v", drain, err)
				}
				pin, err := target.Store.ColdStatus()
				if err != nil || pin.Pin != nil {
					t.Fatalf("survivor %+v: %v", pin, err)
				}
				assertColdRows(t, ctx, nodes, rows, false)
			})
		}
	}
}
