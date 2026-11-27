package indexus

import (
	"testing"
	"time"
)

// COLD-2 eligibility: cool means at or under the rate for CoolFor, not zero
// writes. A burst restarts the clock; observations under a second apart do
// not judge a rate, and unjudged time is not counted as cool.
func TestCOLD2_CoolMeterMeasuresRateNotSilence(t *testing.T) {
	t.Parallel()
	var m coolMeter
	t0 := time.Unix(1000, 0)
	m.reset(t0, 0)
	if got := m.cool(t0.Add(900*time.Millisecond), 0, 10); got != 0 {
		t.Fatalf("unmeasured start counted as cool: %v", got)
	}
	writes := int64(0)
	at := t0
	for i := 0; i < 120; i++ { // two minutes at 5 writes/s, limit 10/s
		at = at.Add(time.Second)
		writes += 5
		m.cool(at, writes, 10)
	}
	if got := m.cool(at, writes, 10); got != 2*time.Minute {
		t.Fatalf("tepid vnode cool for %v, want 2m", got)
	}
	at = at.Add(time.Second)
	writes += 50 // a burst at 50/s
	if got := m.cool(at, writes, 10); got != 0 {
		t.Fatalf("burst did not restart the clock: %v", got)
	}
	at = at.Add(500 * time.Millisecond)
	writes += 40 // too short a window to judge
	if got := m.cool(at, writes, 10); got != 0 {
		t.Fatalf("unjudged time counted as cool: %v", got)
	}
	// Those 40 writes are not forgotten: over the next judged window
	// (1.5 s) they are 27/s, so the clock restarts.
	at = at.Add(time.Second)
	if got := m.cool(at, writes, 10); got != 0 {
		t.Fatalf("writes of a short window were dropped: %v", got)
	}
}
