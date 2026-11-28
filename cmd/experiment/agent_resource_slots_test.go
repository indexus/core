package main

import (
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

func TestResourceSlotsGrowPastFiveAndDeferOnPressure(t *testing.T) {
	var used []*slot
	const addressSlots = 100
	host := autoscale.ResourceSample{MemTotal: 32 << 30, MemPct: 20, DiskOK: true, DiskFreePct: 90}
	facts := func(cpu float64, mode bool) autoscale.MachineFacts {
		slots, ceiling := used, addressSlots
		if !mode {
			slots, ceiling = make([]*slot, addressSlots), 0
			copy(slots, used)
		}
		f := machineFacts(slots, 4, host, ceiling, false)
		f.HostCPUOK = true
		f.CPUPct = max(f.CPUPct, cpu)
		ready, _ := autoscale.ReceiverOffer(f)
		f.ReceiverReady = &ready
		return f
	}
	for i := 0; i < 9; i++ {
		if f := facts(5, true); offeredSlots(f) != 1 || f.ReceiverReady == nil || !*f.ReceiverReady {
			t.Fatalf("cannot grow at %d: %+v", i, f)
		}
		v := &slot{}
		used = append(used, v)
		if f := facts(5, true); offeredSlots(f) != 0 {
			t.Fatalf("second concurrent start allowed: %+v", f)
		}
		at := time.Now()
		v.record("ready", false, 10, nil, at.Add(-time.Second))
		v.record("ready", false, 10, nil, at)
	}
	if f := facts(47.5, true); offeredSlots(f) != 1 {
		t.Fatalf("light receiver should fit: %+v", f)
	}
	if f := facts(47.51, true); offeredSlots(f) != 0 || f.ReceiverReady == nil || *f.ReceiverReady {
		t.Fatalf("CPU headroom ignored: %+v", f)
	}
	if f := facts(40, false); offeredSlots(f) != 91 {
		t.Fatalf("fixed addresses lost: %+v", f)
	}
	host.DiskFreePct = 30
	if f := facts(5, true); offeredSlots(f) != 0 {
		t.Fatal("offered with saturated disk")
	}
	host.DiskFreePct = 90
	host.MemPct = 70
	if f := facts(5, true); offeredSlots(f) != 0 {
		t.Fatal("offered with saturated RAM")
	}
	host.MemPct = 20
	f := facts(5, false)
	f.HostCPUOK = false
	f.AddressSlots = addressSlots
	if offeredSlots(f) != 0 {
		t.Fatal("offered without machine CPU observation")
	}
	for j := range used[0].cpu {
		used[0].cpu[j].at = used[0].cpu[j].at.Add(-time.Minute)
	}
	if f := facts(5, true); offeredSlots(f) != 0 || f.Receiving != 1 {
		t.Fatalf("stale slot looked available: %+v", f)
	}
	used = nil
	f = facts(48, true)
	f.Machine = "warming"
	book, ok := autoscale.ReadyReserve([]autoscale.MachineFacts{f}, "", 0, 1, map[string]bool{f.Machine: true})
	if !ok || book.Warming != 1 || book.Boots() != 0 || f.Free != addressSlots || offeredSlots(f) != 0 {
		t.Fatalf("warm capacity: %+v %+v", f, book)
	}
	if choice := autoscale.Place([]autoscale.MachineFacts{f}, "donor", true); choice.Machine != "" {
		t.Fatalf("unready host placed: %+v", choice)
	}
	f.HostCPUOK = false
	if ready, known := autoscale.ReceiverOffer(f); ready || known {
		t.Fatal("unknown host CPU produced an offer")
	}
	if _, ok := autoscale.ReadyReserve([]autoscale.MachineFacts{f}, "", 15, 1, nil); ok {
		t.Fatal("unknown CPU used to compute global reserve")
	}

}

// Real kernel port selection with a simulated healthy fleet. The old address
// window exhausted at slot 100 even with CPU, disk and RAM headroom.
func TestDynamicAdmissionPastHundred(t *testing.T) {
	c := agentConfig{resourceSlots: true, p2pBase: 21100, monBase: 19100, debugBase: 19200}
	var slots []*slot
	seen := map[int]bool{}
	host := autoscale.ResourceSample{MemTotal: 1 << 30, DiskOK: true, DiskFreePct: 90}
	now := time.Now()
	for i := 0; i < 128; i++ {
		f := machineFacts(slots, 4, host, 0, true)
		f.HostCPUOK = true
		if offeredSlots(f) != 1 || f.Running != i {
			t.Fatalf("slot %d refused: %+v", i, f)
		}
		p, err := c.portsFor(i, slots)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && p != (nodePorts{21100, 19100, 19200}) {
			t.Fatal("bootstrap endpoints changed", p)
		}
		for _, port := range []int{p.p2p, p.monitor, p.debug} {
			if port <= 0 || seen[port] {
				t.Fatal("invalid or reused port", port)
			}
			seen[port] = true
		}
		s := &slot{ports: p}
		s.record("vnode", false, 0, nil, now.Add(-time.Second))
		s.record("vnode", false, 0, nil, now)
		slots = append(slots, s)
	}
	f := machineFacts(slots, 4, host, 0, true)
	if offeredSlots(f) != 0 {
		t.Fatal("admitted with unknown host CPU")
	}
	f.HostCPUOK = true
	f.CPUPct = 95
	if offeredSlots(f) != 0 {
		t.Fatal("admitted with saturated host CPU")
	}
}

func TestReceiverForecastUsesRecentJoinOnThisMachine(t *testing.T) {
	now := time.Now()
	s := &slot{}
	for i := range cpuSamples {
		s.record("joining", true, float64(i)*3, nil, now.Add(time.Duration(i-cpuSamples+1)*factsEvery))
	}
	s.record("joining", false, 16, nil, now)
	facts := func() autoscale.MachineFacts { return machineFacts([]*slot{s}, 4, autoscale.ResourceSample{}, 0, true) }
	if f := facts(); f.ReceiverReserveCores != 1.5 {
		t.Fatal("lost measured join cost", f)
	}
	s.joinSampleAt = now.Add(-2 * time.Minute)
	if f := facts(); f.ReceiverReserveCores != 0.5 {
		t.Fatal("stale receiver estimate", f)
	}
}
