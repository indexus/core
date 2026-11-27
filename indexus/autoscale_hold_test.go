package indexus

import (
	"testing"
	"time"
)

// Autoscale waits while the open transfers keep committing steps, however
// long they take, and stops waiting only when they went quiet for
// transferStallLimit (742c1db counted time since the first open transfer).
func TestHoldForTransfersFollowsStepsNotTotalDuration(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	limit := transferStallLimit
	for _, c := range []struct {
		name                  string
		open                  bool
		openSince, lastStep   time.Time
		now                   time.Time
		wantHold, wantStalled bool
	}{
		{"nothing open", false, time.Time{}, time.Time{}, t0, false, false},
		{"just opened", true, t0, time.Time{}, t0.Add(time.Second), true, false},
		{"long but stepping", true, t0, t0.Add(10*limit - time.Second), t0.Add(10 * limit), true, false},
		{"quiet since its last step", true, t0, t0.Add(limit), t0.Add(2 * limit), false, true},
		{"no step since first seen open", true, t0, time.Time{}, t0.Add(limit), false, true},
		{"a step older than the open window", true, t0, t0.Add(-time.Hour), t0.Add(limit - time.Second), true, false},
	} {
		hold, stalled := holdForTransfers(c.open, c.openSince, c.lastStep, c.now)
		if hold != c.wantHold || stalled != c.wantStalled {
			t.Errorf("%s: hold=%v stalled=%v, want %v %v", c.name, hold, stalled, c.wantHold, c.wantStalled)
		}
	}
}
