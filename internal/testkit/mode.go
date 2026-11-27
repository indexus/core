package testkit

import (
	"os"
	"testing"
	"time"
)

// Long reports whether INDEXUS_LONG=1. Long runs explore more IDs, zones,
// seeds, states, handoffs and nodes. They do not add invariants.
func Long() bool {
	return os.Getenv("INDEXUS_LONG") == "1"
}

// RequireLong skips unless INDEXUS_LONG=1.
func RequireLong(t testing.TB) {
	t.Helper()
	if !Long() {
		t.Skip("long test; set INDEXUS_LONG=1")
	}
}

// RaceScale is d, times 4 under the race detector: a time budget that holds
// a normal run holds a race run of the same work.
func RaceScale(d time.Duration) time.Duration {
	if Race {
		return 4 * d
	}
	return d
}
