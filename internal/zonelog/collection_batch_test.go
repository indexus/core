package zonelog

import (
	"fmt"
	"math/rand"
	"testing"
)

type manifestCounter struct {
	FS
	writes int
}

func (fs *manifestCounter) Create(name string) (File, error) {
	if name == "MANIFEST.tmp" {
		fs.writes++
	}
	return fs.FS.Create(name)
}

func collectionBatch() []Record {
	var batch []Record
	for i := range 16 {
		batch = append(batch, Record{Collection: fmt.Sprintf("new-%d", i), Location: "aa", ID: "row", Metrics: []float64{float64(i + 1)}})
	}
	return batch
}

func TestBatchPersistsNewCollectionsWithOneManifest(t *testing.T) {
	fs := &manifestCounter{FS: newMemFS()}
	s, err := Open("", Options{FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := fs.writes
	batch := collectionBatch()
	if err := s.Apply(batch); err != nil {
		t.Fatal(err)
	}
	if got := fs.writes - before; got != 1 {
		t.Fatalf("new collection batch committed %d manifests, want one", got)
	}
	before = fs.writes
	if err := s.Apply(batch); err != nil {
		t.Fatal(err)
	}
	if fs.writes != before {
		t.Fatal("known collections rewrote the manifest")
	}
}

func TestNewCollectionBatchSurvivesPowerCuts(t *testing.T) {
	// Cover creation, write, file sync, rename, directory sync, log write
	// and log sync. Reopen may find the whole batch or none, never some of it.
	for cut := 1; cut <= 10; cut++ {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			fs := newMemFS()
			s, err := Open("", Options{FS: fs})
			if err != nil {
				t.Fatal(err)
			}
			fs.FailAt = fs.Ops + cut
			batch := collectionBatch()
			applyErr := s.Apply(batch)
			// A failed metadata commit cannot be bypassed by retrying with
			// the collection IDs still present in memory.
			if applyErr != nil {
				fs.FailAt = 0 // the IO failure clears, but provisional IDs remain
				if s.Apply(batch) == nil {
					t.Fatal("write succeeded with collection IDs from a failed commit")
				}
			}
			crashed := fs.crash(rand.New(rand.NewSource(int64(cut))), true)
			s.Close()
			reopened, err := Open("", Options{FS: crashed})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			found := 0
			for _, want := range batch {
				got, ok, err := reopened.Get(want.Collection, want.Location, want.ID)
				if err != nil {
					t.Fatal(err)
				}
				if ok {
					found++
					if len(got.Metrics) != 1 || got.Metrics[0] != want.Metrics[0] {
						t.Fatal("collection ID points at another collection's record")
					}
				}
			}
			if found != 0 && found != len(batch) || applyErr == nil && found != len(batch) {
				t.Fatalf("ACK error %v, recovered %d of %d records", applyErr, found, len(batch))
			}
			if err := reopened.Apply(batch); err != nil {
				t.Fatalf("cannot retry after restart: %v", err)
			}
		})
	}
}
