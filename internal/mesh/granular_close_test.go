package mesh

import (
	"context"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/storage"
)

// Keep producing a sibling after each install, beyond the two discovery
// passes. Closing the region must not turn those later warm copies into a
// regional write freeze.
func TestLateZoneWavesCopyBeforeRegionalFence(t *testing.T) {
	t.Parallel()
	net, source, target := streamFixture(t, 1)
	waves := 0
	net.setBefore(func(request Request) error {
		if request.Kind != "preload" || len(request.Transfer.Snapshots) == 0 {
			return nil
		}
		state, err := source.Store.CausalState()
		if err != nil {
			return err
		}
		if state.Transfers[request.Transfer.Cession.ID].Phase >= causal.TransferSealed {
			return fmt.Errorf("late lot copied after regional fence")
		}
		return nil
	})
	net.setAfter(func(request Request) error {
		if request.Kind != "install-zone" || waves == 5 {
			return nil
		}
		waves++
		r := storage.Record{
			Collection: "collection-00000", Location: "a" + string(rune('a'+waves)),
			ID: fmt.Sprintf("late-%d", waves), Metrics: []float64{1},
		}
		_, err := source.Store.Write(r.ID, r)
		return err
	})
	if err := target.Join(context.Background()); err != nil {
		t.Fatal(err)
	}
	if waves != 5 {
		t.Fatalf("only %d new-zone waves exercised", waves)
	}
	rows, err := target.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, snap := range rows {
		for _, r := range snap.Records {
			seen[r.ID]++
		}
	}
	for i := 1; i <= waves; i++ {
		if seen[fmt.Sprintf("late-%d", i)] != 1 {
			t.Fatalf("late ACK %d missing or duplicated", i)
		}
	}
}

// Even if new zones keep being born on every single install, the warm drain cap
// eventually fences the region so consolidation and residual GC cannot starve.
func TestUnboundedZoneCreationClosesAfterWarmDrainCap(t *testing.T) {
	t.Parallel()
	net, source, target := streamFixture(t, 1)
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	var waves int
	var acked []string
	net.setAfter(func(request Request) error {
		if request.Kind != "install-zone" {
			return nil
		}
		waves++
		loc := string([]byte{'b', alphabet[waves%64], alphabet[(waves/64)%64]})
		r := storage.Record{Collection: "collection-00000", Location: loc, ID: fmt.Sprintf("unbounded-%d", waves), Metrics: []float64{1}}
		if _, err := source.Store.Write(r.ID, r); err == nil {
			acked = append(acked, r.ID)
		}
		return nil
	})
	if err := target.Join(context.Background()); err != nil {
		t.Fatal(err)
	}
	if waves <= 6 {
		t.Fatalf("only %d waves exercised, want > 6 to hit warm drain cap", waves)
	}
	rows, err := target.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, snap := range rows {
		for _, r := range snap.Records {
			seen[r.ID]++
		}
	}
	for _, id := range acked {
		if seen[id] != 1 {
			t.Fatalf("ACK %s present %d times", id, seen[id])
		}
	}
}
