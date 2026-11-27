package summary

import (
	"math/rand"
	"testing"

	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/wire"
)

func TestSummaryChildrenMatchMapOracleThroughReorderedVersions(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	rng := rand.New(rand.NewSource(7631))
	zone := map[string]summaryContribution{}
	reference := map[string]summaryContribution{}
	for step := 0; step < 2000; step++ {
		name := "a" + string(alphabet[rng.Intn(len(alphabet))])
		incoming := summaryContribution{epoch: uint64(rng.Intn(3)), generation: uint64(rng.Intn(4)), revision: uint64(rng.Intn(8)), holder: causal.Endpoint{Owner: causal.Owner{VNode: string(alphabet[rng.Intn(3)])}}, value: wire.NewAbelian(rng.Intn(10), []float64{float64(rng.Intn(100)), float64(rng.Intn(100))})}
		old, exists := reference[name]
		newer := !exists || old.holder != incoming.holder || incoming.epoch > old.epoch || incoming.epoch == old.epoch && (incoming.generation > old.generation || incoming.generation == old.generation && incoming.revision > old.revision)
		if got := acceptChild(zone, name, incoming); got != newer {
			t.Fatalf("step %d: accepted=%v want=%v", step, got, newer)
		}
		if newer {
			reference[name] = incoming
		}
	}
	if len(zone) != 64 {
		t.Fatalf("only exercised %d children", len(zone))
	}
}

// acceptChild is meshAcceptChild's ordering on one child: a contribution
// replaces the one held only when it is newer.
func acceptChild(zone map[string]summaryContribution, name string, in summaryContribution) bool {
	if old, ok := zone[name]; ok && !newerContribution(old, in) {
		return false
	}
	zone[name] = in
	return true
}

func mustSummaryChild(t *testing.T, zone map[string]summaryContribution, name string) summaryContribution {
	t.Helper()
	in, ok := zone[name]
	if !ok {
		t.Fatal("missing child")
	}
	return in
}
