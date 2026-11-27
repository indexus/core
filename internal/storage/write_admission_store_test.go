package storage

import (
	"context"
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/internal/causal"
	"github.com/indexus/go-indexus-core/internal/tree"
	"path/filepath"
	"testing"
	"time"
)

func TestApplyRecordAdmissionCancellationAndReplay(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint("batch=", batch), func(t *testing.T) {
			store, err := OpenWithOptions(filepath.Join(t.TempDir(), "admission.db"), "D", "test", 1<<20, Options{BatchWrites: batch, BatchSize: 8, MaxPendingWrites: 2, MaxPendingBytes: 4096})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			holder, err := store.Endpoint("D")
			if err != nil {
				t.Fatal(err)
			}
			if err = store.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "test", Holder: holder}); err != nil {
				t.Fatal(err)
			}
			record := Record{Collection: "Demo000000000000", Location: "aa", ID: "row", Metrics: []float64{1}}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if _, _, _, err = store.ApplyRecordContext(canceled, "canceled", record); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			blocker := make(chan error, 1)
			go func() {
				blocker <- store.writes.submit(context.Background(), 1, func(*Tx) error { close(entered); <-release; return nil })
			}()
			<-entered
			type outcome struct {
				mutation Mutation
				applied  bool
				err      error
			}
			done := make(chan outcome, 1)
			accepted, cancelAccepted := context.WithCancel(context.Background())
			defer cancelAccepted()
			go func() {
				m, _, a, e := store.ApplyRecordContext(accepted, "accepted", record)
				done <- outcome{m, a, e}
			}()
			waitDiskBatch(t, store.writes, func() bool { return len(store.writes.pending) == 1 })
			if _, _, _, err = store.ApplyRecordContext(context.Background(), "refused", record); !errors.Is(err, ErrWriteOverload) {
				t.Fatalf("full admission: %v", err)
			}
			cancelAccepted()
			select {
			case r := <-done:
				t.Fatalf("accepted write escaped before fsync: %+v", r)
			default:
			}
			close(release)
			if err = <-blocker; err != nil {
				t.Fatal(err)
			}
			result := <-done
			if result.err != nil || !result.applied {
				t.Fatalf("accepted result: %+v", result)
			}
			replay, _, applied, err := store.ApplyRecordContext(context.Background(), "accepted", record)
			if err != nil || applied || replay.Seq != result.mutation.Seq {
				t.Fatalf("idempotent replay: applied=%v seq=%d err=%v", applied, replay.Seq, err)
			}
			stats := assertPlanningParity(t, store)
			if len(stats) != 1 || stats[0].Items != 1 {
				t.Fatalf("accepted rows: %+v", stats)
			}
			oversized := record
			oversized.Payload = bigPayload(32 << 10)
			if _, _, _, err = store.ApplyRecordContext(context.Background(), "oversized", oversized); !errors.Is(err, ErrWriteOverload) {
				t.Fatalf("byte admission: %v", err)
			}
			waitDiskBatch(t, store.writes, func() bool { return store.writes.admitted == 0 && store.writes.admittedBytes == 0 })
		})
	}
}

func TestWriteAdmissionCapacityFailureReleasesBudgetAndReopensReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capacity.db")
	options := Options{BatchWrites: true, BatchSize: 16, BatchDelay: 10 * time.Millisecond, MaxPendingWrites: 4, MaxPendingBytes: 1 << 20}
	store, err := OpenWithOptions(path, "D", "test", 4096, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	holder, err := store.Endpoint("D")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.InitCausal(tree.VNodeDescriptor{Name: "D", ID: tree.ID{1}}, causal.Genesis{Cluster: "test", Holder: holder}); err != nil {
		t.Fatal(err)
	}
	good := Record{Collection: "Demo000000000000", Location: "aa", ID: "good", Metrics: []float64{1}}
	bad := good
	bad.ID = "full"
	bad.Payload = bigPayload(32 << 10)
	type answer struct {
		m       Mutation
		applied bool
		err     error
	}
	results := make(chan answer, 2)
	for _, r := range []Record{good, bad} {
		go func() {
			m, _, a, e := store.ApplyRecordContext(context.Background(), r.ID, r)
			results <- answer{m, a, e}
		}()
	}
	successes, refusals := 0, 0
	var sequence uint64
	for range 2 {
		r := <-results
		if errors.Is(r.err, ErrDataCapacity) {
			refusals++
		} else if r.err == nil && r.applied {
			successes++
			sequence = r.m.Seq
		} else {
			t.Fatalf("result: %+v", r)
		}
	}
	if successes != 1 || refusals != 1 {
		t.Fatalf("success=%d capacity=%d", successes, refusals)
	}
	waitDiskBatch(t, store.writes, func() bool { return store.writes.admitted == 0 && store.writes.admittedBytes == 0 })
	stats := assertPlanningParity(t, store)
	if len(stats) != 1 || stats[0].Items != 1 {
		t.Fatalf("capacity rollback leaked record: %+v", stats)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWithOptions(path, "D", "test", 4096, options)
	if err != nil {
		t.Fatal(err)
	}
	store = reopened
	replay, _, applied, err := store.ApplyRecordContext(context.Background(), "good", good)
	if err != nil || applied || replay.Seq != sequence {
		t.Fatalf("reopened replay applied=%v seq=%d err=%v", applied, replay.Seq, err)
	}
	next := good
	next.ID = "next"
	if _, _, applied, err = store.ApplyRecordContext(context.Background(), "next", next); err != nil || !applied {
		t.Fatalf("budget/capacity remained stuck after rollback: %v", err)
	}
	if got := assertPlanningParity(t, store); len(got) != 1 || got[0].Items != 2 {
		t.Fatalf("post-reopen rows: %+v", got)
	}
}
