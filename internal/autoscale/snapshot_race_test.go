package autoscale_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

// A snapshot is read (monitoring encodes it) while later ticks keep writing
// the pressure signals: it must not share its pressure map with them. Under
// -race this test fails when Snapshot returns the live map.
func TestSnapshotDoesNotShareThePressureMap(t *testing.T) {
	a := autoscale.New(autoscale.Config{Enabled: true, Role: "bootstrap", Window: time.Minute, Cooldown: time.Hour, PressureHold: time.Hour, RiseHold: time.Hour, MemLimitPct: 99, CPULimitPct: 99, MemRisePct: 1000, ItemsLimit: 1, WritesLimit: 1})
	in := autoscale.Input{SelfName: "self", OwnedItems: 10, WriteRate: 10}
	a.Tick(in, func(autoscale.Request) error { return nil })
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			a.Tick(in, func(autoscale.Request) error { return nil })
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			if _, err := json.Marshal(a.Snapshot()); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	first := a.Snapshot()["pressure"].(map[string]any)
	first["probe"] = true
	if _, shared := a.Snapshot()["pressure"].(map[string]any)["probe"]; shared {
		t.Fatal("two snapshots share one pressure map")
	}
}
