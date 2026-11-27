package protocol

import "testing"

func TestAUTO1_NoPressureIsNoop(t *testing.T) {
	if got := EvaluateMachine(MachineState{}); got.Kind != KindNone {
		t.Fatalf("kind=%v", got.Kind)
	}
}
