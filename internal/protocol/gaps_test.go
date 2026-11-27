package protocol

import (
	"testing"
)

func TestAUTO2_SingleSpikeDoesNotPinGrowth(t *testing.T) {
	var e RateEWMA
	e.Alpha = 0.2
	e.Observe(100)
	for i := 0; i < 40; i++ {
		e.Observe(0)
	}
	if e.Value > 1 {
		t.Fatalf("spike pinned growth at %v", e.Value)
	}
}
