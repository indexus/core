package mesh

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
)

func TestPreviouslyInstalledZoneCannotBypassNextHandoffFence(t *testing.T) {
	t.Parallel()
	net, nodes := meshFixture(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	row := storage.Record{Collection: "Probe00000000000", Location: "m", ID: "seed", Metrics: []float64{1}}
	if _, err := nodes[0].Handle(ctx, Request{Kind: "write", RequestID: "seed", Record: &row}); err != nil {
		t.Fatal(err)
	}
	if err := nodes[1].Join(ctx); err != nil {
		t.Fatal(err)
	}
	var probed atomic.Bool
	net.setBefore(func(request Request) error {
		if probed.Load() || request.Kind != "install-zone" || request.Owner != nodes[1].Self {
			return nil
		}
		zones, err := nodes[1].Store.TransferZones(request.Operation)
		if err != nil {
			return err
		}
		for _, st := range zones {
			if st.Zone.Collection != row.Collection || st.Zone.Location != row.Location || !st.Frozen {
				continue
			}
			if !probed.CompareAndSwap(false, true) {
				continue
			}
			for i := range 100 {
				candidate := row
				candidate.ID = fmt.Sprintf("must-refuse-%d", i)
				if _, _, err := nodes[1].Store.Apply(candidate.ID, candidate); !errors.Is(err, storage.ErrFrozen) && !errors.Is(err, storage.ErrNotOwner) {
					return fmt.Errorf("old installed state bypassed current freeze: %v", err)
				}
			}
		}
		return nil
	})
	if err := nodes[2].Join(ctx); err != nil {
		t.Fatal(err)
	}
	if !probed.Load() {
		t.Fatal("fixture never reached the second frozen handoff")
	}
	var count int
	for _, node := range nodes {
		rows, err := node.Store.Inventory(false)
		if err != nil {
			t.Fatal(err)
		}
		for _, zone := range rows {
			count += len(zone.Records)
		}
	}
	if count != 1 {
		t.Fatalf("unexpected accepted rows: %d", count)
	}
}
