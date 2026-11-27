package protocol

import (
	"encoding/json"
	"testing"
)

// Join intents persist this enum as JSON, including while a transfer is pending.
func TestPlanReasonEncodingRemainsStable(t *testing.T) {
	for reason, want := range map[PlanReason]string{ReasonNone: "0", ReasonMoveHost: "1", ReasonHotSplit: "2", ReasonRegionalAdd: "3", ReasonColdBalloon: "6"} {
		got, err := json.Marshal(reason)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s encoded as %s, want %s", reason, got, want)
		}
	}
}
