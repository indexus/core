package main

import (
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/autoscale"
)

// A node that used 2 CPU-seconds per second on a 4-core machine is 50 %; a
// node with no CPU reading yet, or still joining, counts as receiving.
func TestMachineFactsSumsSlots(t *testing.T) {
	start := time.Unix(1000, 0)
	steady := &slot{}
	steady.record("steady", false, 10, nil, start)
	steady.record("steady", false, 30, nil, start.Add(10*time.Second))
	joining := &slot{}
	joining.record("joining", true, 1, nil, start)
	joining.record("joining", true, 6, nil, start.Add(10*time.Second))
	started := &slot{} // alive, never answered
	gone := &slot{exited: start}
	gone.record("gone", false, 0, nil, start)
	gone.record("gone", false, 100, nil, start.Add(time.Second))

	host := autoscale.ResourceSample{MemTotal: 1 << 30, MemPct: 25, DiskOK: true, DiskFreePct: 60}
	got := machineFacts([]*slot{steady, joining, started, gone, nil}, 4, host, 0)
	if got.Running != 3 || got.Free != 2 || got.Receiving != 2 {
		t.Fatalf("running, free, receiving = %d, %d, %d; want 3, 2, 2", got.Running, got.Free, got.Receiving)
	}
	if want := 100 * (2 + 0.5) / 4; got.CPUPct != want || !got.CPUOK || got.Cores != 4 {
		t.Fatalf("cpu = %v (ok %v) on %d cores, want %v", got.CPUPct, got.CPUOK, got.Cores, want)
	}
	if len(got.VNodes) != 2 || got.VNodes[0] != "steady" || got.VNodes[1] != "joining" {
		t.Fatalf("vnodes = %v", got.VNodes)
	}
	if !got.MemOK || got.MemFreePct != 75 || !got.DiskOK || got.DiskFreePct != 60 {
		t.Fatalf("memory and disk: %+v", got)
	}
}

// Owned items go to placement by vnode name; a node that reported none is
// absent, which turns the relief rule off rather than counting it as empty.
func TestMachineFactsCarriesOwnedItems(t *testing.T) {
	at := time.Unix(1000, 0)
	counted, silent := &slot{items: 89_815, itemsOK: true, itemsAt: time.Now()}, &slot{}
	counted.record("counted", false, 0, nil, at)
	silent.record("silent", false, 0, nil, at)
	got := machineFacts([]*slot{counted, silent}, 4, autoscale.ResourceSample{}, 0)
	if len(got.Items) != 1 || got.Items["counted"] != 89_815 {
		t.Fatalf("items = %v, want only counted=89815", got.Items)
	}
}

func TestMachineFactsWithoutHostReadings(t *testing.T) {
	got := machineFacts([]*slot{nil, nil}, 8, autoscale.ResourceSample{}, 0)
	if got.Running != 0 || got.Free != 2 || got.CPUPct != 0 || !got.CPUOK || got.MemOK || got.DiskOK {
		t.Fatalf("empty machine without host readings: %+v", got)
	}
}

// The CPU window keeps the last cpuSamples readings only.
func TestSlotCPUWindowForgetsOldReadings(t *testing.T) {
	s := &slot{}
	at := time.Unix(1000, 0)
	for i := 0; i < 20; i++ { // idle
		s.record("n", false, 0, nil, at)
		at = at.Add(factsEvery)
	}
	used := 0.0
	for i := 0; i < cpuSamples; i++ { // then one core
		used += factsEvery.Seconds()
		s.record("n", false, used, nil, at)
		at = at.Add(factsEvery)
	}
	if got, ok := s.cores(); !ok || got != 1 {
		t.Fatalf("cores = %v, %v; want 1 over the window", got, ok)
	}
	if len(s.cpu) != cpuSamples {
		t.Fatalf("window holds %d readings, want %d", len(s.cpu), cpuSamples)
	}
}

func TestMachineFactsForgetsStaleAndUncountedItems(t *testing.T) {
	now := time.Now()
	n := int64(42)
	s := &slot{name: "n"}
	s.recordItems(&n, false, 0, now)
	if got := machineFacts([]*slot{s}, 4, autoscale.ResourceSample{}, 0); len(got.Items) != 0 {
		t.Fatal("uncounted value became known", got.Items)
	}
	s.recordItems(&n, true, 0, now)
	if got := machineFacts([]*slot{s}, 4, autoscale.ResourceSample{}, 0); got.Items["n"] != 42 {
		t.Fatal("fresh count omitted", got.Items)
	}
	s.itemsAt = now.Add(-autoscale.ItemsCountMaxAge - time.Second)
	if got := machineFacts([]*slot{s}, 4, autoscale.ResourceSample{}, 0); len(got.Items) != 0 {
		t.Fatal("failed observation kept stale count", got.Items)
	}
	s.recordItems(&n, true, autoscale.ItemsCountMaxAge.Milliseconds()+1, now)
	if s.itemsOK {
		t.Fatal("stale observation accepted")
	}
}

// Rates require a full window from each live slot of the same generation.
// A receiver that stops answering cannot turn into an attractive empty host.
func TestAppliedRateCompleteFreshWindow(t *testing.T) {
	now := time.Now()
	first := now.Add(-(cpuSamples - 1) * factsEvery)
	window := func(name string, writes int64) *slot {
		s := &slot{}
		for i := range cpuSamples {
			count := writes * int64(i)
			s.record(name, false, 0, &count, first.Add(time.Duration(i)*factsEvery))
		}
		return s
	}
	measured := window("owner", 20)
	idle := window("idle", 0)
	got := machineFacts([]*slot{measured, idle, nil}, 4, autoscale.ResourceSample{}, 0)
	if got.AppliedRate == nil || got.AppliedRate.PerSecond != 10 || !got.AppliedRate.AsOf.Equal(now) {
		t.Fatalf("rate = %+v", got.AppliedRate)
	}
	if got := machineFacts([]*slot{measured, {}}, 4, autoscale.ResourceSample{}, 0); got.AppliedRate != nil {
		t.Fatalf("unknown slot became zero: %+v", got.AppliedRate)
	}
	if _, ok := measured.appliedRate(now.Add(autoscale.AppliedRateMaxAge + 1)); ok {
		t.Fatal("stale rate accepted")
	}
	for _, test := range []struct {
		name   string
		change func(*slot)
	}{
		{"missing", func(s *slot) { s.cpu[2].applied = -1 }},
		{"reset", func(s *slot) { s.cpu[2].applied = 0 }},
		{"gap", func(s *slot) { s.cpu[0].at = first.Add(-time.Minute) }},
		{"generation", func(s *slot) { n := int64(120); s.record("other", false, 0, &n, now) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := window("a", 20)
			test.change(s)
			if _, ok := s.appliedRate(now); ok {
				t.Fatal("invalid observations accepted")
			}
		})
	}
}
