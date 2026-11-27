package indexus

import (
	"context"
	"errors"
	"testing"
	"time"
)

// REM-88: a slow gossip partner must not leave Resume and Rebalance with an
// expired context. Sync here blocks until its own deadline, as a peer that
// accepts the connection and never answers does.
func TestMaintainPhasesGiveEachPhaseItsOwnDeadline(t *testing.T) {
	t.Parallel()

	slowSync := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	failing := errors.New("first phase failed")
	var ran []error
	phase := func(result error) func(context.Context) error {
		return func(ctx context.Context) error {
			ran = append(ran, ctx.Err())
			return result
		}
	}
	err := maintainPhasesWithin(context.Background(), 30*time.Millisecond, time.Second, slowSync, phase(failing), phase(nil))
	if len(ran) != 2 {
		t.Fatalf("%d phases ran after a failing one, want 2", len(ran))
	}
	for i, e := range ran {
		if e != nil {
			t.Fatalf("phase %d started with an expired context after a slow sync: %v", i, e)
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, failing) {
		t.Fatalf("errors not joined: %v", err)
	}
}
