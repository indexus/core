package indexus

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestCanonicalContentReadFiltersDeletesAndChecksBudget(t *testing.T) {
	t.Parallel()
	n := newNodeOn(t, 64)
	for _, location := range []string{"a", "ab", "b"} {
		if err := n.New(&Item{Collection: "Demo000000000000", Location: location, Id: "pixels", Metrics: []float64{7}, Payload: json.RawMessage(`{"tree":"AQ=="}`)}, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.Delete(&Item{Collection: "Demo000000000000", Location: "a", Id: "pixels"}, "@", nil); err != nil {
		t.Fatal(err)
	}
	rows, err := n.GetRecordsContext(context.Background(), "Demo000000000000", "a", 10)
	if err != nil || len(rows) != 1 || rows[0].Location != "ab" || !bytes.Equal(rows[0].Payload, []byte(`{"tree":"AQ=="}`)) {
		t.Fatalf("read: %+v %v", rows, err)
	}
	if _, err := n.GetRecordsContext(context.Background(), "Demo000000000000", "@", 1); err == nil {
		t.Fatal("accepted partial inventory over budget")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := n.GetRecordsContext(ctx, "Demo000000000000", "@", 10); err == nil {
		t.Fatal("ignored cancellation")
	}
}
