package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/internal/storage"
)

func TestLotByteTargetKeepsLargerIndividualZones(t *testing.T) {
	t.Parallel()
	net, source, target := streamFixture(t, 0)
	expected := map[string]string{}
	for i, size := range []int{2 << 20, 600 << 10, 600 << 10, 600 << 10} {
		r := storage.Record{
			Collection: "collection-00000", Location: "a" + string(rune('a'+i)),
			ID: fmt.Sprintf("payload-%d", i), Metrics: []float64{1},
			Payload: json.RawMessage(`"` + strings.Repeat("x", size) + `"`),
		}
		if _, err := source.Store.Write(r.ID, r); err != nil {
			t.Fatal(err)
		}
		expected[r.ID] = string(r.Payload)
	}
	preloads := 0
	net.setBefore(func(request Request) error {
		if request.Kind != "preload" || len(request.Transfer.Snapshots) == 0 {
			return nil
		}
		preloads++
		size := 0
		for _, snap := range request.Transfer.Snapshots {
			raw, err := json.Marshal(snap)
			if err != nil {
				return err
			}
			size += len(raw)
		}
		if size > 1<<20 && len(request.Transfer.Snapshots) > 1 {
			return fmt.Errorf("%d bytes grouped in one small-lot cutover", size)
		}
		return nil
	})
	if err := target.Join(context.Background()); err != nil {
		t.Fatal(err)
	}
	if preloads < 2 {
		t.Fatal("payloads were not split into separate lots")
	}
	rows, err := target.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, snap := range rows {
		for _, r := range snap.Records {
			if string(r.Payload) != expected[r.ID] {
				t.Fatal("payload changed during transfer")
			}
			delete(expected, r.ID)
		}
	}
	if len(expected) != 0 {
		t.Fatal("large individual zone was not transferred")
	}
}

func TestInitialOversizedZoneRefusesTransferWithoutFreezing(t *testing.T) {
	t.Parallel()
	_, source, target := streamFixture(t, 0)
	r := storage.Record{
		Collection: "collection-00000", Location: "aa", ID: "oversized",
		Metrics: []float64{1}, Payload: json.RawMessage(`"` + strings.Repeat("x", 8<<20) + `"`),
	}
	if _, err := source.Store.Write(r.ID, r); err != nil {
		t.Fatal(err)
	}
	if err := target.Join(context.Background()); !errors.Is(err, storage.ErrInventoryItemTooLarge) {
		t.Fatalf("oversized zone: %v", err)
	}
	r.ID, r.Payload = "after-refusal", nil
	if _, err := source.Store.Write(r.ID, r); err != nil {
		t.Fatalf("refused transfer froze source: %v", err)
	}
}
