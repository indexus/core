package zonelog

import (
	"fmt"
	"testing"
	"time"
)

// Stop the first merge, outside the store lock, while leaving WAL and flush
// I/O free. This isolates scheduling from device throughput and fsync latency.
type pausedMergeFS struct {
	FS
	entered, release chan struct{}
}

func (f *pausedMergeFS) Create(name string) (File, error) {
	if name == segName(5) {
		close(f.entered)
		<-f.release
	}
	return f.FS.Create(name)
}

func TestCommitsProgressDuringMerge(t *testing.T) {
	fs := &pausedMergeFS{FS: Dir(t.TempDir()), entered: make(chan struct{}), release: make(chan struct{})}
	s, err := Open("", Options{FS: fs, MemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	batch := func(group int) []Record {
		records := make([]Record, 100)
		for j := range records {
			records[j] = Record{Collection: testCollection, Location: "@", ID: fmt.Sprintf("%04d-%04d", group, j), Payload: make([]byte, 800)}
		}
		return records
	}
	for i := range 3 {
		if err = s.Apply(batch(i)); err != nil {
			t.Fatal(err)
		}
		if err = s.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Apply(batch(3)); err != nil {
		t.Fatal(err)
	}
	flushing := make(chan error, 1)
	go func() { flushing <- s.Flush() }()
	select {
	case <-fs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("merge did not start")
	}
	done := make(chan error, 1)
	go func() {
		for i := 4; i < 7; i++ {
			if err := s.Apply(batch(i)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var stalled bool
	select {
	case err = <-done:
	case <-time.After(500 * time.Millisecond):
		stalled = true
	}
	close(fs.release)
	if stalled {
		err = <-done
	}
	if e := <-flushing; e != nil {
		t.Error(e)
	}
	if err != nil {
		t.Error(err)
	}
	if err = s.Flush(); err != nil {
		t.Error(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// Publication of a flush while a merge is in flight must retain both on
	// reopen, with durable advancing only through flushed memtables.
	s, err = Open("", Options{FS: fs, MemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range 7 {
		for _, r := range batch(i) {
			if _, ok, e := s.Get(r.Collection, r.Location, r.ID); e != nil || !ok {
				t.Fatalf("reopen %s: present=%v err=%v", r.ID, ok, e)
			}
		}
	}
	if stalled {
		t.Fatal("commits waited >500ms for a merge with WAL and flush I/O available")
	}
}
