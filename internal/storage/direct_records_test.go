package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestDirectPayloadBudgetAndRepresentation(t *testing.T) {
	s, _, _ := ticketStores(t)
	z, _ := tree.NewZoneKey("0000000000000001", "a")
	records := []Record{
		{Collection: z.Collection, Location: "a", ID: "json", Payload: json.RawMessage(`{"tags":["demo"]}`)},
		{Collection: z.Collection, Location: "a", ID: "binary", Binary: true, Payload: json.RawMessage(`"Ymlu"`)},
		{Collection: z.Collection, Location: "a", ID: "empty", Binary: true, Payload: json.RawMessage(`""`)},
		{Collection: z.Collection, Location: "a", ID: "large", Payload: json.RawMessage(`"` + strings.Repeat("x", 2048) + `"`)},
	}
	if err := s.SeedLocalRecords(records); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Flush(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plain, err := s.DirectRecords(ctx, z, 4, 1024)
	if err != nil || len(plain) != 4 {
		t.Fatal(plain, err)
	}
	for _, r := range plain {
		if r.Binary || r.Payload != nil {
			t.Fatal(r.ID)
		}
	}
	if got, err := s.ReadDirectRecords(ctx, z, 4, 1024, true); !errors.Is(err, ErrDirectRecordBudget) || got != nil {
		t.Fatal(got, err)
	}
	got, err := s.ReadDirectRecords(ctx, z, 4, 4096, true)
	if err != nil || len(got) != 4 {
		t.Fatal(err, got)
	}
	for _, r := range got {
		var want *Record
		for i := range records {
			if records[i].ID == r.ID {
				want = &records[i]
				break
			}
		}
		if want == nil || want.Binary != r.Binary || !bytes.Equal(want.Payload, r.Payload) {
			t.Fatalf("unexpected %s body or encoding", r.ID)
		}
	}
}

// Blocking the row visitor leaves the read snapshot open across a committed
// write to the same address. The reader still sees its original snapshot.
func TestDirectRecordsDoNotBlockCommit(t *testing.T) {
	s, _, _ := ticketStores(t)
	z, _ := tree.NewZoneKey("0000000000000001", "aa")
	row := Record{Collection: z.Collection, Location: z.Location, ID: "a"}
	if _, _, _, err := s.ApplyRecord("first", row); err != nil {
		t.Fatal(err)
	}
	ctx := &pauseDirectRead{Context: context.Background(), entered: make(chan struct{}), resume: make(chan struct{})}
	resume := sync.OnceFunc(func() { close(ctx.resume) })
	defer resume()
	readDone := make(chan error, 1)
	go func() {
		rows, err := s.DirectRecords(ctx, z, 8, 4096)
		if err == nil && (len(rows) != 1 || rows[0].ID != "a") {
			err = fmt.Errorf("snapshot changed: %v", rows)
		}
		readDone <- err
	}()
	select {
	case <-ctx.entered:
	case err := <-readDone:
		t.Fatalf("read ended before reaching the row: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("read did not reach the row")
	}
	writeDone := make(chan error, 1)
	go func() {
		replacement := row
		replacement.ID = "b"
		_, _, _, err := s.ApplyRecord("second", replacement)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		resume()
		<-writeDone
		<-readDone
		t.Fatal("the write waited for the read to release its snapshot")
	}
	resume()
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	rows, err := s.DirectRecords(context.Background(), z, 8, 4096)
	if err != nil || len(rows) != 2 {
		t.Fatalf("next snapshot: %v, %v", rows, err)
	}
}

type pauseDirectRead struct {
	context.Context
	entered, resume chan struct{}
	checks          int
}

func (c *pauseDirectRead) Err() error {
	c.checks++
	if c.checks == 2 {
		close(c.entered)
		<-c.resume
	}
	return c.Context.Err()
}
