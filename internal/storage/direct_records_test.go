package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/internal/tree"
	"github.com/indexus/go-indexus-core/internal/zonelog"
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

func TestDirectRecordsBudgetCountsOpaqueBodiesBeforeDecoding(t *testing.T) {
	s, _, _ := ticketStores(t)
	z, _ := tree.NewZoneKey("0000000000000001", "a")
	var records []zonelog.Record
	for _, id := range []string{"one", "two"} {
		r := Record{Collection: z.Collection, Location: z.Location, ID: id, Metrics: []float64{7}, Weight: 3, Payload: json.RawMessage(`"` + strings.Repeat("x", 4096) + `"`)}
		body, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, zonelog.Record{Collection: z.Collection, Location: z.Location, ID: id, Opaque: true, Payload: body})
	}
	malformed, _ := tree.NewZoneKey(z.Collection, "b")
	records = append(records, zonelog.Record{Collection: z.Collection, Location: malformed.Location, ID: "bad", Opaque: true, Payload: bytes.Repeat([]byte("!"), 4096)})
	if err := s.db.Apply(records); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Flush(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rows, err := s.DirectRecords(ctx, z, 2, 16384)
	if err != nil || len(rows) != 2 || rows[0].ID != "one" || rows[1].ID != "two" || rows[0].Weight != 3 || len(rows[0].Metrics) != 1 || rows[0].Metrics[0] != 7 || rows[0].Payload != nil {
		t.Fatal(rows, err)
	}
	// Each legacy body fits individually. The pair exceeds the same budget.
	rows, err = s.DirectRecords(ctx, z, 2, 6000)
	if !errors.Is(err, ErrDirectRecordBudget) || rows != nil {
		t.Fatal(rows, err)
	}
	// The oversized invalid encoding must be refused before the JSON decoder.
	rows, err = s.DirectRecords(ctx, malformed, 1, 1024)
	if !errors.Is(err, ErrDirectRecordBudget) || rows != nil {
		t.Fatal(rows, err)
	}
	_, err = s.DirectRecords(ctx, malformed, 1, 8192)
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatal("expected the in-budget malformed encoding to reach the decoder", err)
	}
}
