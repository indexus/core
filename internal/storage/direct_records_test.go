package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
)

func TestDirectRecordsSkipDescendantKeysOnDisk(t *testing.T) {
	s, _, _ := ticketStores(t)
	z, _ := tree.NewZoneKey("0000000000000001", "a")
	empty, _ := tree.NewZoneKey(z.Collection, "b")
	seed := []Record{{Collection: z.Collection, Location: z.Location, ID: "direct"}}
	for i := range 512 {
		seed = append(seed, Record{Collection: z.Collection, Location: "a0", ID: fmt.Sprint(i)})
	}
	if err := s.SeedLocalRecords(seed); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Flush(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.DirectRecords(ctx, empty, 1, 256); err != nil {
		t.Fatal(err)
	}
	read := func(key tree.ZoneKey, count int) receiverWork {
		return measureReceiver(t, func() error {
			rows, err := s.DirectRecords(ctx, key, 1, 256)
			if err == nil && len(rows) != count {
				t.Fatalf("%s: %+v", key.Location, rows)
			}
			return err
		})
	}
	baseline, direct := read(empty, 0), read(z, 1)
	if direct.keys != baseline.keys+1 {
		t.Fatalf("scanned %d keys on empty address, %d on target", baseline.keys, direct.keys)
	}
	t.Logf("512 descendant rows on disk, %d additional scanned key over the empty address", direct.keys-baseline.keys)
	rows, err := s.DirectRecords(ctx, z, 1, 8)
	if !errors.Is(err, ErrDirectRecordBudget) || rows != nil {
		t.Fatal(rows, err)
	}
}
