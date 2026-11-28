package provisioner

import (
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

func TestBootWindowFollowsPersistentDemand(t *testing.T) {
	var wave bootWave
	start := time.Unix(100, 0)
	check := func(at time.Duration, pending, ready, warming, want int) {
		t.Helper()
		got := wave.observe(start.Add(at), autoscale.Reserve{Pending: pending, Ready: ready, Warming: warming})
		if got != want {
			t.Fatalf("at %v with %d waiting, %d ready, %d warming: window %d, want %d", at, pending, ready, warming, got, want)
		}
	}
	// Fifty requests on one observation get the same single boot credit.
	for i := 1; i <= 50; i++ {
		check(0, i, 0, 0, 1)
	}
	check(placementMemory/2, 50, 0, 0, 1)
	// Confirmed shortage grows the budget without a single host arrival.
	check(placementMemory, 50, 0, 0, 2)
	check(2*placementMemory, 50, 0, 1, 4)
	check(3*placementMemory, 50, 0, 1, 8)
	check(4*placementMemory, 3, 1, 0, 2)
	check(4*placementMemory, 1, 1, 0, 0)
	check(4*placementMemory, 50, 0, 0, 1)
	check(5*placementMemory, 50, 0, 0, 2)
	check(5*placementMemory+warmingPatience, 50, 0, 0, 1)
}

func TestReserveWaveLeavesEarlyPlacementsAvailable(t *testing.T) {
	s := NewServer("key")
	donor, spare := host(2, 0, 10, "v0", "v1"), host(0, 0, 0)
	donor.Machine, spare.Machine = "http://donor", "http://spare"
	for _, m := range []*autoscale.MachineFacts{&donor, &spare} {
		m.MemOK, m.DiskOK = true, true
		m.MemFreePct, m.DiskFreePct = 80, 80
	}
	for _, req := range []string{"v0", "v1"} {
		record, env := s.decidePlacement([]autoscale.MachineFacts{donor}, req, "new-"+req)
		if record.Machine != "" || envOf(env, "INDEXUS_BOOT_ALLOWANCE") != "1" {
			t.Fatalf("first wave for %s: %+v %v", req, record, env)
		}
	}
	spare.CPUPct = 25 // measured but still warming, consumes the one credit
	_, env := s.decidePlacement([]autoscale.MachineFacts{donor, spare}, "v0", "new-v0")
	if envOf(env, "INDEXUS_BOOT_ALLOWANCE") != "0" || envOf(env, "INDEXUS_READY_RESERVE_DEFICIT") != "1" {
		t.Fatalf("warming host outside the window: %v", env)
	}
	spare.CPUPct = 0
	record, _ := s.decidePlacement([]autoscale.MachineFacts{donor, spare}, "v0", "new-v0")
	if record.Machine != spare.Machine {
		t.Fatalf("waiting for credit instead of placing on a ready host: %+v", record)
	}
}

// Replay the initial burst from aws-place-100-direct-1127/machines.log. Keep
// its observed requests and assume none of our newly requested machines has
// registered yet. This tests the controller, not the counterfactual throughput.
func TestBootWindowReplayInitialAWSBurst(t *testing.T) {
	var wave bootWave
	inFlight := 0
	start := time.Unix(0, 0)
	for _, r := range []struct{ second, pending, ready, wantStarts int }{
		{15, 1, 3, 1}, {17, 2, 2, 0}, {18, 9, 3, 0}, {19, 3, 2, 0},
		{20, 11, 1, 0}, {21, 7, 2, 0}, {22, 14, 0, 0}, {26, 14, 0, 0},
		{29, 14, 0, 0}, {31, 14, 0, 1}, {32, 14, 0, 0}, {33, 14, 0, 0},
		{34, 14, 0, 0}, {36, 14, 1, 0}, {38, 14, 0, 0}, {41, 15, 1, 0},
		{43, 15, 1, 0},
	} {
		reserve := autoscale.Reserve{Floor: 1, Pending: r.pending, Ready: r.ready, WaitingRemote: r.second != 29}
		window := wave.observe(start.Add(time.Duration(r.second)*time.Second), reserve)
		starts := max(0, min(window, reserve.Boots())-inFlight)
		if starts != r.wantStarts {
			t.Fatalf("05:44:%02d with %d outstanding: %d additional starts, want %d", r.second, inFlight, starts, r.wantStarts)
		}
		inFlight += starts
	}
}

func TestQuietFleetBootsGraduallyUntilPressureChanges(t *testing.T) {
	var wave bootWave
	start := time.Unix(0, 0)
	for tick, want := range []int{1, 2, 3, 4, 8, 9, 0, 1} {
		reserve := autoscale.Reserve{Pending: 40, ReuseHeadroom: tick != 4}
		if tick == 6 {
			reserve.Ready = 40
		}
		for range 20 {
			if got := wave.observe(start.Add(time.Duration(tick)*placementMemory), reserve); got != want {
				t.Fatalf("interval %d: window %d, want %d", tick, got, want)
			}
		}
	}
}

// Compare identical bursts using the hook's outstanding-start accounting. A
// machine takes 45 s to arrive. Each arrival serves one waiting source lane.
func TestTransientSplitBacklogWithDelayedHostArrivals(t *testing.T) {
	simulate := func(quiet bool, burstSeconds int) (starts int, served int) {
		var wave bootWave
		var arrivals []int
		for sec := 0; sec < 300; sec++ {
			for len(arrivals) > 0 && arrivals[0] == sec {
				arrivals = arrivals[1:]
				if sec < burstSeconds {
					served++
				}
			}
			pending := 0
			if sec < burstSeconds {
				pending = 12 - served
			}
			budget := wave.observe(time.Unix(int64(sec), 0), autoscale.Reserve{Pending: pending, ReuseHeadroom: quiet})
			for len(arrivals) < budget {
				arrivals = append(arrivals, sec+45)
				starts++
			}
		}
		return starts, served
	}
	quiet, _ := simulate(true, 60)
	hot, _ := simulate(false, 60)
	if quiet != 5 || hot != 9 {
		t.Fatalf("one-minute burst: quiet=%d pressured=%d", quiet, hot)
	}
	if starts, served := simulate(true, 300); starts != 12 || served != 12 {
		t.Fatalf("persistent shortage: started %d served %d", starts, served)
	}
}
