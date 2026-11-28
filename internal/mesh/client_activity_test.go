package mesh

import (
	"context"
	"encoding/json"
	"testing"
)

func TestClientActivityHoldsLongAndNestedReads(t *testing.T) {
	var a ClientActivity
	ctx, end := a.Begin(context.Background())
	_, nestedEnd := a.Begin(ctx)
	nestedEnd()
	if starts, active := a.Sample(); starts != 1 || active != 1 {
		t.Fatalf("nested read counted twice or ended early: %d %d", starts, active)
	}
	if starts, active := a.Sample(); starts != 0 || active != 1 {
		t.Fatalf("long read disappeared: %d %d", starts, active)
	}
	end()
	if starts, active := a.Sample(); starts != 0 || active != 0 {
		t.Fatalf("ended read remained: %d %d", starts, active)
	}
}

func TestOnlyDataRPCsCarryActivity(t *testing.T) {
	for _, kind := range []string{KindMembership, KindColdUsage, KindSummaryContribute, "install-zone", "preload", "write"} {
		if (Request{Kind: kind, ClientWork: true}).CarriesClientWork() {
			t.Fatalf("accepted activity on maintenance RPC %s", kind)
		}
	}
	for _, marked := range []bool{false, true} {
		raw, err := json.Marshal(Request{Kind: KindInventoryPrefix, ClientWork: marked})
		if err != nil {
			t.Fatal(err)
		}
		var decoded Request
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.CarriesClientWork() != marked {
			t.Fatalf("lost activity flag %s", raw)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, present := fields["ClientWork"]; present != marked {
			t.Fatalf("unexpected wire encoding %s", raw)
		}
	}
}
