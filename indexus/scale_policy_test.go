package indexus

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

func mustZoneStats(t testing.TB, n *Node) []ZoneStats {
	t.Helper()
	stats, err := n.zoneStats()
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func TestScalePolicyItemsProduceValidatedPlanWithoutDiskPressure(t *testing.T) {
	t.Setenv("INDEXUS_ITEMS_LIMIT", "8")
	d := VNodeDescriptor{ID: ID{16}}
	d.Name = EncodeName(d.ID)
	f := newReadPhaseFixture(t, []VNodeDescriptor{d}, false, false)
	n := f.nodes[0]
	for _, loc := range []string{"0", "4", "G", "K", "W", "a", "q", "y"} {
		for i := 0; i < 2; i++ {
			if err := n.New(&Item{Collection: "0000000000000001", Location: loc, Id: fmt.Sprintf("%s-%d", loc, i), RequestID: fmt.Sprintf("%s-%d", loc, i), Metrics: []float64{1}}, "@", nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	n.EnableAutoscale(autoscale.Config{Enabled: true, ItemsLimit: 8})
	st, err := n.planningState("items")
	if err != nil {
		t.Fatal(err)
	}
	if st.Pressure.Disk >= .9*float64(st.Capacity) || st.WriteCap != 0 {
		t.Fatal("fixture unexpectedly needs physical scale")
	}
	// CPU/memory demand uses this same geometric policy even with the soft
	// item trigger disabled, without fabricating physical disk pressure.
	n.EnableAutoscale(autoscale.Config{Enabled: true})
	for _, reason := range []string{"cpu", "memory"} {
		process, err := n.planningState(reason)
		if err != nil {
			t.Fatal(err)
		}
		decision := EvaluateMachine(process)
		if decision.Kind != KindHotSplit && decision.Kind != KindRegionalAdd {
			t.Fatalf("%s demand: %+v", reason, decision)
		}
		if _, err := ValidatePlan(process.View, decision.Plan, process.Zones); err != nil {
			t.Fatalf("%s demand: %v", reason, err)
		}
		if process.Pressure.Disk != st.Pressure.Disk || process.Capacity != st.Capacity {
			t.Fatal("process demand changed physical byte accounting")
		}
	}
	n.EnableAutoscale(autoscale.Config{Enabled: true, ItemsLimit: 8})
	before := st.View.Digest
	names := n.preferNearsForDemand("items", 1, n.Name())
	if len(names) == 0 {
		t.Fatal("item pressure produced no validated split")
	}
	if n.topologyView().Digest != before {
		t.Fatal("requesting peers changed membership")
	}
	plan := EvaluateMachine(st)
	if _, err := ValidatePlan(st.View, plan.Plan, st.Zones); err != nil {
		t.Fatal(err)
	}
	if n.autoscale.ClientWritesBlocked() {
		t.Fatal("reserved scale intent blocks admission")
	}
}

func TestScalePolicyProcessPressureDoesNotInventByteLoad(t *testing.T) {
	t.Parallel()
	n := ownedNode(t)
	n.EnableAutoscale(autoscale.Config{Enabled: true})
	normal, err := n.planningState("")
	if err != nil {
		t.Fatal(err)
	}
	process, err := n.planningState("cpu")
	if err != nil {
		t.Fatal(err)
	}
	if !process.Rebalance || normal.Rebalance || process.Pressure.Disk != normal.Pressure.Disk || process.Capacity != normal.Capacity {
		t.Fatal("process pressure corrupted byte accounting")
	}
	// One indivisible address cannot provide a useful cardinality split.
	if names := n.preferNearsForDemand("cpu", 1, n.Name()); len(names) != 0 {
		t.Fatalf("atomic process load spawned %v", names)
	}
}

func TestScalePolicyUninitializedStatsCannotBecomePlan(t *testing.T) {
	t.Parallel()
	settings, err := NewSettings("AAAAAAAAAAAAAAAB", 9000, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	n, err := NewNode(settings, testNewContact, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.zoneStats(); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stats: %v", err)
	}
	if n.evaluateScaleDecision().Err == nil {
		t.Fatal("missing durable state became a valid plan")
	}
}

// Real facade writes must remain available while the external provisioner is
// waiting, then during its cooldown. No authority check is bypassed.
func TestScalePolicyDurableWritesWhileProvisionerWaits(t *testing.T) {
	t.Setenv("INDEXUS_ITEMS_LIMIT", "8")
	for _, result := range []string{"success", "error", "timeout"} {
		t.Run(result, func(t *testing.T) {
			d := VNodeDescriptor{ID: ID{16}}
			d.Name = EncodeName(d.ID)
			f := newReadPhaseFixture(t, []VNodeDescriptor{d}, false, false)
			n := f.nodes[0]
			for _, loc := range []string{"0", "y"} {
				for i := 0; i < 8; i++ {
					id := fmt.Sprintf("%s-%d", loc, i)
					if err := n.New(&Item{Collection: "0000000000000001", Location: loc, Id: id, RequestID: id}, "@", nil); err != nil {
						t.Fatal(err)
					}
				}
			}
			release, started := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				if result == "error" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			defer unblock()
			n.EnableAutoscale(autoscale.Config{Enabled: true, ProvisionerURL: server.URL, ItemsLimit: 8, MemRefusePct: 95, Cooldown: time.Hour, ProvisionTimeout: 500 * time.Millisecond})
			a := n.autoscale
			a.Tick(autoscale.Input{OwnedItems: 16, SelfName: n.Name(), Resources: autoscale.ResourceSample{MemPct: 10, DiskFreePct: 100, DiskOK: true}}, func(req autoscale.Request) error {
				return a.PostScale(server.URL, n.Name(), 16, 2, req.PreferNear, req.Reason, req.SpawnCount, req.PreferNears)
			})
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("divisible item load never reached provisioner")
			}
			if !inFlight(a) {
				t.Fatal("provisioner is not in flight")
			}
			write := func(id string) {
				t.Helper()
				if err := n.New(&Item{Collection: "0000000000000001", Location: "0", Id: id, RequestID: id}, "@", nil); err != nil {
					t.Fatalf("durable write refused: %v", err)
				}
			}
			write("during-provision")
			if result != "timeout" {
				unblock()
			}
			deadline := time.Now().Add(2 * time.Second)
			for inFlight(a) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if inFlight(a) {
				t.Fatal("provisioner never finished")
			}
			wantDone := 0
			if result == "success" {
				wantDone = 1
			}
			if got := a.Snapshot()["scale_ups_done"].(int); got != wantDone {
				t.Fatalf("provisioner successes: %d want %d", got, wantDone)
			}
			write("after-provision")
			if count, err := n.Count(); err != nil || count != 18 {
				t.Fatalf("accepted rows: %d %v", count, err)
			}
		})
	}
}

func inFlight(a *autoscale.Controller) bool {
	up, _ := a.Snapshot()["up_in_flight"].(bool)
	return up
}
