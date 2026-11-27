package mesh

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/internal/storage"
	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestCoincidentAncestorZonesTransferBottomUpWithOneWriterEach(t *testing.T) {
	t.Parallel()
	for _, collection := range []string{"0000000000000001", "0000000000000000"} {
		t.Run(collection, func(t *testing.T) {
			net, nodes := meshFixture(t, 2)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var zones []tree.ZoneKey
			var records []storage.Record
			for i, location := range []string{"0", "00", "000"} {
				zone, err := tree.NewZoneKey(collection, location)
				if err != nil {
					t.Fatal(err)
				}
				zones = append(zones, zone)
				record := storage.Record{Collection: zone.Collection, Location: zone.Location, ID: "row", Metrics: []float64{float64(i)}}
				records = append(records, record)
				if _, err := nodes[0].Handle(ctx, Request{Kind: "write", RequestID: "seed:" + location, Record: &record}); err != nil {
					t.Fatal(err)
				}
			}
			if zones[0].ID != zones[1].ID || zones[1].ID != zones[2].ID {
				t.Fatal("fixture has no real address collision")
			}
			nodes[1].VNode.ID = zones[0].ID
			if nodes[1].VNode.ID.IsZero() {
				nodes[1].VNode.ID[11] = 1
			}
			installed := map[tree.Key]bool{}
			var order []tree.ZoneKey
			net.setAfter(func(request Request) error {
				if request.Kind != "install-zone" {
					return nil
				}
				for _, transferred := range request.ZoneTransfers {
					order = append(order, transferred.Zone)
					installed[transferred.Zone.Key()] = true
				}
				for i, zone := range zones {
					acks := 0
					for participant, node := range nodes {
						_, applied, err := node.Store.Apply(fmt.Sprintf("probe:%d:%d:%s", len(order), participant, zone.Location), records[i])
						if err == nil && applied {
							acks++
							if (participant == 1) != installed[zone.Key()] {
								return fmt.Errorf("wrong writer for %s after %d flips", zone.Location, len(order))
							}
						} else if !errors.Is(err, storage.ErrNotOwner) && !errors.Is(err, storage.ErrNotReady) && !errors.Is(err, storage.ErrFrozen) {
							return fmt.Errorf("probe %s: %w", zone.Location, err)
						}
					}
					if acks != 1 {
						return fmt.Errorf("%d writers for %s after %d flips", acks, zone.Location, len(order))
					}
				}
				return nil
			})
			if err := nodes[1].Join(ctx); err != nil {
				t.Fatal(err)
			}
			if len(order) != 3 {
				t.Fatalf("transferred %d zone identities, want 3", len(order))
			}
			for i, zone := range order {
				if zone != zones[2-i] {
					t.Fatalf("parent preceded child: %+v", order)
				}
			}
			rows, err := nodes[1].Store.Inventory(false)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 3 {
				t.Fatalf("receiver has %d zone snapshots, want 3", len(rows))
			}
			for _, row := range rows {
				if len(row.Records) != 1 || row.Records[0].ID != "row" {
					t.Fatal("coincident zone records were merged or lost")
				}
			}

		})
	}
}
