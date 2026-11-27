package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHostCPUCountsSystemWaitAndStealWithoutDoubleCountingGuests(t *testing.T) {
	var m hostCPUMeter
	start := time.Unix(5000, 0)
	first := "cpu 100 0 0 100 0 0 0 0 50 0\ncpu0 ignored"
	if got := m.observe(first, start); got.valid || got.Breakdown != nil || got.Status != "baseline" {
		t.Fatalf("one sample cannot measure a rate: %+v", got)
	}
	end := start.Add(2400 * time.Millisecond)
	raw := "cpu 115 5 10 140 10 1 4 15 60 5\ncpu0 ignored"
	got := m.observe(raw, end)
	want := hostCPUPercent{UserNice: 20, System: 10, IRQSoftIRQ: 5, IOWait: 10, Steal: 15, Idle: 40}
	if !got.valid || got.busyPct != 60 || got.Status != "measured" || got.Breakdown == nil || *got.Breakdown != want || got.IntervalMS != 2400 || got.EndUnixMS != end.UnixMilli() || got.Ticks != 100 {
		t.Fatalf("wrong interval: %+v breakdown %+v", got, got.Breakdown)
	}
	// A concurrent placement poll sees the same ticks. Its time must not
	// become the end of this interval or the start of the next interval.
	dup := m.observe(raw, end.Add(time.Millisecond))
	got.Status = "reused"
	if dup != got {
		t.Fatalf("same tick lost the last measured interval: %+v, want %+v", dup, got)
	}
	next := m.observe("cpu 115 5 10 240 10 1 4 15", end.Add(2*time.Second))
	if !next.valid || next.IntervalMS != 2000 || next.busyPct != 0 || next.Breakdown == nil || next.Breakdown.Idle != 100 {
		t.Fatalf("lost genuine all-idle interval: %+v", next)
	}
	data, err := json.Marshal(next)
	if err != nil || !strings.Contains(string(data), `"steal_pct":0`) {
		t.Fatalf("zero field unavailable in JSON %s, %v", data, err)
	}
	if got := m.observe("cpu 0 0 0 0", end.Add(3*time.Second)); got.valid || got.Status != "counter_reset" || got.Breakdown != nil {
		t.Fatalf("reset counters are not a rate: %+v", got)
	}
	for _, raw := range []string{"", "cpu0 1 2 3 4", "cpu 1 invalid 3 4"} {
		got := m.observe(raw, end.Add(4*time.Second))
		data, err := json.Marshal(got)
		if got.valid || got.Status != "unavailable" || err != nil || strings.Contains(string(data), "breakdown") {
			t.Fatalf("invalid aggregate %q encoded as %s, %v", raw, data, err)
		}
	}
}

func TestHostCPUPartialCountersPreserveAggregate(t *testing.T) {
	for _, tt := range []struct {
		first, next string
		busy        float64
	}{
		{"cpu 0 0 0 0", "cpu 10 0 0 10", 50},
		{"cpu 0 0 0 0 10 0 0 0", "cpu 11 0 0 10 9 0 0 0", 50},
	} {
		var m hostCPUMeter
		start := time.Unix(100, 0)
		m.observe(tt.first, start)
		got := m.observe(tt.next, start.Add(time.Second))
		if !got.valid || got.busyPct != tt.busy || got.Status != "partial" || got.Breakdown != nil {
			t.Fatalf("unreliable distribution for %q: %+v", tt.next, got)
		}
	}
}
