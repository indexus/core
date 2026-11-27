package mesh

import (
	"context"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestCutoverWaitWakesOnEndCutoverAndRespectsContext(t *testing.T) {
	s := &Service{}
	z, err := tree.NewZoneKey("aaaaaaaaaaaaaaaa", "AA")
	if err != nil {
		t.Fatal(err)
	}
	// No active cutover returns immediately with false.
	if s.waitCutover(context.Background(), z) {
		t.Fatal("waitCutover returned true with no active cutover")
	}

	// Active cutover wakes waiter as soon as endCutover runs.
	done := s.beginCutover([]tree.ZoneKey{z})
	woke := make(chan bool, 1)
	go func() {
		woke <- s.waitCutover(context.Background(), z)
	}()
	time.Sleep(15 * time.Millisecond)
	s.endCutover([]tree.ZoneKey{z}, done)
	select {
	case ok := <-woke:
		if !ok {
			t.Fatal("waitCutover returned false after endCutover")
		}
	case <-time.After(time.Second):
		t.Fatal("waitCutover did not wake after endCutover")
	}

	// Context cancellation aborts wait without waiting for cutoverWaitLimit.
	done2 := s.beginCutover([]tree.ZoneKey{z})
	defer s.endCutover([]tree.ZoneKey{z}, done2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if s.waitCutover(ctx, z) {
		t.Fatal("waitCutover returned true on canceled context")
	}
}
