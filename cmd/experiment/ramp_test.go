package main

import (
	"testing"
	"time"
)

func TestRampLevel(t *testing.T) {
	s := time.Second
	for _, c := range []struct {
		at    time.Duration
		share float64
		want  float64
	}{
		{0, 1, 1}, {29 * s, 1, 1}, // held
		{30 * s, 1, 1}, {60 * s, 1, 1.5}, {90 * s, 1, 2}, {150 * s, 1, 4}, // continuous: 2x every 60 s
		{60 * s, 0, 1}, {89 * s, 0, 1}, {90 * s, 0, 2}, {150 * s, 0, 4}, // instant 2x steps
		{60 * s, 0.5, 1.25}, {90 * s, 0.5, 2}, {120 * s, 0.5, 2.5}, // half linear, half jump
	} {
		if got := rampLevel(c.at, time.Minute, 30*s, c.share); got != c.want {
			t.Errorf("rampLevel(%v, share %v) = %v, want %v", c.at, c.share, got, c.want)
		}
	}
}

func TestKneeStopsAfterAWindowBelowTheRatio(t *testing.T) {
	// The first window never stops, whatever the ratio.
	warm := &knee{below: 0.9, window: 3}
	for i := 0; i < 3; i++ {
		if warm.add(100, 0) {
			t.Fatalf("second %d: stopped inside the first window", i)
		}
	}
	// 100 offered each second: 280/300 = 93% goes on, 260/300 = 87% stops.
	k := &knee{below: 0.9, window: 3}
	acked := []int64{100, 100, 100, 100, 80, 80}
	for i, a := range acked {
		if got := k.add(100, a); got != (i == len(acked)-1) {
			t.Fatalf("second %d: stop=%v", i, got)
		}
	}
	if (&knee{}).add(100, 0) {
		t.Fatal("a knee without stop-below never stops")
	}
}
