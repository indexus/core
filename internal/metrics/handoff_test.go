package metrics

import (
	"testing"
	"time"
)

func TestHandoffRetainsFailedPhaseUntilRetryAndDropsOnCompletion(t *testing.T) {
	const op = "phase-test"
	HandoffStarted(op, "receiver")
	HandoffPhase(op, "install")
	HandoffEnded(op, false)
	open.Lock()
	phase := open.phase[op]
	_, exists := open.since[op]
	open.Unlock()
	if !exists || phase.running || phase.name != "install" {
		t.Fatalf("failed phase lost: %+v", phase)
	}
	if attempt := HandoffStarted(op, "receiver"); attempt != 2 {
		t.Fatalf("attempt=%d", attempt)
	}
	HandoffPhase(op, "resume-installs")
	HandoffEnded(op, true)
	open.Lock()
	_, exists = open.phase[op]
	open.Unlock()
	if exists {
		t.Fatal("finished phase retained")
	}
}

func TestMilestoneHasOneObserverPerEvent(t *testing.T) {
	var install, write milestone
	ages := make(chan time.Duration, 32)
	for range cap(ages) {
		go func() { ages <- install.Mark() }()
	}
	var first time.Duration
	for range cap(ages) {
		if age := <-ages; age > 0 {
			if first > 0 {
				t.Fatal("installation reported twice")
			}
			first = age
		}
	}
	if first == 0 || int64(first) != install.nanos.Load() || write.Mark() < first || write.Mark() != 0 {
		t.Fatal("install and write must capture independent, ordered process ages")
	}
}

func TestPhysicalMoveProgressSurvivesRetryUntilCompletion(t *testing.T) {
	const id = "move-retry"
	HandoffQueued(id, "dst", true)
	HandoffStarted(id, "dst")
	HandoffInstalled(id, 12)
	HandoffEnded(id, false)
	HandoffStarted(id, "dst")
	defer HandoffEnded(id, true)
	for _, row := range openSnapshot()["list"].([]map[string]any) {
		if row["op"] == id {
			if row["attempts"] != 2 || row["physical_move"] != true || row["installed_zones"] != 12 || row["last_install_seconds"] == nil {
				t.Fatalf("lost move progress on retry: %+v", row)
			}
			return
		}
	}
	t.Fatal("missing open move")
}
